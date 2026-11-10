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
	if rec.Code != http.StatusOK || status.CurrentVersion != "dev" || status.CurrentKnown || status.CanInstall || !status.AutoEnabled || status.AutoTime != "06:30" {
		t.Fatalf("unexpected status: %d %+v", rec.Code, status)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["current_known"] != false {
		t.Fatalf("API must expose current_known=false: %s", rec.Body.String())
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
const nodes = new Map(), toasts = [];
let response, fail = false, navigated, opened;
const ctx = {
  // 真实 DOM 元素一定有 style：更新通道那行会写 el.style.color，桩里少了它会在
  // 「字段名写错」之外的时机炸掉，掩盖真正的断言失败。
  $: id => { if (!nodes.has(id)) nodes.set(id, {textContent:'', style:{}, scrollIntoView(){}}); return nodes.get(id); },
  api: async () => { if(fail) throw Error('offline'); return response; },
  toast: (...args) => toasts.push(args), confirm: () => true, go: v => { navigated = v; },
  open: (...args) => { opened = args; }
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
  const knownResponse = response;
  for (const current_version of ['dev', 'vdev', '', 'v1.2.3', 'v2026.11.3', '2026.11.1.0905']) {
    response = {current_version, current_known:false, latest_version:'2026.11.1.905',
      update_available:false, can_install:false,
      release_url:'https://github.com/example/project/releases/tag/v2026.11.1.905'};
    await ctx.checkForUpdates();
    assert.equal(nodes.get('navUpdate').hidden, true, 'unknown build hides update badge');
    assert.equal(nodes.get('navUpdate').title, '');
    assert.equal(nodes.get('btnUpdateInstall').hidden, true);
    assert.equal(nodes.get('btnUpdateRelease').hidden, false, 'unknown build keeps Release link');
    assert(nodes.get('updateNote').textContent.includes('无法比较'));
    assert(nodes.get('updateNote').textContent.includes('最新 Release v2026.11.1.905'));
    assert.equal(toasts.at(-1)[0], nodes.get('updateNote').textContent);
    assert(!toasts.at(-1)[0].includes('已是最新版本'), 'unknown must not mean up to date');
    if (!current_version || current_version.replace(/^v/, '') === 'dev') {
      assert.equal(nodes.get('navVer').textContent, 'dev');
      assert(nodes.get('updateNote').textContent.includes('开发构建'));
    } else {
      assert(!nodes.get('updateNote').textContent.includes('开发构建'), 'invalid release is not necessarily a dev build');
    }
    ctx.openLatestRelease();
    assert.equal(opened[0], response.release_url);
    assert.equal(opened[2], 'noopener');
    ctx.renderUpdateStatus({...response, release_url:''});
    assert.equal(nodes.get('btnUpdateRelease').hidden, true, 'no URL means no Release button');
    ctx.renderUpdateStatus({...response, latest_version:'', release_url:''});
    assert(!nodes.get('updateNote').textContent.includes('最新'));
    ctx.renderUpdateStatus({...response, last_error:'offline'});
    assert.equal(nodes.get('updateNote').textContent, '更新失败：offline');
    ctx.renderUpdateStatus({...response, last_error:'offline', installing:true});
    assert.equal(nodes.get('updateNote').textContent, '正在准备更新…');
    assert.equal(nodes.get('btnUpdateCheck').disabled, true);
    ctx.renderUpdateStatus(response);
  }
  for (const current_version of ['2026.11.1.905', '2026.11.2.2117']) {
    response = {...response, current_version, current_known:true};
    await ctx.checkForUpdates();
    assert.equal(nodes.get('navUpdate').hidden, true);
    assert.equal(nodes.get('btnUpdateRelease').hidden, true);
    assert.equal(toasts.at(-1)[0], '当前已是最新版本');
  }
  response = {...knownResponse, current_known:true};
  await ctx.checkForUpdates();
  assert.equal(nodes.get('navUpdate').hidden, false);
  assert.equal(nodes.get('btnUpdateInstall').hidden, false);
  assert.equal(toasts.at(-1)[0], '发现新版本 v' + response.latest_version);
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
