package server

import (
	"context"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// 客户端先断开时不得报成「疑似上游断流」，也不得再发一次注定失败的续写。
//
// 实测踩过：CodeBuddy 在 189s 后放弃，上游被动 cancel，日志记成「疑似上游断流」
// 把排障引向错误方向；另一条 seg=1 续写失败也是 ctx 已取消后白发的请求。
func TestContinueClientGoneNotUpstreamCut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &continueReader{ctx: ctx, acct: &auth.Auth{UID: "u"}, body: []byte(`{"max_tokens":100}`)}
	r.text.WriteString("partial")
	cancel()

	if !r.clientGone() {
		t.Fatal("ctx 已取消时应判定客户端断开")
	}
	if r.canContinue(false) {
		t.Fatal("客户端断开后不该再发续写请求")
	}
}

// 读错误为 context.Canceled（上游 body 被本地 cancel）时同样归因为客户端断开。
func TestContinueClientGoneViaReadErr(t *testing.T) {
	r := &continueReader{ctx: context.Background(), acct: &auth.Auth{UID: "u"}, body: []byte(`{"max_tokens":100}`)}
	r.readErr = context.Canceled
	if !r.clientGone() {
		t.Fatal("读错误 context.Canceled 应判定客户端断开")
	}
	// 上游真断流（无 ctx 取消、无 readErr）不得被误判
	r2 := &continueReader{ctx: context.Background(), acct: &auth.Auth{UID: "u"}, body: []byte(`{"max_tokens":100}`)}
	if r2.clientGone() {
		t.Fatal("无取消信号时不该判定客户端断开（那是上游断流）")
	}
}
