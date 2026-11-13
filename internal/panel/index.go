// index.go 面板静态资源与安全响应头。
//
// 资源经 go:embed 打进二进制（随服务部署，无外部构建步骤）：
//   - index.html  页面骨架
//   - theme.js    首屏主题预热（<head> 同步加载，早于样式表）
//   - app.js      全部前端逻辑（独立文件而非内联，为了启用无需 unsafe-inline 的严格 CSP）
//
// 安全头对"面板页面与全部 /panel/api/* 响应"统一生效：CSP 限制脚本只能来自本服务，
// 禁止被 iframe 嵌套（防点击劫持），禁 MIME 嗅探，并声明不泄露 Referer 出去。
package panel

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"strings"
)

//go:embed index.html
var indexHTMLRaw []byte

//go:embed theme.js
var themeJS []byte

//go:embed app.js
var appJS []byte

// 内容指纹（issue #33）：把脚本 URL 绑到脚本内容上。
//
// 为什么需要：面板原先靠 `Cache-Control: no-store` 保证不拿到旧脚本，那是对
// 浏览器的一维约定；中间任何一层（反代缓存、浏览器扩展、企业网关的静态资源
// 缓存、离线缓存）只要回放了一次旧 app.js，新 HTML 配旧 JS 就会因 DOM 结构
// 漂移而中途抛异常，后半段的 let/const 不再执行——表现为「账号列表能刷新，
// 积分构成/用量初始化失败」（#33 报告者受控复现的正是这一形态）。
// 加性防御：no-store 原样保留，URL 另带内容指纹，跨版本时 URL 必然不同，
// 旧条目即使被回放也命不中新页面要的那个 key。
//
// 取 sha256 前 8 字节（16 个十六进制字符）：够用（碰撞概率对「两版面板脚本」
// 这种量级毫无意义），又不至于把 URL 撑长。指纹在服务进程启动时算一次
// （init），进程内恒定，无需清理策略，也不依赖构建注入。
func fingerprint(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// indexHTML 是把 <script src="theme.js"> / <script src="app.js"> 换成带指纹
// URL 之后的页面字节。原始 index.html 里的引用保持无查询串形态（源码可读、
// diff 干净），指纹只在运行时注入。
var indexHTML = func() []byte {
	s := strings.ReplaceAll(string(indexHTMLRaw), `src="theme.js"`, `src="theme.js?v=`+fingerprint(themeJS)+`"`)
	s = strings.ReplaceAll(s, `src="app.js"`, `src="app.js?v=`+fingerprint(appJS)+`"`)
	return []byte(s)
}()

// csp 内容安全策略（严格版，无需 unsafe-inline）：
//   - default-src 'none'        默认全禁，逐个开口
//   - script-src 'self'         只跑同源脚本（app.js）；页面无内联事件处理器/内联脚本
//   - style-src 'self' 'unsafe-inline'
//     style 的内联是设计取舍：页面有少量 style="..." 属性（进度条宽度、表格列宽），
//     允许内联样式不会导致脚本执行；仍禁止外部样式域与 @import 外链。
//   - connect-src 'self'        前端 fetch 只能打本服务
//   - img-src 'self' data:      图标/内联图
//   - form-action 'none'        页面无表单提交目标（配置页是 JS 提交）
//   - frame-ancestors 'none'    禁止被任何站点 iframe 嵌套（点击劫持）
//   - base-uri 'none'          禁止注入 <base> 改写相对路径
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// setSecurityHeaders 写入面板统一安全响应头（页面与 API 都要，API 也含 JSON 数据）。
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff") // 禁 MIME 嗅探
	// 禁收录：公网部署的面板会被搜索引擎与资产测绘平台当成可发现入口（CSP 管不到抓取）。
	// 只能挡住此后的抓取，已收录的旧快照要在搜索引擎控制台提交移除。
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("X-Frame-Options", "DENY")        // 老浏览器兜底（CSP frame-ancestors 的等价项）
	w.Header().Set("Referrer-Policy", "no-referrer") // 不外泄面板地址给外部站点
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}

// index 输出面板页面（静态无秘密；数据接口 /panel/api/* 才走鉴权）。
func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(indexHTML)
}

// themeScript 输出首屏主题预热脚本。
//
// 它必须被 <head> 以同步外链方式加载（CSP 禁内联）：晚于样式表就回到「刷新先闪深色」，
// 因为用户的明暗偏好只存在浏览器 localStorage 里，服务端无从得知。
func (p *Panel) themeScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(themeJS)
}

// appScript 输出前端逻辑（同源脚本，供 CSP script-src 'self' 加载）。
func (p *Panel) appScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(appJS)
}
