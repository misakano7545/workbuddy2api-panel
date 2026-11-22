//go:build windows

package main

import (
	"context"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

//go:embed tray.ico
var trayIcon []byte

const (
	trayCallBackMessage = 0x8000 + 1
	trayExitMessage     = 0x8000 + 2

	trayIDOpenConsole = 1
	trayIDExit        = 2
	trayIDCredits     = 3
	trayIDToday       = 4

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004

	nimAdd    = 0
	nimDelete = 2

	mfString    = 0x00000000
	mfGrayed    = 0x00000001
	mfDisabled  = 0x00000002
	mfSeparator = 0x00000800

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100

	wmNull          = 0x0000
	wmDestroy       = 0x0002
	wmClose         = 0x0010
	wmCommand       = 0x0111
	wmContextMenu   = 0x007B
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205
	idcArrow        = 32512
	iconResourceV3  = 0x00030000
	trayIconUID     = 1
	trayIconSize    = 32
)

var (
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")

	procFreeConsole            = kernel32.NewProc("FreeConsole")
	procGetModuleHandleW       = kernel32.NewProc("GetModuleHandleW")
	procRegisterClassExW       = user32.NewProc("RegisterClassExW")
	procCreateWindowExW        = user32.NewProc("CreateWindowExW")
	procDefWindowProcW         = user32.NewProc("DefWindowProcW")
	procGetMessageW            = user32.NewProc("GetMessageW")
	procTranslateMessage       = user32.NewProc("TranslateMessage")
	procDispatchMessageW       = user32.NewProc("DispatchMessageW")
	procPostMessageW           = user32.NewProc("PostMessageW")
	procPostQuitMessage        = user32.NewProc("PostQuitMessage")
	procCreateIconFromResource = user32.NewProc("CreateIconFromResource")
	procDestroyIcon            = user32.NewProc("DestroyIcon")
	procCreatePopupMenu        = user32.NewProc("CreatePopupMenu")
	procAppendMenuW            = user32.NewProc("AppendMenuW")
	procTrackPopupMenu         = user32.NewProc("TrackPopupMenu")
	procDestroyMenu            = user32.NewProc("DestroyMenu")
	procGetCursorPos           = user32.NewProc("GetCursorPos")
	procSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	procLoadCursorW            = user32.NewProc("LoadCursorW")
	procSetProcessDPIAware     = user32.NewProc("SetProcessDPIAware")
	procShellNotifyIconW       = shell32.NewProc("Shell_NotifyIconW")
)

type point struct {
	X int32
	Y int32
}

type msg struct {
	Hwnd     windows.HWND
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       point
	LPrivate uint32
}

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     windows.Handle
	HIcon         windows.Handle
	HCursor       windows.Handle
	HbrBackground windows.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       windows.Handle
}

type notifyIconDataW struct {
	CbSize           uint32
	HWnd             windows.HWND
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            windows.Handle
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UTimeout         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         windows.GUID
	HBalloonIcon     windows.Handle
}

type trayState struct {
	hwnd     windows.HWND
	panelURL string
	stop     context.CancelFunc
	stats    desktopStatsFunc
	stopOnce sync.Once
}

var currentTray *trayState

// setDpiAwareness 优先用 Per-Monitor DPI Awareness V2（-4）；该 API 在 Win10 1607
// 之前不存在，所以先 FindProc，避免 LazyProc.Call 在旧系统上直接 panic。
func setDpiAwareness() {
	const perMonitorAwareV2 = ^uintptr(3)
	// 这里必须主动 LoadDLL：进程刚启动时 user32 可能还没加载，
	// GetModuleHandle 会失败，直接落到系统级感知（多显示器缩放下菜单会发虚）。
	dll, err := windows.LoadDLL("user32.dll")
	if err == nil {
		if proc, err := dll.FindProc("SetProcessDpiAwarenessContext"); err == nil {
			if ok, _, _ := proc.Call(perMonitorAwareV2); ok != 0 {
				return
			}
		}
	}
	procSetProcessDPIAware.Call()
}

