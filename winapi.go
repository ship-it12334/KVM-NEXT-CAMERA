package main

// Минимални Windows API обвивки през syscall — без външни модули.

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	whKeyboardLL = 13 // WH_KEYBOARD_LL
	whMouseLL    = 14 // WH_MOUSE_LL
	hcAction     = 0

	wmMouseMove     = 0x0200
	wmLButtonDown   = 0x0201
	wmLButtonUp     = 0x0202
	wmRButtonDown   = 0x0204
	wmRButtonUp     = 0x0205
	wmMButtonDown   = 0x0207
	wmMButtonUp     = 0x0208
	wmMouseWheel    = 0x020A
	wmXButtonDown   = 0x020B
	wmXButtonUp     = 0x020C
	wmKeyDown       = 0x0100
	wmKeyUp         = 0x0101
	wmSysKeyDown    = 0x0104
	wmSysKeyUp      = 0x0105
	wmQuit          = 0x0012
	wmAppShowCursor = 0x8001

	llkhfExtended        = 0x01
	llkhfLowerILInjected = 0x02

	inputMouse_    = 0
	inputKeyboard_ = 1

	mouseeventfMove      = 0x0001
	mouseeventfLeftDown  = 0x0002
	mouseeventfLeftUp    = 0x0004
	mouseeventfRightDown = 0x0008
	mouseeventfRightUp   = 0x0010
	mouseeventfMidDown   = 0x0020
	mouseeventfMidUp     = 0x0040
	mouseeventfXDown     = 0x0080
	mouseeventfXUp       = 0x0100
	mouseeventfWheel     = 0x0800

	keyeventfExtendedKey = 0x0001
	keyeventfKeyUp       = 0x0002

	smCxScreen = 0
	smCyScreen = 1

	// виртуален екран (обединение на всички монитори)
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79

	// virtual-key codes за modifiers
	vkShift   = 0x10
	vkControl = 0x11
	vkMenu    = 0x12 // Alt
	vkLWin    = 0x5B
	vkRWin    = 0x5C
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	procSetWindowsHookExW   = user32.NewProc("SetWindowsHookExW")
	procCallNextHookEx      = user32.NewProc("CallNextHookEx")
	procUnhookWindowsHookEx = user32.NewProc("UnhookWindowsHookEx")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procSetCursorPos        = user32.NewProc("SetCursorPos")
	procClipCursor          = user32.NewProc("ClipCursor")
	procShowCursor          = user32.NewProc("ShowCursor")
	procSendInput           = user32.NewProc("SendInput")
	procGetSystemMetrics    = user32.NewProc("GetSystemMetrics")
	procGetAsyncKeyState    = user32.NewProc("GetAsyncKeyState")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")

	procGetCurrentThreadId = kernel32.NewProc("GetCurrentThreadId")
	procGetModuleHandleW   = kernel32.NewProc("GetModuleHandleW")

	procRegisterClassExW   = user32.NewProc("RegisterClassExW")
	procCreateWindowExW    = user32.NewProc("CreateWindowExW")
	procDefWindowProcW     = user32.NewProc("DefWindowProcW")
	procDestroyWindow      = user32.NewProc("DestroyWindow")
	procPostThreadMessageW = user32.NewProc("PostThreadMessageW")
	procLoadCursorW        = user32.NewProc("LoadCursorW")

	procRegisterRawInputDevices = user32.NewProc("RegisterRawInputDevices")
	procGetRawInputData         = user32.NewProc("GetRawInputData")

	// DPI + монитори
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDPIAware            = user32.NewProc("SetProcessDPIAware")
	procGetDpiForSystem               = user32.NewProc("GetDpiForSystem")
	procMonitorFromPoint              = user32.NewProc("MonitorFromPoint")
	procGetMonitorInfoW               = user32.NewProc("GetMonitorInfoW")
	procEnumDisplayMonitors           = user32.NewProc("EnumDisplayMonitors")
	procPeekMessageW                  = user32.NewProc("PeekMessageW")

	// ShellExecute (elevation)
	procShellExecuteW   = shell32.NewProc("ShellExecuteW")
	procShellExecuteExW = shell32.NewProc("ShellExecuteExW")

	// токен / процеси
	procOpenProcessToken    = advapi32.NewProc("OpenProcessToken")
	procGetTokenInformation = advapi32.NewProc("GetTokenInformation")
	procGetCurrentProcess   = kernel32.NewProc("GetCurrentProcess")
	procWaitForSingleObject = kernel32.NewProc("WaitForSingleObject")
	procGetExitCodeProcess  = kernel32.NewProc("GetExitCodeProcess")
)

