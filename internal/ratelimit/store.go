package ratelimit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// policyVersion 是策略文件格式版本。加字段不用升版本（读到零值即「未设置」），
// 只有语义变更（老文件按新语义会算出不同额度）才升，并在 LoadPolicy 里做迁移。
const policyVersion = 1

// policyFile 是落盘格式：版本号独立字段，Policy 内联。
type policyFile struct {
	Version int `json:"version"`
	Policy
}

// LoadPolicy 读取策略文件。
//
// 文件不存在是正常状态（全新部署）：返回 DefaultPolicy 与 nil，让页面一打开就有
// 「跟随上游」的默认口径，而不是要求先点一次保存。
// 解析失败则返回 DefaultPolicy 与错误：错误要冒泡到日志，但服务仍按默认策略跑，
// 不能因为一个坏文件把网关的请求路径带崩。
func LoadPolicy(path string) (Policy, error) {
	if path == "" {
		return DefaultPolicy(), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return DefaultPolicy(), nil
		}
		return DefaultPolicy(), fmt.Errorf("读取模型限流映射策略 %s: %w", path, err)
	}
	var f policyFile
	if err := json.Unmarshal(b, &f); err != nil {
		return DefaultPolicy(), fmt.Errorf("解析模型限流映射策略 %s: %w", path, err)
	}
	if f.Version > policyVersion {
		return DefaultPolicy(), fmt.Errorf("模型限流映射策略 %s 版本 %d 高于本程序支持的 %d", path, f.Version, policyVersion)
	}
	p := f.Policy
	p.Default = Normalize(p.Default)
	for i := range p.Overrides {
		p.Overrides[i] = Normalize(p.Overrides[i])
	}
	if err := Validate(p); err != nil {
		return DefaultPolicy(), fmt.Errorf("模型限流映射策略 %s 不合规，已回落到默认口径: %w", path, err)
	}
	return p, nil
}

// SavePolicy 原子落盘策略文件（同目录临时文件 + rename）。
//
// rename 在同一文件系统上是原子的：读侧要么看到旧文件、要么看到新文件，
// 不会读到写了一半的 JSON（那会让重启后的策略校验直接失败并回落到默认）。
func SavePolicy(path string, p Policy) error {
	if path == "" {
		return errors.New("模型限流映射策略路径为空")
	}
	if err := Validate(p); err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建策略目录 %s: %w", dir, err)
		}
	}
	b, err := json.MarshalIndent(policyFile{Version: policyVersion, Policy: p}, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化模型限流映射策略: %w", err)
	}
	b = append(b, ' ')
	tmp := path + ".tmp"
	// 0o600：策略里含运营的预算口径，不是秘密但也不必外泄。
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("写入策略临时文件 %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换策略文件 %s: %w", path, err)
	}
	return nil
}