// prepareDesktopUI 在 main 最开头调用：双击 exe 时不再弹 cmd，日志改写到用户目录，
// 否则窗口隐藏后启动失败信息无从查看。发布包还会在链接期用 -H=windowsgui 彻底
// 避免控制台一闪而过；这里再调用 FreeConsole 兼顾直接 go build 出来的 exe。
func prepareDesktopUI() {
	// 必须在创建任何窗口之前设置，否则原生托盘菜单会被 Windows 按 DPI 虚拟化
	// 缩放成位图，字体发虚。Win10 1607+ 走 PerMonitorV2，旧系统回退到系统级感知。
	setDpiAwareness()

	if os.Getenv("WB2A_CONSOLE") == "" {
		procFreeConsole.Call()
	}

	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	logDir := filepath.Join(dir, "workbuddy2api")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return
	}
	desktopLogWriter = newDailyLogWriter(logDir)
	log.SetOutput(io.MultiWriter(os.Stderr, desktopLogWriter))
}

// desktopLogTargets 把主流程后续重设的输出目标也带上桌面日志文件，
// 否则隐藏窗口后只有启动前半段日志落盘，横幅和退出信息全丢。
func desktopLogTargets(extra io.Writer) []io.Writer {
	if desktopLogWriter != nil {
		return []io.Writer{os.Stderr, extra, desktopLogWriter}
	}
	return []io.Writer{os.Stderr, extra}
}

// startDesktop 启动后自动打开控制台，并常驻系统托盘。托盘失败只记日志，
// 不影响 HTTP 服务继续运行。
func startDesktop(ctx context.Context, stop context.CancelFunc, panelURL string, stats desktopStatsFunc) {
	openBrowser(panelURL)
	runTray(ctx, stop, panelURL, stats)
}

func runTray(ctx context.Context, stop context.CancelFunc, panelURL string, stats desktopStatsFunc) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hInstance, _, _ := procGetModuleHandleW.Call(0)
	className, err := windows.UTF16PtrFromString("WorkBuddy2APITray" + strconv.Itoa(os.Getpid()))
	if err != nil {
		log.Printf("WARN: 托盘窗口类名无效: %v", err)
		return
	}
	windowName, _ := windows.UTF16PtrFromString("")

	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
	wc := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		LpfnWndProc:   syscall.NewCallback(trayWndProc),
		HInstance:     windows.Handle(hInstance),
		HCursor:       windows.Handle(cursor),
		LpszClassName: className,
	}
	if atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		log.Printf("WARN: 注册托盘窗口类失败: %v", callErr)
		return
	}

	st := &trayState{panelURL: panelURL, stop: stop, stats: stats}
	currentTray = st
	defer func() { currentTray = nil }()

	hwnd, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(windowName)),
		0, 0, 0, 0, 0,
		0, 0, hInstance, 0,
	)
	if hwnd == 0 {
		log.Printf("WARN: 创建托盘窗口失败: %v", callErr)
		return
	}
	st.hwnd = windows.HWND(hwnd)

	hIcon, err := iconFromICO(trayIcon)
	if err != nil {
		log.Printf("WARN: 加载托盘图标失败: %v", err)
		return
	}

	tip, _ := windows.UTF16FromString("workbuddy2api 控制台")
	nid := notifyIconDataW{
		HWnd:             st.hwnd,
		UID:              trayIconUID,
		UFlags:           nifMessage | nifIcon | nifTip,
		UCallbackMessage: trayCallBackMessage,
		HIcon:            windows.Handle(hIcon),
	}
	copy(nid.SzTip[:], tip)
	nid.CbSize = uint32(unsafe.Sizeof(nid))

	added, _, callErr := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
	if added == 0 {
		log.Printf("WARN: 添加托盘图标失败: %v", callErr)
		procDestroyIcon.Call(uintptr(hIcon))
		return
	}
	defer func() {
		nid.UFlags = 0
		procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
		procDestroyIcon.Call(uintptr(hIcon))
	}()

	go func() {
		<-ctx.Done()
		procPostMessageW.Call(uintptr(st.hwnd), trayExitMessage, 0, 0)
	}()

	for {
		var m msg
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if ret == 0 || ret == ^uintptr(0) {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func trayWndProc(hwnd windows.HWND, message uint32, wParam, lParam uintptr) uintptr {
	st := currentTray
	switch message {
	case trayCallBackMessage:
		// 默认 NOTIFYICON_VERSION：wParam 是图标 ID，lParam 直接是鼠标消息。
		// （VERSION_4 才会把事件和图标 ID 分别塞进 LOWORD/HIWORD(lParam)。）
		if st == nil || uint32(wParam&0xffffffff) != trayIconUID {
			break
		}
		mouseMsg := uint32(lParam & 0xffffffff)
		trayDebugf("callback mouse=0x%x", mouseMsg)
		switch mouseMsg {
		case wmLButtonDblClk:
			openBrowser(st.panelURL)
		case wmRButtonUp, wmContextMenu:
			showTrayMenu(st)
		}
		return 0
	case wmCommand:
		if st != nil {
			handleTrayCommand(st, uint32(wParam&0xffff))
		}
		return 0
	case wmClose:
		if st != nil {
			st.quit()
		}
		return 0
	case trayExitMessage, wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(message), wParam, lParam)
	return ret
}

func (s *trayState) quit() {
	s.stopOnce.Do(func() {
		s.stop()
		procPostMessageW.Call(uintptr(s.hwnd), trayExitMessage, 0, 0)
	})
}

func trayDebugf(format string, args ...any) {
	if os.Getenv("WB2A_TRAY_DEBUG") != "" {
		log.Printf("[tray] "+format, args...)
	}
}

func showTrayMenu(st *trayState) {
	trayDebugf("show menu")
	hMenu, _, callErr := procCreatePopupMenu.Call()
	if hMenu == 0 {
		log.Printf("WARN: 创建托盘菜单失败: %v", callErr)
		return
	}
	defer procDestroyMenu.Call(hMenu)

	if st.stats != nil {
		stats := st.stats()
		if stats.CreditsLine != "" {
			creditsText, _ := windows.UTF16PtrFromString(stats.CreditsLine)
			procAppendMenuW.Call(hMenu, mfString|mfGrayed|mfDisabled, trayIDCredits, uintptr(unsafe.Pointer(creditsText)))
		}
		if stats.TodayLine != "" {
			todayText, _ := windows.UTF16PtrFromString(stats.TodayLine)
			procAppendMenuW.Call(hMenu, mfString|mfGrayed|mfDisabled, trayIDToday, uintptr(unsafe.Pointer(todayText)))
		}
		if stats.CreditsLine != "" || stats.TodayLine != "" {
			procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
		}
	}

	openText, _ := windows.UTF16PtrFromString("打开控制台")
	exitText, _ := windows.UTF16PtrFromString("退出")
	procAppendMenuW.Call(hMenu, mfString, trayIDOpenConsole, uintptr(unsafe.Pointer(openText)))
	procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
	procAppendMenuW.Call(hMenu, mfString, trayIDExit, uintptr(unsafe.Pointer(exitText)))

	var pos point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pos)))
	procSetForegroundWindow.Call(uintptr(st.hwnd))
	cmd, _, _ := procTrackPopupMenu.Call(
		hMenu,
		tpmRightButton|tpmReturnCmd,
		uintptr(pos.X), uintptr(pos.Y),
		0, uintptr(st.hwnd), 0,
	)
	procPostMessageW.Call(uintptr(st.hwnd), wmNull, 0, 0)
	trayDebugf("menu cmd=%d", cmd)

	handleTrayCommand(st, uint32(cmd))
}

