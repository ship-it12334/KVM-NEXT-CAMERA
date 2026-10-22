package main

// Визуален указател за peer (listener) роля.
//
// Windows 10 понякога НЕ показва системния курсор, макар позицията му да се
// движи (напр. броячът на ShowCursor е останал отрицателен след убит процес
// или системата е стартирала без мишка). Затова рисуваме собствен указател в
// най-горен, прозрачен за кликове прозорец — стрелка, която следва отдалечената
// позиция. Прозорецът е click-through, така че не пречи на работата.

import (
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

const (
	overlayClassName = "MultiKVMCursorOverlay"
	overlayHideAfter = 1600 * time.Millisecond

	overlayKeyColor = 0x00000000 // чист черен = прозрачен (color key)
	overlayInkDark  = 0x00202020 // тъмният контур (не е color key)

	wmPaint       = 0x000F
	wmEraseBkgnd  = 0x0014
	wmNcHitTest   = 0x0084
	htTransparent = ^uintptr(0) // -1

	psSolid = 0

	// базови точки на стрелката (класически курсор), мащабират се според DPI;
	// върхът е в (0,0) — там е „горещата точка“ на курсора
	overlayBaseW = 12.0
	overlayBaseH = 20.0
)

type paintStruct struct {
	Hdc         uintptr
	FErase      int32
	RcPaint     rect
	FRestore    int32
	FIncUpdate  int32
	RgbReserved [32]byte
}

type cursorOverlay struct {
	mu       sync.Mutex
	hwnd     uintptr
	brushKey uintptr // черен — color key (прозрачен)
	brushInk uintptr // тъмен контур
	brushFg  uintptr // бяла стрелка
	pen      uintptr
	pts      []point
	ptsBig   []point
	ox, oy   int // отстъп на върха в прозореца
	cb       uintptr

	enabled atomic.Bool
	shown   atomic.Bool
	last    atomic.Int64 // unix nano
}

var (
	overlay      = &cursorOverlay{}
	overlayReady atomic.Bool
)

// startOverlay създава overlay прозореца (веднъж).
func startOverlay() {
	if overlayReady.Swap(true) {
		return
	}
	overlay.cb = syscallNewCallback(overlayWndProc)

	// отделен thread с message loop — иначе прозорецът не получава WM_PAINT
	startHookThread(func() func() {
		overlay.createWindow()
		return nil
	}, nil)
	go overlay.hideLoop()
}

// setOverlayEnabled включва/изключва указателя.
func (o *cursorOverlay) setEnabled(on bool) {
	o.enabled.Store(on)
	if !on {
		o.hide()
	}
}

// move мести указателя на екранни координати (x,y) и го показва.
func (o *cursorOverlay) move(x, y int) {
	if !o.enabled.Load() {
		return
	}
	startOverlay()
	o.last.Store(time.Now().UnixNano())

	o.mu.Lock()
	hwnd := o.hwnd
	ox, oy := o.ox, o.oy
	o.mu.Unlock()
	if hwnd == 0 {
		return
	}
	if o.shown.CompareAndSwap(false, true) {
		procShowWindow.Call(hwnd, swShowNoActivate)
	}
	procSetWindowPos.Call(hwnd, hwndTopMost,
		uintptr(uint32(int32(x-ox))), uintptr(uint32(int32(y-oy))),
		0, 0, swpNosize|swpNoActivate)
	// принуждаваме прерисуване
	procInvalidateRect.Call(hwnd, 0, 1)
}

func (o *cursorOverlay) hide() {
	o.mu.Lock()
	hwnd := o.hwnd
	o.mu.Unlock()
	if hwnd == 0 || !o.shown.CompareAndSwap(true, false) {
		return
	}
	procShowWindow.Call(hwnd, swHide)
}

// hideLoop скрива указателя след кратко бездействие.
func (o *cursorOverlay) hideLoop() {
	for {
		time.Sleep(250 * time.Millisecond)
		if !o.shown.Load() {
			continue
		}
		if time.Since(time.Unix(0, o.last.Load())) > overlayHideAfter {
			o.hide()
		}
	}
}

func (o *cursorOverlay) createWindow() {
	scale := 1.0
	if r, _, _ := procGetDpiForSystem.Call(); r > 0 {
		scale = float64(r) / 96.0
	}
	s := scale * 1.25
	w := int(overlayBaseW*s) + 6
	h := int(overlayBaseH*s) + 6
	o.ox, o.oy = 3, 3

	o.pts = overlayArrow(int(overlayBaseW*s), int(overlayBaseH*s))
	o.ptsBig = overlayArrow(int(overlayBaseW*s*1.16), int(overlayBaseH*s*1.16))
	for i := range o.pts {
		o.pts[i].X += int32(o.ox)
		o.pts[i].Y += int32(o.oy)
		o.ptsBig[i].X += int32(o.ox)
		o.ptsBig[i].Y += int32(o.oy)
	}

	wc := wndClassEx{
		Style:         csHRedraw,
		LpfnWndProc:   o.cb,
		HInstance:     getModuleHandle(),
		LpszClassName: utf16Ptr(overlayClassName),
	}
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	if r, _, _ := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return
	}

	exStyle := uintptr(wsExLayered | wsExTransparent | wsExTopmost | wsExToolWindow | wsExNoActivate)
	hwnd, _, _ := procCreateWindowExW.Call(exStyle,
		uintptr(unsafe.Pointer(utf16Ptr(overlayClassName))),
		uintptr(unsafe.Pointer(utf16Ptr("MultiKVM pointer"))),
		wsPopup, 0, 0, uintptr(w), uintptr(h), 0, 0, getModuleHandle(), 0)
	if hwnd == 0 {
		return
	}
	// всичко в color-key цвят (чисто черно) е прозрачно
	procSetLayeredWindowAttributes.Call(hwnd, overlayKeyColor, 0, lwaColorKey)

	brushKey, _, _ := procCreateSolidBrush.Call(overlayKeyColor)
	brushInk, _, _ := procCreateSolidBrush.Call(overlayInkDark)
	brushFg, _, _ := procCreateSolidBrush.Call(0x00FFFFFF)
	pen, _, _ := procCreatePen.Call(psSolid, 1, overlayInkDark)

	o.mu.Lock()
	o.hwnd = hwnd
	o.brushKey, o.brushInk, o.brushFg, o.pen = brushKey, brushInk, brushFg, pen
	o.mu.Unlock()

	procShowWindow.Call(hwnd, swHide)
}

