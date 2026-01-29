package services

import (
	mp "github.com/webtor-io/webhook/models/patreon"
	"testing"
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
