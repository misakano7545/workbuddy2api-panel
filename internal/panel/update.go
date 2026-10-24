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
