// login.go — WorkBuddy OAuth 登录（设备授权流程，CN realm；--realm=global 供国际版）。
//
// 两个子命令，由 login.sh 顺序驱动：
//
//	login [--realm=cn|global] url   → POST /v2/plugin/auth/state?platform=<站点 Platform> 拿 state+authUrl，
//	                                  state 落 /tmp/wb2api-login-state.json，stdout 打印授权 URL
//	login [--realm=cn|global] poll  → 读 state，GET /v2/plugin/auth/token?state= 一次，
//	                                  成功再 GET /v2/plugin/login/account?state= 拿 uid/nickname，
//	                                  stdout 打印完整 token+account JSON（含 realm 键）
//
// --realm 默认 cn。按 realm 切换上游端点与 Origin/Referer：
//
//	cn     → https://copilot.tencent.com（Origin: https://www.codebuddy.cn）
//	global → https://www.workbuddy.ai（Origin: https://www.workbuddy.ai）
//
// state 落盘带 realm，poll 读回校验与命令行 --realm 一致（防混域）。
// 无 PKCE（workbuddy 设备流由服务端签发 state）。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"time"

	auth2 "github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/loginsite"
)

// 上游常量：站点表与 UA 统一来自 internal/loginsite（官方口径唯一来源）。
// CN → copilot.tencent.com；国际站两套主机名并存：
// intl → www.codebuddy.ai（官方 CLI externalDomain）、workbuddy → www.workbuddy.ai
// （旧桌面端）。UA 按站点取（loginsite.Site.UserAgent），不再有全局 CLI 串。
//
// 登录 state 落盘路径（var 便于测试替换临时文件）
// Portable across OSes: the upstream hardcoded "/tmp/...", which on
// Windows resolves to <drive>:	mp\... and aborts the OAuth flow with
// "The system cannot find the path specified". os.TempDir() is /tmp on Linux.
var stateFile = filepath.Join(os.TempDir(), "wb2api-login-state.json")

// exitFunc 供测试替换（默认 os.Exit；测试持临时替换为 panic 以进程内捕获 fatal）。
var exitFunc = os.Exit

// realmConfig 按 realm 返回上游 base 与 Origin/Referer origin（兼容旧调用）：
// global → 旧桌面端 workbuddy.ai；cn/非法/缺省 → copilot.tencent.com + codebuddy.cn。
// 新代码请直接用 loginsite.Site（要官方国际站用 SiteIntl）。
func realmConfig(realm string) (base, origin string) {
	s := loginsite.ForRealm(realm)
	return s.API, s.Origin
}

// commonHeaders 设备码端点的公共头族，直接取站点定义的真机头族
// （loginsite.Site.EndpointHeaders）。返回 func(*http.Request)，由调用方按站点
// 构造一次后复用。
//
// 不再发 Origin / Referer / X-Requested-With：2026-10-30 双版本真机抓包显示这三条
// 端点由桌面端 node fetch 发出，恒无这三个头；浏览器的同源语义只属于「打开授权页」
// 那一步，不属于 API 调用。UA 也换成桌面端 RPC 族，不再是 CodeBuddy CLI 家族串。
//
// 端点差异头（x-device-token / x-no-* / Bearer）由 withExtra 叠加。
func commonHeaders(site loginsite.Site) func(*http.Request) {
	hdrs := site.EndpointHeaders()
	return func(req *http.Request) {
		for k, v := range hdrs {
			if v != "" {
				req.Header.Set(k, v)
			}
		}
	}
}

// withExtra 在基础头函数之上叠加端点差异头（空值跳过）。
func withExtra(base func(*http.Request), extra map[string]string) func(*http.Request) {
	return func(req *http.Request) {
		base(req)
		for k, v := range extra {
			if v != "" {
				req.Header.Set(k, v)
			}
		}
	}
}

// apiEnvelope 与 main.go:429-433 一致
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// envelopeError 上游业务码非 0 的结构化错误：调用方按官方码表区分
// 「继续轮询」（11217/12151）与「终态失败」（12005/11212/11216）。
type envelopeError struct {
	Code   int
	Msg    string
	Status int
}

func (e *envelopeError) Error() string {
	return fmt.Sprintf("code=%d msg=%s", e.Code, e.Msg)
}

