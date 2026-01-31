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
	nats   *cs.NATS
}

func NewPatreon(c *cli.Context, db *cs.PG, nats *cs.NATS) *Patreon {
	return &Patreon{
		secret: c.String(patreonSecretFlag),
		db:     db,
		nats:   nats,
	}
}

func (s *Patreon) Handle(w http.ResponseWriter, r *http.Request) {
	db := s.db.Get()
	b := r.Body
	defer b.Close()
	bb, err := ioutil.ReadAll(b)
	if err != nil {
		log.WithError(err).Error("failed to read request body")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	event := r.Header.Get("X-Patreon-Event")
	signature := r.Header.Get("X-Patreon-Signature")
	log.
		WithField("payload_len", len(bb)).
		WithField("event", event).
		WithField("signature", signature).
		Info("request received")
	var p mp.Payload
	err = json.Unmarshal(bb, &p)
	if err != nil {
		log.WithError(err).Errorf("failed to unmarshal request body=%v", bb)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	ds, err := hex.DecodeString(signature)
	if err != nil {
		log.WithError(err).Errorf("failed to decode signature=%v", signature)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !s.validate(bb, ds, []byte(s.secret)) {
		log.Warn("signature validation failed")
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
		log.WithError(err).Errorf("failed to store patreon message=%+v", m)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	log.WithField("message", m).Info("message stored")
	s.publish(p)
	w.WriteHeader(http.StatusOK)
}

func (s *Patreon) publish(p mp.Payload) {
	email := s.getEmail(p)
	if email == "" {
		return
	}
	if s.nats == nil {
		log.WithField("email", email).Info("nats service not configured, skipping publish")
		return
	}
	msg := struct {
		Email string `json:"email"`
	}{
		Email: email,
	}
	b, err := json.Marshal(msg)
	if err != nil {
		log.WithError(err).Error("failed to marshal nats message")
		return
	}
	nc := s.nats.Get()
	if nc == nil {
		log.Error("failed to get nats connection")
		return
	}
	err = nc.Publish("user.updated", b)
	if err != nil {
		log.WithError(err).Error("failed to publish to nats")
		return
	}
	log.WithField("email", email).Info("published to nats")
}

func (s *Patreon) getEmail(p mp.Payload) string {
	data, ok := p["data"].(map[string]interface{})
	if !ok {
		return ""
	}
	attrs, ok := data["attributes"].(map[string]interface{})
	if !ok {
		return ""
	}
	email, ok := attrs["email"].(string)
	if !ok {
		return ""
	}
	return email
}

func (s *Patreon) Close() {
}

func (s *Patreon) validate(message, messageMAC, key []byte) bool {
	mac := hmac.New(md5.New, key)
	mac.Write(message)
	expectedMAC := mac.Sum(nil)
	return hmac.Equal(messageMAC, expectedMAC)
}