// overlayArrow връща точките на стрелката, мащабирани до w×h.
func overlayArrow(w, h int) []point {
	bx := []float64{0, 0, 4.2, 7.0, 10.0, 7.0, 12.0}
	by := []float64{0, 17.0, 13.0, 20.0, 18.5, 12.0, 12.0}
	sx := float64(w) / overlayBaseW
	sy := float64(h) / overlayBaseH
	out := make([]point, len(bx))
	for i := range bx {
		out[i] = point{X: int32(bx[i]*sx + 0.5), Y: int32(by[i]*sy + 0.5)}
	}
	return out
}

func overlayWndProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch msg {
	case wmPaint:
		overlay.paint(hwnd)
		return 0
	case wmEraseBkgnd:
		return 1
	case wmNcHitTest:
		return htTransparent // кликовете минават през указателя
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

func (o *cursorOverlay) paint(hwnd uintptr) {
	o.mu.Lock()
	key, ink, fg, pen := o.brushKey, o.brushInk, o.brushFg, o.pen
	pts, ptsBig := o.pts, o.ptsBig
	o.mu.Unlock()
	if len(pts) == 0 || key == 0 {
		return
	}

	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 {
		return
	}

	// фонът е color key → прозрачен
	bg := rect{Left: -1, Top: -1, Right: 4000, Bottom: 4000}
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&bg)), key)

	// тъмният контур е по-голяма стрелка под бялата
	old := selectObj(hdc, ink)
	selectObj(hdc, pen)
	procPolygon.Call(hdc, uintptr(unsafe.Pointer(&ptsBig[0])), uintptr(len(ptsBig)))
	selectObj(hdc, old)

	// бялата стрелка отгоре
	old = selectObj(hdc, fg)
	procPolygon.Call(hdc, uintptr(unsafe.Pointer(&pts[0])), uintptr(len(pts)))
	selectObj(hdc, old)

	procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
}

func selectObj(hdc, obj uintptr) uintptr {
	r, _, _ := procSelectObject.Call(hdc, obj)
	return r
}
