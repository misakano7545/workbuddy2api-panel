package upstream

import (
	"net/http"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// do is the single account-aware HTTP exit, including nonstandard header families.
//
// 头名拼写在这里统一纠正（headcase.go）：所有 13 个出站点都收敛到本函数，
// 挂一次即覆盖全部出站，不需要在每个 Headers() 里各写一遍。
func (c *Client) do(req *http.Request, a *auth.Auth, chat bool) (*http.Response, error) {
	req = normalizeHeaderSpelling(req)
	client := c.HTTP
	if chat {
		client = c.chatHTTP()
	}
	if c.ProxyPool == nil || a == nil {
		return client.Do(req)
	}
	return c.ProxyPool.Do(client, req, a.UID, a.Realm())
}
