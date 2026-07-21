package services

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	mb "github.com/webtor-io/webhook/models/billing"
)

func TestNowPayments_CanonicalJSON(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in       string
		expected string
	}{
		{
			name:     "sorts keys recursively",
			in:       `{"b":1,"a":{"z":true,"y":null}}`,
			expected: `{"a":{"y":null,"z":true},"b":1}`,
		},
		{
			name:     "preserves number literals",
			in:       `{"actually_paid":0.00500000,"payment_id":4945313071}`,
			expected: `{"actually_paid":0.00500000,"payment_id":4945313071}`,
		},
		{
			name:     "no html escaping",
			in:       `{"url":"https://a.b/c?d=1&e=<2>"}`,
			expected: `{"url":"https://a.b/c?d=1&e=<2>"}`,
		},
		{
			name:     "arrays keep order",
			in:       `{"a":[{"b":2,"a":1},3]}`,
			expected: `{"a":[{"a":1,"b":2},3]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := canonicalJSON([]byte(tc.in))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(out) != tc.expected {
				t.Errorf("expected %s, got %s", tc.expected, out)
			}
		})
	}
}

func TestNowPayments_Validate(t *testing.T) {
	secret := "test-ipn-secret"
	s := &NowPayments{ipnSecret: secret}
	body := []byte(`{"payment_status":"finished","order_id":"c08b7505-4518-42fc-bccd-0623ff4438c1","actually_paid":0.0050000}`)
	// Signature over the canonical (key-sorted) form, computed independently.
	canonical := `{"actually_paid":0.0050000,"order_id":"c08b7505-4518-42fc-bccd-0623ff4438c1","payment_status":"finished"}`
	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write([]byte(canonical))
	sig := hex.EncodeToString(mac.Sum(nil))

	if !s.validate(body, sig) {
		t.Error("expected valid signature")
	}
	if !s.validate(body, strings.ToUpper(sig)) {
		t.Error("expected upper-case signature to verify")
	}
	if s.validate(body, sig[:len(sig)-2]+"00") {
		t.Error("expected tampered signature to fail")
	}
	if s.validate([]byte(`{"payment_status":"waiting"}`), sig) {
		t.Error("expected signature over different body to fail")
	}
	if s.validate(body, "") {
		t.Error("expected empty signature to fail")
	}
	if s.validate([]byte(`not-json`), sig) {
		t.Error("expected non-json body to fail")
	}
}

func TestNowPayments_CanTransition(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		expected bool
	}{
		{mb.StatusCreated, mb.StatusWaiting, true},
		{mb.StatusWaiting, mb.StatusFinished, true},
		{mb.StatusPartiallyPaid, mb.StatusFinished, true},
		{mb.StatusFinished, mb.StatusRefunded, true},
		{mb.StatusFinished, mb.StatusFinished, false},
		{mb.StatusFinished, mb.StatusWaiting, false},
		{mb.StatusFinished, mb.StatusFailed, false},
		// A late on-chain confirmation must still grant after the invoice
		// lapsed or was marked failed.
		{mb.StatusExpired, mb.StatusFinished, true},
		{mb.StatusFailed, mb.StatusFinished, true},
		{mb.StatusExpired, mb.StatusFailed, false},
		{mb.StatusConfirmed, mb.StatusConfirming, false},
		{"bogus", mb.StatusFinished, false},
		{mb.StatusCreated, "bogus", false},
	} {
		if got := canTransition(tc.from, tc.to); got != tc.expected {
			t.Errorf("canTransition(%s, %s): expected %v, got %v", tc.from, tc.to, tc.expected, got)
		}
	}
}

func TestNowPayments_Fields(t *testing.T) {
	payload := map[string]any{}
	body := []byte(`{"payment_id":4945313071,"actually_paid":"0.005","pay_currency":"btc"}`)
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if got := stringField(payload, "payment_id"); got != "4945313071" {
		t.Errorf("expected 4945313071, got %s", got)
	}
	if got := stringField(payload, "pay_currency"); got != "btc" {
		t.Errorf("expected btc, got %s", got)
	}
	if got, ok := floatField(payload, "actually_paid"); !ok || got != 0.005 {
		t.Errorf("expected 0.005, got %v (ok=%v)", got, ok)
	}
	if _, ok := floatField(payload, "missing"); ok {
		t.Error("expected missing field to report !ok")
	}
}
