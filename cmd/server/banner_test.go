package main

import (
	"strings"
	"testing"
)

// 掩码只留首尾各两位，短密钥/空密钥整条隐去——横幅是写进 journal 的，不能出明文。
func TestMaskSecret(t *testing.T) {
	const key = "gZb95oiQqT-A7VdssgI27FO"
	cases := []struct{ in, want string }{
		{"", "(未设置)"},
		{"abc", "***"},
		{key, "gZ" + strings.Repeat("*", len(key)-4) + "FO"},
		{"密钥密钥密钥密钥密钥", "密钥" + strings.Repeat("*", 6) + "密钥"},
	}
	for _, c := range cases {
		if got := maskSecret(c.in); got != c.want {
			t.Errorf("maskSecret(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// issue #155：改了 config.json 却不生效时，旧横幅只写 api_key=true，看不出是
// WB2A_API_KEY 覆盖了文件，还是改错了文件。横幅必须报来源，且密钥只出掩码。
func TestListenBannerReportsKeySource(t *testing.T) {
	const key = "gZb95oiQqT-A7VdssgI27FO"
	t.Setenv("WB2A_API_KEY", "")

	got := listenBanner("127.0.0.1:7863", "/etc/wb/config.json", key)
	want := "workbuddy2api listening on 127.0.0.1:7863 (api_key=gZ" + strings.Repeat("*", len(key)-4) +
		"FO, 来源 配置文件 /etc/wb/config.json)，控制台 http://127.0.0.1:7863/panel/"
	if got != want {
		t.Fatalf("横幅=%q want %q", got, want)
	}
	if strings.Contains(got, key) {
		t.Fatalf("横幅泄露了明文 api_key：%q", got)
	}
	// 前缀不能动：面板「运行日志」按它归类（ring_test.go 的 ChSys 用例）。
	if !strings.HasPrefix(got, "workbuddy2api listening on 127.0.0.1:7863") {
		t.Fatalf("横幅前缀被改动，面板日志归类会失配：%q", got)
	}

	// env 非空即覆盖文件（同 applyEnv）：来源必须改口。
	t.Setenv("WB2A_API_KEY", "env-key-12345678")
	got = listenBanner(":7863", "/etc/wb/config.json", "env-key-12345678")
	if !strings.Contains(got, "来源 环境变量 WB2A_API_KEY") {
		t.Fatalf("env 覆盖时来源仍报文件，issue #155 的困惑照旧：%q", got)
	}

	// 目录只读、自动生成失败 → 退回默认+env：来源不许继续写那份没读到的路径。
	t.Setenv("WB2A_API_KEY", "")
	got = listenBanner(":7863", "", "abc")
	if !strings.Contains(got, "来源 内置默认值") || strings.Contains(got, "配置文件") {
		t.Fatalf("退回默认时来源应报内置默认值、且不许再提配置文件：%q", got)
	}
}
