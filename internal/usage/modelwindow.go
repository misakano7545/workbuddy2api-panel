package usage

import (
	"sort"
	"strings"
	"time"
)

// ModelUsage 是 (域, 账号, 模型) 在窗口内的消耗合计。
//
// 为什么单列一个类型而不复用 Agg：模型限流映射的配额是**按账号**发的（单账号 200M token /
// 24h 滚动），而 Agg 把账号这一维合并掉了；这里只带限流用得到的四个数，字段少到可以
// 逐条喂给 internal/ratelimit 的 Deps.Usage，中间不需要任何再解释。
type ModelUsage struct {
	Realm    string  `json:"realm"`
	UID      string  `json:"uid"`
	Model    string  `json:"model"`
	Tokens   int64   `json:"tokens"`
	Requests int64   `json:"requests"`
	Errors   int64   `json:"errors"`
	Credits  float64 `json:"credits"`
	// FirstAt 是窗口内最早一笔消耗所在的整点桶起点（近似值）。
	//
	// 模型限流映射要靠它推窗口边界（上游窗口从该号该模型第一次请求起算），桶只有整点
	// 粒度，所以是近似值——误差在一小时内，够用来给客户端一个 Retry-After 提示。
	FirstAt time.Time `json:"first_at,omitempty"`
}

// modelUsageKey 是聚合键。分桶 key 里还含「请求时生效的倍率」，同一账号同一模型在
// 倍率变化前后落在不同桶里，所以这里必须重新按三元组聚合，不能直接把桶当行。
type modelUsageKey struct {
	realm string
	uid   string
	model string
}

// ModelWindow 把窗口内的桶聚合成 (realm, uid, model) 行。
//
// 窗口边界复用 Window.bounds() / bucketTime()，与 Snapshot、BucketRows 同一口径：
// 小时桶按整点、日桶（90 天前折叠出来的长期数据）按日起点入窗。窗口判定只写一处，
// 改口径时不会在这里漂移成第二套。
//
// r 为 nil（用量记录未启用）返回 nil：调用方按「窗口内没有任何消耗」处理，不能
// 因为记录器缺席就把额度算成已用尽。
//
// 输出按 realm、model、uid 排序：请求路径与页面读的都是这份结果，顺序稳定才能让
// 同一份数据两次拉取逐字节相同（便于对账，也让测试能直接断言行序）。
func (r *Recorder) ModelWindow(w Window) []ModelUsage {
	if r == nil {
		return nil
	}
	from, to := w.bounds()
	windowed := !from.IsZero() || !to.IsZero()

	r.mu.Lock()
	agg := make(map[modelUsageKey]ModelUsage, len(r.buckets))
	for _, b := range r.buckets {
		// ts 一直算：窗口判定要用它，FirstAt 也要用它（脏 scope 只是没有 FirstAt，
		// 不影响其余字段的统计）。
		ts, hasTS := bucketTime(b.Scope)
		if windowed {
			if !hasTS {
				continue // 脏 scope 不进任何口径（与 BucketRows 一致）
			}
			if !from.IsZero() && ts.Before(from) {
				continue
			}
			if !to.IsZero() && ts.After(to) {
				continue
			}
		}
		key := modelUsageKey{
			realm: strings.ToLower(strings.TrimSpace(b.Realm)),
			uid:   b.UID,
			model: canonicalUsageModel(b.Model),
		}
		// 值类型累加：桶数多、并发低，指针 map 换不来什么，反而多一层解引用与生命周期。
		cur := agg[key]
		cur.Tokens += b.TT
		cur.Requests += b.Req
		cur.Errors += b.Err
		cur.Credits += b.CR
		if hasTS && (cur.FirstAt.IsZero() || ts.Before(cur.FirstAt)) {
			cur.FirstAt = ts
		}
		agg[key] = cur
	}
	r.mu.Unlock()

	out := make([]ModelUsage, 0, len(agg))
	for key, v := range agg {
		v.Realm, v.UID, v.Model = key.realm, key.uid, key.model
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Realm != b.Realm {
			return a.Realm < b.Realm
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.UID < b.UID
	})
	return out
}
