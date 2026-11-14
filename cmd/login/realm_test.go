package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/loginsite"
)

// TestParseRealmArgs 覆盖 --realm 参数解析：缺省 cn、等号/分离式写法、
// 非法值/缺值报错、大小写归一、剥离 flag 后剩余参数保持相对顺序。
func TestParseRealmArgs(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantRealm string
		wantRest  []string
		wantErr   bool
	}{
		{name: "缺省 cn", args: []string{"url"}, wantRealm: "cn", wantRest: []string{"url"}},
		{name: "等号 global 在前", args: []string{"--realm=global", "url"}, wantRealm: "global", wantRest: []string{"url"}},
		{name: "等号 cn 在后", args: []string{"poll", "--realm=cn"}, wantRealm: "cn", wantRest: []string{"poll"}},
		{name: "分离式 global", args: []string{"--realm", "global", "url"}, wantRealm: "global", wantRest: []string{"url"}},
		{name: "非法值报错", args: []string{"--realm=foo", "url"}, wantErr: true},
		{name: "分离式缺值报错", args: []string{"--realm", "url"}, wantErr: true},
		{name: "大小写归一", args: []string{"--realm=GLOBAL", "url"}, wantRealm: "global", wantRest: []string{"url"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			realm, rest, err := parseRealmArgs(c.args)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseRealmArgs(%v) err=nil, want error", c.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRealmArgs(%v) err=%v", c.args, err)
			}
			if realm != c.wantRealm {
				t.Errorf("realm=%q want %q", realm, c.wantRealm)
			}
			if !reflect.DeepEqual(rest, c.wantRest) {
				t.Errorf("rest=%v want %v", rest, c.wantRest)
			}
		})
	}
}

// TestParseSiteArgs 覆盖 --site 解析：显式 --site 优先于 --realm，未知值报错，
// 缺省/无参回落国内站；同时确认 --site/--realm 都被剥离，子命令留在 rest。
func TestParseSiteArgs(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantID   string
		wantRest []string
		wantErr  bool
	}{
		{name: "缺省 cn", args: []string{"url"}, wantID: "cn", wantRest: []string{"url"}},
		{name: "site=intl", args: []string{"--site=intl", "url"}, wantID: "intl", wantRest: []string{"url"}},
		{name: "site=workbuddy 分离式", args: []string{"--site", "workbuddy", "poll"}, wantID: "workbuddy", wantRest: []string{"poll"}},
		{name: "site 大小写归一", args: []string{"--site=INTL", "url"}, wantID: "intl", wantRest: []string{"url"}},
		// --site 优先：老脚本同时传两个也不冲突。
		{name: "site 优先于 realm", args: []string{"--site=intl", "--realm=cn", "url"}, wantID: "intl", wantRest: []string{"url"}},
		// 只传 realm 时保持历史语义（global → workbuddy 旧桌面端）。
		{name: "realm=global 映射旧桌面端", args: []string{"--realm=global", "url"}, wantID: "workbuddy", wantRest: []string{"url"}},
		{name: "realm=cn", args: []string{"--realm=cn", "url"}, wantID: "cn", wantRest: []string{"url"}},
		{name: "未知 site 报错", args: []string{"--site=global", "url"}, wantErr: true},
		{name: "site 缺值报错", args: []string{"--site"}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			site, rest, err := parseSiteArgs(c.args)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseSiteArgs(%v) err=nil, want error", c.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSiteArgs(%v) err=%v", c.args, err)
			}
			if site.ID != c.wantID {
				t.Errorf("site.ID=%q want %q", site.ID, c.wantID)
			}
			if !reflect.DeepEqual(rest, c.wantRest) {
				t.Errorf("rest=%v want %v", rest, c.wantRest)
			}
		})
	}
}

