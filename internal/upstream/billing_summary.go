// billing_summary.go 国际版（global realm）计费新口径：summary + paid/free packages。
//
// 为什么单独一条链：官方 5.8.4 国际版的额度查询改成了三件套（真机抓包实测，
// 2026-10-20，www.workbuddy.ai）：
//
//	POST /billing/meter/get-user-resource-summary      {}   ← 当前周期聚合（remain/total）
//	POST /billing/meter/get-user-resource-paid-packages {}  ← 付费包明细（带到期时间）
//	POST /billing/meter/get-user-resource-free-packages {}  ← 免费包明细（带到期时间）
//
// 而本仓原先走的是 CN 那套 `/billing/meter/get-user-resource` 带分页 body
// （PageNumber/PageSize/ProductCode/Status/PackageEndTimeRange*）。国际版对该路径
// 返回的形态已与 CN 不同（summary 里 CycleTotalCapacity / CycleRemainCapacity 是
// **字符串小数**，且没有 Accounts 树），继续按 CN 解析会得到恒 0 的余额——积分保底
// 随即把这些号当耗尽号挡出选号。
//
// 分工：summary 给权威的 remain/total（当前周期口径），paid/free 明细只用来补齐
// 「哪天到期、那一批还剩多少」。两者按 PackageCode 关联——summary 的 Packages 也带
// PackageCode，故能精确对齐，不必靠顺序或名字猜。明细拉不到时降级为「只有总额没有
// 到期批次」（面板少一行到期提醒），绝不因为明细失败把总额也丢掉。
package upstream

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/cst"
	"github.com/misakano7545/workbuddy2api-panel/internal/logfmt"
)

const (
	billingSummaryPath = "/billing/meter/get-user-resource-summary"
	billingPaidPath    = "/billing/meter/get-user-resource-paid-packages"
	billingFreePath    = "/billing/meter/get-user-resource-free-packages"
)

// flexFloat 解析「可能是 JSON 数字也可能是字符串」的容量字段。
//
// 上游同一语义字段两套形态并存：summary 里是字符串小数（"335.47000026"），
// 明细里是 JSON 整数 + 独立的 *Precise 字符串。用这个类型兜住，避免为每个字段
// 写两遍解析，也避免字符串形态被 json.Unmarshal 直接判错整段失败。
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(raw []byte) error {
	s := strings.TrimSpace(string(raw))
	if s == "null" || s == `""` {
		*f = 0
		return nil
	}
	s = strings.Trim(s, `"`)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		// 单个脏字段不该让整次余额查询失败：按 0 处理（该项计不进去，其余照算）。
		*f = 0
		return nil
	}
	*f = flexFloat(v)
	return nil
}

// summaryPackage summary.Packages[] 的单条：当前周期的用量聚合。
type summaryPackage struct {
	PackageCode         string    `json:"PackageCode"`
	CycleTotalCapacity  flexFloat `json:"CycleTotalCapacity"`
	CycleRemainCapacity flexFloat `json:"CycleRemainCapacity"`
	CycleUsedCapacity   flexFloat `json:"CycleUsedCapacity"`
	CapacityUnit        string    `json:"CapacityUnit"`
}

// packageDetail paid/free packages 的单条：与 CN 的 Accounts[] 同构，另带 *Precise
// 字符串形态，以及用于判定到期的三个时间字段。
type packageDetail struct {
	PackageCode         string    `json:"PackageCode"`
	PackageName         string    `json:"PackageName"`
	DeductionEndTime    int64     `json:"DeductionEndTime"` // epoch 毫秒，真实扣费截止
	ExpiredTime         string    `json:"ExpiredTime"`
	CycleEndTime        string    `json:"CycleEndTime"`
	CapacitySize        flexFloat `json:"CapacitySize"`
	CapacityRemain      flexFloat `json:"CapacityRemain"`
	CapacityUsed        flexFloat `json:"CapacityUsed"`
	CycleCapacitySize   flexFloat `json:"CycleCapacitySize"`
	CycleCapacityRemain flexFloat `json:"CycleCapacityRemain"`
	CycleCapacityUsed   flexFloat `json:"CycleCapacityUsed"`
	// *Precise 优先：整数形态会把 0.47 这类小数截断，两者都在时以 Precise 为准。
	CapacitySizePrecise        flexFloat `json:"CapacitySizePrecise"`
	CapacityRemainPrecise      flexFloat `json:"CapacityRemainPrecise"`
	CycleCapacitySizePrecise   flexFloat `json:"CycleCapacitySizePrecise"`
	CycleCapacityRemainPrecise flexFloat `json:"CycleCapacityRemainPrecise"`
}

// summaryReply / packagesReply 是两条端点的信封内层。
type summaryReply struct {
	Packages                []summaryPackage `json:"Packages"`
	SubscriptionPackageName string           `json:"SubscriptionPackageName"`
	IsPaidUser              bool             `json:"IsPaidUser"`
}

