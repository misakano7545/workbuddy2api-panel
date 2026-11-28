// 模型限流映射（model limits）的面板接口。
//
// 页面上的「还剩多少」与请求路径真正拦下来的「还剩多少」必须是同一份口径，所以
// 这里一个数字都不算：快照、判定、预设、导出包全部来自 internal/ratelimit，面板只做
// 搬运与入参解码（口径契约见 internal/ratelimit/README.md）。这也是为什么本文件没有一个
// 额度常量——一旦在这里写死 200M，页面与请求路径就会各说一套。
package panel

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/misakano7545/workbuddy2api-panel/internal/ratelimit"
)

// modelLimitsDisabledMessage 是 tracker 未装配时的统一话术。
// 说「未启用」而不是返回空表：空表会被读成「所有额度都用光了」，那是完全相反的结论。
const modelLimitsDisabledMessage = "模型限流映射未启用"

// modelLimits GET /panel/api/model_limits
//
// 一次返回整页所需：视图快照 + 最近一次加载/落盘错误 + 池的 model_fallback 表。
// fallback 单独给：页面要把「老机制认为 A 可以降到 B」与「策略里的链」并排显示，
// 否则运营会把两条互相独立的来源看成一条。
func (p *Panel) modelLimits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tr := p.cfg.ModelLimits
	if tr == nil {
		writeErr(w, http.StatusServiceUnavailable, modelLimitsDisabledMessage)
		return
	}
	// ?refresh=1 是页面「刷新」按钮用的：后台每 30s 会重算一次，但运营调完池子/倍率
	// 后不该等半分钟才看到新数字。重算是纯函数读闭包，立即执行不会打上游。
	if r.URL.Query().Get("refresh") == "1" {
		tr.Refresh()
	}
	errText := ""
	if err := tr.Err(); err != nil {
		errText = err.Error()
	}
	// 显式声明 nil map：nil 会写成 JSON null，页面据此区分「没配」与「配了空表」。
	var fallback map[string]string
	if p.cfg.ModelFallback != nil {
		fallback = p.cfg.ModelFallback()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"error":    errText,
		"view":     tr.View(),
		"fallback": fallback,
	})
}

// modelLimitsPolicy POST /panel/api/model_limits/policy
//
// 五个操作共用一条入口：upsert / delete / preset / reset / save。合成一个端点而不是
// 五个的理由是它们改的都是同一份策略——拆开就要各自重写「改完立即重算快照」，那一步
// 一旦漏在某个分支里，页面显示与实际拦截就会出现一段时间的不一致。
func (p *Panel) modelLimitsPolicy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tr := p.cfg.ModelLimits
	if tr == nil {
		writeErr(w, http.StatusServiceUnavailable, modelLimitsDisabledMessage)
		return
	}
	var req struct {
		Op     string                `json:"op"`
		Limit  *ratelimit.ModelLimit `json:"limit"`
		Realm  string                `json:"realm"`
		Model  string                `json:"model"`
		ID     string                `json:"id"`
		Policy *ratelimit.Policy     `json:"policy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	var err error
	switch strings.ToLower(strings.TrimSpace(req.Op)) {
	case "upsert":
		if req.Limit == nil {
			writeErr(w, http.StatusBadRequest, "缺少 limit")
			return
		}
		_, err = tr.Upsert(*req.Limit)
	case "delete":
		_, err = tr.Delete(req.Realm, req.Model)
	case "preset":
		_, err = tr.ApplyPreset(req.ID)
	case "reset":
		_, err = tr.Reset()
	case "save":
		if req.Policy == nil {
			writeErr(w, http.StatusBadRequest, "缺少 policy")
			return
		}
		_, err = tr.Save(*req.Policy)
	case "":
		writeErr(w, http.StatusBadRequest, "缺少 op")
		return
	default:
		writeErr(w, http.StatusBadRequest, "未知操作 "+req.Op)
		return
	}
	if err != nil {
		// 校验失败（金额为负、域写错、窗口越界）一律 400：这些是「配置写坏了」，
		// 不是服务端故障，页面要能把原因原样显示出来。
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "view": tr.View()})
}

// modelLimitsPreview POST /panel/api/model_limits/preview
//
// 请求可带一条尚未保存的草稿 limit：页面调参数时不该先落盘再看效果（那会把
// 半成品额度写进请求路径）。草稿的叠加规则与 Upsert 完全一致——预览与实际保存
// 后生效的口径必须能对上，否则这个按钮就只是装饰。
func (p *Panel) modelLimitsPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tr := p.cfg.ModelLimits
	if tr == nil {
		writeErr(w, http.StatusServiceUnavailable, modelLimitsDisabledMessage)
		return
	}
	var req struct {
		Realm string                `json:"realm"`
		Model string                `json:"model"`
		Limit *ratelimit.ModelLimit `json:"limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	preview := tr.Preview(req.Realm, req.Model)
	if req.Limit != nil {
		preview = tr.PreviewPolicy(draftPolicy(tr.Policy(), *req.Limit), req.Realm, req.Model)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "preview": preview})
}

// modelLimitsExport POST /panel/api/model_limits/export
//
// bundle（整包 JSON）给上游复算，markdown 给人读：提上游时两件事都得说清，只有
// JSON 会让评审重新猜口径，只有 Markdown 则没法复算。缺省 format = bundle。
func (p *Panel) modelLimitsExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tr := p.cfg.ModelLimits
	if tr == nil {
		writeErr(w, http.StatusServiceUnavailable, modelLimitsDisabledMessage)
		return
	}
	var req struct {
		Format string `json:"format"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	bundle := tr.Export()
	switch strings.ToLower(strings.TrimSpace(req.Format)) {
	case "", "bundle", "json":
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "bundle": bundle})
	case "markdown", "md":
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "markdown": bundle.Markdown})
	default:
		writeErr(w, http.StatusBadRequest, "未知导出格式 "+req.Format)
	}
}

// draftPolicy 把一条未保存的草稿叠进策略副本。
//
// 叠加规则照抄 Tracker.Upsert：作用域（域 + 模型）相同的覆盖项就地替换，没有就追加；
// 两者全空的草稿改的是默认项。刻意不合并字段——预览要回答的是「保存后会怎样」，
// 而保存走的就是这同一条替换语义。
func draftPolicy(base ratelimit.Policy, draft ratelimit.ModelLimit) ratelimit.Policy {
	n := ratelimit.Normalize(draft)
	out := base
	if n.Realm == "" && n.Model == "" {
		out.Default = n
		return out
	}
	overrides := make([]ratelimit.ModelLimit, 0, len(base.Overrides)+1)
	replaced := false
	for _, o := range base.Overrides {
		if sameLimitScope(o, n) {
			overrides = append(overrides, n)
			replaced = true
			continue
		}
		overrides = append(overrides, o)
	}
	if !replaced {
		overrides = append(overrides, n)
	}
	out.Overrides = overrides
	return out
}

// sameLimitScope 判断两条策略是否指向同一作用域（忽略大小写与首尾空白）。
func sameLimitScope(a, b ratelimit.ModelLimit) bool {
	return strings.EqualFold(strings.TrimSpace(a.Realm), strings.TrimSpace(b.Realm)) &&
		strings.EqualFold(strings.TrimSpace(a.Model), strings.TrimSpace(b.Model))
}
