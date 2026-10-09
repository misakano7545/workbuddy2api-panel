// 分池选号域：按 realm（cn/global）过滤选号与可用集合。realm=="" 退化为现状。
package pool

import (
	"math"
	"sort"
	"time"
)

// expiringVirtualSlots 快过期账号在新会话候选集中的虚拟实例权重（**未注入窗口时的
// 兼容口径**，见 expiringUrgency）。3:1 是温和偏好，不是固定比例：账号组成变化会
// 自然改变最终占比。
const expiringVirtualSlots = 3

// expiringUrgencyMin / expiringUrgencyMax 平滑紧迫度倍率的两端（灵感：上游 issue #140
// 第 1、2 点）：窗口边缘取 Min，越逼近到期越攀升到 Max。
//
// Max 必须 **≥ 基础权重的天花板**（1 + credits 项 10 + 闲置项上限 5 = 16），否则 issue
// 第 2 点无解：余量 50/500 分的号基础权重 2，压不过 500 分号的 11~16，倍率再陡也翻不了盘
// （实测 linear 5 / sqrt 5 都不够，见 expiring_urgency_test.go 的 flip 用例）。
//
// 已知边界：极端情形下仍可能压不过——高余额号**同时**闲置满载（权重 16）而低余额号刚用过
// （权重 2，无闲置补偿）。要连这种也压过，把 Max 提到 35；那会让批次临终几小时几乎独占流量
// （仍是加权随机，不是硬过滤），故不再默认加大。
// ponytail: 先作常量；要按部署调档再升级为 pool.* 配置项。
const (
	expiringUrgencyMin = 1.5
	expiringUrgencyMax = 20.0

	// expiringCreditScale 窗口内账号的 credits 项系数（原 10 → 2）。
	//
	// issue #140 第 2 点的根因：余额基数**恒**主导临期决策——余量 500 分号的 credits 项是 10、
	// 50 分号只有 1，而紧迫度倍率对「1 天 vs 6 天」这种差值能达到的比上限只有 ~8.4 倍（无论
	// Min/Max 怎么调、形状多陡），压不过 10:1 的基数差。所以必须把两轴对调：紧迫度为主轴
	// （1.5→20），余额退为次轴（1→3，只用来在同紧迫度内挑余量大的）。
	expiringCreditScale = 0.2
)

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// realm=="" 退化为 AvailableUIDs（现状语义）。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// WeightedAvailableUIDsForModelRealm 返回带虚拟实例权重的可用账号列表。
//
// 普通账号出现 1 次；有效快过期账号出现 expiringVirtualSlots 次。调用方继续按
// 原有序列表哈希，即可让新会话对快过期账号形成温和偏好。重复项按 UID 排序后
// 展开，保证同一账号拓扑下不同进程得到一致列表。
//
// 该方法是现有 AvailableUIDsForModelRealm 的增量入口，不改变旧方法语义，也不
// 修改配置、状态或 Redis schema。prefer_expiring=false 时退化为逐账号一次。
func (p *Pool) WeightedAvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)

	out := make([]string, 0, len(uids))
	for _, uid := range uids {
		slots := 1
		if p.preferExpiring {
			if u := p.expiringUrgency(p.byUID[uid], now); u > 1 {
				slots = int(math.Round(u))
			}
		}
		for i := 0; i < slots; i++ {
			out = append(out, uid)
		}
	}
	return out
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}
