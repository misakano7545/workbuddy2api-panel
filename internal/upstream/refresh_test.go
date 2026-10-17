package upstream

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const refreshOKBody = `{"code":0,"msg":"ok","data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":3600}}`

func refreshAuth(uid, rt string, ttl time.Duration) *auth.Auth {
	return &auth.Auth{
		UID:          uid,
		AccessToken:  "at",
		RefreshToken: rt,
		ExpiresAt:    time.Now().Add(ttl).Unix(),
	}
}

func withRefreshGrace(t *testing.T, d time.Duration) {
	t.Helper()
	old := refreshGrace
	refreshGrace = d
	t.Cleanup(func() { refreshGrace = old })
}

// TestRefreshTokenIfNeededSingleFlight 同一个 RT 的两个按需刷新并发时，
// 只允许一个 leader 打上游；另一个等结果，不得重复消费 RT。
func TestRefreshTokenIfNeededSingleFlight(t *testing.T) {
	withRefreshGrace(t, 60*time.Second)

	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		once.Do(func() { close(started) })
		<-release
		return jsonResp(200, refreshOKBody), nil
	})

	a := refreshAuth("u1", "r1", time.Minute)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- c.RefreshTokenIfNeeded(a, 10*time.Minute)
		}()
	}
	<-started
	time.Sleep(50 * time.Millisecond) // 让第二个 goroutine 进入在途等待
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream refresh calls=%d want 1", got)
	}
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Fatalf("tokens=%q/%q want newat/newrt", a.AccessToken, a.RefreshToken)
	}
}

// TestRefreshForcedAndNeededShareCall keepalive 的强制刷新与请求路径的按需刷新
// 撞在一起时，同样只花一次 RT：强制调用等 leader 的结果，不额外发请求。
func TestRefreshForcedAndNeededShareCall(t *testing.T) {
	withRefreshGrace(t, 60*time.Second)

	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		once.Do(func() { close(started) })
		<-release
		return jsonResp(200, refreshOKBody), nil
	})

	a := refreshAuth("u1", "r1", time.Minute)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs <- c.RefreshToken(a)
	}()
	go func() {
		defer wg.Done()
		errs <- c.RefreshTokenIfNeeded(a, 10*time.Minute)
	}()
	<-started
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream refresh calls=%d want 1", got)
	}
}

// TestRefreshGraceReusesResult 刷新成功后 60 秒内，拿着旧 RT 的调用方直接复用
// 结果，不再发请求；旧快照也要被合并成新 token（对应插件的 renewed 宽限）。
func TestRefreshGraceReusesResult(t *testing.T) {
	withRefreshGrace(t, 60*time.Second)

	var calls atomic.Int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResp(200, refreshOKBody), nil
	})

	a := refreshAuth("u1", "r1", time.Minute)
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	old := refreshAuth("u1", "r1", time.Minute)
	if err := c.RefreshTokenIfNeeded(old, 10*time.Minute); err != nil {
		t.Fatalf("grace refresh: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream refresh calls=%d want 1 (grace reuse)", got)
	}
	if old.AccessToken != "newat" || old.RefreshToken != "newrt" {
		t.Fatalf("old snapshot not merged: %q/%q", old.AccessToken, old.RefreshToken)
	}
}

// TestRefreshGraceExpires 宽限窗口外，旧 RT 的调用会重新打上游（用于确认
// 60 秒只是并发窗口，不是长期缓存）。
func TestRefreshGraceExpires(t *testing.T) {
	withRefreshGrace(t, 0)

	var calls atomic.Int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResp(200, refreshOKBody), nil
	})

	a := refreshAuth("u1", "r1", time.Minute)
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	old := refreshAuth("u1", "r1", time.Minute)
	if err := c.RefreshTokenIfNeeded(old, 10*time.Minute); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream refresh calls=%d want 2 after grace", got)
	}
}

// TestRefreshForcedBypassesGrace 强制刷新（keepalive）必须真正问上游：它承担
// 12153 连续失败计数，不能吃按需刷新的 60 秒宽限，否则上游不换 RT 时会把后续
// 真失败掩盖成成功。并发场景仍由在途共享保护（见上一个用例）。
func TestRefreshForcedBypassesGrace(t *testing.T) {
	withRefreshGrace(t, 60*time.Second)

	var calls atomic.Int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResp(200, refreshOKBody), nil
	})

	a := refreshAuth("u1", "r1", time.Minute)
	if err := c.RefreshTokenIfNeeded(a, 10*time.Minute); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	old := refreshAuth("u1", "r1", time.Minute)
	if err := c.RefreshToken(old); err != nil {
		t.Fatalf("forced refresh: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream refresh calls=%d want 2 (forced bypasses grace)", got)
	}
}

// TestRefreshLoserNotSessionDead 并发落败方拿到 12153 时，如果共享 Auth 已经被
// 另一个刷新换新，应返回 nil 而不是 ErrSessionDead（避免误禁用账号）。
func TestRefreshLoserNotSessionDead(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, refreshOKBody), nil
	})
	a := &auth.Auth{UID: "u1", AccessToken: "newat", RefreshToken: "newrt"}
	lost := &refreshResult{
		err: &Error{Kind: ErrSessionDead, Status: 401, Msg: "12153: refresh token is invalid"},
		at:  time.Now(),
	}
	if err := c.applyRefresh(a, lost, "at", "r1"); err != nil {
		t.Fatalf("concurrent loser must not surface session-dead: %v", err)
	}

	// 反例：token 没变时，真正的 12153 仍要透出。
	b := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "r1"}
	if err := c.applyRefresh(b, lost, "at", "r1"); err == nil {
		t.Fatal("real session-dead must surface")
	}
}

// TestRefreshIfNeededSkipsFresh 新鲜 token 不应触发任何上游请求。
func TestRefreshIfNeededSkipsFresh(t *testing.T) {
	var calls atomic.Int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResp(200, refreshOKBody), nil
	})
	a := refreshAuth("u1", "r1", time.Hour)
	if err := c.RefreshTokenIfNeeded(a, 10*time.Minute); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("upstream refresh calls=%d want 0", got)
	}
}

// TestRefreshPanicReleasesWaiters leader 在刷新中 panic 时，等待者必须被释放并
// 拿到错误，而不是永久挂在 done 上。
func TestRefreshPanicReleasesWaiters(t *testing.T) {
	withRefreshGrace(t, 60*time.Second)

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	c := testClient(func(r *http.Request) (*http.Response, error) {
		once.Do(func() { close(started) })
		<-release
		panic("boom")
	})

	a := refreshAuth("u1", "r1", time.Minute)
	leader := make(chan any, 1)
	go func() {
		defer func() { leader <- recover() }()
		_ = c.RefreshToken(a)
	}()
	<-started

	waiter := make(chan error, 1)
	go func() { waiter <- c.RefreshTokenIfNeeded(a, 10*time.Minute) }()
	time.Sleep(20 * time.Millisecond) // 让 waiter 进入在途等待
	close(release)

	if r := <-leader; r == nil {
		t.Fatal("leader should panic")
	}
	select {
	case err := <-waiter:
		if err == nil || !strings.Contains(err.Error(), "refresh panic") {
			t.Fatalf("waiter err=%v want refresh panic", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter hung after leader panic")
	}
}
