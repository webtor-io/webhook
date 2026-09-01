package services

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-pg/pg/v10"
	"github.com/pkg/errors"
	uuid "github.com/satori/go.uuid"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
	mb "github.com/webtor-io/webhook/models/billing"
	mnp "github.com/webtor-io/webhook/models/nowpayments"
)

const (
	npAPIKeyFlag      = "nowpayments-api-key"
	npIPNSecretFlag   = "nowpayments-ipn-secret"
	npAPIURLFlag      = "nowpayments-api-url"
	npIPNCallbackFlag = "nowpayments-ipn-callback-url"
	npSuccessURLFlag  = "nowpayments-success-url"
	npCancelURLFlag   = "nowpayments-cancel-url"

	npSigHeader           = "x-nowpayments-sig"
	npAPITimeout          = 30 * time.Second
	npOrderDescriptionFmt = "Webtor %s tier (%d days)"
)

func RegisterNowPaymentsFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   npAPIKeyFlag,
			Usage:  "NOWPayments API key (empty disables invoice creation)",
			Value:  "",
			EnvVar: "NOWPAYMENTS_API_KEY",
		},
		cli.StringFlag{
			Name:   npIPNSecretFlag,
			Usage:  "NOWPayments IPN secret (empty disables IPN processing)",
			Value:  "",
			EnvVar: "NOWPAYMENTS_IPN_SECRET",
		},
		cli.StringFlag{
			Name:   npAPIURLFlag,
			Usage:  "NOWPayments API base url (use https://api-sandbox.nowpayments.io for sandbox)",
			Value:  "https://api.nowpayments.io",
			EnvVar: "NOWPAYMENTS_API_URL",
		},
		cli.StringFlag{
			Name:   npIPNCallbackFlag,
			Usage:  "public url of the /nowpayments IPN endpoint",
			Value:  "",
			EnvVar: "NOWPAYMENTS_IPN_CALLBACK_URL",
		},
		cli.StringFlag{
			Name:   npSuccessURLFlag,
			Usage:  "url the user returns to after paying (payment_id query param is appended)",
			Value:  "",
			EnvVar: "NOWPAYMENTS_SUCCESS_URL",
		},
		cli.StringFlag{
			Name:   npCancelURLFlag,
			Usage:  "url the user returns to after cancelling checkout",
			Value:  "",
			EnvVar: "NOWPAYMENTS_CANCEL_URL",
		},
	)
}

// statusRank orders payment statuses so IPN retries and out-of-order callbacks
// can never move a payment backwards. finished outranks failed/expired: a user
// whose invoice lapsed (expired) can still have the on-chain transfer confirm
// afterwards, and that late finished must grant the membership. refunded may
// follow finished.
var statusRank = map[string]int{
	mb.StatusCreated:       0,
	mb.StatusWaiting:       1,
	mb.StatusConfirming:    2,
	mb.StatusConfirmed:     3,
	mb.StatusSending:       4,
	mb.StatusPartiallyPaid: 5,
	mb.StatusFailed:        6,
	mb.StatusExpired:       6,
	mb.StatusFinished:      7,
	mb.StatusRefunded:      8,
}

func canTransition(from, to string) bool {
	fr, ok := statusRank[from]
	if !ok {
		return false
	}
	tr, ok := statusRank[to]
	if !ok {
		return false
	}
	return tr > fr
}

type NowPayments struct {
	db          *cs.PG
	nats        *cs.NATS
	cl          *http.Client
	apiKey      string
	ipnSecret   string
	apiURL      string
	ipnCallback string
	successURL  string
	cancelURL   string
}

func NewNowPayments(c *cli.Context, db *cs.PG, nats *cs.NATS) *NowPayments {
	return &NowPayments{
		db:          db,
		nats:        nats,
		cl:          &http.Client{Timeout: npAPITimeout},
		apiKey:      c.String(npAPIKeyFlag),
		ipnSecret:   c.String(npIPNSecretFlag),
		apiURL:      strings.TrimRight(c.String(npAPIURLFlag), "/"),
		ipnCallback: c.String(npIPNCallbackFlag),
		successURL:  c.String(npSuccessURLFlag),
		cancelURL:   c.String(npCancelURLFlag),
	}
}

func (s *NowPayments) Close() {
}

