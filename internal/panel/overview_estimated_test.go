package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// 上游标记的「到期日按计量周期推算」必须一路带到 overview，面板才能显示推算标注：
// upstream.ExpiryBucket -> poolExpiryBuckets -> 池快照 -> /panel/api/overview。
func TestOverviewCarriesEstimatedExpiry(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "A", AccessToken: "at"})
	at := time.Now().Add(72 * time.Hour)
	p.SetCreditsSnapshot("u1", pool.CreditsSnapshot{
		Remain: 100, Total: 100,
		ExpiryBuckets: poolExpiryBuckets([]upstream.ExpiryBucket{
			{Date: at.Format("2006-01-02"), ExpireAt: at, Remain: 40, Estimated: true},
		}),
	})
	pn := New(Config{Version: "test", Pool: p})

	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil))
	var d struct {
		Accounts []struct {
			CreditsExpiryBuckets []pool.ExpiryBucket `json:"credits_expiry_buckets"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Accounts) != 1 || len(d.Accounts[0].CreditsExpiryBuckets) != 1 ||
		!d.Accounts[0].CreditsExpiryBuckets[0].Estimated {
		t.Fatalf("overview 丢了 estimated: %s", rec.Body)
	}
}
