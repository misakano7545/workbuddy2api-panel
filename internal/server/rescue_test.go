package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

func TestRescueMemoryIsolationExpiryCapacity(t *testing.T) {
	key := func(body, realm, model, mode string) string { return rescueKey([]byte(body), realm, model, mode, "") }
	a := `{"messages":[{"role":"system","content":"A"}],"tools":[{"name":"one"}]}`
	k := key(a, "cn", "m", "passthrough")
	for _, other := range []string{
		key(strings.Replace(a, `"A"`, `"B"`, 1), "cn", "m", "passthrough"),
		key(a, "global", "m", "passthrough"), key(a, "cn", "other", "passthrough"),
		key(a, "cn", "m", "append"), key(strings.Replace(a, "one", "two", 1), "cn", "m", "passthrough"),
	} {
		if other == k {
			t.Fatal("unrelated input shared a rescue key")
		}
	}
	var m rescueMemory
	now := time.Now()
	steps := []int{1, 2}
	m.remember(k, steps, now)
	steps[0] = 3
	if got := m.lookup(k, now); len(got) != 2 || got[0] != 1 {
		t.Fatal(got)
	}
	m.remember(k, []int{3}, now.Add(29*time.Minute))
	if got := m.lookup(k, now.Add(30*time.Minute)); got != nil {
		t.Fatal("cache hits extended raw probe deadline")
	}
	for i := 0; i < 1025; i++ {
		m.remember(fmt.Sprint(i), []int{1}, now.Add(time.Duration(i)))
	}
	if len(m.entries) != 1024 || m.lookup("0", now) != nil {
		t.Fatal("LRU capacity not enforced")
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				m.remember("race", []int{2}, now)
				m.lookup("race", now)
				m.forget("race")
			}
		}()
	}
	wg.Wait()
}

func TestRescueLearningReplayAndInvalidation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var bodies []string
			blockAll := false
			up := &upstream.Client{ChatBaseCN: "https://fake.example", HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				raw, _ := io.ReadAll(r.Body)
				bodies = append(bodies, string(raw))
				var req struct {
					Messages []struct{ Role, Content string }
				}
				_ = json.Unmarshal(raw, &req)
				blocked := blockAll
				for _, m := range req.Messages {
					if m.Role == "system" && strings.Contains(m.Content, "RESCUE") {
						blocked = true
					}
				}
				if blocked {
					return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"code":11128,"msg":"blocked by security policy"}`))}, nil
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
			})}}
			h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up, PromptMode: "passthrough"})
			call := func(sys string) *httptest.ResponseRecorder {
				t.Helper()
				rec := httptest.NewRecorder()
				body := fmt.Sprintf(`{"model":"glm-5.2","stream":%t,"messages":[{"role":"system","content":%q},{"role":"user","content":"hi"}]}`, stream, sys)
				h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
				return rec
			}
			const system = "RESCUE first sentence. Clean second sentence."
			if rec := call(system); rec.Code != 200 || len(bodies) != 3 {
				t.Fatalf("first code=%d calls=%d body=%s", rec.Code, len(bodies), rec.Body)
			}
			learnedBody := bodies[2]
			bodies = nil
			if rec := call(system); rec.Code != 200 || len(bodies) != 1 || bodies[0] != learnedBody {
				t.Fatalf("did not replay exact successful sequence: code=%d calls=%d", rec.Code, len(bodies))
			}
			bodies = nil
			if rec := call("Unrelated single sentence"); rec.Code != 200 || len(bodies) != 1 || !strings.Contains(bodies[0], `"content":"Unrelated single sentence","role":"system"`) {
				t.Fatalf("unrelated input modified: %v", bodies)
			}
			blockAll = true
			bodies = nil
			if rec := call(system); rec.Code != 400 || len(bodies) > 3 {
				t.Fatalf("stale memory code=%d calls=%d", rec.Code, len(bodies))
			}
			if len(h.rescue.entries) != 0 {
				t.Fatal("failed memory retained or upgraded")
			}
			bodies = nil
			if rec := call(system); rec.Code != 400 || len(bodies) != 4 {
				t.Fatalf("expected all four forms, code=%d calls=%d", rec.Code, len(bodies))
			}
		})
	}
}

func TestRescueBoundWithAccountFailures(t *testing.T) {
	calls := 0
	up := &upstream.Client{ChatBaseCN: "https://fake.example", HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		status, body := 400, `{"code":11128,"msg":"blocked by security policy"}`
		if calls >= 4 {
			status, body = 500, `{"msg":"server unavailable"}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}, &auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough", MaxRotate: 2})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"system","content":"First sentence. Second sentence."},{"role":"user","content":"hi"}]}`)))
	if calls != 5 || rec.Code != 503 {
		t.Fatalf("calls=%d want MaxRotate+3=5; code=%d", calls, rec.Code)
	}
	if len(h.rescue.entries) != 0 {
		t.Fatal("learned unsuccessful rescue")
	}
}
