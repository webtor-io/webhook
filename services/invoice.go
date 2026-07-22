package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-pg/pg/v10"
	uuid "github.com/satori/go.uuid"
	log "github.com/sirupsen/logrus"
	cs "github.com/webtor-io/common-services"
	mb "github.com/webtor-io/webhook/models/billing"
)

// InvoiceProvider creates a hosted-checkout invoice for a payment at a
// concrete payment provider and returns the provider's invoice id and the
// checkout url.
type InvoiceProvider interface {
	// Enabled reports whether the provider is configured; a disabled
	// provider must be rejected before any payment row is created.
	Enabled() bool
	CreateInvoice(ctx context.Context, p *mb.Payment, tierName string) (providerInvoiceID, url string, err error)
}

// Invoice is the provider-agnostic payment API consumed by web-ui:
//
//	PUT /invoice/{id}   create (idempotent — the caller supplies the uuid)
//	GET /invoice/{id}   payment state
//	GET /prices         purchasable plans
//
// There is deliberately no auth here, matching the other cluster-internal
// webtor services. The ONLY thing keeping these routes off the internet is
// the ingress path whitelist (values: paths ['/patreon', '/nowpayments']) —
// never widen it back to '/'.
type Invoice struct {
	db        *cs.PG
	providers map[string]InvoiceProvider
}

func NewInvoice(db *cs.PG, providers map[string]InvoiceProvider) *Invoice {
	return &Invoice{
		db:        db,
		providers: providers,
	}
}

func (s *Invoice) RegisterHandlers(web *Web) {
	web.RegisterProvider("PUT /invoice/{id}", s.handlePut)
	web.RegisterProvider("GET /invoice/{id}", s.handleGet)
	web.RegisterProvider("GET /invoices", s.handleList)
	web.RegisterProvider("GET /prices", s.handlePrices)
}

type putInvoiceRequest struct {
	Provider   string `json:"provider"`
	UserID     string `json:"user_id"`
	Email      string `json:"email"`
	TierID     int    `json:"tier_id"`
	PeriodDays int    `json:"period_days"`
}

type invoiceResponse struct {
	ID         string  `json:"id"`
	Provider   string  `json:"provider"`
	UserID     string  `json:"user_id"`
	Status     string  `json:"status"`
	TierID     int     `json:"tier_id"`
	PeriodDays int     `json:"period_days"`
	AmountUSD  float64 `json:"amount_usd"`
	URL        string  `json:"url"`
}

func invoiceResponseFromPayment(p *mb.Payment) invoiceResponse {
	return invoiceResponse{
		ID:         p.ID.String(),
		Provider:   p.Provider,
		UserID:     p.UserID,
		Status:     p.Status,
		TierID:     p.TierID,
		PeriodDays: p.PeriodDays,
		AmountUSD:  p.AmountUSD,
		URL:        p.InvoiceURL,
	}
}

