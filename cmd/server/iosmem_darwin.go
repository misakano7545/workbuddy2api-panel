//go:build darwin

package main

// iOS 上守护进程的死因一直是「无信号、无 panic、无 bye」的 SIGKILL。
// 唯一的现场是第一份 JetsamEvent：
//
//	"reason":"per-process-limit","rpages":1152,"lifetimeMax":1152
//
// 1152 页 × 16KB = 18.0MB —— 进程撞上一个 18MB 的 per-process 上限被内核砍掉。
// plist 里的 JetsamProperties 只在 system 域生效；RootHide 的 launchd 会把
// LaunchDaemon 一律塞进 user/foreground（install.log 实测：bootstrap system 报 OK，
// print 出来仍是 domain = user/501，且 jetsam memory limit 只有 6MB）。
//
// 唯一的、不依赖 job 落在哪个域的办法：进程自己把上限抬上去。
// memorystatus_control(MEMORYSTATUS_CMD_SET_JETSAM_TASK_LIMIT) 需要
// com.apple.private.memorystatus 这条 entitlement（已打进签名）。

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// XNU bsd/sys/syscall.h: __memorystatus_control = 440
const sysMemorystatusControl = 440

// memorystatus.h: 把本 task 的 jetsam 上限设成 flags（字节）。
const cmdSetJetsamTaskLimit = 6

// 256MiB。进程真实 footprint 约 24MB，这里只是把天花板抬到「永远不会碰到」的高度。
const jetsamLimitBytes = 256 << 20

// XNU bsd/sys/syscall.h: __proc_info = 336；libproc 的 proc_pid_rusage 就是
// __proc_info(PROC_INFO_CALL_PIDRUSAGE, pid, flavor, 0, buffer, size)。
const (
	sysProcInfo           = 336
	procInfoCallPIDRusage = 9
	rusageInfoV0          = 0
	rusageV0Size          = 96
)

// 退路计数器：syscall 取不到 footoint 时，每 30 次心跳（5 分钟）问一次 launchctl。
var (
	lcCounter int
	lastFP    int
)

// callMemorystatus 直接把参数塞进寄存器：CGO 关着，没法走 libSystem 的头文件。
//
//	int memorystatus_control(uint32_t command, int32_t pid, uint32_t flags,
//	                         void *buffer, size_t buffersize);
func callMemorystatus(cmd, pid, flags uintptr) (int, syscall.Errno) {
	r, _, e := syscall.Syscall6(sysMemorystatusControl, cmd, pid, flags, 0, 0, 0)
	return int(r), e
}

// applyProcessLimits 在 main 早期执行，返回一行能直接写进日志的结论。
func applyProcessLimits() string {
	pid := uintptr(os.Getpid())
	ret, errno := callMemorystatus(cmdSetJetsamTaskLimit, pid, jetsamLimitBytes)
	if errno != 0 {
		return fmt.Sprintf("抬高失败 ret=%d errno=%d(%v) 目标=%dMB", ret, int(errno), errno, jetsamLimitBytes>>20)
	}
	return fmt.Sprintf("已抬到 %dMB ret=%d", jetsamLimitBytes>>20, ret)
}

type procRusage struct {
	Footprint uint64 // ri_phys_footprint —— 内核 jetsam 记账的那个数
	Resident  uint64 // ri_resident_size
	Wkups     uint64 // ri_interrupt_wkups（累计）
	IdleWkups uint64 // ri_pkg_idle_wkups（累计）
	Valid     bool
}