// TestRunURLDecoratesAuthURL 覆盖 url 子命令的输出：授权链接必须带官方 version
// 参数（对齐 CLI appendAuthUrlParams），且 state 落盘带 realm+site 供 poll 校验。
func TestRunURLDecoratesAuthURL(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/state") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("platform"); got != "workbuddy-ai" {
			t.Errorf("platform=%q want workbuddy-ai（intl 站点真机口径）", got)
		}
		// 授权 URL 用测试服务器自身地址，便于断言参数。
		base := "http://" + r.Host
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":0,"msg":"OK","data":{"state":"st-1","authUrl":"%s/login?platform=CLI&state=st-1"}}`, base)
	}))
	defer srv.Close()

	// 用自定义站点指向测试服务器（真实站点表不便打外网）。
	site := loginsite.For(loginsite.SiteIntl)
	site.API = srv.URL
	site.Origin = srv.URL

	var out bytes.Buffer
	runURL(site, statePath, srv.Client(), &out)

	got := strings.TrimSpace(out.String())
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("解析授权 URL 失败 %q: %v", got, err)
	}
	// 授权 URL 补 version（真机 GL 链接带 version=5.9.4），但不补 loginSessionId
	// （官方只对 platform=workbuddy 追加）。
	if v := u.Query().Get("version"); v != loginsite.ClientVersionWorkBuddyGL {
		t.Errorf("授权 URL version=%q want %q：%q", v, loginsite.ClientVersionWorkBuddyGL, got)
	}
	if s := u.Query().Get("loginSessionId"); s != "" {
		t.Errorf("国际版授权 URL 不该带 loginSessionId：%q", got)
	}
	if s := u.Query().Get("state"); s != "st-1" {
		t.Errorf("state=%q want st-1", s)
	}

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("state 未落盘: %v", err)
	}
	var ls loginState
	if err := json.Unmarshal(raw, &ls); err != nil {
		t.Fatalf("state 解析失败: %v", err)
	}
	if ls.State != "st-1" || ls.Realm != "global" || ls.Site != "intl" {
		t.Errorf("state={state:%s realm:%s site:%s} want {st-1 global intl}", ls.State, ls.Realm, ls.Site)
	}
}

// TestRunPollFatalCode 覆盖 poll 的终态码处理：上游返回 12005（席位满）时必须
// 直接退出并给出上游 msg，而不是报「登录未完成」让用户反复重按 y。
func TestRunPollFatalCode(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(statePath, []byte(`{"state":"st-1","realm":"cn","site":"cn"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":12005,"msg":"license seat limit reached"}`)
	}))
	defer srv.Close()

	site := loginsite.For(loginsite.SiteCN)
	site.API = srv.URL
	site.Origin = srv.URL

	// 捕获 fatal（exitFunc 换成 panic，与既有 realm_test 的用法一致）。
	var msg string
	oldExit := exitFunc
	exitFunc = func(code int) { panic("exit") }
	defer func() { exitFunc = oldExit }()
	// fatal 写 stderr，这里只看 panic 是否发生。
	func() {
		defer func() {
			if r := recover(); r != nil {
				msg = "fatal"
			}
		}()
		runPoll(site, statePath, srv.Client(), io.Discard)
	}()
	if msg != "fatal" {
		t.Fatal("12005 应触发 fatal 退出，实际未退出（会被误当成「登录未完成」）")
	}
}

