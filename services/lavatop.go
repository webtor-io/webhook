package services

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-pg/pg/v10"
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
			Usage:  "comma-separated uuids of our lava.top storefront products (events for other products are ignored)",
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
// is no invoice/checkout API integration on our side). The subscription tiers
// are offers of ONE lava.top product, and the webhook payload only carries
// the product id — so the tier comes from the offer NAME on the contract
// looked up at lava.top ("Bronze Backer" → tier 'bronze', same first-word
// convention the patreon.member matview applies to Patreon tier titles), and
// the granted period from the contract's subscriptionDetails.expiredAt. The
// GREATEST() upsert makes redelivered webhooks converge on the same expiry,
// so no dedup state is needed.
type Lavatop struct {
	db         *cs.PG
	nats       *cs.NATS
	cl         *http.Client
	apiKey     string
	webhookKey string
	apiURL     string
	products   map[string]bool
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

// parseLavatopProducts parses a comma-separated product uuid list into an
// ours-filter set.
func parseLavatopProducts(v string) (map[string]bool, error) {
	products := map[string]bool{}
	if strings.TrimSpace(v) == "" {
		return products, nil
	}
	for _, entry := range strings.Split(v, ",") {
		entry = strings.TrimSpace(entry)
		id, err := uuid.FromString(entry)
		if err != nil {
			return nil, errors.Wrapf(err, "malformed product uuid %q", entry)
		}
		products[id.String()] = true
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

// lavatop payload status values that mean money actually arrived, and known
// failure values that legitimately carry no money. A status in NEITHER set on
// a success event is treated as an error (5xx → lava.top retries): it is most
// likely a success value this build doesn't know yet, and acking it would
// silently drop a paid sale.
var ltSuccessStatuses = map[string]bool{
	"completed":           true,
	"subscription-active": true,
}

var ltFailureStatuses = map[string]bool{
	"failed":              true,
	"subscription-failed": true,
}

func (s *Lavatop) processSuccess(ctx context.Context, payload map[string]any) (string, error) {
	productID := strings.ToLower(nestedString(payload, "product", "id"))
	if !s.products[productID] {
		// Another product on the same lava.top account.
		log.WithField("product_id", productID).Warn("lavatop event for unknown product, skipping")
		return "", nil
	}
	email := strings.ToLower(strings.TrimSpace(nestedString(payload, "buyer", "email")))
	contractID := stringField(payload, "contractId")
	status := stringField(payload, "status")
	if email == "" || contractID == "" {
		log.WithField("payload", payload).Warn("lavatop success event without buyer email or contractId, skipping")
		return "", nil
	}
	if _, err := uuid.FromString(contractID); err != nil {
		// Also keeps a crafted contractId out of the fetchContract URL path.
		log.WithField("contract_id", contractID).Warn("lavatop contractId is not a uuid, skipping")
		return "", nil
	}
	if ltFailureStatuses[status] {
		// Contradictory success event carrying a failure status — no money.
		log.WithField("status", status).
			WithField("contract_id", contractID).
			Warn("lavatop success event with failure status, skipping")
		return "", nil
	}
	if !ltSuccessStatuses[status] {
		return "", errors.Errorf("unknown status %q on success event for contract %v", status, contractID)
	}
	// A subscription must get its real expiry; only one-time products may use
	// the fallback period.
	isSubscription := status == "subscription-active" ||
		stringField(payload, "eventType") == ltEventRecurringSuccess
	// 5xx on any failure below → lava.top retries with backoff, which also
	// rides out the lookup racing the contract becoming visible on their
	// side. A paid sale must never be ack-and-dropped past this point.
	contract, err := s.fetchContract(ctx, contractID)
	if err != nil {
		return "", errors.Wrapf(err, "failed to fetch contract %v", contractID)
	}
	// A renewal's contractId is the child charge contract; when it comes back
	// without the offer or expiry, fall back to the parent subscription
	// contract from the payload.
	parentID := stringField(payload, "parentContractId")
	if parentID != "" && parentID != contractID &&
		(contract.OfferName == "" || (isSubscription && contract.ExpiredAt.IsZero())) {
		if _, err := uuid.FromString(parentID); err == nil {
			parent, err := s.fetchContract(ctx, parentID)
			if err != nil {
				return "", errors.Wrapf(err, "failed to fetch parent contract %v", parentID)
			}
			if contract.OfferName == "" {
				contract.OfferName = parent.OfferName
			}
			if contract.ExpiredAt.IsZero() {
				contract.ExpiredAt = parent.ExpiredAt
			}
			if contract.TerminatedAt == "" {
				contract.TerminatedAt = parent.TerminatedAt
			}
		}
	}
	if contract.TerminatedAt != "" {
		// Terminated on the lava.top side (refund/chargeback) before this
		// (re)delivery was processed — granting now would hand out access for
		// returned money. Retries won't change it, so ack.
		log.WithField("contract_id", contractID).
			WithField("email", email).
			WithField("terminated_at", contract.TerminatedAt).
			Warn("lavatop contract terminated, skipping grant")
		return "", nil
	}
	tierName := lavatopTierName(contract.OfferName)
	if tierName == "" {
		return "", errors.Errorf("contract %v without offer name, cannot resolve tier", contractID)
	}
	expireAt := contract.ExpiredAt
	if expireAt.IsZero() {
		if isSubscription {
			// Not yet populated on the lava.top side — 5xx and let the retry
			// ladder deliver the real expiry; a fallback grant here would ack
			// e.g. a yearly purchase with a month of access, unrepaired until
			// the renewal a year away.
			return "", errors.Errorf("subscription contract %v without expiry", contractID)
		}
		// One-time product: no expiry by design. Anchor the fallback to the
		// event timestamp, not time.Now(), so redeliveries converge.
		ts, err := time.Parse(time.RFC3339Nano, stringField(payload, "timestamp"))
		if err != nil {
			return "", errors.Wrapf(err, "cannot anchor fallback grant for contract %v", contractID)
		}
		log.WithField("contract_id", contractID).
			Warnf("lavatop contract without expiry, granting %d days", ltFallbackDays)
		expireAt = ts.Add(ltFallbackDays * 24 * time.Hour)
	}
	expireAt = expireAt.Add(time.Duration(s.graceDays) * 24 * time.Hour)
	db := s.db.Get()
	res, err := db.ExecContext(ctx, `
		INSERT INTO billing.member (email, tier_id, expire_at, updated_at)
		SELECT ?, t.tier_id, ?, now() FROM tier t WHERE t.name = ?
		ON CONFLICT (email, tier_id) DO UPDATE
		SET expire_at = GREATEST(billing.member.expire_at, EXCLUDED.expire_at),
		    updated_at = now()
	`, email, expireAt, tierName)
	if err != nil {
		return "", errors.Wrap(err, "failed to upsert member")
	}
	if res.RowsAffected() == 0 {
		// The offer name doesn't resolve to a tier (renamed in the lava.top
		// dashboard?) — fail loudly, lava.top keeps retrying meanwhile.
		return "", errors.Errorf("no tier named %q for contract %v offer %q", tierName, contractID, contract.OfferName)
	}
	log.WithField("contract_id", contractID).
		WithField("email", email).
		WithField("tier", tierName).
		WithField("expire_at", expireAt).
		Info("lavatop membership granted")
	return email, nil
}

// ltWordRe matches the first \w+ run, mirroring the PG substring() call in
// the patreon.member matview.
var ltWordRe = regexp.MustCompile(`\w+`)

// lavatopTierName maps an offer name to a tier table name by its first word —
// "Bronze Backer" → "bronze" — the same lower(substring(name, '\w+'))
// convention the patreon.member matview applies to Patreon tier titles (see
// manual_ddl_snapshot.sql).
func lavatopTierName(offerName string) string {
	return strings.ToLower(ltWordRe.FindString(offerName))
}

// lavatopContract is what we need from an InvoiceResponseV2/V3 contract.
type lavatopContract struct {
	// OfferName is product.offer — the subscription level, e.g. "Bronze Backer".
	OfferName string
	// ExpiredAt is subscriptionDetails.expiredAt; zero when the contract
	// carries none (one-time product).
	ExpiredAt time.Time
	// TerminatedAt is subscriptionDetails.terminatedAt verbatim; non-empty
	// means access was closed on the lava.top side (refund/chargeback).
	TerminatedAt string
}

// fetchContract looks the contract up at lava.top.
func (s *Lavatop) fetchContract(ctx context.Context, contractID string) (*lavatopContract, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%v/api/v2/invoices/%v", s.apiURL, contractID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(ltAPIKeyHeader, s.apiKey)
	res, err := s.cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	rb, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, errors.Errorf("lavatop contract request failed status=%v body=%v", res.StatusCode, string(rb))
	}
	return extractContract(rb)
}

// extractContract pulls the offer name and subscriptionDetails.expiredAt out
// of a contract response body; an absent/null expiry yields a zero time
// without error.
func extractContract(body []byte) (*lavatopContract, error) {
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal contract response=%v", string(body))
	}
	c := &lavatopContract{
		OfferName:    nestedString(out, "product", "offer"),
		TerminatedAt: nestedString(out, "subscriptionDetails", "terminatedAt"),
	}
	v := nestedString(out, "subscriptionDetails", "expiredAt")
	if v == "" {
		return c, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse contract expiredAt=%v", v)
	}
	c.ExpiredAt = t
	return c, nil
}

// CheckOffers verifies in the background that every offer of the configured
// products resolves to a tier row, so a dashboard rename is caught at deploy
// time instead of at the first paid sale. Non-fatal: the webhook service also
// serves Patreon/NOWPayments and must come up even when lava.top is down.
func (s *Lavatop) CheckOffers() {
	if s.apiKey == "" || len(s.products) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), ltAPITimeout)
		defer cancel()
		names, err := s.fetchOfferNames(ctx)
		if err != nil {
			log.WithError(err).Error("failed to check lavatop offers against tiers")
			return
		}
		db := s.db.Get()
		ok := true
		for _, name := range names {
			tierName := lavatopTierName(name)
			var exists bool
			if _, err := db.QueryContext(ctx, pg.Scan(&exists),
				"SELECT EXISTS(SELECT 1 FROM tier WHERE name = ?)", tierName); err != nil {
				log.WithError(err).Error("failed to check lavatop offer tier")
				return
			}
			if !exists {
				ok = false
				log.WithField("offer", name).
					WithField("tier", tierName).
					Error("lavatop offer does not resolve to a tier — grants for it WILL fail; fix the offer name in the lava.top dashboard or the tier table")
			}
		}
		if ok {
			log.WithField("offers", len(names)).Info("lavatop offers resolve to tiers")
		}
	}()
}

// fetchOfferNames lists the offer names of the configured products from the
// lava.top products API (tolerating both the flat and the {type,data} item
// shapes the endpoint has been seen returning).
func (s *Lavatop) fetchOfferNames(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.apiURL+"/api/v2/products", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(ltAPIKeyHeader, s.apiKey)
	res, err := s.cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	rb, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, errors.Errorf("lavatop products request failed status=%v body=%v", res.StatusCode, string(rb))
	}
	return extractOfferNames(rb, s.products)
}

func extractOfferNames(body []byte, products map[string]bool) ([]string, error) {
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal products response")
	}
	items, _ := out["items"].([]any)
	var names []string
	for _, it := range items {
		node, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if data, ok := node["data"].(map[string]any); ok {
			node = data
		}
		if !products[strings.ToLower(stringField(node, "id"))] {
			continue
		}
		offers, _ := node["offers"].([]any)
		for _, of := range offers {
			offer, ok := of.(map[string]any)
			if !ok {
				continue
			}
			if name := stringField(offer, "name"); name != "" {
				names = append(names, name)
			}
		}
	}
	if len(names) == 0 {
		return nil, errors.New("no offers found for the configured lavatop products")
	}
	return names, nil
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