// readRusage 读 rusage_info_v0：
//
//	struct rusage_info_v0 {
//	    uint8_t  ri_uuid[16];
//	    uint64_t ri_user_time;          // 16
//	    uint64_t ri_system_time;        // 24
//	    uint64_t ri_pkg_idle_wkups;     // 32
//	    uint64_t ri_interrupt_wkups;    // 40
//	    uint64_t ri_pageins;            // 48
//	    uint64_t ri_wired_size;         // 56
//	    uint64_t ri_resident_size;      // 64
//	    uint64_t ri_phys_footprint;     // 72
//	    uint64_t ri_proc_start_abstime; // 80
//	    uint64_t ri_proc_exit_abstime;  // 88
//	};
func readRusage() procRusage {
	var buf [rusageV0Size]byte
	_, _, e := syscall.Syscall6(sysProcInfo, procInfoCallPIDRusage, uintptr(os.Getpid()),
		rusageInfoV0, 0, uintptr(unsafe.Pointer(&buf[0])), rusageV0Size)
	runtime.KeepAlive(&buf)
	if e != 0 {
		return procRusage{}
	}
	u64 := func(off int) uint64 { return binary.LittleEndian.Uint64(buf[off : off+8]) }
	r := procRusage{
		IdleWkups: u64(32),
		Wkups:     u64(40),
		Resident:  u64(64),
		Footprint: u64(72),
	}
	// 0 或离谱大值说明布局猜错了，宁可不报也不报错数。
	if r.Footprint == 0 || r.Footprint > 1<<40 || r.Resident > 1<<40 {
		return procRusage{}
	}
	r.Valid = true
	return r
}

// footprintFromLaunchctl 是退路：launchctl procinfo <pid> 的输出里就有
// physicalFootprint（老大导出的 wb2api-10772.plist 就是这条命令的产物）。
func footprintFromLaunchctl() (int, bool) {
	var cands []string
	for _, jb := range jbrootCandidates() {
		cands = append(cands, filepath.Join(jb, "usr/bin/launchctl"), filepath.Join(jb, "bin/launchctl"))
	}
	cands = append(cands, "/usr/bin/launchctl")
	for _, c := range cands {
		if _, err := os.Stat(c); err != nil {
			continue
		}
		out, err := exec.Command(c, "procinfo", strconv.Itoa(os.Getpid())).Output()
		if err != nil {
			continue
		}
		m := reFootprint.FindSubmatch(out)
		if len(m) < 2 {
			continue
		}
		n, err := strconv.Atoi(string(m[1]))
		if err != nil || n <= 0 {
			continue
		}
		return n >> 20, true
	}
	return 0, false
}

var reFootprint = regexp.MustCompile(`(?s)<key>physicalFootprint</key>\s*<integer>(\d+)</integer>`)

// memSample 给心跳用：footprint(MB) 与累计 wakeup 数（拿不到就 -1）。
func memSample() (int, int) {
	if r := readRusage(); r.Valid {
		return int(r.Footprint >> 20), int(r.Wkups)
	}
	lcCounter++
	if lcCounter == 1 || lcCounter%30 == 0 {
		if mb, ok := footprintFromLaunchctl(); ok {
			lastFP = mb
		}
	}
	if lastFP > 0 {
		return lastFP, -1
	}
	return -1, -1
}

// logBootMemory 启动时报一次真实账本，顺便验证 syscall 布局猜得对不对。
func logBootMemory() {
	if r := readRusage(); r.Valid {
		log.Printf("[mem] footprint=%dMB resident=%dMB wkups=%d（proc_info syscall）",
			r.Footprint>>20, r.Resident>>20, r.Wkups)
		return
	}
	if mb, ok := footprintFromLaunchctl(); ok {
		lastFP = mb
		log.Printf("[mem] footprint=%dMB（launchctl procinfo 兜底；proc_info 取不到）", mb)
		return
	}
	log.Printf("[mem] footprint 取不到：proc_info 与 launchctl 都失败，只有 rss 可用")
}

// maxRSSMB 是生命周期 RSS 峰值（getrusage）。Darwin 的 Maxrss 单位是字节
// （Linux 是 KB），所以不能跨平台共用一份实现。它含干净的 file-backed 页，
// 比 footprint 大，只当上界看。
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

