package services

import (
	"context"
	"encoding/json"
	"maps"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLavatop_ParseProducts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		wantErr bool
		want    map[string]bool
	}{
		{
			name: "empty",
			in:   "",
			want: map[string]bool{},
		},
		{
			name: "valid list with spaces and mixed case",
			in:   "D31384B8-e412-4be5-a2ec-297ae6666c8f, 72d53efb-3696-469f-b856-f0d815748dd6",
			want: map[string]bool{
				"d31384b8-e412-4be5-a2ec-297ae6666c8f": true,
				"72d53efb-3696-469f-b856-f0d815748dd6": true,
			},
		},
		{
			name:    "bad uuid",
			in:      "not-a-uuid",
			wantErr: true,
		},
		{
			name:    "trailing comma",
			in:      "d31384b8-e412-4be5-a2ec-297ae6666c8f,",
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
			if !maps.Equal(got, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, got)
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
	s := &Lavatop{products: map[string]bool{"72d53efb-3696-469f-b856-f0d815748dd6": true}}
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
			name:    "failure status on success event",
			payload: `{"eventType": "payment.success", "product": {"id": "72d53efb-3696-469f-b856-f0d815748dd6"}, "buyer": {"email": "a@b.c"}, "contractId": "c5a0cacc-3453-44b0-9532-aa492f1ba191", "status": "subscription-failed"}`,
		},
		{
			name:    "non-uuid contract id",
			payload: `{"eventType": "payment.success", "product": {"id": "72d53efb-3696-469f-b856-f0d815748dd6"}, "buyer": {"email": "a@b.c"}, "contractId": "../products", "status": "completed"}`,
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

func TestLavatop_ExtractContract(t *testing.T) {
	for _, tc := range []struct {
		name           string
		in             string
		wantOffer      string
		wantTime       time.Time
		wantTerminated string
		wantErr        bool
	}{
		{
			name:      "offer and expiry present",
			in:        `{"id": "c5a0cacc-3453-44b0-9532-aa492f1ba191", "product": {"name": "Subscription webtor", "offer": "Bronze Backer"}, "subscriptionDetails": {"expiredAt": "2024-03-06T08:44:49.123932Z"}}`,
			wantOffer: "Bronze Backer",
			wantTime:  time.Date(2024, 3, 6, 8, 44, 49, 123932000, time.UTC),
		},
		{
			name:      "expiry null",
			in:        `{"product": {"offer": "Gold Backer"}, "subscriptionDetails": {"expiredAt": null}}`,
			wantOffer: "Gold Backer",
		},
		{
			name:           "terminated",
			in:             `{"product": {"offer": "Gold Backer"}, "subscriptionDetails": {"expiredAt": "2024-03-06T08:44:49.123932Z", "terminatedAt": "2024-02-10T00:00:00Z"}}`,
			wantOffer:      "Gold Backer",
			wantTime:       time.Date(2024, 3, 6, 8, 44, 49, 123932000, time.UTC),
			wantTerminated: "2024-02-10T00:00:00Z",
		},
		{
			name: "details and offer absent",
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
			got, err := extractContract([]byte(tc.in))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.OfferName != tc.wantOffer {
				t.Errorf("expected offer %q, got %q", tc.wantOffer, got.OfferName)
			}
			if !got.ExpiredAt.Equal(tc.wantTime) {
				t.Errorf("expected %v, got %v", tc.wantTime, got.ExpiredAt)
			}
			if got.TerminatedAt != tc.wantTerminated {
				t.Errorf("expected terminatedAt %q, got %q", tc.wantTerminated, got.TerminatedAt)
			}
		})
	}
}

func TestLavatop_TierName(t *testing.T) {
	for in, want := range map[string]string{
		"Bronze Backer":       "bronze",
		"Silver Backer":       "silver",
		"Gold Backer":         "gold",
		"  gold  ":            "gold",
		"Bronze-Backer":       "bronze",
		"Bronze(RUB) Special": "bronze",
		"—":                   "",
		"":                    "",
	} {
		if got := lavatopTierName(in); got != want {
			t.Errorf("lavatopTierName(%q): expected %q, got %q", in, want, got)
		}
	}
}

// Unknown status on a success event must ERROR (5xx → lava retries), never
// ack-and-drop: money arrived, this build just doesn't know the status yet.
func TestLavatop_ProcessEventUnknownStatusErrors(t *testing.T) {
	s := &Lavatop{products: map[string]bool{"72d53efb-3696-469f-b856-f0d815748dd6": true}}
	var payload map[string]any
	if err := json.Unmarshal([]byte(`{
		"eventType": "payment.success",
		"product": {"id": "72d53efb-3696-469f-b856-f0d815748dd6"},
		"buyer": {"email": "a@b.c"},
		"contractId": "c5a0cacc-3453-44b0-9532-aa492f1ba191",
		"status": "subscription-renewed"
	}`), &payload); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := s.processEvent(context.Background(), payload); err == nil {
		t.Error("expected unknown status to error so lava.top retries")
	}
}

func TestLavatop_ExtractOfferNames(t *testing.T) {
	products := map[string]bool{"506cc5ce-3997-40db-84fd-0a6e0d84107f": true}
	// flat item shape (as observed live)
	flat := `{"items":[{"id":"506cc5ce-3997-40db-84fd-0a6e0d84107f","title":"Subscription webtor",
		"offers":[{"id":"1","name":"Bronze Backer"},{"id":"2","name":"Silver Backer"}]},
		{"id":"aaaaaaaa-0000-0000-0000-000000000000","offers":[{"id":"3","name":"Other"}]}]}`
	names, err := extractOfferNames([]byte(flat), products)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(names, ",") != "Bronze Backer,Silver Backer" {
		t.Errorf("unexpected names: %v", names)
	}
	// spec {type,data} item shape
	wrapped := `{"items":[{"type":"PRODUCT","data":{"id":"506CC5CE-3997-40db-84fd-0a6e0d84107f",
		"offers":[{"name":"Gold Backer"}]}}]}`
	names, err = extractOfferNames([]byte(wrapped), products)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(names, ",") != "Gold Backer" {
		t.Errorf("unexpected names: %v", names)
	}
	// no matching products → error
	if _, err = extractOfferNames([]byte(`{"items":[]}`), products); err == nil {
		t.Error("expected error for empty items")
	}
}
