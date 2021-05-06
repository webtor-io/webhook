package services

import (
	"encoding/hex"
	"encoding/json"
	"io/ioutil"
	"net/http"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
	mp "github.com/webtor-io/webhook/models/patreon"

	"crypto/hmac"
	"crypto/md5"
)

const (
	patreonSecretFlag = "patreon-secret"
)

func RegisterPatreonFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   patreonSecretFlag,
			Usage:  "patreon secret",
			Value:  "",
			EnvVar: "PATREON_SECRET",
		},
	)
}

type Patreon struct {
	db     *cs.PG
	secret string
}

func NewPatreon(c *cli.Context, db *cs.PG) *Patreon {
	return &Patreon{
		secret: c.String(patreonSecretFlag),
		db:     db,
	}
}

func (s *Patreon) Handle(w http.ResponseWriter, r *http.Request) {
	db := s.db.Get()
	b := r.Body
	defer b.Close()
	bb, err := ioutil.ReadAll(b)
	if err != nil {
		log.WithError(err).Error("Failed to read request body")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	event := r.Header.Get("X-Patreon-Event")
	signature := r.Header.Get("X-Patreon-Signature")
	log.
		WithField("payload_len", len(bb)).
		WithField("event", event).
		WithField("signature", signature).
		Info("Request received")
	var p mp.Payload
	err = json.Unmarshal(bb, &p)
	if err != nil {
		log.WithError(err).Errorf("Failed to unmarshal request body=%v", bb)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	ds, err := hex.DecodeString(signature)
	if err != nil {
		log.WithError(err).Errorf("Failed to decode signature=%v", signature)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !s.validate(bb, ds, []byte(s.secret)) {
		log.Warn("Signature validation failed")
		w.WriteHeader(http.StatusForbidden)
		return
	}
	m := &mp.Message{
		Payload:   p,
		Event:     event,
		Signature: ds,
	}
	_, err = db.Model(m).Insert()
	if err != nil {
		log.WithError(err).Errorf("Failed to store patreon message=%+v", m)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	log.WithField("message", m).Info("Message stored")
	w.WriteHeader(http.StatusOK)
}

func (s *Patreon) validate(message, messageMAC, key []byte) bool {
	mac := hmac.New(md5.New, key)
	mac.Write(message)
	expectedMAC := mac.Sum(nil)
	return hmac.Equal(messageMAC, expectedMAC)
}