// logJetsamEvidence 启动时把 CrashReporter 里最近三个 JetsamEvent 抄进 server.log，
// 命中 wb2api 就把 reason / rpages / lifetimeMax 摊出来 —— 不用再让人拿 Filza 翻。
func logJetsamEvidence() {
	seen := map[string]bool{}
	var files []string
	for _, d := range []string{
		"/var/mobile/Library/Logs/CrashReporter",
		"/private/var/mobile/Library/Logs/CrashReporter",
	} {
		m, _ := filepath.Glob(filepath.Join(d, "JetsamEvent-*.ips"))
		for _, f := range m {
			// /var/mobile 与 /private/var/mobile 是同一处，按 basename 去重
			// （-15 就是按目录字符串去重，同一个文件被打了两次）。
			b := filepath.Base(f)
			if seen[b] {
				continue
			}
			seen[b] = true
			files = append(files, f)
		}
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
		// 一份 JetsamEvent 约 160KB；-15 只读前 96KB，wb2api 那段在后面，白读。
		raw, _ := io.ReadAll(io.LimitReader(fh, 2<<20))
		fh.Close()

		// .ips = 首行元数据 JSON + 正文 JSON
		body := raw
		if n := bytes.IndexByte(raw, '\n'); n >= 0 {
			body = raw[n+1:]
		}
		var v interface{}
		if err := json.Unmarshal(body, &v); err != nil {
			continue
		}
		log.Printf("[jetsam]   %s 事件: reason=%s largestProcess=%s",
			filepath.Base(f), eventField(v, "reason"), eventField(v, "largestProcess"))

		// JetsamEvent 会把内存最高的几十个进程全抄一遍，**只有被杀的条目带 reason**。
		// -16 把「快照成员」当成「死因」报了三回，这里按 reason 分流。
		procs := collectProcs(v)
		victims := 0
		for _, p := range procs {
			r := asText(p["reason"])
			if r == "" {
				continue
			}
			victims++
			if victims <= 4 {
				log.Printf("[jetsam]   %s 被杀: name=%s reason=%s rpages=%v lifetimeMax=%v",
					filepath.Base(f), asText(p["name"]), r, p["rpages"], p["lifetimeMax"])
			}
		}
		if victims == 0 {
			log.Printf("[jetsam]   %s 没有带 reason 的条目（本次事件未杀进程）", filepath.Base(f))
		}
		for _, p := range procs {
			if !strings.Contains(strings.ToLower(asText(p["name"])), "wb2api") {
				continue
			}
			note := "快照成员，未杀"
			if asText(p["reason"]) != "" {
				note = "被杀的就是我们"
			}
			// 键名一并打出来：下次不用再猜字段叫什么。
			log.Printf("[jetsam]   %s 我方: rpages=%v lifetimeMax=%v footprint=%v（%s）keys=%s",
				filepath.Base(f), p["rpages"], p["lifetimeMax"], p["phys_footprint"], note, keysOf(p))
			break
		}
	}
}

// eventField 取事件顶层的单个字段（reason=highwater / largestProcess 之类）。
func eventField(v interface{}, key string) string {
	m, ok := v.(map[string]interface{})
	if !ok {
		return "-"
	}
	if s := asText(m[key]); s != "" {
		return s
	}
	return "-"
}

// collectProcs 在任意形状的 JSON 里收集「进程条目」：有 name，且带 pid 或 rpages。
// 不假设 processes 是数组还是以 pid 为键的字典 —— 各机型格式不一样。
func collectProcs(v interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	switch t := v.(type) {
	case map[string]interface{}:
		if _, ok := t["name"].(string); ok {
			_, hasPid := t["pid"]
			_, hasRPages := t["rpages"]
			if hasPid || hasRPages {
				return append(out, t)
			}
		}
		for _, vv := range t {
			out = append(out, collectProcs(vv)...)
		}
	case []interface{}:
		for _, vv := range t {
			out = append(out, collectProcs(vv)...)
		}
	}
	return out
}

// asText 把 JSON 里的任意标量转成字符串；null / 缺失一律空串。
func asText(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}

// keysOf 打出条目的键名，最多 14 个 —— 只用于把 schema 暴露出来。
func keysOf(m map[string]interface{}) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	if len(ks) > 14 {
		ks = ks[:14]
		ks = append(ks, "…")
	}
	return strings.Join(ks, ",")
}
