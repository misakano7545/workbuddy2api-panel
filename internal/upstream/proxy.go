package upstream

import (
	"net/http"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// officialConnectionValue 给出该 path 上官方客户端实际发送的 Connection 值。
//
// 2026-10-30 双版本真机抓包（CN 5.9.3 / GL 5.9.4）：官方在每条 RPC 上都显式
// 写了 Connection —— 除 /v3/config 是 close 外，其余全是 keep-alive。
// HTTP/1.1 默认即复用，Go 因此从不发送这一行，但头集合少了它就是少一个字段；
// 风控按白名单查头时，"缺一个官方必发的头"和"多一个官方不发的头"一样显眼。
//
// 没抓到过的 path 返回空串，保持 Go 默认行为，不臆造。
func officialConnectionValue(path string) string {
	switch path {
	case "/v3/config":
		return "close"
	case "/v2/chat/completions", "/v2/chat/busy", "/v2/report":
		return "keep-alive"
	}
	return ""
}

// applyConnectionHeader 把官方那个显式的 Connection 头补回去。
//
// close 不能写进 Header：net/http 的 transferWriter 会按 req.Close 自己写一行
// Connection: close，手写会重复成两行。keep-alive 相反 —— Go 认为它是默认值所以
// 从不发出，必须手写才会出现在线上字节里。
func applyConnectionHeader(req *http.Request) {
	switch officialConnectionValue(req.URL.Path) {
	case "close":
		req.Close = true
	case "keep-alive":
		req.Header.Set("Connection", "keep-alive")
	}
}

// do is the single account-aware HTTP exit, including nonstandard header families.
//
// 头名拼写在这里统一纠正（headcase.go），Connection 在这里补发（官方必发，
// Go 默认不发）。所有 13 个出站点都收敛到本函数，挂一次即覆盖全部出站。
func (c *Client) do(req *http.Request, a *auth.Auth, chat bool) (*http.Response, error) {
	req = normalizeHeaderSpelling(req)
	applyConnectionHeader(req)
	client := c.HTTP
	if chat {
		client = c.chatHTTP()
	}
	if c.ProxyPool == nil || a == nil {
		return client.Do(req)
	}
	return c.ProxyPool.Do(client, req, a.UID, a.Realm())
}