// doJSON 与 oauth.go:33-66 一致：{code,msg,data} 信封，code!=0 → error
func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		// 缺省头：国内站（与原 commonHeaders() 行为一致，零回归）
		commonHeaders(loginsite.For(""))(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		// 结构化：调用方按官方码表区分 pending / 终态。
		return nil, resp.StatusCode, &envelopeError{Code: env.Code, Msg: env.Msg, Status: resp.StatusCode}
	}
	return env.Data, resp.StatusCode, nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	exitFunc(1)
}

type loginState struct {
	State string `json:"state"`
	Realm string `json:"realm,omitempty"` // url 落盘时写回的 realm，poll 读回校验防混域
	Site  string `json:"site,omitempty"`  // 站点标识（cn/intl/workbuddy），poll 读回校验防换站
}

// realm 取值枚举（与 internal/auth 的 Realm() 归一化输出一致）。
const (
	realmCN     = "cn"
	realmGlobal = "global"
)

// parseRealmArgs 解析开头的 --realm=cn|global（或分离式 --realm <v>）flag，缺省 cn。
// 大小写不敏感归一化；非法值/缺值报错。桌椅剩余参数（子命令）顺序不变。
func parseRealmArgs(args []string) (realm string, rest []string, err error) {
	realm = realmCN
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--realm":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("--realm requires a value")
			}
			v := strings.ToLower(strings.TrimSpace(args[i+1]))
			if v != realmCN && v != realmGlobal {
				return "", nil, fmt.Errorf("invalid --realm %q (want cn|global)", args[i+1])
			}
			realm = v
			i++
		case strings.HasPrefix(a, "--realm="):
			v := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(a, "--realm=")))
			if v != realmCN && v != realmGlobal {
				return "", nil, fmt.Errorf("invalid --realm %q (want cn|global)", v)
			}
			realm = v
		default:
			rest = append(rest, a)
		}
	}
	return realm, rest, nil
}

// resolveRealmInput 把交互式选域的一行输入归一化为 realm（纯函数，login.sh 交互分支
// 的核心决策，可测）。规则：
//
//	"1"/"cn"（大小写不敏感）/""（回车默认）→ cn
//	"2"/"global" → global
//	其他 → ("", false)（调用方回默认 cn）
func resolveRealmInput(input string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "", "1", "cn":
		return realmCN, true
	case "2", "global":
		return realmGlobal, true
	}
	return "", false
}

// parseSiteArgs 解析 --site=cn|intl|workbuddy（官方域表），并把剩余参数里的
// --realm 一并消化：显式 --site 优先；否则按 --realm 映射（cn → 国内站，
// global → 旧桌面端 workbuddy.ai，保持老脚本语义）。
func parseSiteArgs(args []string) (site loginsite.Site, rest []string, err error) {
	var siteID string
	var kept []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--site":
			if i+1 >= len(args) {
				return loginsite.Site{}, nil, fmt.Errorf("--site requires a value")
			}
			siteID = args[i+1]
			i++
		case strings.HasPrefix(a, "--site="):
			siteID = strings.TrimPrefix(a, "--site=")
		default:
			kept = append(kept, a)
		}
	}
	if siteID != "" {
		id := strings.ToLower(strings.TrimSpace(siteID))
		if !loginsite.ValidID(id) {
			return loginsite.Site{}, nil, fmt.Errorf("invalid --site %q (want cn|intl|workbuddy)", siteID)
		}
		// 仍消化 --realm（老脚本会同时传），但以 --site 为准。
		_, kept, err = parseRealmArgs(kept)
		if err != nil {
			return loginsite.Site{}, nil, err
		}
		return loginsite.For(id), kept, nil
	}
	realm, kept, err := parseRealmArgs(kept)
	if err != nil {
		return loginsite.Site{}, nil, err
	}
	return loginsite.ForRealm(realm), kept, nil
}

// promptRealm 交互式选域：向 out 打印选项提示（out 接 stderr，stdout 留给 realm 本身），
// 从 in 读一行，返回归一化 realm。非法输入警告后回落 cn；EOF（非交互/管道）回落 cn。
func promptRealm(in io.Reader, out io.Writer) string {
	fmt.Fprintln(out, "选择登录版本: 1) 国内版(cn) 2) 国际版(global) [默认 1/cn]: ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		// EOF/非交互 → 回落默认 cn
		return realmCN
	}
	if realm, ok := resolveRealmInput(line); ok {
		return realm
	}
	fmt.Fprintln(out, "无效选择，默认国内版 cn")
	return realmCN
}

