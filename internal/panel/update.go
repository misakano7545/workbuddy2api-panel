package panel

import (
	"context"
	"net/http"
	"time"
)

func (p *Panel) updateStatus(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Updater == nil {
		writeErr(w, http.StatusNotImplemented, "update api not available")
		return
	}
	writeJSON(w, http.StatusOK, p.cfg.Updater.Status())
}

func (p *Panel) checkUpdate(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Updater == nil {
		writeErr(w, http.StatusNotImplemented, "update api not available")
		return
	}
	if err := p.cfg.Updater.Check(r.Context()); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p.cfg.Updater.Status())
}

func (p *Panel) installUpdate(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Updater == nil {
		writeErr(w, http.StatusNotImplemented, "update api not available")
		return
	}
	if err := p.cfg.Updater.Install(context.WithoutCancel(r.Context())); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok":         true,
		"restarting": true,
		"status":     p.cfg.Updater.Status(),
	})
	p.cfg.Updater.RestartAfter(800 * time.Millisecond)
}

// probeUpdate POST /panel/api/update/probe —— 分别探「版本元数据」与「资产下载」
// 两条路（issue #198）。
//
// 为什么单独一个接口：检查更新只回答「有没有新版本」，通道坏时它只能抛一句原始
// 超时，看不出是 DNS / TCP / TLS / 等响应头 / 镜像路径哪一段。这里两条路各发一个
// 极短请求，把「通不通 + 类目 + 耗时」直接摆到面板上。
func (p *Panel) probeUpdate(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Updater == nil {
		writeErr(w, http.StatusNotImplemented, "update api not available")
		return
	}
	meta, asset := p.cfg.Updater.Probe(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     meta.OK && asset.OK,
		"meta":   meta,
		"asset":  asset,
		"update": p.cfg.Updater.Status(),
	})
}