// Enabled implements InvoiceProvider: invoice creation needs the API key.
func (s *NowPayments) Enabled() bool {
	return s.apiKey != ""
}

// HandleIPN processes NOWPayments payment notifications. It must stay
// idempotent: NOWPayments retries callbacks and may deliver them out of order.
func (s *NowPayments) HandleIPN(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.ipnSecret == "" {
		log.Error("nowpayments ipn secret not configured, rejecting ipn")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	bb, err := io.ReadAll(r.Body)
	if err != nil {
		log.WithError(err).Error("failed to read ipn body")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	sig := r.Header.Get(npSigHeader)
	if !s.validate(bb, sig) {
		log.WithField("signature", sig).Warn("nowpayments signature validation failed")
		w.WriteHeader(http.StatusForbidden)
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(bb, &payload); err != nil {
		log.WithError(err).Error("failed to unmarshal ipn body")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	db := s.db.Get()
	im := &mnp.Message{
		Payload:   payload,
		Signature: sig,
	}
	if _, err := db.Model(im).Insert(); err != nil {
		log.WithError(err).Error("failed to store ipn message")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	email, err := s.processIPN(r.Context(), payload)
	if err != nil {
		log.WithError(err).Errorf("failed to process ipn payload=%v", payload)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	if email != "" {
		s.publish(email)
	}
}

// processIPN applies the status transition and grants membership on finished.
// Returns the member email when a grant happened, so the caller can publish
// user.updated only after a real tier change.
func (s *NowPayments) processIPN(ctx context.Context, payload map[string]any) (string, error) {
	orderID := stringField(payload, "order_id")
	status := stringField(payload, "payment_status")
	if orderID == "" || status == "" {
		// Not an error: NOWPayments also sends non-payment notifications.
		log.WithField("payload", payload).Warn("ipn without order_id/payment_status, skipping")
		return "", nil
	}
	id, err := uuid.FromString(orderID)
	if err != nil {
		// Not ours (another store on the same account, sandbox leftovers).
		// Ack with 200 — a 5xx would only make the provider retry forever.
		log.WithField("order_id", orderID).Warn("ipn order_id is not a uuid, skipping")
		return "", nil
	}
	if _, ok := statusRank[status]; !ok {
		// A status this build doesn't know (provider added one): the raw
		// message is already stored — ack with 200, a 5xx would only make
		// the provider retry a callback we will never accept.
		log.WithField("payment_status", status).Warn("unknown payment_status, skipping")
		return "", nil
	}
	var grantedEmail string
	db := s.db.Get()
	err = db.RunInTransaction(ctx, func(tx *pg.Tx) error {
		grantedEmail = ""
		p := &mb.Payment{}
		err := tx.Model(p).Where("payment_id = ?", id).For("UPDATE").Select()
		if err == pg.ErrNoRows {
			// Unknown order: same ack-and-drop as a non-uuid order_id.
			log.WithField("payment_id", id).Warn("ipn for unknown payment, skipping")
			return nil
		}
		if err != nil {
			return errors.Wrap(err, "failed to select payment")
		}
		if !canTransition(p.Status, status) {
			log.WithField("payment_id", id).
				WithField("from", p.Status).
				WithField("to", status).
				Info("ignoring non-forward status transition")
			return nil
		}
		q := tx.Model(p).
			Set("status = ?", status).
			Set("updated_at = now()").
			Where("payment_id = ?", id)
		if v := stringField(payload, "payment_id"); v != "" {
			q = q.Set("provider_payment_id = ?", v)
		}
		if v := stringField(payload, "pay_currency"); v != "" {
			q = q.Set("pay_currency = ?", v)
		}
		if v, ok := floatField(payload, "actually_paid"); ok {
			q = q.Set("actually_paid = ?", v)
		}
		if _, err := q.Update(); err != nil {
			return errors.Wrap(err, "failed to update payment")
		}
		if status == mb.StatusRefunded && p.Status == mb.StatusFinished {
			// Refund policy is manual: the finished payment already granted
			// billing.member days — flag it for the operator instead of
			// auto-revoking (partial refunds, goodwill refunds).
			log.WithField("payment_id", id).
				WithField("email", p.Email).
				WithField("tier_id", p.TierID).
				Warn("payment refunded after finish — manual billing.member revocation may be required")
		}
		if status != mb.StatusFinished {
			return nil
		}
		email := strings.ToLower(p.Email)
		if _, err := tx.Exec(`
			INSERT INTO billing.member (email, tier_id, expire_at, updated_at)
			VALUES (?, ?, now() + make_interval(days => ?), now())
			ON CONFLICT (email, tier_id) DO UPDATE
			SET expire_at = GREATEST(billing.member.expire_at, now()) + make_interval(days => ?),
			    updated_at = now()
		`, email, p.TierID, p.PeriodDays, p.PeriodDays); err != nil {
			return errors.Wrap(err, "failed to upsert member")
		}
		grantedEmail = email
		log.WithField("payment_id", id).
			WithField("email", email).
			WithField("tier_id", p.TierID).
			WithField("period_days", p.PeriodDays).
			Info("membership granted")
		return nil
	})
	if err != nil {
		return "", err
	}
	return grantedEmail, nil
}

// CreateInvoice implements InvoiceProvider: it creates a hosted-checkout
// invoice at NOWPayments for an already-stored payment.
func (s *NowPayments) CreateInvoice(ctx context.Context, p *mb.Payment, tierName string) (invoiceID, invoiceURL string, err error) {
	successURL := s.successURL
	if successURL != "" {
		sep := "?"
		if strings.Contains(successURL, "?") {
			sep = "&"
		}
		successURL += sep + "payment_id=" + p.ID.String()
	}
	body := map[string]any{
		"price_amount":      p.AmountUSD,
		"price_currency":    "usd",
		"order_id":          p.ID.String(),
		"order_description": fmt.Sprintf(npOrderDescriptionFmt, tierName, p.PeriodDays),
		"ipn_callback_url":  s.ipnCallback,
		"success_url":       successURL,
		"cancel_url":        s.cancelURL,
	}
	bb, err := json.Marshal(body)
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiURL+"/v1/invoice", bytes.NewReader(bb))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", s.apiKey)
	res, err := s.cl.Do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	rb, err := io.ReadAll(res.Body)
	if err != nil {
		return "", "", err
	}
	if res.StatusCode != http.StatusOK {
		return "", "", errors.Errorf("nowpayments invoice request failed status=%v body=%v", res.StatusCode, string(rb))
	}
	var out map[string]any
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", "", errors.Wrapf(err, "failed to unmarshal invoice response=%v", string(rb))
	}
	invoiceID = stringField(out, "id")
	invoiceURL = stringField(out, "invoice_url")
	if invoiceURL == "" {
		return "", "", errors.Errorf("invoice response without invoice_url: %v", string(rb))
	}
	return invoiceID, invoiceURL, nil
}

func (s *NowPayments) publish(email string) {
	publishUserUpdated(s.nats, UserUpdated{Email: email, Source: "crypto"})
}

// validate checks x-nowpayments-sig: HMAC-SHA512 over the callback body
// re-serialized with recursively sorted keys (NOWPayments IPN contract).
func (s *NowPayments) validate(body []byte, sig string) bool {
	if sig == "" {
		return false
	}
	canonical, err := canonicalJSON(body)
	if err != nil {
		log.WithError(err).Warn("failed to canonicalize ipn body")
		return false
	}
	mac := hmac.New(sha512.New, []byte(s.ipnSecret))
	mac.Write(canonical)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(strings.ToLower(sig)))
}

// canonicalJSON re-serializes a JSON document with object keys sorted
// recursively, preserving number literals verbatim and without HTML escaping —
// matching JSON.stringify(sortObject(payload)) that NOWPayments signs.
func canonicalJSON(b []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(t))
	case json.Number:
		buf.WriteString(t.String())
	case string:
		writeJSONString(buf, t)
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeJSONString(buf, k)
			buf.WriteByte(':')
			if err := writeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return errors.Errorf("unexpected json value %T", v)
	}
	return nil
}

func writeJSONString(buf *bytes.Buffer, s string) {
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	// Encode of a string cannot fail; Encode appends a trailing newline.
	_ = enc.Encode(s)
	buf.Write(bytes.TrimRight(tmp.Bytes(), "\n"))
}

func stringField(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

func floatField(m map[string]any, key string) (float64, bool) {
	switch v := m[key].(type) {
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(v, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.WithError(err).Error("failed to write json response")
	}
}