// validateRealmMatch 校验 state 文件 realm 与命令行 --realm 一致（防混域）：
// state 无 realm（旧文件）放行；非空且不一致 → error。
func validateRealmMatch(stateRealm, cliRealm string) error {
	if stateRealm != "" && stateRealm != cliRealm {
		return fmt.Errorf("realm mismatch: state file realm=%q, command --realm=%q（url 与 poll 需同一 realm）", stateRealm, cliRealm)
	}
	return nil
}

// runURL 执行 url 子命令：向站点的 state 端点 POST 取授权 URL，state 落盘
// （带 realm+site），stdout 打印 authURL（已按官方补齐 version / ww_device_nonce）。
// out 接 stdout；statePath 为落盘路径（可注入临时文件便于测试）。
func runURL(site loginsite.Site, statePath string, client *http.Client, out io.Writer) {
	stateURL, _, _, _ := site.Endpoints()
	// auth/state 真机带 x-device-token（取到才发）+ x-custom-channel-id + 四个 x-no-*。
	headers := withExtra(commonHeaders(site), site.HeadersForState(loginDeviceToken()))
	data, _, err := doJSON(client, http.MethodPost, stateURL, headers, bytes.NewReader([]byte("{}")))
	if err != nil {
		fatal("auth state failed: %v", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		fatal("auth state: missing state or authUrl")
	}
	raw, _ := json.Marshal(loginState{State: st.State, Realm: site.Realm, Site: site.ID})
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		fatal("write state: %v", err)
	}
	fmt.Fprintln(out, loginsite.DecorateAuthURL(st.AuthURL, site, loginDeviceNonce(), loginsite.NewLoginSessionID()))
}

// loginDeviceNonce 官方 ww_device_nonce 的来源（桌面端 Turing 设备 SDK）。本仓无
// SDK，取部署侧环境变量；空 = 不加该参数（不伪造设备指纹）。
func loginDeviceNonce() string {
	return strings.TrimSpace(os.Getenv("CODEBUDDY_LOGIN_DEVICE_NONCE"))
}

// loginDeviceToken 设备风控 token（X-Device-Token，auth/state 出站头）。
//
// 真机为 `v3:<base64…>`，由桌面端 qimei 组件本地生成，无法从 machineId 推导。
// 取不到就返回空串（调用方据此不发该头）——伪造比缺失更容易被判成非客户端。
// 来源：CODEBUDDY_LOGIN_DEVICE_TOKEN 环境变量，或 CODEBUDDY_LOGIN_DEVICE_TOKEN_FILE。
func loginDeviceToken() string {
	if v := strings.TrimSpace(os.Getenv("CODEBUDDY_LOGIN_DEVICE_TOKEN")); v != "" {
		return v
	}
	if p := strings.TrimSpace(os.Getenv("CODEBUDDY_LOGIN_DEVICE_TOKEN_FILE")); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

// runPoll 执行 poll 子命令：读 state 文件（realm/site 校验），向站点 token 端点
// GET 一次，成功再 GET login/account（带 Bearer），stdout 打印完整 token+account JSON。
//
// 官方码表语义：11217（RetryFetchToken）/ 12151（RetryFetchAccount）是 pending，
// 12005/11212/11216 是终态许可错误——后者必须直接报错退出，否则 login.sh 的
// 「登录还没完成就按了 y」提示会把「席位满 / 许可证过期」误导成操作问题。
func runPoll(site loginsite.Site, statePath string, client *http.Client, out io.Writer) {
	raw, err := os.ReadFile(statePath)
	if err != nil {
		fatal("read state: %v (先跑 login url)", err)
	}
	var ls loginState
	if err := json.Unmarshal(raw, &ls); err != nil {
		fatal("parse state: %v", err)
	}
	// 防混域/换站：state 落盘标识与本次站点不一致则拒绝（url 与 poll 必须同站）。
	if err := validateRealmMatch(ls.Realm, site.Realm); err != nil {
		fatal("%v", err)
	}
	if ls.Site != "" && ls.Site != site.ID {
		fatal("site mismatch: state file site=%q, command site=%q（url 与 poll 需同一站点）", ls.Site, site.ID)
	}
	// auth/token 真机是唯一不带 x-domain 的端点（只有公共头 + 四个 x-no-*）。
	tokenH := withExtra(commonHeaders(site), site.HeadersForToken())
	// handlePollLogin (oauth.go:108-162)：auth/token 是权威登录状态端点，
	// pending 时业务码 11217，完成时 code=0 + token bundle
	tokRaw, status, errTok := doJSON(client, http.MethodGet, site.API+"/v2/plugin/auth/token?state="+ls.State, tokenH, nil)
	if errTok != nil {
		var envErr *envelopeError
		if errors.As(errTok, &envErr) {
			if loginsite.CodeFatal(envErr.Code) {
				fatal("登录被上游拒绝（code=%d）：%s", envErr.Code, envErr.Msg)
			}
			if loginsite.CodePending(envErr.Code) {
				fatal("登录未完成（waiting for login）。请确认已在浏览器完成登录再按 y")
			}
		}
		if status == 0 || status >= 500 {
			fatal("token endpoint error: %v", errTok)
		}
		// 走到这里的是「非信封 4xx」（HTML 错误页等）：state 多半已过期/失效，
		// 反复「确认浏览器登录 + 按 y」永远不会再成功——正确动作是重跑 login url，
		// 不能与「还没登录完」共用同一句提示。
		fatal("登录状态查询被拒（HTTP %d）：%v\nstate 可能已过期，请重新运行 login url", status, errTok)
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		fatal("登录未完成（waiting for login）。请确认已在浏览器完成登录再按 y")
	}
	// login/account 拿 uid/nickname（带 Bearer）。12151 是「token 已发、账号信息还在
	// 落库」的 pending 态，提示重试而不是当成失败。
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	// login/account 真机带 x-domain + Bearer + 三个 x-no-*（无 x-no-authorization）。
	acctHeaders := withExtra(commonHeaders(site), site.HeadersForAccount(tok.AccessToken))
	acctRaw, _, errAcct := doJSON(client, http.MethodGet, site.API+"/v2/plugin/login/account?state="+ls.State, acctHeaders, nil)
	if errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	} else {
		var envErr *envelopeError
		if errors.As(errAcct, &envErr) {
			if loginsite.CodeFatal(envErr.Code) {
				fatal("登录被上游拒绝（code=%d）：%s", envErr.Code, envErr.Msg)
			}
			if loginsite.CodePending(envErr.Code) {
				fatal("账号信息尚未就绪（code=%d）。请稍等几秒再按 y 重试", envErr.Code)
			}
		}
	}
	raw, _ = json.Marshal(buildLoginOutput(tok, site.Realm, acct))
	fmt.Fprintln(out, string(raw))
	os.Remove(statePath)
}

