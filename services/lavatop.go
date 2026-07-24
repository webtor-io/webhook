package services

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	uuid "github.com/satori/go.uuid"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
	mlt "github.com/webtor-io/webhook/models/lavatop"
)

const (
	ltAPIKeyFlag     = "lavatop-api-key"
	ltWebhookKeyFlag = "lavatop-webhook-key"
	ltAPIURLFlag     = "lavatop-api-url"
	ltProductsFlag   = "lavatop-products"
	ltGraceDaysFlag  = "lavatop-grace-days"

	ltAPIKeyHeader = "X-Api-Key"
	ltAPITimeout   = 30 * time.Second

	// ltFallbackDays covers a success webhook whose contract carries no
	// subscriptionDetails.expiredAt (a one-time product, or lava-side lag):
	// grant a month rather than nothing.
	ltFallbackDays = 31
)

// lava.top webhook eventType values.
const (
	ltEventPaymentSuccess   = "payment.success"
	ltEventPaymentFailed    = "payment.failed"
	ltEventRecurringSuccess = "subscription.recurring.payment.success"
	ltEventRecurringFailed  = "subscription.recurring.payment.failed"
	ltEventCancelled        = "subscription.cancelled"
)

func RegisterLavatopFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   ltAPIKeyFlag,
			Usage:  "lava.top API key for contract lookups (empty disables webhook processing)",
			Value:  "",
			EnvVar: "LAVATOP_API_KEY",
		},
		cli.StringFlag{
			Name:   ltWebhookKeyFlag,
			Usage:  "shared key lava.top sends with webhooks, as X-Api-Key or Basic password (empty disables webhook processing)",
			Value:  "",
			EnvVar: "LAVATOP_WEBHOOK_KEY",
		},
		cli.StringFlag{
			Name:   ltAPIURLFlag,
			Usage:  "lava.top API base url",
			Value:  "https://gate.lava.top",
			EnvVar: "LAVATOP_API_URL",
		},
		cli.StringFlag{
			Name:   ltProductsFlag,
			Usage:  "productUUID:tierID comma-separated list mapping lava.top storefront products to tiers",
			Value:  "",
			EnvVar: "LAVATOP_PRODUCTS",
		},
		cli.IntFlag{
			Name:   ltGraceDaysFlag,
			Usage:  "days added on top of the lava.top subscription expiry (covers renewal retry window)",
			Value:  2,
			EnvVar: "LAVATOP_GRACE_DAYS",
		},
	)
}

// Lavatop grants billing.member access from lava.top storefront webhooks.
// Purchases happen entirely on the lava.top storefront (Patreon-style — there
// is no invoice/checkout API integration on our side): a success webhook is
// bound to a tier via the product id and to a user via the buyer email, and
// the granted period comes from the contract's subscriptionDetails.expiredAt
// looked up at lava.top. The GREATEST() upsert makes redelivered webhooks
// converge on the same expiry, so no dedup state is needed.
type Lavatop struct {
	db         *cs.PG
	nats       *cs.NATS
	cl         *http.Client
	apiKey     string
	webhookKey string
	apiURL     string
	products   map[string]int
	graceDays  int
}

func NewLavatop(c *cli.Context, db *cs.PG, nats *cs.NATS) *Lavatop {
	products, err := parseLavatopProducts(c.String(ltProductsFlag))
	if err != nil {
		log.WithError(err).Fatal("failed to parse lavatop products mapping")
	}
	return &Lavatop{
		db:         db,
		nats:       nats,
		cl:         &http.Client{Timeout: ltAPITimeout},
		apiKey:     c.String(ltAPIKeyFlag),
		webhookKey: c.String(ltWebhookKeyFlag),
		apiURL:     strings.TrimRight(c.String(ltAPIURLFlag), "/"),
		products:   products,
		graceDays:  c.Int(ltGraceDaysFlag),
	}
}

func (s *Lavatop) Close() {
}

