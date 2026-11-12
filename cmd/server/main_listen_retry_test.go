package main

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestListenWithRetryWaitsOnAddrInUse 反证「端口被占就退出」这个旧行为：
// 端口被占时必须一直重试到 ctx 取消，并以 http.ErrServerClosed 结束
// （上层据此不 log.Fatal）。旧代码在这里会直接把整个进程 os.Exit(1)。
func TestListenWithRetryWaitsOnAddrInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听失败: %v", err)
	}
	defer ln.Close()

	srv := &http.Server{Addr: ln.Addr().String()}
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = listenWithRetry(ctx, srv)
	elapsed := time.Since(start)

	if err != http.ErrServerClosed {
		t.Fatalf("端口被占时应持续重试到 ctx 取消并返回 ErrServerClosed，实际 %v", err)
	}
	if elapsed < 2*time.Second {
		t.Fatalf("提前退出，只坚持了 %v（应至少撑到 ctx 的 2.5s）", elapsed)
	}
}

// TestListenWithRetryBindsAfterRelease 端口释放后必须在一轮退避内接管，
// 这是「daemon 抢先手、App 关掉后立刻恢复」的判据。
func TestListenWithRetryBindsAfterRelease(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听失败: %v", err)
	}
	addr := ln.Addr().String()

	srv := &http.Server{Addr: addr}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- listenWithRetry(ctx, srv) }()

	time.Sleep(1500 * time.Millisecond) // 先撞至少一次 EADDRINUSE
	ln.Close()                          // 释放端口

	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("端口释放后 5 秒内仍未接管")
		case <-time.After(200 * time.Millisecond):
		}
		c, derr := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if derr != nil {
			continue
		}
		c.Close()
		srv.Close() // 测试里没有 main 的 shutdown goroutine，这里自己收尾
		if e := <-done; e != http.ErrServerClosed {
			t.Fatalf("接管后应以 ErrServerClosed 收尾，实际 %v", e)
		}
		return
	}
}