// buildLoginOutput 组装 poll 输出的完整 JSON（login.sh 据此落盘 auth 文件）。
// realm 永不空：显式 --realm 优先（ResolveRealm 处理），否则按上游返回的 domain 推断——
// 保证登录落盘的 auth 文件恒带 realm 键。
func buildLoginOutput(tok struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}, realm string, acct struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}) map[string]any {
	return map[string]any{
		"access_token":  tok.AccessToken,
		"refresh_token": tok.RefreshToken,
		"expires_in":    tok.ExpiresIn,
		"domain":        tok.Domain,
		"realm":         auth2.ResolveRealm(realm, tok.Domain),
		"uid":           acct.UID,
		"enterprise_id": acct.EnterpriseID,
		"nickname":      acct.Nickname,
	}
}

func main() {
	site, rest, err := parseSiteArgs(os.Args[1:])
	if err != nil {
		fatal("%v (usage: login [--site=cn|intl|workbuddy] [--realm=cn|global] <url|poll|realm|site>)", err)
	}
	if len(rest) < 1 {
		fatal("usage: login [--site=cn|intl|workbuddy] [--realm=cn|global] <url|poll|realm|site>")
	}
	// 每个流程独立 cookie jar（oauth.go:22-29：多账号登录互不串会话）
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}

	switch rest[0] {
	case "url":
		runURL(site, stateFile, client, os.Stdout)

	case "poll":
		runPoll(site, stateFile, client, os.Stdout)

	case "realm":
		// 交互式选域（login.sh 无 --realm 传参且 stdin 为 tty 时调用）。
		// 提示打到 stderr，stdout 只输出归一化 realm，供 $( ) 捕获。
		realm := promptRealm(os.Stdin, os.Stderr)
		fmt.Println(realm)

	case "site":
		// 输出归一化站点标识（cn/intl/workbuddy），供脚本 $( ) 捕获。
		fmt.Println(site.ID)

	default:
		fatal("unknown subcommand %q (want url|poll|realm|site)", rest[0])
	}
}
