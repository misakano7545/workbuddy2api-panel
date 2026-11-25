// global 模型目录探测：纯动态产出模型名及其窗口 / 能力元数据（v3-config-merge）。
//
// 探测两路并发：/v3/config（主路，IDE UA 完整能力版）+ 企业端点家族
// （/v2 → /console 补缺），并集 = v3 条目为主、企业端点补 v3 缺失的 id
// （如 gpt-5.3-codex 只在 /v2 下发）。倍率字段（credits）虽随目录下发，但
// 只透出展示，不注入 costTier、不参与选号。
//
// 纯动态：不再回落任何静态名单——拉不出目录即意味着该域上游不可用，
// 假名单只会让客户端选到 11102 的模型（产品决策：无兜底）。
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// GlobalModelNames 国际版（global realm）历史静态名单（2026-10-29 WorkBuddy AI 5.9.3
// 真机 /v3/config 实测 25 条，与 GlobalCatalogSource=v3-desktop 的目录逐条对齐）。
//
// 纯动态化后**不再作为模型目录的基底/兜底**：/v1/models 只透出上游实际下发的模型。
// 保留仅作历史对照（global e2e 观测日志差集参照）与「目录是否整体换了代」的肉眼基准。
//
// 上一版是 5.8.4 时期的 21 条，含 gpt-5.5 / gpt-5.4 / gpt-5.3-codex / gemini-3.5-flash /
// glm-5.2 / kimi-k2.6 / hy4-preview-f 等已下架 id，且缺 gpt-6 系与 glm-5.3-flash、
// kimi-k3.1-pro、grok-4.7、gemini-3.8-flash、minimax-m3.1-flash 等 5.9.3 新增——拿它做
// 差集参照会得出一半假差异，故按实测刷新。
var GlobalModelNames = []string{
	"default-model",
	"fast-model",
	"balanced-model",
	"primary-model",
	"deepseek-v4.1-flash",
	"deepseek-v4.1-flash-sg",
	"glm-5.3",
	"glm-5.3-flash",
	"hy4-preview",
	"hy3",
	"kimi-k3",
	"kimi-k3.1-pro",
	"kimi-k3.1-pro-highspeed",
	"minimax-m3.1-flash",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"gpt-6-astra",
	"gpt-6-sol",
	"gpt-6-luna",
	"gpt-6.1-sol",
	"gemini-3.8-flash",
	"grok-4.7",
	// 媒体生成（tags 决定进 GenerationModels 而不是对话列表）
	"hy-image-3.5",
	"kling-video-v3",
}

// fetchGlobalModelsCache 探测结果缓存（语义参照 CN 侧 handler.dynamicModelsCache：1h TTL +
// 5min 失败负缓存）。按 Client 实例持有（effortsMu 同模式），测试新建 Client 即隔离。
// Mutex 内嵌，与 modelList 无并发读路径竞争（唯一读写点本文件内）。
type fetchGlobalModelsCache struct {
	sync.Mutex
	names    []string    // 成功缓存：并集模型名（已去重）；nil = 未探测/失败
	infos    []ModelInfo // 成功缓存：对象形态的全字段条目（窄表/失败形态为 nil）
	fetched  time.Time
	lastFail time.Time
	// source 本次成功探测的目录来源：globalCatalogSourceDesktop / ...Union / ...Enterprise。
	// 面板用它把「拿到的是窄目录/旧目录」直接摆给运维看（issue #216 现象 1 就是
	// v3 探测失败静默降级到企业端点，用户只看到「少了 11 个模型」）。
	source string
}

// 目录来源标识（fetchGlobalModelsCache.source）。
const (
	globalCatalogSourceDesktop    = "v3-desktop" // 桌面端 UA 族权威目录（官方 picker 口径）
	globalCatalogSourceUnion      = "v3-union"   // 桌面族探测失败的回落：IDE+CLI 并集
	globalCatalogSourceEnterprise = "enterprise" // v3 全失败，只剩企业端点（窄目录 + 旧倍率）
)

// GlobalCatalogSource 返回最近一次成功探测的目录来源（空 = 尚未探测）。
func (c *Client) GlobalCatalogSource() string {
	if c == nil {
		return ""
	}
	c.globalModels.Lock()
	defer c.globalModels.Unlock()
	return c.globalModels.source
}

// InvalidateGlobalModelsCache 清空 global 目录探测缓存（names/infos/成功与失败时间戳），
// 使下一次 FetchGlobalModels/FetchGlobalModelInfos 立即重新探测上游。
//
// 存在理由：面板「模型与档位 → 重新获取」此前只是重读 1h 缓存，点了数量纹丝不动
// （issue #216 现象 3）。上游目录变动后运维需要一个立即生效的按钮，而不是等 TTL。
func (c *Client) InvalidateGlobalModelsCache() {
	if c == nil {
		return
	}
	c.globalModels.Lock()
	defer c.globalModels.Unlock()
	c.globalModels.names = nil
	c.globalModels.infos = nil
	c.globalModels.fetched = time.Time{}
	c.globalModels.lastFail = time.Time{}
	c.globalModels.source = ""
}