// handlePut creates an invoice for the caller-supplied uuid. A repeated PUT
// with an id that already exists returns the stored invoice, so retries after
// network hiccups never double-charge.
func (s *Invoice) handlePut(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.FromString(r.PathValue("id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var req putInvoiceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	provider, ok := s.providers[req.Provider]
	if !ok || req.UserID == "" || req.Email == "" || req.PeriodDays <= 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !provider.Enabled() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	db := s.db.Get()

	price := &mb.Price{}
	err = db.Model(price).
		Where("tier_id = ?", req.TierID).
		Where("period_days = ?", req.PeriodDays).
		Where("available").
		Select()
	if err == pg.ErrNoRows {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		log.WithError(err).Error("failed to select price")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	// Insert-first: two concurrent PUTs with the same id both survive this
	// (the loser's insert is a no-op) and converge on the same row below.
	p := &mb.Payment{
		ID:         id,
		Provider:   req.Provider,
		UserID:     req.UserID,
		Email:      strings.ToLower(req.Email),
		TierID:     req.TierID,
		PeriodDays: req.PeriodDays,
		AmountUSD:  price.AmountUSD,
		Status:     mb.StatusCreated,
	}
	if _, err := db.Model(p).OnConflict("(payment_id) DO NOTHING").Insert(); err != nil {
		log.WithError(err).Error("failed to insert payment")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if err := db.Model(p).Where("payment_id = ?", id).Select(); err != nil {
		log.WithError(err).Error("failed to select payment")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	// Anything past 'created' (or 'created' with an invoice already attached)
	// is a completed earlier attempt — the idempotent-retry answer. A bare
	// 'created' row means the provider call never succeeded (fresh insert,
	// crashed pod, provider 5xx) — (re)try it now.
	if p.Status != mb.StatusCreated || p.InvoiceURL != "" {
		writeJSON(w, invoiceResponseFromPayment(p))
		return
	}
	providerInvoiceID, invoiceURL, err := provider.CreateInvoice(r.Context(), p, s.tierName(p.TierID))
	if err != nil {
		log.WithError(err).Error("failed to create provider invoice")
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	// The status guard keeps this from racing an IPN backwards: if a callback
	// already advanced the payment while the provider call was in flight,
	// this update matches nothing and the stored state wins.
	res, err := db.Model((*mb.Payment)(nil)).
		Set("provider_invoice_id = ?", providerInvoiceID).
		Set("invoice_url = ?", invoiceURL).
		Set("status = ?", mb.StatusWaiting).
		Set("updated_at = now()").
		Where("payment_id = ?", id).
		Where("status = ?", mb.StatusCreated).
		Update()
	if err != nil {
		log.WithError(err).Error("failed to update payment with invoice")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if res.RowsAffected() == 0 {
		if err := db.Model(p).Where("payment_id = ?", id).Select(); err != nil {
			log.WithError(err).Error("failed to reselect payment")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(w, invoiceResponseFromPayment(p))
		return
	}
	p.ProviderInvoiceID = providerInvoiceID
	p.InvoiceURL = invoiceURL
	p.Status = mb.StatusWaiting
	log.WithField("payment_id", p.ID).
		WithField("provider", req.Provider).
		WithField("email", p.Email).
		Info("invoice created")
	writeJSON(w, invoiceResponseFromPayment(p))
}

func (s *Invoice) handleGet(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.FromString(r.PathValue("id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	db := s.db.Get()
	p := &mb.Payment{}
	err = db.Model(p).Where("payment_id = ?", id).Select()
	if err == pg.ErrNoRows {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		log.WithError(err).Error("failed to select payment")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	writeJSON(w, invoiceResponseFromPayment(p))
}

type listInvoicesResponse struct {
	Invoices []invoiceListItem `json:"invoices"`
}

type invoiceListItem struct {
	ID          string    `json:"id" pg:"id"`
	Provider    string    `json:"provider" pg:"provider"`
	Status      string    `json:"status" pg:"status"`
	TierID      int       `json:"tier_id" pg:"tier_id"`
	TierName    string    `json:"tier_name" pg:"tier_name"`
	PeriodDays  int       `json:"period_days" pg:"period_days"`
	AmountUSD   float64   `json:"amount_usd" pg:"amount_usd"`
	PayCurrency string    `json:"pay_currency" pg:"pay_currency"`
	URL         string    `json:"url" pg:"url"`
	CreatedAt   time.Time `json:"created_at" pg:"created_at"`
}

// handleList returns the payment history of one user, newest first.
func (s *Invoice) handleList(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	db := s.db.Get()
	items := []invoiceListItem{}
	_, err := db.Query(&items, `
		SELECT p.payment_id::text AS id, p.provider, p.status, p.tier_id,
		       COALESCE(t.name, '') AS tier_name, p.period_days, p.amount_usd,
		       COALESCE(p.pay_currency, '') AS pay_currency,
		       COALESCE(p.invoice_url, '') AS url, p.created_at
		FROM billing.payment p
		LEFT JOIN tier t ON t.tier_id = p.tier_id
		WHERE p.user_id = ?
		ORDER BY p.created_at DESC
		LIMIT 100
	`, userID)
	if err != nil {
		log.WithError(err).Error("failed to select payments")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	writeJSON(w, listInvoicesResponse{Invoices: items})
}

type listPricesResponse struct {
	Prices []priceItem `json:"prices"`
}

type priceItem struct {
	TierID     int     `json:"tier_id" pg:"tier_id"`
	TierName   string  `json:"tier_name" pg:"tier_name"`
	PeriodDays int     `json:"period_days" pg:"period_days"`
	AmountUSD  float64 `json:"amount_usd" pg:"amount_usd"`
	Available  bool    `json:"available" pg:"available"`
}

func (s *Invoice) handlePrices(w http.ResponseWriter, r *http.Request) {
	db := s.db.Get()
	var items []priceItem
	_, err := db.Query(&items, `
		SELECT p.tier_id, t.name AS tier_name, p.period_days, p.amount_usd, p.available
		FROM price p
		JOIN tier t ON t.tier_id = p.tier_id
		ORDER BY p.tier_id, p.period_days
	`)
	if err != nil {
		log.WithError(err).Error("failed to select prices")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	writeJSON(w, listPricesResponse{Prices: items})
}

// tierName resolves the display name from the manually-managed tier table;
// the invoice still gets created if the lookup fails.
func (s *Invoice) tierName(tierID int) string {
	db := s.db.Get()
	var name string
	_, err := db.Query(pg.Scan(&name), "SELECT name FROM tier WHERE tier_id = ?", tierID)
	if err != nil || name == "" {
		log.WithError(err).WithField("tier_id", tierID).Warn("failed to resolve tier name")
		return fmt.Sprintf("tier %d", tierID)
	}
	return name
}