func handleTrayCommand(st *trayState, cmd uint32) {
	switch cmd {
	case trayIDOpenConsole:
		openBrowser(st.panelURL)
	case trayIDExit:
		st.quit()
	}
}

// iconFromICO 从内嵌 ICO 中取第一个 32x32 BMP 项，交给 CreateIconFromResource。
func iconFromICO(data []byte) (windows.Handle, error) {
	if len(data) < 22 ||
		binary.LittleEndian.Uint16(data[0:2]) != 0 ||
		binary.LittleEndian.Uint16(data[2:4]) != 1 ||
		binary.LittleEndian.Uint16(data[4:6]) == 0 {
		return 0, errors.New("图标文件不是有效的 ICO")
	}
	size := int(binary.LittleEndian.Uint32(data[14:18]))
	offset := int(binary.LittleEndian.Uint32(data[18:22]))
	if size <= 0 || offset < 22 || offset+size > len(data) {
		return 0, errors.New("图标文件内容不完整")
	}
	if binary.LittleEndian.Uint32(data[offset+4:offset+8]) < trayIconSize {
		return 0, fmt.Errorf("图标尺寸小于 %dpx", trayIconSize)
	}

	hIcon, _, _ := procCreateIconFromResource.Call(
		uintptr(unsafe.Pointer(&data[offset])),
		uintptr(size),
		1,
		iconResourceV3,
	)
	if hIcon == 0 {
		return 0, errors.New("CreateIconFromResource 失败")
	}
	return windows.Handle(hIcon), nil
}