type packagesReply struct {
	Accounts   []packageDetail `json:"Accounts"`
	TotalCount int             `json:"TotalCount"`
}

// globalUserResource 国际版额度查询：summary 给总额，paid/free 明细补到期批次。
// 返回值口径与 CN 路径完全一致（remain/total/expiring/earliest/buckets），
// 上游调用方无感知。
func (c *Client) globalUserResource(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, earliestAt time.Time, earliestRemaining int64, buckets []ExpiryBucket, err error) {
	var raw json.RawMessage
	// summary 是主口径，失败即整次失败（明细单独拿到也没总额可对）。
	raw, err = c.billingJSON(a, "POST", billingSummaryPath, map[string]any{})
	if err != nil {
		return 0, 0, 0, time.Time{}, 0, nil, err
	}
	var sum summaryReply
	if uerr := json.Unmarshal(raw, &sum); uerr != nil {
		return 0, 0, 0, time.Time{}, 0, nil, fmt.Errorf("billing summary parse: %w", uerr)
	}
	now := time.Now()
	// summary 的 Packages 是「当前周期的权威用量」：remain/total 以它为准。
	remainByCode := make(map[string]int64, len(sum.Packages))
	for _, p := range sum.Packages {
		r := int64(p.CycleRemainCapacity)
		s := int64(p.CycleTotalCapacity)
		if r < 0 {
			r = 0
		}
		if r > s && s > 0 {
			r = s
		}
		remain += r
		total += s
		if p.PackageCode != "" {
			remainByCode[p.PackageCode] = r
		}
	}

	// 明细只为「到期批次」服务：失败不影响总额（降级为无到期提醒）。
	codes := make([]string, 0, len(sum.Packages))
	for _, p := range sum.Packages {
		if p.PackageCode != "" {
			codes = append(codes, p.PackageCode)
		}
	}
	details := c.globalPackageDetails(a, codes)
	// 归并的最小单位是**（PackageCode, 计量周期）**，不是 PackageCode 本身。
	//
	// 错在哪（issue #200，2026-11-10 线上取证）：国际版 Bonus Pack 每天发一个包，
	// **所有包共用同一个 PackageCode**，明细里因此同码多条、CycleEndTime 逐日递推
	// （实测某号 32 条同码，11-11 起每天一批、每批 30 分）。此前只按码归并、取该码
	// 「最早的未来到期」那条，再把 summary 里**当前周期**的剩余配到这个旧 end 上：
	//
	//	旧周期（已用完）的 end + 新周期的 remain → 面板显示「927 分明天到期」，
	//	而真实情况是「明天 27 分、后天 30 分……逐日到期」。
	//
	// 更坏的是 expiring（7 天内快过期额度）也被整笔算进去，池子会把这号当成
	// 「积分马上作废」优先消耗——这不只是显示问题。
	//
	// 现在：每行明细用它**自己的** end 与**自己的** balanceRemain()，同一 (码, end)
	// 只留剩余最大的一条（付费/免费两条端点可能给同一批）。
	type pick struct {
		d        packageDetail
		end      time.Time
		estimate bool // end 是按计量周期推算的
	}
	type cycleKey struct {
		code string
		end  int64 // Unix 秒：同一周期同码的 end 必然相同，用它当周期标识
	}
	merged := make(map[cycleKey]pick, len(details))
	cycleFallback := 0
	for _, d := range details {
		end, fromCycle, ok := packageEndAtEx(d.DeductionEndTime, d.ExpiredTime, d.CycleEndTime)
		if !ok || !end.After(now) {
			continue
		}
		if fromCycle {
			cycleFallback++
		}
		k := cycleKey{d.PackageCode, end.Unix()}
		if cur, seen := merged[k]; seen {
			// 已有一条：留剩余更大的（同一批可能同时出现在 paid / free 两条端点）。
			if d.balanceRemain() > cur.d.balanceRemain() {
				merged[k] = pick{d: d, end: end, estimate: fromCycle}
			}
			continue
		}
		merged[k] = pick{d: d, end: end, estimate: fromCycle}
	}
	if cycleFallback > 0 {
		// 日期是按计量周期末日推算的（缺 DeductionEndTime / ExpiredTime）。仍然计入，
		// 但留一行 WARN：这个口径不对等于真实扣费截止，排障时别被它带偏。
		log.Printf("WARN: [billing] %d 个积分类的到期日只能按计量周期（CycleEndTime）推算，非真实扣费截止（acct=%s）",
			cycleFallback, logfmt.Label(a.UID, a.Nickname))
	}
	// 每个码有多少个未来周期：只有一个时，「借用 summary 余额」不会跨周期串味，
	// 保留给「明细自报 0 而 summary 有数」的套餐型包（历史行为）；多个时一律不借，
	// 否则又回到把新周期余额挂到所有周期上的老问题。
	futurePerCode := make(map[string]int, len(merged))
	for k := range merged {
		futurePerCode[k.code]++
	}
	byDate := make(map[string]*ExpiryBucket)
	for k, p := range merged {
		d := p.d
		r := int64(d.balanceRemain())
		if r <= 0 && futurePerCode[k.code] == 1 {
			if sr, ok := remainByCode[k.code]; ok {
				r = sr
			}
		}
		if r <= 0 {
			// 该周期已用完 / 明细没给剩余：不贡献额度，也不该拉早 earliestAt。
			continue
		}
		end := p.end
		if earliestAt.IsZero() || end.Before(earliestAt) {
			earliestAt = end
		}
		day := end.In(softRateResetLoc).Format("2006-01-02")
		b, seen := byDate[day]
		if !seen {
			b = &ExpiryBucket{Date: day, ExpireAt: end}
			byDate[day] = b
		} else if end.Before(b.ExpireAt) {
			b.ExpireAt = end
		}
		if p.estimate {
			// 同日多批里只要有一批是推算的，整行就标注——不能只标最早那批：
			// 面板一行只展示「该日」，混了两种口径时保守地显示为推算。
			b.Estimated = true
		}
		b.Remain += r
		if soon > 0 && !end.After(now.Add(soon)) {
			expiring += r
		}
	}
	dates := make([]string, 0, len(byDate))
	for d := range byDate {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	if len(dates) > 0 {
		buckets = make([]ExpiryBucket, 0, len(dates))
		for _, d := range dates {
			buckets = append(buckets, *byDate[d])
		}
		earliestRemaining = buckets[0].Remain
	}
	c.notifyCredits(a, remain, nil)
	return remain, total, expiring, earliestAt, earliestRemaining, buckets, nil
}

// globalPackageDetails 拉 paid + free 两份明细并合并。
// 两条都失败 → nil（调用方降级为「无到期批次」）；一条失败只损失那一类的到期信息。
//
// **两条端点的请求体不同形**（2026-10-20 双版本真机抓包实测，CN/GL 完全一致）：
//
//	paid-packages: {PageNumber, PageSize, PackageCodes, Status:[0,3], NeedRenewInfo:true}
//	               —— **没有** SlicePeriod 时间切片
//	free-packages: {PageNumber, PageSize, PackageCodes, Status:[0,3],
//	               SlicePeriodStartTime:<当日> 00:00:00, SlicePeriodEndTime:<当日> 23:59:59}
//	               —— 带时间切片，**没有** NeedRenewInfo
//
// 此前两条共用同一个 body（都带时间切片、都不带 NeedRenewInfo），与官方客户端不符：
// `NeedRenewInfo` 是「是否要返回续订信息」的开关，paid 包靠它拿续订窗口。
// PackageCodes 用 summary 已给的码缩小扫描范围（不按时间盲扫，避免漏包）；
// Status [0,3] = 有效/已过期都要（过期包带权威 DeductionEndTime）。
func (c *Client) globalPackageDetails(a *auth.Auth, codes []string) []packageDetail {
	now := time.Now().In(softRateResetLoc)
	paidBody := map[string]any{
		"PageNumber":    1,
		"PageSize":      200,
		"PackageCodes":  codes,
		"Status":        []int{0, 3},
		"NeedRenewInfo": true,
	}
	freeBody := map[string]any{
		"PageNumber":           1,
		"PageSize":             200,
		"PackageCodes":         codes,
		"Status":               []int{0, 3},
		"SlicePeriodStartTime": cst.Day(now) + " 00:00:00",
		"SlicePeriodEndTime":   cst.Day(now) + " 23:59:59",
	}
	var out []packageDetail
	for _, req := range []struct {
		path string
		body map[string]any
	}{
		{billingPaidPath, paidBody},
		{billingFreePath, freeBody},
	} {
		raw, err := c.billingJSON(a, "POST", req.path, req.body)
		if err != nil {
			continue
		}
		var rep packagesReply
		if json.Unmarshal(raw, &rep) != nil {
			continue
		}
		out = append(out, rep.Accounts...)
	}
	return out
}

// balanceRemain 明细自报的剩余额度：Cycle 期套餐优先，回退整包容量；Precise 形态优先。
func (d packageDetail) balanceRemain() float64 {
	prefer := func(precise, plain, preciseCycle, plainCycle flexFloat) float64 {
		if preciseCycle > 0 || plainCycle > 0 {
			if preciseCycle > 0 {
				return float64(preciseCycle)
			}
			return float64(plainCycle)
		}
		if precise > 0 {
			return float64(precise)
		}
		return float64(plain)
	}
	return prefer(d.CapacityRemainPrecise, d.CapacityRemain,
		d.CycleCapacityRemainPrecise, d.CycleCapacityRemain)
}