const (
	ridInput       = 0x10000003
	ridevInputSink = 0x00000100
	hwndMessage    = ^uintptr(2) // (HWND)-3
	idcArrow       = 32512
	csHRedraw      = 0x0002
	csVRedraw      = 0x0001
	wmInput        = 0x00FF
)

type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type rawInputDevice struct {
	UsUsagePage uint16
	UsUsage     uint16
	DwFlags     uint32
	HwndTarget  uintptr
}

func getCurrentThreadID() uintptr {
	r, _, _ := procGetCurrentThreadId.Call()
	return r
}

func postThreadMessage(tid uint32, wm uint32) bool {
	r, _, _ := procPostThreadMessageW.Call(uintptr(tid), uintptr(wm), 0, 0)
	return r != 0
}

// createMessageWindow създава message-only window за raw input (HWND_MESSAGE parent).
func createMessageWindow(wndProc uintptr, class *uint16) uintptr {
	hInst, _, _ := procGetModuleHandleW.Call(0)
	wc := wndClassEx{
		CbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		LpfnWndProc:   wndProc,
		HCursor:       func() uintptr { r, _, _ := procLoadCursorW.Call(0, idcArrow); return r }(),
		LpszClassName: class,
	}
	wc.HInstance = hInst
	_, _, regErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	hwnd, _, creatErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(class)),
		0,
		0,
		0, 0, 0, 0,
		hwndMessage,
		0, // hMenu — задължително 0, иначе "Invalid menu handle"
		hInst,
		0)
	if hwnd == 0 {
		lastCreateWinErr = fmt.Sprintf("regErr=%v createErr=%v", regErr, creatErr)
	}
	return hwnd
}

var lastCreateWinErr string

// registerRawInputMouse регистрира raw mouse input към даден window.
func registerRawInputMouse(hwnd uintptr) bool {
	dev := rawInputDevice{
		UsUsagePage: 0x01, // generic desktop
		UsUsage:     0x02, // mouse
		DwFlags:     ridevInputSink,
		HwndTarget:  hwnd,
	}
	r, _, _ := procRegisterRawInputDevices.Call(uintptr(unsafe.Pointer(&dev)), 1, unsafe.Sizeof(dev))
	return r != 0
}

