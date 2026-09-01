package services

import (
	"encoding/json"
	"testing"

	mp "github.com/webtor-io/webhook/models/patreon"
)

func TestPatreon_GetEmail(t *testing.T) {
	s := &Patreon{}
	payload := mp.Payload{
		"data": map[string]interface{}{
			"id":   "c08b7505-4518-42fc-bccd-0623ff4438c1",
			"type": "member",
			"attributes": map[string]interface{}{
				"email": "vkwork1128@gmail.com",
			},
		},
	}

	expected := "vkwork1128@gmail.com"
	actual := s.getEmail(payload)

	if actual != expected {
		t.Errorf("Expected %s, got %s", expected, actual)
	}
}

func TestPatreon_GetEmail_Missing(t *testing.T) {
	s := &Patreon{}
	payload := mp.Payload{
		"data": map[string]interface{}{
			"attributes": map[string]interface{}{
				"full_name": "TOP7",
			},
		},
	}

	expected := ""
	actual := s.getEmail(payload)

	if actual != expected {
		t.Errorf("Expected empty string, got %s", actual)
	}
}

func TestUserUpdatedFromPatreon(t *testing.T) {
	p := mp.Payload{
		"data": map[string]interface{}{
			"attributes": map[string]interface{}{
				"email":            "user@example.com",
				"patron_status":    "active_patron",
				"is_free_trial":    true,
				"next_charge_date": "2026-09-09T00:00:00.000+00:00",
			},
		},
	}
	msg := userUpdatedFromPatreon(p, "members:create")
	if msg.Email != "user@example.com" || msg.Source != "patreon" || msg.Event != "members:create" {
		t.Fatalf("identity fields: %+v", msg)
	}
	if msg.PatronStatus != "active_patron" || msg.NextChargeDate != "2026-09-09T00:00:00.000+00:00" {
		t.Errorf("membership fields: %+v", msg)
	}
	if msg.IsFreeTrial == nil || !*msg.IsFreeTrial {
		t.Errorf("is_free_trial must be carried as a known true: %+v", msg)
	}
}

// The wire format must stay a superset of the old {"email"}: consumers that
// decode only Email keep working, and absent facts are omitted rather than
// sent as zero values a consumer could mistake for knowledge.
func TestUserUpdatedFromPatreon_MissingFieldsAreOmitted(t *testing.T) {
	p := mp.Payload{"data": map[string]interface{}{"attributes": map[string]interface{}{"email": "u@example.com"}}}
	b, err := json.Marshal(userUpdatedFromPatreon(p, ""))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if got != `{"email":"u@example.com","source":"patreon"}` {
		t.Errorf("unexpected wire format: %s", got)
	}
}