// globalModelsTTL / globalModelsFailCooldown 探测缓存时长：成功 1h，失败 5min 负缓存。
const (
	globalModelsTTL          = time.Hour
	globalModelsFailCooldown = 5 * time.Minute
)

// globalModelsProbePaths global 企业模型目录端点候选序列（按 realm 切 base，路径"家族"）：
// /v2 家族优先（实测 /v2/enterprises/personal/models 200 含完整模型表），
// /console 作 fallback（同域旧路径，或 500）。v3-config-merge 后该家族降为企业补充路
// （/v3/config 为主路，与家族并发探测；gpt-5.3-codex 等家族独有模型经此进并集）。
var globalModelsProbePaths = []string{
	"/v2/enterprises/personal/models",
	"/console/enterprises/personal/models",
}

// FetchGlobalModels 探测 global 账号的模型名目录并返回**模型名列表**（无元数据）。
//
// 纯动态：成功返回并集结果（去重），缓存 1h；失败（两路全非 2xx / 解析失败 /
// 空列表）记 5min 负缓存，返回 nil（无静态回落）。缓存/负缓存命中：直接返回，零上游调用。
//
// 调用方负责：仅在有 global 账号时调用（无则不探测）；GlobalEnabled 关闭时（逃生门）
// 不得调用——本方法由 globalOn(a) 内部兜底，若账号因开关回落 cn 则返回 nil。
func (c *Client) FetchGlobalModels(a *auth.Auth) []string {
	names, _ := c.fetchGlobalModelsOnce(a)
	return names
}

// FetchGlobalModelInfos 探测 global 账号的模型目录并返回全字段 ModelInfo 列表。
// 与 FetchGlobalModels 共享同一次探测与缓存（names + infos 一体落缓存）：
// 对象形态 200 → 全字段条目；窄表形态 / 探测失败 / 负缓存 / 非 global 路由账号
// → nil（调用方按 ID 名单输出裸条目，不编造字段）。
// 账号因 GlobalEnabled 开关回落 cn 时不探测（globalOn 兜底，零上游调用）。
func (c *Client) FetchGlobalModelInfos(a *auth.Auth) []ModelInfo {
	_, infos := c.fetchGlobalModelsOnce(a)
	return infos
}

// fetchGlobalModelsOnce 单次探测决策（缓存命中/负缓存/触发探测），返回 (names, infos)。
// 纯动态：成功 = 并集结果去重；一切失败 = nil（不回落静态）。
// infos 仅对象形态成功探测时非 nil。
func (c *Client) fetchGlobalModelsOnce(a *auth.Auth) (names []string, infos []ModelInfo) {
	if !c.globalOn(a) {
		// 逃生门兜底：账号不路由 global 上游 → 不探测（零上游调用）。
		return nil, nil
	}

	c.globalModels.Lock()
	if len(c.globalModels.names) > 0 && time.Since(c.globalModels.fetched) < globalModelsTTL {
		names, infos := c.globalModels.names, c.globalModels.infos
		c.globalModels.Unlock()
		return names, infos
	}
	if !c.globalModels.lastFail.IsZero() && time.Since(c.globalModels.lastFail) < globalModelsFailCooldown {
		// 负缓存冷却期内：避免反复打上游，直接按失败处理（无静态回落）。
		c.globalModels.Unlock()
		return nil, nil
	}
	c.globalModels.Unlock()

	names, infos, efforts, defaults, source, err := c.probeGlobalModels(a)
	if err != nil || len(names) == 0 {
		// 探测失败：负缓存 + 返回 nil（effort 桶不写，prepareBody 走 globalEffortMap 静态兜底）。
		c.globalModels.Lock()
		c.globalModels.lastFail = time.Now()
		c.globalModels.names = nil
		c.globalModels.infos = nil
		c.globalModels.Unlock()
		return nil, nil
	}
	// global 域 effort 能力：探测下发的 supportedEfforts/defaultEffort 权威写入 global 桶
	// （raw remote，不并入静态表——静态兜底在 prepareBody 的 globalEffortMap 与
	// /v1/models 的 EffortListing 里按需 fallback）。空探测不写（防清既有桶）。
	if len(efforts) > 0 || len(defaults) > 0 {
		c.storeEfforts("global", efforts, defaults)
	}
	c.storeModelRates("global", infos)
	c.storeModelMaxOut("global", infos)

	// 成功：探测结果去重。names/infos 均落缓存；倍率等选号敏感字段只透出展示，
	// 不注入 costTier。
	seen := make(map[string]bool, len(names))
	merged := make([]string, 0, len(names))
	for _, id := range names {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		merged = append(merged, id)
	}

	c.globalModels.Lock()
	c.globalModels.names = merged
	c.globalModels.infos = infos
	c.globalModels.fetched = time.Now()
	c.globalModels.lastFail = time.Time{}
	c.globalModels.source = source
	c.globalModels.Unlock()
	return merged, infos
}

