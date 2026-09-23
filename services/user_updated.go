package services

import (
	"encoding/json"

	log "github.com/sirupsen/logrus"
	cs "github.com/webtor-io/common-services"
)

// UserUpdated is the user.updated event. Email is the only field consumers
// have always had; everything else is optional and was added so a consumer
// can tell WHAT changed without re-reading the raw Patreon message — a
// welcome letter needs to know whether this is a trial and when it charges.
// Fields absent from a source (crypto has no trial) are omitted, so old
// consumers that decode only Email keep working unchanged.
type UserUpdated struct {
	Email string `json:"email"`
	// Source names the membership provider: "patreon" or "crypto".
	Source string `json:"source,omitempty"`
	// Event is the provider's own event type (Patreon: X-Patreon-Event,
	// e.g. members:create, members:pledge:create).
	Event string `json:"event,omitempty"`
	// PatronStatus mirrors Patreon's patron_status (active_patron, …).
	PatronStatus string `json:"patron_status,omitempty"`
	// IsFreeTrial is Patreon's is_free_trial; nil when unknown.
	IsFreeTrial *bool `json:"is_free_trial,omitempty"`
	// NextChargeDate is Patreon's next_charge_date verbatim (RFC 3339
	// timestamp string); empty when absent.
	NextChargeDate string `json:"next_charge_date,omitempty"`
	// LastChargeStatus is Patreon's last_charge_status (Paid, Declined,
	// Fraud, …); empty when absent.
	LastChargeStatus string `json:"last_charge_status,omitempty"`
	// LifetimeSupportCents is Patreon's campaign_lifetime_support_cents:
	// what the member has actually paid this campaign. 0 means no charge has
	// ever gone through — the one fact that tells a failed FIRST charge (the
	// end of a trial) from a failed renewal. nil when unknown.
	LifetimeSupportCents *int `json:"lifetime_support_cents,omitempty"`
}

// publishUserUpdated emits the user.updated event every membership source
// (Patreon, crypto) publishes after a change, so consumers (web-ui) drop
// their cached claims for the email and can react to the change itself.
func publishUserUpdated(nats *cs.NATS, msg UserUpdated) {
	if msg.Email == "" {
		return
	}
	if nats == nil {
		log.WithField("email", msg.Email).Info("nats service not configured, skipping publish")
		return
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
	log.WithField("email", msg.Email).WithField("event", msg.Event).Info("published to nats")
}
