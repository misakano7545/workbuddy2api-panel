package alert

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeSrc struct{ healthy int }

func (f fakeSrc) Health(realm string) Health {
	if realm == "cn" {
		return Health{Total: 2, Healthy: f.healthy}
	}
	return Health{}
}
func (f fakeSrc) WAFActive() bool { return false }

func TestAlertFiresOnce(t *testing.T) {
	var n int
	var gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		body, _ := io.ReadAll(r.Body)
		gotSig = r.Header.Get("X-WB2A-Signature")
		mac := hmac.New(sha256.New, []byte("s3"))
		mac.Write(body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if gotSig != want {
			t.Errorf("sig %s want %s", gotSig, want)
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	m := New(Config{
		Enabled: true, WebhookURL: srv.URL, Secret: "s3",
		StartupGrace: time.Nanosecond, ForTicks: 1, ClearTicks: 1,
		MinHealthyCN: 1, Interval: time.Hour, Timeout: time.Second,
	}, fakeSrc{})
	now := m.startedAt.Add(time.Second)
	m.Tick(now)
	m.Tick(now.Add(time.Second))
	if n != 1 {
		t.Fatalf("posts=%d want 1 (edge trigger, no repeat)", n)
	}
}
