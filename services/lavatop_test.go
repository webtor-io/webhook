package services

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLavatop_ParseProducts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		wantErr bool
		want    map[string]int
	}{
		{
			name: "empty",
			in:   "",
			want: map[string]int{},
		},
		{
			name: "valid list with spaces and mixed case",
			in:   "D31384B8-e412-4be5-a2ec-297ae6666c8f:1, 72d53efb-3696-469f-b856-f0d815748dd6:3",
			want: map[string]int{
				"d31384b8-e412-4be5-a2ec-297ae6666c8f": 1,
				"72d53efb-3696-469f-b856-f0d815748dd6": 3,
			},
		},
		{
			name:    "missing tier",
			in:      "d31384b8-e412-4be5-a2ec-297ae6666c8f",
			wantErr: true,
		},
		{
			name:    "bad uuid",
			in:      "not-a-uuid:1",
			wantErr: true,
		},
		{
			name:    "bad tier",
			in:      "d31384b8-e412-4be5-a2ec-297ae6666c8f:bronze",
			wantErr: true,
		},
		{
			name:    "zero tier",
			in:      "d31384b8-e412-4be5-a2ec-297ae6666c8f:0",
			wantErr: true,
		},
		{
			name:    "duplicate product",
			in:      "d31384b8-e412-4be5-a2ec-297ae6666c8f:1,d31384b8-e412-4be5-a2ec-297ae6666c8f:2",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLavatopProducts(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, got)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("expected %v=%v, got %v", k, v, got[k])
				}
			}
		})
	}
}

func TestLavatop_Authorized(t *testing.T) {
	s := &Lavatop{webhookKey: "secret-key"}

	r := httptest.NewRequest("POST", "/lavatop", nil)
	r.Header.Set(ltAPIKeyHeader, "secret-key")
	if !s.authorized(r) {
		t.Error("expected X-Api-Key auth to pass")
	}

	r = httptest.NewRequest("POST", "/lavatop", nil)
	r.SetBasicAuth("lava", "secret-key")
	if !s.authorized(r) {
		t.Error("expected Basic auth to pass")
	}

	r = httptest.NewRequest("POST", "/lavatop", nil)
	r.Header.Set(ltAPIKeyHeader, "wrong")
	if s.authorized(r) {
		t.Error("expected wrong X-Api-Key to fail")
	}

	r = httptest.NewRequest("POST", "/lavatop", nil)
	r.SetBasicAuth("lava", "wrong")
	if s.authorized(r) {
		t.Error("expected wrong Basic password to fail")
	}

	r = httptest.NewRequest("POST", "/lavatop", nil)
	if s.authorized(r) {
		t.Error("expected missing credentials to fail")
	}
}

// documented payment.success payload from the lava.top API reference
const ltSuccessPayload = `{
	"eventType": "payment.success",
	"product": {
		"id": "72d53efb-3696-469f-b856-f0d815748dd6",
		"title": "Тестовая подписка"
	},
	"buyer": {"email": "Emwbwj@lava.top"},
	"contractId": "c5a0cacc-3453-44b0-9532-aa492f1ba191",
	"amount": 2049.85,
	"currency": "RUB",
	"timestamp": "2024-02-05T08:44:32.42176Z",
	"status": "subscription-active",
	"errorMessage": ""
}`

func TestLavatop_NestedString(t *testing.T) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(ltSuccessPayload), &payload); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := nestedString(payload, "buyer", "email"); got != "Emwbwj@lava.top" {
		t.Errorf("expected buyer email, got %q", got)
	}
	if got := nestedString(payload, "product", "id"); got != "72d53efb-3696-469f-b856-f0d815748dd6" {
		t.Errorf("expected product id, got %q", got)
	}
	if got := nestedString(payload, "buyer", "missing"); got != "" {
		t.Errorf("expected empty for missing key, got %q", got)
	}
	if got := nestedString(payload, "contractId"); got != "c5a0cacc-3453-44b0-9532-aa492f1ba191" {
		t.Errorf("expected contractId, got %q", got)
	}
	if got := nestedString(payload, "amount", "nope"); got != "" {
		t.Errorf("expected empty for non-object path, got %q", got)
	}
}

// processEvent paths that must ack-and-drop before touching the DB or the
// lava.top API (s.db and s.cl stay nil — a touch would panic the test).
func TestLavatop_ProcessEventSkips(t *testing.T) {
	s := &Lavatop{products: map[string]int{"72d53efb-3696-469f-b856-f0d815748dd6": 2}}
	for _, tc := range []struct {
		name    string
		payload string
	}{
		{
			name:    "unknown event type",
			payload: `{"eventType": "payment.refunded"}`,
		},
		{
			name:    "payment failed",
			payload: `{"eventType": "payment.failed", "contractId": "x", "errorMessage": "Not sufficient funds"}`,
		},
		{
			name:    "recurring failed",
			payload: `{"eventType": "subscription.recurring.payment.failed", "contractId": "x"}`,
		},
		{
			name:    "cancelled",
			payload: `{"eventType": "subscription.cancelled", "contractId": "x", "willExpireAt": "2024-03-06T08:44:49.123932Z"}`,
		},
		{
			name:    "unmapped product",
			payload: `{"eventType": "payment.success", "product": {"id": "d31384b8-e412-4be5-a2ec-297ae6666c8f"}, "buyer": {"email": "a@b.c"}, "contractId": "x", "status": "completed"}`,
		},
		{
			name:    "missing email",
			payload: `{"eventType": "payment.success", "product": {"id": "72d53efb-3696-469f-b856-f0d815748dd6"}, "contractId": "x", "status": "completed"}`,
		},
		{
			name:    "missing contract",
			payload: `{"eventType": "payment.success", "product": {"id": "72d53efb-3696-469f-b856-f0d815748dd6"}, "buyer": {"email": "a@b.c"}, "status": "completed"}`,
		},
		{
			name:    "non-success status",
			payload: `{"eventType": "payment.success", "product": {"id": "72d53efb-3696-469f-b856-f0d815748dd6"}, "buyer": {"email": "a@b.c"}, "contractId": "x", "status": "subscription-failed"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var payload map[string]any
			if err := json.Unmarshal([]byte(tc.payload), &payload); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			email, err := s.processEvent(context.Background(), payload)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if email != "" {
				t.Errorf("expected no grant, got %q", email)
			}
		})
	}
}

func TestLavatop_ExtractExpiredAt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		want    time.Time
		wantErr bool
	}{
		{
			name: "expiry present",
			in:   `{"id": "c5a0cacc-3453-44b0-9532-aa492f1ba191", "subscriptionDetails": {"expiredAt": "2024-03-06T08:44:49.123932Z"}}`,
			want: time.Date(2024, 3, 6, 8, 44, 49, 123932000, time.UTC),
		},
		{
			name: "expiry null",
			in:   `{"subscriptionDetails": {"expiredAt": null}}`,
		},
		{
			name: "details absent",
			in:   `{"id": "c5a0cacc-3453-44b0-9532-aa492f1ba191"}`,
		},
		{
			name:    "bad time",
			in:      `{"subscriptionDetails": {"expiredAt": "tomorrow"}}`,
			wantErr: true,
		},
		{
			name:    "bad json",
			in:      `not-json`,
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractExpiredAt([]byte(tc.in))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("expected %v, got %v", tc.want, got)
			}
		})
	}
}