// desktopProbeUA /v3/config 第三路（桌面端形态）探测用的 UA。
//
// 该端点按 UA 家族下发不同目录，三族各有独有模型（2026-10-03 实测，同一账号
// 同一端点：CN IDE 19 / CLI 32 / 桌面端 54，global 13 / 22 / 29）。global 侧
// gpt-6-luna 只在桌面端目录出现，但请求实测可正常返回 200——只探 IDE + CLI 时
// 它永远进不了 /v1/models（用户侧表现为「模型列表不全」）。版本号升降不改结果
// （IDE 4.12→5.0、CLI 2.63→2.80 目录条数一致），故只补家族。
//
// 平台段固定用 CN 形态（`WorkBuddy`，= defaultWorkBuddyUAFor(nil)）：本次实测即
// 该形态、两个 base 都返回最大目录；global 形态（`WorkBuddy AI`）未实测，不臆造。
func (c *Client) desktopProbeUA() string {
	return c.defaultWorkBuddyUAFor(nil)
}

// probeGlobalModels 发起一次 global 模型目录探测（v3-config-merge）：
// /v3/config（主，IDE / CLI / 桌面端**三种 UA 家族**并发探测取并集）与企业端点
// 家族（/v2 → /console 兜底，补缺）**并发**探测后并集合并。返回模型名列表
// （已合并、未再去重——去重在 fetchGlobalModelsOnce）、全字段 ModelInfo（对象
// 形态；窄表为 nil）及 effort 能力桶（supportedEfforts/defaultEffort，可为空）。
// 合并口径：v3 条目为主（credits 等字段以 v3 为准），企业端点只补 v3 缺失的
// 模型 id；去重 key = 模型 id，输出顺序稳定。v3 与家族两路全失败才返回错误
// （等价原「家族端点全非 2xx」负缓存语义）；单路失败降级为其余路结果 + warn
// 日志，互不拖累。
func (c *Client) probeGlobalModels(a *auth.Auth) (names []string, infos []ModelInfo, efforts map[string][]string, defaults map[string]string, source string, err error) {
	type probeResult struct {
		names []string
		infos []ModelInfo
		err   error
	}
	// probeV3 单次 /v3/config 探测（UA 参数化）。该端点按 UA **家族**（客户端形态）
	// 下发**不同模型集合**：IDE / CLI / 桌面端三族各有独有模型，故并发多路取并集。
	// 实测（2026-10-03，同一账号同一端点）目录条数：CN 19 / 32 / 54，global 13 / 22 / 29；
	// 版本号升降不改变结果，只有家族变（见 desktopProbeUA 注释）。
	probeV3 := func(ua string) chan probeResult {
		ch := make(chan probeResult, 1)
		go func() {
			// chatBase 已按 realm 切 global base。
			byID, pick, perr := c.fetchV3ConfigModelMap(a, ua)
			if perr != nil {
				ch <- probeResult{err: perr}
				return
			}
			// 出图模型旁路留存（/v1/models 以 image_generation 标记列出），照旧不进对话列表。
			// 必须在可见性过滤前做：媒体模型不在 agent 菜单里。
			c.stashGenerationModels("global", sortedInfos(byID))
			ids := make([]string, 0, len(byID))
			outInfos := make([]ModelInfo, 0, len(byID))
			if vis := pick.ordered(byID); vis != nil {
				// 客户端 picker 口径（见 clientpick.go）：哪一族 UA 探测，就按那一族
				// 自己下发的 agents 名单裁剪——客户端用哪族 UA 就只显示那族的菜单项。
				// INTL 实测：目录 25 条，菜单 22 条（去掉 paidId deepseek-v4.1-flash-sg
				// 与 hy-image-3.5 / kling-video-v3 两个媒体模型）。
				for _, id := range vis {
					mi := byID[id]
					if nonChatModel(mi.ID, mi.MaxTokens, mi.Tags) {
						continue
					}
					ids = append(ids, id)
					outInfos = append(outInfos, mi)
				}
				ch <- probeResult{names: ids, infos: outInfos}
				return
			}
			// 口径缺失（上游无 agents）：照旧全量 + 排序，保持既有输出稳定语义。
			for _, mi := range byID {
				if nonChatModel(mi.ID, mi.MaxTokens, mi.Tags) {
					continue
				}
				ids = append(ids, mi.ID)
				outInfos = append(outInfos, mi)
			}
			sort.Strings(ids) // map 迭代序随机，排序保输出稳定
			ch <- probeResult{names: ids, infos: outInfos}
		}()
		return ch
	}
	v3IDECh := probeV3(c.v3ProbeUA(a))
	v3CLICh := probeV3(c.v3ProbeUALite(a))
	// 第三路：桌面端形态。缺它就会漏掉仅桌面端下发的模型——global 侧 gpt-6-luna
	// 即此例（目录里没有、实际可调用），用户侧表现为「模型列表不全」。
	v3DesktopCh := probeV3(c.desktopProbeUA())
	enterpriseCh := make(chan probeResult, 1)
	go func() {
		// 企业端点家族：/v2 首选 → /console 兜底（既有探活序，零回归）。
		var lastErr error
		for _, path := range globalModelsProbePaths {
			names, infos, perr := c.globalModelsOnce(a, path)
			if perr != nil {
				lastErr = perr
				continue
			}
			enterpriseCh <- probeResult{names: names, infos: infos}
			return
		}
		enterpriseCh <- probeResult{err: lastErr}
	}()
	v3IDE := <-v3IDECh
	v3CLI := <-v3CLICh
	v3Desktop := <-v3DesktopCh
	enterprise := <-enterpriseCh
	// v3 多路自合并：IDE 路字段权威（响应更大、单条字段更全），CLI 路与桌面路只补
	// 缺失的模型 id。任一路成功即用为主路；全失败才带 err 进入下游降级判断。
	// 单路失败照旧 WARN，不拖累其他路（与上游 PR #103 同口径）。
	v3Probes := []struct {
		label string
		res   probeResult
	}{
		{"IDE-UA", v3IDE},
		{"CLI-UA", v3CLI},
		{"desktop-UA", v3Desktop},
	}
	var okProbes []probeResult
	for _, p := range v3Probes {
		if p.res.err != nil {
			log.Printf("WARN: [upstream] global models: v3/config %s probe failed: %v", p.label, p.res.err)
			continue
		}
		okProbes = append(okProbes, p.res)
	}
	var v3 probeResult
	// catalogSource 记录本次目录由哪一族给出（写入缓存供面板展示）。
	catalogSource := globalCatalogSourceDesktop
	switch {
	case len(okProbes) == 0:
		// 三路全失败：err 非空，下游按既有「v3 失败」分支降级或负缓存。
		v3 = probeResult{err: v3IDE.err}
	case v3Desktop.err == nil:
		// 桌面端 UA 族是**目录权威**（issue #216）：官方桌面端 picker 就是这一族，
		// 2026-10-17 实测 global 侧桌面族 27 条与 picker 一一对应，而 IDE/CLI 族的
		// 独有模型（o4-mini / auto-chat / deep-model）在同一 chat 端点回 11102
		// 「service info not found」——列出来只会让客户端选到死模型。
		//
		// 故：桌面族成功时，**模型 id 只取桌面族**；IDE/CLI 族降级为元数据
		// （context/efforts/credits 缺字段时补），不再贡献 id。
		v3 = probeResult{names: v3Desktop.names, infos: v3Desktop.infos}
		var enrichSources [][]ModelInfo
		if v3IDE.err == nil {
			enrichSources = append(enrichSources, v3IDE.infos)
		}
		if v3CLI.err == nil {
			enrichSources = append(enrichSources, v3CLI.infos)
		}
		if len(enrichSources) > 0 {
			v3.infos = enrichModelInfos(v3.infos, enrichSources...)
		}
	case len(okProbes) == 1:
		v3 = okProbes[0]
		catalogSource = globalCatalogSourceUnion
	default:
		// 桌面族探测失败（或未返回）：回落既有并集语义，宁可多列也不空表；
		// 来源标成 union，面板据此提示「拿到的是回落目录」。
		vn, vi := okProbes[0].names, okProbes[0].infos
		for _, p := range okProbes[1:] {
			vn, vi = mergeGlobalCatalog(vn, vi, p.names, p.infos)
		}
		v3 = probeResult{names: vn, infos: vi}
		catalogSource = globalCatalogSourceUnion
	}

	if v3.err != nil && enterprise.err != nil {
		// 两路全失败 → 负缓存语义（等价原家族端点全非 2xx）。
		return nil, nil, nil, nil, "", v3.err
	}
	if v3.err != nil {
		// /v3 失败降级：不拖累企业端点结果（降级仅企业端点 + warn）。
		// 这条路径就是 issue #216 的现场：企业端点只有 18 条旧目录，且 hy4-preview
		// 的 credits 在那里是 x0.00（v3 是 x0.29）——来源必须能被面板看见，否则
		// 用户只会看到「模型少了 11 个、倍率是 0」而不知道是降级。
		degradedWarnOnce("global-models/v3", "WARN: [upstream] global models: v3/config probe failed (degraded to enterprise endpoint): %v", v3.err)
		names, infos, efforts, defaults = extractEfforts(enterprise.infos)
		if len(names) == 0 {
			names = enterprise.names
		}
		return names, infos, efforts, defaults, globalCatalogSourceEnterprise, nil
	}
	if enterprise.err != nil {
		degradedWarnOnce("global-models/enterprise", "WARN: [upstream] global models: enterprise endpoint failed (degraded to v3/config, catalog served normally): %v", enterprise.err)
		names, infos, efforts, defaults = extractEfforts(v3.infos)
		if len(names) == 0 {
			names = v3.names
		}
		return names, infos, efforts, defaults, catalogSource, nil
	}
	v3Names, v3Infos, v3Efforts, v3Defaults := extractEfforts(v3.infos)
	if len(v3Names) == 0 {
		v3Names = v3.names
	}
	entNames, entInfos, entEfforts, entDefaults := extractEfforts(enterprise.infos)
	if len(entNames) == 0 {
		entNames = enterprise.names
	}
	if catalogSource == globalCatalogSourceDesktop {
		// 桌面族权威：企业端点**不贡献 id**（它带着 deep-model / gpt-5.3-codex 这类
		// 已下架或 11102 的旧条目），只按同 id 补元数据（它给的 credits 也可能过期，
		// 故只补缺失字段，不覆盖桌面族已有值）。
		names, infos = v3Names, enrichModelInfos(v3Infos, entInfos)
	} else {
		names, infos = mergeGlobalCatalog(v3Names, v3Infos, entNames, entInfos)
	}
	efforts = mergeEffortBuckets(v3Efforts, entEfforts)
	defaults = mergeEffortDefaults(v3Defaults, entDefaults)
	return names, infos, efforts, defaults, catalogSource, nil
}

