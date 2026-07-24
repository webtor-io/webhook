package lavatop

import (
	"time"

	uuid "github.com/satori/go.uuid"
)

// Message is a raw lava.top webhook callback stored verbatim after the shared
// key check (mirrors nowpayments.Message; lava.top webhooks carry no
// signature, so there is nothing else to keep).
type Message struct {
	tableName struct{}       `pg:"lavatop.message,alias:lm"`
	ID        uuid.UUID      `pg:"message_id,type:uuid,pk,default:uuid_generate_v4()"`
	Payload   map[string]any `pg:",notnull"`
	CreatedAt time.Time      `pg:",default:now()"`
}
