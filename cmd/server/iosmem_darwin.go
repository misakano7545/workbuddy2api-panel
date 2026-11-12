//go:build darwin

package main

// iOS 上守护进程的死因一直是「无信号、无 panic、无 bye」的 SIGKILL。
// 唯一留下的现场是第一份 JetsamEvent：
//
//	"reason":"per-process-limit","rpages":1152,"lifetimeMax":1152
//
// 1152 页 × 16KB = 18.0MB —— 进程撞上一个 18MB 的 per-process 上限被内核砍掉。
// plist 里的 JetsamProperties 只在 system 域生效；这个 job 落在 user/501
// （launchctl print 的 domain = user/501 是铁证），所以那些键一直是空转。
//
// 唯一的、不依赖 job 落在哪个域的办法：进程自己把上限抬上去。
// memorystatus_control(MEMORYSTATUS_CMD_SET_JETSAM_TASK_LIMIT) 需要
// com.apple.private.memorystatus 这条 entitlement（已打进签名）。

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// XNU bsd/sys/syscall.h: __memorystatus_control = 440
const sysMemorystatusControl = 440

// memorystatus.h: 把本 task 的 jetsam 上限设成 flags（字节）。
const cmdSetJetsamTaskLimit = 6

// 256MiB。进程真实占用不到 20MB，这里只是把天花板抬到「永远不会碰到」的高度。
const jetsamLimitBytes = 256 << 20

// callMemorystatus 直接把参数塞进寄存器：CGO 关着，没法走 libSystem 的头文件。
//   int memorystatus_control(uint32_t command, int32_t pid, uint32_t flags,
//                            void *buffer, size_t buffersize);
func callMemorystatus(cmd, pid, flags uintptr) (int, syscall.Errno) {
	r, _, e := syscall.Syscall6(sysMemorystatusControl, cmd, pid, flags, 0, 0, 0)
	return int(r), e
}

// applyProcessLimits 在 main 早期执行，返回一行能直接写进日志的结论。
// 失败不影响启动：拿不到上限就退回「把堆压小」那条路（见 plist 的 GOMEMLIMIT）。
func applyProcessLimits() string {
	pid := uintptr(os.Getpid())
	ret, errno := callMemorystatus(cmdSetJetsamTaskLimit, pid, jetsamLimitBytes)
	if errno != 0 {
		return fmt.Sprintf("抬高失败 ret=%d errno=%d(%v) 目标=%dMB", ret, int(errno), errno, jetsamLimitBytes>>20)
	}
	return fmt.Sprintf("已抬到 %dMB ret=%d", jetsamLimitBytes>>20, ret)
}

// maxRSSMB 取进程生命周期内的 RSS 峰值。Darwin 的 getrusage 单位就是字节
// （Linux 是 KB），所以不能跨平台共用一份实现。
// 心跳里带上它，SIGKILL 之后再回看就能分辨「撞内存」和「被外部杀」。
func maxRSSMB() int {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return -1
	}
	if ru.Maxrss <= 0 {
		return 0
	}
	return int(ru.Maxrss >> 20)
}

// logJetsamEvidence 启动时把 CrashReporter 里的 JetsamEvent 名单抄进 server.log。
// 之前只能靠人拿 Filza 去翻；现在每次重启都会自己留下「最近三次内核杀进程」的现场。
func logJetsamEvidence() {
	dirs := []string{
		"/var/mobile/Library/Logs/CrashReporter",
		"/private/var/mobile/Library/Logs/CrashReporter",
	}
	var files []string
	seen := map[string]bool{}
	for _, d := range dirs {
		if seen[d] {
			continue
		}
		seen[d] = true
		m, _ := filepath.Glob(filepath.Join(d, "JetsamEvent-*.ips"))
		files = append(files, m...)
	}
	if len(files) == 0 {
		log.Printf("[jetsam] CrashReporter 里没有 JetsamEvent-*.ips（或目录不可读）")
		return
	}
	sort.Slice(files, func(i, j int) bool {
		si, ei := os.Stat(files[i])
		sj, ej := os.Stat(files[j])
		if ei != nil || ej != nil {
			return files[i] > files[j]
		}
		return si.ModTime().After(sj.ModTime())
	})
	for i, f := range files {
		if i >= 3 {
			break
		}
		st, err := os.Stat(f)
		if err != nil {
			continue
		}
		log.Printf("[jetsam] 近期事件 %s  %s  %dB", filepath.Base(f), st.ModTime().Format("01-02 15:04:05"), st.Size())
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(fh, 96<<10))
		fh.Close()
		s := string(b)
		if !strings.Contains(s, "wb2api") {
			continue
		}
		for _, key := range []string{`"reason"`, `"rpages"`, `"lifetimeMax"`, `"pageSize"`} {
			idx := strings.Index(s, key)
			if idx < 0 {
				continue
			}
			end := idx + 90
			if end > len(s) {
				end = len(s)
			}
			log.Printf("[jetsam] %s … %s", filepath.Base(f), strings.TrimSpace(s[idx:end]))
		}
	}
}