// degradedWarnOnce 只在一路模型目录探测**首次**降级时打一行 WARN。
//
// 为什么需要（issue #283）：企业端点是永续 500，而 /v3/config 每次都接住，功能正常；
// 每轮 warm model rates 都重打一行 WARN，4 天 49 次，把真故障淹掉。同状态后续静默。
//
// shortcut: 不区分「恢复后再失败」，进程重启前每种降级只报一次；逐次可见性靠
// warm model rates 那行（每轮都写，ok / failed 都能看出来）。
func degradedWarnOnce(key, format string, args ...any) {
	degradedWarnMu.Lock()
	defer degradedWarnMu.Unlock()
	if degradedWarnSeen[key] {
		return
	}
	degradedWarnSeen[key] = true
	log.Printf(format, args...)
}

var (
	degradedWarnMu   sync.Mutex
	degradedWarnSeen = map[string]bool{}
)

// extractEfforts 从条目列表抽取 effort 能力桶（supportedEfforts 数组优先；
// 缺数组但 defaultEffort 单档非空也入 defaults 桶）并顺带返回有序 names。
func extractEfforts(infos []ModelInfo) (names []string, out []ModelInfo, efforts map[string][]string, defaults map[string]string) {
	names = make([]string, 0, len(infos))
	for _, mi := range infos {
		if mi.ID == "" {
			continue
		}
		names = append(names, mi.ID)
		out = append(out, mi)
		if len(mi.Efforts) > 0 {
			if efforts == nil {
				efforts = make(map[string][]string)
			}
			efforts[mi.ID] = mi.Efforts
		}
		if mi.DefaultEffort != "" {
			if defaults == nil {
				defaults = make(map[string]string)
			}
			defaults[mi.ID] = mi.DefaultEffort
		}
	}
	return names, out, efforts, defaults
}

