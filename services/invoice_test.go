package services

import (
	"encoding/json"
	"testing"
	"time"
)

// The catalog contract web-ui relies on: every list is an array even when the
// tables are empty (a missing key means an older webhook), and an unlimited
// tier carries null — never 0, which would read as "no Vault".
func TestListPricesResponseJSON(t *testing.T) {
	rate := int64(50)
	vp := int64(250)
	cases := []struct {
		name string
		resp listPricesResponse
		want string
	}{
		{
			name: "empty tables stay arrays",
			resp: newListPricesResponse(nil, nil, nil),
			want: `{"prices":[],"tiers":[],"discounts":[]}`,
		},
		{
			name: "offer terms and tier facts",
			resp: newListPricesResponse(
				[]priceItem{{TierID: 2, TierName: "silver", PeriodDays: 30, AmountUSD: 5, TrialDays: 7, IsPromo: true}},
				[]tierItem{
					{TierID: 2, Name: "silver", DownloadRate: &rate, VaultPoints: &vp, SiteNoAds: true, EmbedNoAds: true},
					{TierID: 6, Name: "sparkling", SiteNoAds: true, EmbedNoAds: true},
				},
				[]discountItem{{Code: "SAMPLE50", PercentOff: 50, PeriodDays: 30,
					ExpiresAt: time.Date(2027, 3, 21, 21, 0, 0, 0, time.UTC)}},
			),
			want: `{"prices":[{"tier_id":2,"tier_name":"silver","period_days":30,"amount_usd":5,"available":false,"trial_days":7,"is_promo":true}],` +
				`"tiers":[{"tier_id":2,"name":"silver","download_rate":50,"vault_points":250,"site_noads":true,"embed_noads":true},` +
				`{"tier_id":6,"name":"sparkling","download_rate":null,"vault_points":null,"site_noads":true,"embed_noads":true}],` +
				`"discounts":[{"code":"SAMPLE50","percent_off":50,"period_days":30,"expires_at":"2027-03-21T21:00:00Z"}]}`,
		},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.resp)
		if err != nil {
			t.Fatalf("%s: marshal: %v", c.name, err)
		}
		if string(b) != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, b, c.want)
		}
	}
}
