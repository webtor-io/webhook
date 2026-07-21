package services

import (
	"encoding/json"

	log "github.com/sirupsen/logrus"
	cs "github.com/webtor-io/common-services"
)

// publishUserUpdated emits the user.updated event every membership source
// (Patreon, crypto) publishes after a change, so consumers (web-ui) drop
// their cached claims for the email.
func publishUserUpdated(nats *cs.NATS, email string) {
	if email == "" {
		return
	}
	if nats == nil {
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
	nc := nats.Get()
	if nc == nil {
		log.Error("failed to get nats connection")
		return
	}
	if err := nc.Publish("user.updated", b); err != nil {
		log.WithError(err).Error("failed to publish to nats")
		return
	}
	log.WithField("email", email).Info("published to nats")
}