// enrichModelInfos 仅按同 id 补元数据：base 里已有非零值的字段一律保留（base 权威），
// 缺字段才从后续来源取第一个非零值。**不新增 id**——这是与 mergeGlobalCatalog 的关键
// 差别：issue #216 要求模型目录对齐官方桌面端 picker，别的 UA 族/企业端点只用来给
// 同一批模型补 context/efforts/credits 等字段。
func enrichModelInfos(base []ModelInfo, sources ...[]ModelInfo) []ModelInfo {
	if len(base) == 0 || len(sources) == 0 {
		return base
	}
	index := make(map[string]ModelInfo, len(sources[0]))
	for _, src := range sources {
		for _, mi := range src {
			if mi.ID == "" {
				continue
			}
			if _, ok := index[mi.ID]; !ok {
				index[mi.ID] = mi
			}
		}
	}
	out := make([]ModelInfo, 0, len(base))
	for _, mi := range base {
		extra, ok := index[mi.ID]
		if !ok {
			out = append(out, mi)
			continue
		}
		if mi.ContextWindow == 0 {
			mi.ContextWindow = extra.ContextWindow
		}
		if mi.MaxTokens == 0 {
			mi.MaxTokens = extra.MaxTokens
		}
		if mi.MaxAllowedSize == 0 {
			mi.MaxAllowedSize = extra.MaxAllowedSize
		}
		if mi.Credits == "" {
			mi.Credits = extra.Credits
		}
		if len(mi.Tags) == 0 {
			mi.Tags = extra.Tags
		}
		if len(mi.Efforts) == 0 {
			mi.Efforts = extra.Efforts
		}
		if mi.DefaultEffort == "" {
			mi.DefaultEffort = extra.DefaultEffort
		}
		if mi.Description == "" {
			mi.Description = extra.Description
		}
		if mi.Vendor == "" {
			mi.Vendor = extra.Vendor
		}
		if !mi.SupportsImagesSet && extra.SupportsImagesSet {
			mi.SupportsImages, mi.SupportsImagesSet = extra.SupportsImages, true
		}
		if !mi.SupportsReasoning {
			mi.SupportsReasoning = extra.SupportsReasoning
		}
		if !mi.SupportsToolCall {
			mi.SupportsToolCall = extra.SupportsToolCall
		}
		if !mi.OnlyReasoning {
			mi.OnlyReasoning = extra.OnlyReasoning
		}
		if !mi.CanDisableThinking {
			mi.CanDisableThinking = extra.CanDisableThinking
		}
		out = append(out, mi)
	}
	return out
}