// parseRawMouse връща (dx, dy, ok) от WM_INPUT lParam. RAWMOUSE.lLastX/lLastY са
// сурови хардуерни дилти — работят дори когато курсорът е clip-нат на ръба.
func parseRawMouse(lParam uintptr) (int32, int32, bool) {
	var size uint32 = 64
	var buf [64]byte
	r, _, _ := procGetRawInputData.Call(lParam, ridInput, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 24)
	if r == 0 || size < 48 {
		return 0, 0, false
	}
	// RAWINPUTHEADER = 24 байта на x64; RAWMOUSE започва след него:
	// usFlags(2) pad(2) ulButtons(4) ulRawButtons(4) lLastX(4) lLastY(4)
	dx := int32(le32(buf[36:40]))
	dy := int32(le32(buf[40:44]))
	return dx, dy, true
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

type point struct{ X, Y int32 }

type rect struct{ Left, Top, Right, Bottom int32 }

type msllHookStruct struct {
	Pt          point
	MouseData   uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type kbdllHookStruct struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type mouseInput struct {
	Dx          int32
	Dy          int32
	MouseData   uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type keybdInput struct {
	WVk         uint16
	WScan       uint16
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

// INPUT: type + padding + union. Windows очаква sizeof(INPUT) == 40 на x64.
type inputMouse struct {
	Type uint32
	_    [4]byte
	Mi   mouseInput
}

type inputKey struct {
	Type uint32
	_    [4]byte
	Ki   keybdInput
	_    [8]byte
}

func setWindowsHookEx(idHook int32, lpfn uintptr, hmod uintptr, dwThreadId uint32) uintptr {
	r, _, _ := procSetWindowsHookExW.Call(uintptr(idHook), lpfn, hmod, uintptr(dwThreadId))
	return r
}

func callNextHookEx(hhk uintptr, nCode int32, wParam, lParam uintptr) uintptr {
	r, _, _ := procCallNextHookEx.Call(hhk, uintptr(nCode), wParam, lParam)
	return r
}

func unhookWindowsHookEx(hhk uintptr) bool {
	r, _, _ := procUnhookWindowsHookEx.Call(hhk)
	return r != 0
}

func getCursorPos() (int32, int32, bool) {
	var p point
	r, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	return p.X, p.Y, r != 0
}

func setCursorPos(x, y int32) bool {
	r, _, _ := procSetCursorPos.Call(uintptr(uint32(x)), uintptr(uint32(y)))
	return r != 0
}

func clipCursor(rc *rect) {
	var p uintptr
	if rc != nil {
		p = uintptr(unsafe.Pointer(rc))
	}
	procClipCursor.Call(p)
}

func showCursor(show bool) {
	v := int(1)
	if !show {
		v = -1
	}
	// показваме/скриваме докато се изчерпа броячът
	for i := 0; i < 16; i++ {
		r, _, _ := procShowCursor.Call(uintptr(uint32(int32(v))))
		if (show && int32(r) >= 0) || (!show && int32(r) < 0) {
			break
		}
	}
}

func getSystemMetrics(idx int32) int {
	r, _, _ := procGetSystemMetrics.Call(uintptr(uint32(idx)))
	return int(int32(r))
}

func getAsyncKeyState(vk int32) bool {
	r, _, _ := procGetAsyncKeyState.Call(uintptr(uint32(vk)))
	return uint16(r)&0x8000 != 0
}

// sendMouse инжектира едно мишка събитие. Връща false, ако SendInput е
// приел 0 от събитията (напр. блокиран от UIPI — по-висок интегритетен процес).
func sendMouse(flags, data uint32, dx, dy int32) bool {
	var inp inputMouse
	inp.Type = inputMouse_
	inp.Mi = mouseInput{Dx: dx, Dy: dy, MouseData: data, Flags: flags}
	r, _, _ := procSendInput.Call(1, uintptr(unsafe.Pointer(&inp)), unsafe.Sizeof(inp))
	return r == 1
}

// sendKey инжектира клавиша. scan и ext идват от hook-а (точни за текущия layout).
func sendKey(vk, scan uint32, up, ext bool) bool {
	var flags uint32
	if ext {
		flags |= keyeventfExtendedKey
	}
	if up {
		flags |= keyeventfKeyUp
	}
	var inp inputKey
	inp.Type = inputKeyboard_
	inp.Ki = keybdInput{WVk: uint16(vk), WScan: uint16(scan), DwFlags: flags}
	r, _, _ := procSendInput.Call(1, uintptr(unsafe.Pointer(&inp)), unsafe.Sizeof(inp))
	return r == 1
}

func syscallNewCallback(fn any) uintptr {
	return syscall.NewCallback(fn)
}

func utf16Ptr(s string) *uint16 {
	b, _ := syscall.UTF16PtrFromString(s)
	return b
}

// ---- DPI / монитори / elevation ----

const (
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4
	dpiContextPerMonitorV2 = ^uintptr(3)
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE = -3
	dpiContextPerMonitor = ^uintptr(2)

	monDefaultToNearest = 2

	tokenQuery     = 0x0008
	tokenElevation = 20
)

// dpiAwarenessMode пази използвания режим на DPI awareness (за диагностиката).
var dpiAwarenessMode = "unknown"

// setDpiAwareness прави процеса Per-Monitor aware. Без това Windows
// виртуализира координатите на не-DPI-aware процеси: GetSystemMetrics
// връща 1440x900 вместо реалните 2880x1800 при 200% мащабиране и
// преходите между екрани се чупят (ръбът се намира в средата на екрана).
func setDpiAwareness() string {
	if procSetProcessDpiAwarenessContext.Find() == nil {
		if r, _, _ := procSetProcessDpiAwarenessContext.Call(dpiContextPerMonitorV2); r != 0 {
			dpiAwarenessMode = "Per-Monitor V2"
			return dpiAwarenessMode
		}
		if r, _, _ := procSetProcessDpiAwarenessContext.Call(dpiContextPerMonitor); r != 0 {
			dpiAwarenessMode = "Per-Monitor"
			return dpiAwarenessMode
		}
	}
	if procSetProcessDPIAware.Find() == nil {
		if r, _, _ := procSetProcessDPIAware.Call(); r != 0 {
			dpiAwarenessMode = "System (DPI aware)"
			return dpiAwarenessMode
		}
	}
	dpiAwarenessMode = "none (DPI unaware)"
	return dpiAwarenessMode
}

// getDpiForSystem връща системния DPI (96 = 100%).
func getDpiForSystem() int {
	if procGetDpiForSystem.Find() == nil {
		if r, _, _ := procGetDpiForSystem.Call(); r != 0 {
			return int(r)
		}
	}
	return 96
}

type monitorInfo struct {
	CbSize    uint32
	RcMonitor rect
	RcWork    rect
	DwFlags   uint32
}

// monitorFromPoint връща дескриптора на монитора под точката.
func monitorFromPoint(x, y int32) uintptr {
	h, _, _ := procMonitorFromPoint.Call(uintptr(uint32(x)), uintptr(uint32(y)), monDefaultToNearest)
	return h
}

// getMonitorInfo връща информацията за даден монитор.
func getMonitorInfo(h uintptr) (monitorInfo, bool) {
	var mi monitorInfo
	mi.CbSize = uint32(unsafe.Sizeof(monitorInfo{}))
	if r, _, _ := procGetMonitorInfoW.Call(h, uintptr(unsafe.Pointer(&mi))); r == 0 {
		return mi, false
	}
	return mi, true
}

// isElevated връща true, ако процесът има повишен (администраторски) токен.
func isElevated() bool {
	if procOpenProcessToken.Find() != nil || procGetTokenInformation.Find() != nil {
		return false
	}
	var tok syscall.Handle
	proc := procGetCurrentProcess
	hProc, _, _ := proc.Call()
	if r, _, _ := procOpenProcessToken.Call(hProc, tokenQuery, uintptr(unsafe.Pointer(&tok))); r == 0 {
		return false
	}
	defer syscall.CloseHandle(tok)
	var elev uint32
	var n uint32
	r, _, _ := procGetTokenInformation.Call(uintptr(tok), tokenElevation, uintptr(unsafe.Pointer(&elev)), 4, uintptr(unsafe.Pointer(&n)))
	return r != 0 && elev != 0
}

// shellExecuteInfo е SHELLEXECUTEINFOW.
type shellExecuteInfo struct {
	CbSize     uint32
	FMask      uint32
	Hwnd       uintptr
	Verb       *uint16
	File       *uint16
	Parameters *uint16
	Directory  *uint16
	Show       int32
	InstApp    uintptr
	IDList     uintptr
	Class      *uint16
	KeyClass   uintptr
	HotKey     uint32
	IconOrMon  uintptr
	Process    uintptr
}

const seeMaskNoCloseProcess = 0x00000040

// runElevated стартира програма с повишени права (UAC) и чака да приключи.
// Връща exit code-а на стартираната програма.
func runElevated(exe, args string) (int32, error) {
	if procShellExecuteExW.Find() != nil {
		return 0, fmt.Errorf("ShellExecuteExW липсва")
	}
	sei := shellExecuteInfo{
		CbSize:     uint32(unsafe.Sizeof(shellExecuteInfo{})),
		FMask:      seeMaskNoCloseProcess,
		Verb:       utf16Ptr("runas"),
		File:       utf16Ptr(exe),
		Parameters: utf16Ptr(args),
		Show:       0, // SW_HIDE
	}
	r, _, _ := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&sei)))
	if r == 0 {
		return 0, fmt.Errorf("ShellExecuteExW не стартира програмата")
	}
	h := sei.Process
	if h == 0 {
		return 0, nil
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	procWaitForSingleObject.Call(h, 0xFFFFFFFF) // INFINITE
	var code uint32
	if r, _, _ := procGetExitCodeProcess.Call(h, uintptr(unsafe.Pointer(&code))); r == 0 {
		return 0, fmt.Errorf("не е възможно да се прочете кодът за изход")
	}
	return int32(code), nil
}

// openURL отваря адрес в браузъра на потребителя.
func openURL(url string) {
	if procShellExecuteW.Find() != nil {
		return
	}
	procShellExecuteW.Call(0, uintptr(unsafe.Pointer(utf16Ptr("open"))),
		uintptr(unsafe.Pointer(utf16Ptr(url))), 0, 0, 0)
}

// ensureMessageQueue създава опашката съобщения на текущия thread.
// Без това PostThreadMessage(WM_QUIT) може да се загуби и hook thread-ът
// да остане жив -&gt; два едновременни hook-а след рестарт на ролята.
func ensureMessageQueue() {
	if procPeekMessageW.Find() != nil {
		return
	}
	var m winMsg
	procPeekMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, 0 /*PM_NOREMOVE*/)
}

type winMsg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

// hookThread стартира dedicated OS thread с message loop (изискване на low-level hook-овете).
// onExit се извиква преди thread-ът да умре.
func startHookThread(install func() (uninstall func()), onExit func()) {
	ready := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		// Създаваме опашката съобщения ВЕДНАГА - иначе PostThreadMessage(WM_QUIT)
		// може да се загуби и старият hook thread да остане жив.
		ensureMessageQueue()
		un := install()
		close(ready)
		var m winMsg
		// Блокиращ message loop — hook callback-ите се извикват от този thread
		// при всяко извикване на GetMessage. Цикълът е задължителен: спира се
		// само при WM_QUIT (GetMessage връща 0). Превеждаме и разпределяме
		// съобщенията, за да стигне WM_INPUT до rawWndProc.
		for {
			r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if int32(r) == 0 || int32(r) == -1 {
				break
			}
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
		if un != nil {
			un()
		}
		if onExit != nil {
			onExit()
		}
		runtime.UnlockOSThread()
	}()
	<-ready
}
