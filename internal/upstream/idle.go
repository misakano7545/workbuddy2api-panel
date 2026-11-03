// idle.go 聊天 SSE 流中空闲监控：活跃吐数据续命不掐，静默超过阈值才断流（释放租约）。
package upstream

import (
	"context"
	"io"
	"sync"
	"time"
)

// idleMonitoringBody 包在聊天 SSE body 外层：
// 每次读到底层数据（n>0）就刷新 lastRead；后台 goroutine 周期检查，
// 静默超过 idle 就 cancel 请求 context，中断阻塞中的 Read。
type idleMonitoringBody struct {
	rc       io.ReadCloser
	mu       sync.Mutex
	lastRead time.Time
	stopCh   chan struct{}
	cancel   context.CancelFunc

	closeOnce sync.Once
	closeErr  error
}

func (b *idleMonitoringBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.mu.Lock()
		b.lastRead = time.Now()
		b.mu.Unlock()
	}
	return n, err
}

// tailDrainMax / tailDrainWait Close 前读掉流尾的上限：读到 EOF 连接才回连接池。
// 读者在 [DONE] 处就停了，上游随后才发 chunked 结束块（几个字节，实测晚到十几毫秒）；
// 直接取消 context 会让 Transport 丢弃连接，流式请求每次都要重新 TCP+TLS（经代理时
// 还要再建一次隧道，首字节多出几百毫秒）。流没结束（客户端中途断开、换号）时最多多读
// 4KB、多等 100ms 就照旧取消、关闭。
const (
	tailDrainMax  = 4 << 10
	tailDrainWait = 100 * time.Millisecond
)

// Close 停掉后台 goroutine，读掉流尾让连接可复用，再取消请求 context、关闭底流，保证无泄漏。
// 读尾放在独立 goroutine 里限时等：Close 不能指望底流的 Read 一定会被取消打断；超时后
// 取消与关闭照常进行，HTTP body 被关闭时阻塞的 Read 随即返回，读尾 goroutine 退出。
//
// 幂等：只有第一次真正执行，之后原样返回第一次的结果。调用链里常有两层各关一次（续写读取器
// 在 [DONE] 处关一次，处理函数结束时外层再关一次）；第二次若再去读流尾，读到的是「body 已
// 关闭」，经代理时会被当成代理故障、清掉节点的空闲连接。
func (b *idleMonitoringBody) Close() error {
	b.closeOnce.Do(func() { b.closeErr = b.close() })
	return b.closeErr
}

func (b *idleMonitoringBody) close() error {
	close(b.stopCh)
	drained := make(chan struct{})
	go func() {
		_, _ = io.CopyN(io.Discard, b.rc, tailDrainMax)
		close(drained)
	}()
	t := time.NewTimer(tailDrainWait)
	select {
	case <-drained:
	case <-t.C:
	}
	t.Stop()
	b.cancel()
	return b.rc.Close()
}

func (b *idleMonitoringBody) idleFor() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Since(b.lastRead)
}

// monitorBody 若 idle<=0 直接返回原底流（禁用空闲监控）；
// 否则包上流中空闲监控。计时从返回 body 之后开始——首字节阶段由
// Transport.ResponseHeaderTimeout 管，这里不抢跑。
func monitorBody(rc io.ReadCloser, idle time.Duration, cancel context.CancelFunc) io.ReadCloser {
	if idle <= 0 {
		return rc
	}
	b := &idleMonitoringBody{
		rc:       rc,
		lastRead: time.Now(),
		stopCh:   make(chan struct{}),
		cancel:   cancel,
	}
	go func() {
		t := time.NewTicker(idleTick(idle))
		defer t.Stop()
		for {
			select {
			case <-b.stopCh:
				return
			case <-t.C:
				if b.idleFor() > idle {
					cancel()
					return
				}
			}
		}
	}()
	return b
}

// idleTick 返回监控周期：idle/4，钳在 [10ms, 1s]。小 idle 也能快速发现，大小值避免空转。
func idleTick(idle time.Duration) time.Duration {
	d := idle / 4
	if d > time.Second {
		d = time.Second
	}
	if d < 10*time.Millisecond {
		d = 10 * time.Millisecond
	}
	return d
}
