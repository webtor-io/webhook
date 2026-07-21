package billing

import (
	"time"

	uuid "github.com/satori/go.uuid"
)

const (
	StatusCreated       = "created"
	StatusWaiting       = "waiting"
	StatusConfirming    = "confirming"
	StatusConfirmed     = "confirmed"
	StatusSending       = "sending"
	StatusPartiallyPaid = "partially_paid"
	StatusFinished      = "finished"
	StatusFailed        = "failed"
	StatusExpired       = "expired"
	StatusRefunded      = "refunded"
)

type Payment struct {
	tableName         struct{}  `pg:"billing.payment,alias:bp"`
	ID                uuid.UUID `pg:"payment_id,type:uuid,pk,default:uuid_generate_v4()"`
	Provider          string    `pg:",notnull,default:'nowpayments'"`
	UserID            string    `pg:"user_id,notnull"`
	Email             string    `pg:",notnull"`
	TierID            int       `pg:"tier_id,notnull,use_zero"`
	PeriodDays        int       `pg:",notnull"`
	AmountUSD         float64   `pg:"amount_usd,notnull"`
	Status            string    `pg:",notnull,default:'created'"`
	ProviderInvoiceID string    `pg:"provider_invoice_id"`
	ProviderPaymentID string    `pg:"provider_payment_id"`
	PayCurrency       string    `pg:"pay_currency"`
	ActuallyPaid      float64   `pg:"actually_paid"`
	InvoiceURL        string    `pg:"invoice_url"`
	CreatedAt         time.Time `pg:",default:now()"`
	UpdatedAt         time.Time `pg:",default:now()"`
}

type Member struct {
	tableName struct{}  `pg:"billing.member,alias:bm"`
	Email     string    `pg:",pk"`
	TierID    int       `pg:"tier_id,pk,use_zero"`
	ExpireAt  time.Time `pg:",notnull"`
	UpdatedAt time.Time `pg:",default:now()"`
}

type Price struct {
	tableName  struct{} `pg:"price,alias:pr"`
	TierID     int      `pg:"tier_id,pk,use_zero"`
	PeriodDays int      `pg:",pk"`
	AmountUSD  float64  `pg:"amount_usd,notnull"`
}