// mergeGlobalCatalog 两路合并（v3 主、企业补缺）：names 按 id 去重（v3 原序在前、
// 企业端点补充项在其原序后追加——稳定输出）；infos 同步合并（v3 条目字段权威，
// 企业端点条目只在 id 缺失时进并集）。
// 窄表形态（infos nil）时保持 nil——无对象字段不编造。
func mergeGlobalCatalog(primaryNames []string, primaryInfos []ModelInfo, secondaryNames []string, secondaryInfos []ModelInfo) (names []string, infos []ModelInfo) {
	if len(secondaryNames) == 0 {
		return primaryNames, primaryInfos
	}
	seen := make(map[string]bool, len(primaryNames)+len(secondaryNames))
	out := make([]string, 0, len(primaryNames)+len(secondaryNames))
	for _, id := range primaryNames {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	var outInfos []ModelInfo
	if primaryInfos != nil {
		outInfos = make([]ModelInfo, 0, len(primaryInfos)+len(secondaryInfos))
		outInfos = append(outInfos, primaryInfos...)
	}
	for _, id := range secondaryNames {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		// 窄表企业响应（secondaryInfos nil / 超出条目数）时该 id 无对象字段，
		// infos 保持原样（调用方按 id 名单输出裸条目，不编造字段）。
		for _, mi := range secondaryInfos {
			if mi.ID == id {
				outInfos = append(outInfos, mi)
				break
			}
		}
	}
	return out, outInfos
}

// mergeEffortBuckets 合并两路 effort 桶：主路（v3）权威，企业端点只补主路缺失的模型档位。
func mergeEffortBuckets(primary, secondary map[string][]string) map[string][]string {
	if len(secondary) == 0 {
		return primary
	}
	out := primary
	if out == nil {
		out = make(map[string][]string, len(secondary))
	}
	for id, v := range secondary {
		if _, ok := out[id]; !ok {
			out[id] = v
		}
	}
	return out
}

// mergeEffortDefaults 合并两路 defaultEffort：主路（v3）权威，企业端点只补缺失。
func mergeEffortDefaults(primary, secondary map[string]string) map[string]string {
	if len(secondary) == 0 {
		return primary
	}
	out := primary
	if out == nil {
		out = make(map[string]string, len(secondary))
	}
	for id, v := range secondary {
		if _, ok := out[id]; !ok {
			out[id] = v
		}
	}
	return out
}

// globalModelsOnce 单端点探测。2xx + 解析出非空名单 → (names, infos, nil)；否则 (nil, nil, err)。
func (c *Client) globalModelsOnce(a *auth.Auth, path string) ([]string, []ModelInfo, error) {
	url := c.chatBase(a) + path // 按 realm 切 base：global 账号 → global base
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	c.CommonHeaders(req, a) // 共享请求头（Origin/Referer/UA），与 FetchModels 同款
	// AccessToken 加锁快照（见 auth.AccessTokenValue：keepalive 刷新在 a.mu 内改写）。
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	resp, err := c.do(req, a, false)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		// 读失败 → 传输层错误：半截 body 不进解析（探测负缓存走 lastFail，不罚号）。
		return nil, nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("global models status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	names, infos, _, _, err := parseGlobalModelNames(raw)
	// 媒体生成模型的旁路留存不在这里做：parse 已把它们剔出对话名单，出图/出视频模型由
	// /v3/config 那条探测路径留存（那份目录完整、含 tag，见 probeGlobalModels）。
	return names, infos, err
}

// parseGlobalModelNames 多信封兼容解析模型目录（国际站曾在多种 envelope 间切换，
// 只认单一形态会把登录成功的账号误判为"无模型"）：
//   - 信封：code 存在且非 0 才拒（无 code 字段也放行）；payload = data（非空非
//     null 时）否则整包；
//   - 模型数组定位：payload 本身是数组，或是 map 下 models/items/list/data/result
//     任一键（递归一层层往下找第一个非空数组）——覆盖 data.models / data.items /
//     data.list / 顶层 models 等变体；
//   - 数组主形态：对象数组按 dynModelEntry 全字段解析（与 CN 目录同构，字段零
//     漂移）：maxInputTokens/maxOutputTokens/maxAllowedSize/supportsReasoning/
//     supportsImages/reasoning.*；id 缺省时回退 name；disabled 剔除；
//   - 窄表：字符串数组 → 仅 ID，元数据留空（窗口由调用方四级查找链兜底）；
//   - 动态兜底：主形态解析不出任何可用条目时（上游换了键名），逐对象宽松解析——
//     id 依次回退 id/modelId/model/name，窗口键回退 contextWindow/maxTokens，
//     reasoning 档位（defaultEffort 新键优先、effort 老键兜底）保留。
//
// 同时产出 effort 能力桶（supportedEfforts/defaultEffort）。
// 解析成功但名单为空 → 返回错误（等价"该端点没给全"）。
func parseGlobalModelNames(raw []byte) (names []string, infos []ModelInfo, efforts map[string][]string, defaults map[string]string, err error) {
	fail := func(e error) ([]string, []ModelInfo, map[string][]string, map[string]string, error) {
		return nil, nil, nil, nil, e
	}
	// 信封层：code 拒绝非 0 业务码（字段缺失 = 放行，兼容无信封直出的目录端点）。
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fail(fmt.Errorf("global models parse: %w", err))
	}
	if codeRaw, ok := envelope["code"]; ok {
		var code int
		if json.Unmarshal(codeRaw, &code) == nil && code != 0 {
			return fail(fmt.Errorf("global models code=%d", code))
		}
	}
	payload := json.RawMessage(raw)
	if data, ok := envelope["data"]; ok {
		if t := strings.TrimSpace(string(data)); t != "" && t != "null" {
			payload = data
		}
	}
	arr, ok := resolveGlobalModelsArray(payload)
	if !ok {
		return fail(fmt.Errorf("global models empty list"))
	}

	// 主形态：对象数组 → dynModelEntry 全字段（与 CN FetchModels 共用解析，零口径漂移）。
	var entries []dynModelEntry
	if json.Unmarshal(arr, &entries) == nil {
		out := make([]string, 0, len(entries))
		objInfos := make([]ModelInfo, 0, len(entries))
		for _, m := range entries {
			// id 回退链 id→modelId→model→name（宽松键仅出现在 global 目录变体里）。
			id := m.ID
			if id == "" {
				id = m.ModelID
			}
			if id == "" {
				id = m.Model
			}
			if id == "" {
				id = m.Name
			}
			if id == "" || m.Disabled {
				continue
			}
			// 非对话条目同样剔掉：这条路径此前**完全没过滤**——seedance-2.5（text-to-video）
			// 因此被当成对话模型列进 global 名单，面板里显示"不支持思考"。媒体生成模型
			// （出图/出视频）的旁路留存由调用方 globalModelsOnce 做（它手上有 Client）。
			mi := m.modelInfo()
			mi.ID = id // name 兜底形态下 id 取自 name，对齐 names 输出
			if nonChatModel(id, m.MaxOutputTokens, m.Tags) {
				continue
			}
			out = append(out, id)
			objInfos = append(objInfos, mi)
			if len(m.Reasoning.SupportedEfforts) > 0 {
				if efforts == nil {
					efforts = make(map[string][]string)
				}
				efforts[id] = m.Reasoning.SupportedEfforts
			}
			if d := m.Reasoning.DefaultEffort; d != "" {
				if defaults == nil {
					defaults = make(map[string]string)
				}
				defaults[id] = d
			}
		}
		if len(out) > 0 {
			return out, objInfos, efforts, defaults, nil
		}
		// 可用条目为 0（键名全对不上）：落入下方动态兜底，不在这里报空。
		efforts, defaults = nil, nil
	}

	// 窄表：字符串数组 → 仅 ID（无 effort 元数据、无对象字段 → infos nil）。
	var strs []string
	if json.Unmarshal(arr, &strs) == nil {
		out := make([]string, 0, len(strs))
		for _, id := range strs {
			if id = strings.TrimSpace(id); id != "" {
				out = append(out, id)
			}
		}
		if len(out) > 0 {
			return out, nil, nil, nil, nil
		}
		return fail(fmt.Errorf("global models empty list"))
	}

	// 动态兜底：逐对象宽松解析（上游换键名时不至于整域空列表）。
	var items []any
	if json.Unmarshal(arr, &items) != nil {
		return fail(fmt.Errorf("global models parse: unsupported payload shape"))
	}
	out := make([]string, 0, len(items))
	infos = make([]ModelInfo, 0, len(items))
	for _, item := range items {
		switch v := item.(type) {
		case string: // 混合数组里的裸 ID：按窄表条目处理
			if id := strings.TrimSpace(v); id != "" {
				out = append(out, id)
				infos = append(infos, ModelInfo{ID: id})
			}
		case map[string]any:
			mi, ok := parseGlobalModelLoose(v)
			if !ok {
				continue
			}
			out = append(out, mi.ID)
			infos = append(infos, mi)
			if len(mi.Efforts) > 0 {
				if efforts == nil {
					efforts = make(map[string][]string)
				}
				efforts[mi.ID] = mi.Efforts
			}
			if mi.DefaultEffort != "" {
				if defaults == nil {
					defaults = make(map[string]string)
				}
				defaults[mi.ID] = mi.DefaultEffort
			}
		}
	}
	if len(out) == 0 {
		return fail(fmt.Errorf("global models empty list"))
	}
	return out, infos, efforts, defaults, nil
}

// resolveGlobalModelsArray 从 payload 定位模型数组：payload 本身是数组直接用；
// 是 map 则依次尝试 models/items/list/data/result 键（递归下钻，取第一个能解析出
// 数组的分支）。找不到数组 → false。
func resolveGlobalModelsArray(payload json.RawMessage) (json.RawMessage, bool) {
	trimmed := strings.TrimSpace(string(payload))
	if strings.HasPrefix(trimmed, "[") {
		return payload, true
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(payload, &obj) != nil {
		return nil, false
	}
	for _, key := range []string{"models", "items", "list", "data", "result"} {
		if nested, ok := obj[key]; ok {
			if arr, ok2 := resolveGlobalModelsArray(nested); ok2 {
				return arr, true
			}
		}
	}
	return nil, false
}

// parseGlobalModelLoose 单模型对象的宽松解析（多信封兜底路径）：id 依次回退
// id/modelId/model/name；窗口/上限键回退 contextWindow/maxTokens；reasoning 档位
// defaultEffort 新键优先、effort 老键兜底。disabled 剔除。Credits 恒不解析
// （PLAN §3.D2：倍率不进 global 路径）。
func parseGlobalModelLoose(obj map[string]any) (ModelInfo, bool) {
	str := func(key string) string {
		s, _ := obj[key].(string)
		return strings.TrimSpace(s)
	}
	id := str("id")
	for _, k := range []string{"modelId", "model", "name"} {
		if id != "" {
			break
		}
		id = str(k)
	}
	if id == "" {
		return ModelInfo{}, false
	}
	if disabled, ok := obj["disabled"].(bool); ok && disabled {
		return ModelInfo{}, false
	}
	num := func(keys ...string) int64 {
		for _, k := range keys {
			if n, ok := obj[k].(float64); ok && n > 0 {
				return int64(n)
			}
		}
		return 0
	}
	mi := ModelInfo{
		ID:             id,
		Name:           str("name"),
		ContextWindow:  num("maxInputTokens", "contextWindow"),
		MaxTokens:      num("maxOutputTokens", "maxTokens"),
		MaxAllowedSize: num("maxAllowedSize"),
	}
	mi.SupportsReasoning, _ = obj["supportsReasoning"].(bool)
	// ok 必须保留：上游「没声明 supportsImages」与「显式声明 false」是两件事。此前
	// 丢掉 ok 把两者都折成 false，于是 11133 的 gateway_hint 会对目录里根本没声明
	// 图片能力的模型断言「不支持图片」（本仓实测 /v1/models 52 个模型 0 个 false，
	// 即 false 基本只来自「缺声明」）——那是编造能力事实，客户端会照 hint 白换模型。
	if v, ok := obj["supportsImages"].(bool); ok {
		mi.SupportsImages, mi.SupportsImagesSet = v, true
	}
	if r, ok := obj["reasoning"].(map[string]any); ok {
		reasonStr := func(key string) string {
			s, _ := r[key].(string)
			return strings.TrimSpace(s)
		}
		mi.DefaultEffort = reasonStr("defaultEffort")
		if mi.DefaultEffort == "" {
			mi.DefaultEffort = reasonStr("effort") // 老模型键兜底，与 CN 侧同款
		}
		mi.CanDisableThinking, _ = r["canDisableThinking"].(bool)
		if arr, ok := r["supportedEfforts"].([]any); ok {
			for _, item := range arr {
				if s, ok := item.(string); ok {
					if s = strings.TrimSpace(s); s != "" {
						mi.Efforts = append(mi.Efforts, s)
					}
				}
			}
		}
	}
	return mi, true
}
