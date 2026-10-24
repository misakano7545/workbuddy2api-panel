package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/updater"
)

func TestUpdateAPIRequiresAuthentication(t *testing.T) {
	for _, endpoint := range []struct{ method, path string }{
		{http.MethodGet, "/panel/api/update"},
		{http.MethodPost, "/panel/api/update/check"},
		{http.MethodPost, "/panel/api/update/install"},
	} {
		p := newTestPanel()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(endpoint.method, endpoint.path, nil)
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: got %d", endpoint.path, rec.Code)
		}
		req.Header.Set("Authorization", "Bearer test-key")
		rec = httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("unconfigured %s: got %d", endpoint.path, rec.Code)
		}
	}
}

func TestUpdateStatusAndUnsupportedInstall(t *testing.T) {
	p := New(Config{APIKey: "test-key", Updater: updater.New("dev", "", "", updater.Settings{AutoEnabled: true, AutoTime: "06:30"})})
	req := httptest.NewRequest(http.MethodGet, "/panel/api/update", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	var status updater.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || status.CurrentVersion != "dev" || status.CanInstall || !status.AutoEnabled || status.AutoTime != "06:30" {
		t.Fatalf("unexpected status: %d %+v", rec.Code, status)
	}
	req = httptest.NewRequest(http.MethodPost, "/panel/api/update/install", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway || p.cfg.Updater.Status().Installing {
		t.Fatalf("dev install must fail without pending state: %d %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateFrontendStates(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	script := `
const fs = require('fs'), vm = require('vm'), assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const nodes = new Map();
let response, fail = false, navigated;
const ctx = {
  $: id => { if (!nodes.has(id)) nodes.set(id, {textContent:'', scrollIntoView(){}}); return nodes.get(id); },
  api: async () => { if(fail) throw Error('offline'); return response; },
  toast(){}, confirm: () => true, go: v => { navigated = v; }
};
vm.createContext(ctx);
vm.runInContext('let updateData = null, updateBusy = false;\n' +
  src.slice(src.indexOf('function displayVersion('), src.indexOf('async function loadOverview(')), ctx);
(async () => {
  assert.equal(ctx.displayVersion('dev'), 'dev');
  assert.equal(ctx.displayVersion('v2026.10.23.1859'), 'v2026.10.23.1859');
  response = {current_version:'2026.10.22.1823', latest_version:'2026.10.23.1859', update_available:true, can_install:true};
  await ctx.checkForUpdates();
  assert.equal(nodes.get('btnUpdateInstall').disabled, false, 'install re-enabled after checking');
  nodes.get('navUpdate').onclick();
  assert.equal(navigated, 'config');
  fail = true;
  await ctx.checkForUpdates();
  assert.equal(nodes.get('btnUpdateCheck').disabled, false, 'retry after network error');
  assert.equal(nodes.get('btnUpdateInstall').disabled, false);
  fail = false;
  ctx.renderUpdateStatus({...response, last_error:'checksum mismatch'});
  assert(nodes.get('updateNote').textContent.includes('checksum mismatch'));
  ctx.renderUpdateStatus({...response, can_install:false});
  assert.equal(nodes.get('btnUpdateInstall').hidden, true, 'unsupported platform hides install');
  response = {status:{...response, installing:true}};
  ctx.renderUpdateStatus(response.status);
  assert.equal(nodes.get('btnUpdateInstall').disabled, true);
  assert.equal(nodes.get('btnUpdateCheck').disabled, true);
})().catch(e => { console.error(e); process.exitCode = 1; });
`
	path := filepath.Join(t.TempDir(), "update.cjs")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, "app.js").CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