// parseLavatopProducts parses "productUUID:tierID,..." into a lookup map.
func parseLavatopProducts(v string) (map[string]int, error) {
	products := map[string]int{}
	if strings.TrimSpace(v) == "" {
		return products, nil
	}
	for _, entry := range strings.Split(v, ",") {
		entry = strings.TrimSpace(entry)
		parts := strings.Split(entry, ":")
		if len(parts) != 2 {
			return nil, errors.Errorf("malformed products entry %q", entry)
		}
		id, err := uuid.FromString(parts[0])
		if err != nil {
			return nil, errors.Wrapf(err, "malformed product uuid in entry %q", entry)
		}
		tierID, err := strconv.Atoi(parts[1])
		if err != nil || tierID <= 0 {
			return nil, errors.Errorf("malformed tier id in entry %q", entry)
		}
		key := strings.ToLower(id.String())
		if _, ok := products[key]; ok {
			return nil, errors.Errorf("duplicate product %q", key)
		}
		products[key] = tierID
	}
	return products, nil
}

// HandleWebhook processes lava.top webhooks. It must stay idempotent:
// lava.top retries deliveries (~19 attempts on 4xx/5xx).
func (s *Lavatop) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.webhookKey == "" || s.apiKey == "" || len(s.products) == 0 {
		log.Error("lavatop is not fully configured, rejecting webhook")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if !s.authorized(r) {
		// lava.top's documented source is 158.160.60.174 — logged via
		// X-Forwarded-For for diagnostics only, never enforced.
		log.WithField("remote_addr", r.RemoteAddr).
			WithField("forwarded_for", r.Header.Get("X-Forwarded-For")).
			Warn("lavatop webhook auth failed")
		w.WriteHeader(http.StatusForbidden)
		return
	}
	bb, err := io.ReadAll(r.Body)
	if err != nil {
		log.WithError(err).Error("failed to read lavatop webhook body")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(bb, &payload); err != nil {
		log.WithError(err).Error("failed to unmarshal lavatop webhook body")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	db := s.db.Get()
	im := &mlt.Message{
		Payload: payload,
	}
	if _, err := db.Model(im).Insert(); err != nil {
		log.WithError(err).Error("failed to store lavatop message")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	email, err := s.processEvent(r.Context(), payload)
	if err != nil {
		log.WithError(err).Errorf("failed to process lavatop event payload=%v", payload)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	if email != "" {
		publishUserUpdated(s.nats, email)
	}
}

// authorized accepts the shared key either as an X-Api-Key header or as the
// HTTP Basic password — the two auth styles the lava.top dashboard offers.
func (s *Lavatop) authorized(r *http.Request) bool {
	key := []byte(s.webhookKey)
	if h := r.Header.Get(ltAPIKeyHeader); h != "" &&
		subtle.ConstantTimeCompare([]byte(h), key) == 1 {
		return true
	}
	if _, pass, ok := r.BasicAuth(); ok &&
		subtle.ConstantTimeCompare([]byte(pass), key) == 1 {
		return true
	}
	return false
}

// processEvent dispatches one webhook. Returns the member email when a grant
// happened, so the caller publishes user.updated only after a real change.
// Anything not ours or not understood is ack-and-dropped (the raw message is
// already stored): a 5xx would only make lava.top retry a delivery we will
// never accept.
func (s *Lavatop) processEvent(ctx context.Context, payload map[string]any) (string, error) {
	eventType := stringField(payload, "eventType")
	switch eventType {
	case ltEventPaymentSuccess, ltEventRecurringSuccess:
		return s.processSuccess(ctx, payload)
	case ltEventPaymentFailed, ltEventRecurringFailed:
		log.WithField("contract_id", stringField(payload, "contractId")).
			WithField("parent_contract_id", stringField(payload, "parentContractId")).
			WithField("error_message", stringField(payload, "errorMessage")).
			Info("lavatop payment failed")
		return "", nil
	case ltEventCancelled:
		// Access simply runs out at billing.member.expire_at.
		log.WithField("contract_id", stringField(payload, "contractId")).
			WithField("will_expire_at", stringField(payload, "willExpireAt")).
			Info("lavatop subscription cancelled")
		return "", nil
	default:
		log.WithField("event_type", eventType).Warn("unknown lavatop event type, skipping")
		return "", nil
	}
}

// lavatop payload status values that mean money actually arrived.
var ltSuccessStatuses = map[string]bool{
	"completed":           true,
	"subscription-active": true,
}

func (s *Lavatop) processSuccess(ctx context.Context, payload map[string]any) (string, error) {
	productID := strings.ToLower(nestedString(payload, "product", "id"))
	tierID, ok := s.products[productID]
	if !ok {
		// Another product on the same lava.top account.
		log.WithField("product_id", productID).Warn("lavatop event for unmapped product, skipping")
		return "", nil
	}
	email := strings.ToLower(strings.TrimSpace(nestedString(payload, "buyer", "email")))
	contractID := stringField(payload, "contractId")
	status := stringField(payload, "status")
	if email == "" || contractID == "" {
		log.WithField("payload", payload).Warn("lavatop success event without buyer email or contractId, skipping")
		return "", nil
	}
	if !ltSuccessStatuses[status] {
		log.WithField("status", status).
			WithField("contract_id", contractID).
			Warn("lavatop success event with non-success status, skipping")
		return "", nil
	}
	expireAt, err := s.fetchExpiredAt(ctx, contractID)
	if err != nil {
		// 5xx → lava.top retries with backoff, which also rides out the
		// lookup racing the contract becoming visible on their side.
		return "", errors.Wrapf(err, "failed to fetch contract %v", contractID)
	}
	if expireAt.IsZero() {
		log.WithField("contract_id", contractID).
			Warnf("lavatop contract without subscription expiry, granting %d days", ltFallbackDays)
		expireAt = time.Now().Add(ltFallbackDays * 24 * time.Hour)
	}
	expireAt = expireAt.Add(time.Duration(s.graceDays) * 24 * time.Hour)
	db := s.db.Get()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO billing.member (email, tier_id, expire_at, updated_at)
		VALUES (?, ?, ?, now())
		ON CONFLICT (email, tier_id) DO UPDATE
		SET expire_at = GREATEST(billing.member.expire_at, EXCLUDED.expire_at),
		    updated_at = now()
	`, email, tierID, expireAt); err != nil {
		return "", errors.Wrap(err, "failed to upsert member")
	}
	log.WithField("contract_id", contractID).
		WithField("email", email).
		WithField("tier_id", tierID).
		WithField("expire_at", expireAt).
		Info("lavatop membership granted")
	return email, nil
}

// fetchExpiredAt looks the contract up at lava.top and returns its
// subscriptionDetails.expiredAt (zero time when the contract carries none).
func (s *Lavatop) fetchExpiredAt(ctx context.Context, contractID string) (time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%v/api/v2/invoices/%v", s.apiURL, contractID), nil)
	if err != nil {
		return time.Time{}, err
	}
	req.Header.Set(ltAPIKeyHeader, s.apiKey)
	res, err := s.cl.Do(req)
	if err != nil {
		return time.Time{}, err
	}
	defer res.Body.Close()
	rb, err := io.ReadAll(res.Body)
	if err != nil {
		return time.Time{}, err
	}
	if res.StatusCode != http.StatusOK {
		return time.Time{}, errors.Errorf("lavatop contract request failed status=%v body=%v", res.StatusCode, string(rb))
	}
	return extractExpiredAt(rb)
}

// extractExpiredAt pulls subscriptionDetails.expiredAt out of an
// InvoiceResponseV3 body; absent/null yields a zero time without error.
func extractExpiredAt(body []byte) (time.Time, error) {
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return time.Time{}, errors.Wrapf(err, "failed to unmarshal contract response=%v", string(body))
	}
	v := nestedString(out, "subscriptionDetails", "expiredAt")
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, errors.Wrapf(err, "failed to parse contract expiredAt=%v", v)
	}
	return t, nil
}

// nestedString walks nested JSON objects and stringField's the final key.
func nestedString(m map[string]any, keys ...string) string {
	for i := 0; i < len(keys)-1; i++ {
		next, ok := m[keys[i]].(map[string]any)
		if !ok {
			return ""
		}
		m = next
	}
	return stringField(m, keys[len(keys)-1])
}
