package nowpayments

import (
	"time"

	uuid "github.com/satori/go.uuid"
)

// Message is a raw NOWPayments IPN callback stored verbatim after signature
// verification (mirrors patreon.Message).
type Message struct {
	tableName struct{}       `pg:"nowpayments.message,alias:nm"`
	ID        uuid.UUID      `pg:"message_id,type:uuid,pk,default:uuid_generate_v4()"`
	Payload   map[string]any `pg:",notnull"`
	Signature string         `pg:",notnull"`
	CreatedAt time.Time      `pg:",default:now()"`
}
