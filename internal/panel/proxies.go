// Package panel proxies.go 面板出站代理池管理接口：查询状态快照、重置熔断与分配。
package panel

import (
	"net/http"

	"github.com/misakano7545/workbuddy2api-panel/internal/proxypool"
)

// proxiesHandler 返回当前代理池快照（密码脱敏）。
func (p *Panel) proxiesHandler(w http.ResponseWriter, r *http.Request) {
	pool := proxypool.Default()
	snap := pool.Snapshot()

	deadCount := 0
	for _, it := range snap {
		if it.Dead {
			deadCount++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"mode":    pool.Mode(),
		"total":   len(snap),
		"healthy": len(snap) - deadCount,
		"dead":    deadCount,
		"proxies": snap,
	})
}

// proxiesResetDead 清除所有出口的 dead 标记与连续失败计数（一键恢复重试）。
func (p *Panel) proxiesResetDead(w http.ResponseWriter, r *http.Request) {
	pool := proxypool.Default()
	n := pool.ResetDead()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"reset": n,
	})
}

// proxiesResetAssign 清空账号↔出口绑定，让所有账号在下次请求时重新选路。
func (p *Panel) proxiesResetAssign(w http.ResponseWriter, r *http.Request) {
	pool := proxypool.Default()
	pool.ResetAssign()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
	})
}
