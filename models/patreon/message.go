package patreon

import (
	"time"

	uuid "github.com/satori/go.uuid"
)

type Message struct {
	tableName struct{}  `pg:"patreon.message,alias:pm"`
	ID        uuid.UUID `pg:"message_id,type:uuid,pk,default:uuid_generate_v4()"`
	Payload   Payload   `pg:",notnull"`
	Event     string    `pg:",notnull"`
	Signature []byte    `pg:",notnull"`
	CreatedAt time.Time `pg:",default:now()"`
}

type Payload map[string]interface{}