// TestRunPollSiteMismatch 覆盖防换站：state 文件记录的站点与本次 --site 不一致时
// 必须拒绝（国际域两套主机名同后端，混用会把请求打到非签发域）。
func TestRunPollSiteMismatch(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(statePath, []byte(`{"state":"st-1","realm":"global","site":"workbuddy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	oldExit := exitFunc
	exitFunc = func(code int) { panic("exit") }
	defer func() { exitFunc = oldExit }()

	var fatal bool
	func() {
		defer func() {
			if r := recover(); r != nil {
				fatal = true
			}
		}()
		runPoll(loginsite.For(loginsite.SiteIntl), statePath, http.DefaultClient, io.Discard)
	}()
	if !fatal {
		t.Fatal("site 不一致应拒绝，实际继续执行了")
	}
}

// TestResolveRealmInput 覆盖交互式选域的输入→realm 映射（login.sh 交互分支的核心决策）：
// "1"/"cn"/""（回车默认）→ cn；"2"/"global" → global；大小写不敏感；非法 → ("",false)。
func TestResolveRealmInput(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{input: "1", want: "cn"},
		{input: "cn", want: "cn"},
		{input: "CN", want: "cn"},
		{input: "", want: "cn"}, // 回车默认
		{input: "2", want: "global"},
		{input: "global", want: "global"},
		{input: "GLOBAL", want: "global"},
	}
	for _, c := range cases {
		got, ok := resolveRealmInput(c.input)
		if !ok {
			t.Errorf("resolveRealmInput(%q) ok=false want true", c.input)
			continue
		}
		if got != c.want {
			t.Errorf("resolveRealmInput(%q)=%q want %q", c.input, got, c.want)
		}
	}
	// 非法输入 → (false)。
	for _, bad := range []string{"3", "cnn", "globalx", "foo"} {
		if got, ok := resolveRealmInput(bad); ok {
			t.Errorf("resolveRealmInput(%q)=(%q,true) want (_,false)", bad, got)
		}
	}
}

// TestPromptRealm 覆盖 promptRealm 的 I/O 行为（out 将接 os.Stderr，stdout 只出 realm）：
//
//	1 / 2 / 回车默认 → 分别输出来 cn / global / cn；均打印"选择登录版本"提示；
//	非法输入 → 警告并回落 cn；EOF（非交互直接管道）→ 回落 cn。
func TestPromptRealm(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		want     string
		wantHint bool // 输出中出现"选择登录版本"提示
	}{
		{name: "选 cn", input: "1\n", want: "cn", wantHint: true},
		{name: "选 global", input: "2\n", want: "global", wantHint: true},
		{name: "回车默认 cn", input: "\n", want: "cn", wantHint: true},
		{name: "非法回落 cn", input: "foo\n", want: "cn", wantHint: true},
		{name: "EOF 回落 cn", input: "", want: "cn", wantHint: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			got := promptRealm(strings.NewReader(c.input), &out)
			if got != c.want {
				t.Errorf("promptRealm(%q)=%q want %q", c.input, got, c.want)
			}
			if c.wantHint && !strings.Contains(out.String(), "选择登录版本") {
				t.Errorf("prompt should contain '选择登录版本', got %q", out.String())
			}
		})
	}
}

// TestRealmSubcommandSelection 通过命令形式验证 realm 子命令（login.sh 交互分支调用）：
// 子命令剥离 --realm，剩余参数为首个 "realm"。
func TestRealmSubcommandSelection(t *testing.T) {
	realm, rest, err := parseRealmArgs([]string{"realm"})
	if err != nil {
		t.Fatalf("parseRealmArgs(realm) err=%v", err)
	}
	if realm != "cn" {
		t.Errorf("realm default=%q want cn", realm)
	}
	if len(rest) != 1 || rest[0] != "realm" {
		t.Errorf("rest=%v want [realm]", rest)
	}
}

// TestBuildLoginOutputRealmAlwaysSet 登录产物（login.sh 据此落盘 auth 文件）恒含 realm 键：
// 显式 --realm 优先，缺省时按 domain 推断（echo 出的 global 账号即便未显式指定也带 global）。
// 这是「登录落盘永远带 realm 标识」契约的测试载体。
func TestBuildLoginOutputRealmAlwaysSet(t *testing.T) {
	tok := struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}{AccessToken: "at", RefreshToken: "rt", ExpiresIn: 3600, Domain: "www.codebuddy.cn"}
	acct := struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}{UID: "u1", Nickname: "n1"}

	cases := []struct {
		name      string
		realm     string
		domain    string
		wantRealm string
	}{
		{name: "显式 global 优先", realm: "global", domain: "www.codebuddy.cn", wantRealm: "global"},
		{name: "显式 cn 优先", realm: "cn", domain: "www.workbuddy.ai", wantRealm: "cn"},
		{name: "缺省按 domain 推断 global", realm: "", domain: "www.workbuddy.ai", wantRealm: "global"},
		{name: "缺省空 domain 回落 cn", realm: "", domain: "", wantRealm: "cn"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tok.Domain = c.domain
			out := buildLoginOutput(tok, c.realm, acct)
			m, ok := out["realm"].(string)
			if !ok {
				t.Fatalf("missing realm key in login output: %v", out)
			}
			if m != c.wantRealm {
				t.Errorf("realm=%q want %q", m, c.wantRealm)
			}
		})
	}
}
