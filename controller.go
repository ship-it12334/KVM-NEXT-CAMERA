package main

// Controller (primary роля): инсталира global mouse/keyboard hook-ове,
// засича когато курсорът удари ръба на екрана и "захваща" управлението,
// пращайки абсолютни координати към текущия peer.

import (
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

const (
	stLocal = iota
	stRemote
)

const (
	// switchCooldown предотвратява нежелан незабавен преход
	// (курсорът стои на ръба след връщане).
	switchCooldown = 220 * time.Millisecond
	// edgeArmedDelta е колко сурово движение към ръба трябва
	// да натрупаме, за да захванем (защита срещу трептене).
	edgeArmedDelta = 4
	// edgePushDelay е колко дълго трябва потребителят да натиска ръба
	// (в резервния режим, когато няма raw input).
	edgePushDelay = 150 * time.Millisecond
	// edgePushGap е максималната пауза между две събития на движение,
	// при която натискането се счита за непрекъснато.
	edgePushGap = 250 * time.Millisecond
)

type Screen struct {
	PeerIdx int // -1 = primary
	W, H    int
}

type Controller struct {
	app *App

	hookMu     sync.Mutex
	mouseHook  uintptr
	keyHook    uintptr
	rawWnd     uintptr
	hookTid    atomic.Uint32
	hookDone   chan struct{}
	stopped    atomic.Bool
	ctlEnabled atomic.Bool // споделянето разрешено ли е (toggle от UI/hotkey)

	// преходи
	stateMu sync.Mutex
	state   int // stLocal / stRemote
	// cur е индекс в layout-а ([left…][primary][right…]), а НЕ индекс в
	// cfg.Peers. За изпращане трябва layout[cur].PeerIdx — виж peerIdxFor.
	cur        int
	vx, vy     int
	lastSwitch time.Time

	// сурово движение, натрупвано за edge-детекция в local режим
	edgeAccX    int
	rawCount    atomic.Int64
	lastEdgeLog time.Time
	lastFwdWarn time.Time
	hookMouse   atomic.Int64
	hookKey     atomic.Int64
	rawIn       atomic.Int64
	fwdBtn      atomic.Int64
	fwdKey      atomic.Int64
	fwdWl       atomic.Int64

	// --- резервен (fallback) механизъм за движението ---
	// Raw input (WM_INPUT) дава истинските дилти дори когато курсорът е
	// закачен на ръба. Но при някои системи/конфигурации WM_INPUT изобщо
	// не пристига (напр. блокиран raw input, някои драйвери). Затова
	// имаме втори източник: low-level mouse hook-ът (WH_MOUSE_LL), който
	// работи ВИНАГИ и дава абсолютната позиция на курсора.
	// Позиция от последния hook извикване:
	lastPX, lastPY  int32
	havePos         bool
	expectMoveUntil time.Time // позицията е променена от нас (SetCursorPos) — игнорирай
	remoteViaRaw    bool      // в remote режим използваме raw дилтите (не LL hook)
	pushStart       time.Time
	pushLast        time.Time

	// rawMove брои суровите дилти, които наистина са движение (не нули).
	// rawOK() гледа именно този брояч — ако WM_INPUT идва, но винаги е 0,
	// значи raw входът е неизползваем и трябва да се ползва резервният режим.
	rawMove        atomic.Int64
	lastRawMoveAt  atomic.Int64 // unix nano на последния ненулев raw делта
	lastHookMoveAt atomic.Int64 // unix nano на последното движение от hook-а

	sends atomic.Int64 // брой изпратени съобщения (за UI статистика)
}

func NewController(app *App) *Controller {
	return &Controller{app: app, state: stLocal}
}

// ---- layout ----

// buildLayout подрежда екрани: [left peers...] [primary] [right peers...].
func (a *App) buildLayout() []Screen {
	cfg := a.cfg.snapshot()
	area := localArea()
	var lefts, rights []Screen
	for i, p := range cfg.Peers {
		w, h := a.peerScreenSize(i)
		s := Screen{PeerIdx: i, W: w, H: h}
		if p.Dir == "left" {
			lefts = append(lefts, s)
		} else {
			rights = append(rights, s)
		}
	}
	// left peers: най-близкият до primary е последен в списъка
	out := make([]Screen, 0, len(lefts)+len(rights)+1)
	for i := len(lefts) - 1; i >= 0; i-- {
		out = append(out, lefts[i])
	}
	// primary заема виртуалния екран (всички монитори) на тази машина
	out = append(out, Screen{PeerIdx: -1, W: area.W, H: area.H})
	out = append(out, rights...)
	return out
}

// ---- hook thread ----

var (
	cbMouse uintptr
	cbKey   uintptr
	cbWnd   uintptr
	cbOnce  sync.Once
	ctlrRef atomic.Pointer[Controller]
)

func (c *Controller) Start() {
	c.stopped.Store(false)
	ctlrRef.Store(c)
	cbOnce.Do(func() {
		cbMouse = syscallNewCallback(mouseHookProc)
		cbKey = syscallNewCallback(keyHookProc)
		cbWnd = syscallNewCallback(rawWndProc)
	})
	done := make(chan struct{})
	c.hookMu.Lock()
	c.hookDone = done
	c.hookMu.Unlock()

	startHookThread(func() func() {
		c.hookTid.Store(uint32(getCurrentThreadID()))
		c.mouseHook = setWindowsHookEx(whMouseLL, cbMouse, 0, 0)
		c.keyHook = setWindowsHookEx(whKeyboardLL, cbKey, 0, 0)
		c.rawWnd = createMessageWindow(cbWnd, utf16Ptr("MultiKVMRawInput"))
		regOK := false
		if c.rawWnd != 0 {
			regOK = registerRawInputMouse(c.rawWnd)
		}
		c.app.logf("DBG raw hwnd=%d reg=%v mouseHook=%d keyHook=%d %s",
			c.rawWnd, regOK, c.mouseHook, c.keyHook, lastCreateWinErr)
		if c.mouseHook == 0 || c.keyHook == 0 {
			c.app.logf("⚠️ Hook-ът не е инсталиран (mouse=%d key=%d) — бутони/клавиатура няма да се препращат!",
				c.mouseHook, c.keyHook)
		}
		return func() {
			if c.mouseHook != 0 {
				unhookWindowsHookEx(c.mouseHook)
			}
			if c.keyHook != 0 {
				unhookWindowsHookEx(c.keyHook)
			}
			if c.rawWnd != 0 {
				procDestroyWindow.Call(c.rawWnd)
			}
		}
	}, func() {
		c.hookTid.Store(0)
		close(done)
	})
}

func (c *Controller) Stop() {
	c.stopped.Store(true)
	c.ungrab()
	tid := c.hookTid.Swap(0)
	if tid != 0 {
		postThreadMessage(tid, wmQuit)
		// изчакваме thread-а да излезе, иначе при рестарт на ролята
		// остават два едновременни hook-а
		c.hookMu.Lock()
		done := c.hookDone
		c.hookMu.Unlock()
		if done != nil {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				c.app.logf("⚠️ Hook thread-ът не се спря навреме")
			}
		}
	}
	ctlrRef.Store(nil)
}

func (c *Controller) running() bool {
	return c.hookTid.Load() != 0
}

// ---- hook callbacks (изпълняват се на hook thread) ----

func mouseHookProc(nCode uintptr, wParam, lParam uintptr) uintptr {
	c := ctlrRef.Load()
	if c == nil || c.stopped.Load() || int32(nCode) < 0 {
		return callNextHookEx(0, int32(nCode), wParam, lParam)
	}
	if !c.ctlEnabled.Load() {
		return callNextHookEx(0, int32(nCode), wParam, lParam)
	}
	// Далечният peer получава само локалните събития чрез inject; този hook
	// управлява преходи и consume-ва събития, докато сме в remote режим.
	ms := (*msllHookStruct)(unsafe.Pointer(lParam))
	wm := uint32(wParam)

	c.stateMu.Lock()
	remote := c.state == stRemote
	c.stateMu.Unlock()

	// диагностика: логваме само бутон/скрол (не movement — рядко е)
	if wm != wmMouseMove && wm != 0 {
		c.app.logf("BTN wm=%d remote=%v", wm, remote)
	}

	if wm == wmMouseMove {
		// Low-level hook-ът работи ВИНАГИ (за разлика от raw input) и дава
		// абсолютната позиция — това е основният източник за движението.
		c.onMouseMoveAbs(ms.Pt.X, ms.Pt.Y)
	}

	if remote {
		// consume-ваме всички мишка събития — курсорът е закачен на ръба
		switch wm {
		case wmMouseMove:
			// движението идва чрез raw input (WM_INPUT), не тук
		case wmLButtonDown, wmLButtonUp:
			c.forwardButton(btnLeft, wm == wmLButtonDown)
		case wmRButtonDown, wmRButtonUp:
			c.forwardButton(btnRight, wm == wmRButtonDown)
		case wmMButtonDown, wmMButtonUp:
			c.forwardButton(btnMiddle, wm == wmMButtonDown)
		case wmXButtonDown, wmXButtonUp:
			b := btnX1
			if ms.MouseData>>16&0xFF == 2 {
				b = btnX2
			}
			c.forwardButton(b, wm == wmXButtonDown)
		case wmMouseWheel:
			delta := int(int16(ms.MouseData >> 16))
			c.forwardWheel(delta)
		}
		return 1
	}

	// local режим: следим за курсор на ръба
	if wm == wmMouseMove {
		// Засичането на ръба става в onRawMove (суровият input продължава
		// да идва дори когато курсорът е закачен на ръба, докато WM_MOUSEMOVE
		// спира да се генерира там). Тук само минаваме нататък.
	}
	return callNextHookEx(0, int32(nCode), wParam, lParam)
}

func keyHookProc(nCode uintptr, wParam, lParam uintptr) uintptr {
	c := ctlrRef.Load()
	if c == nil || c.stopped.Load() || int32(nCode) < 0 {
		return callNextHookEx(0, int32(nCode), wParam, lParam)
	}
	wm := uint32(wParam)

	// Hotkey: Ctrl+Alt+S — включва/изключва споделянето (достъпен винаги)
	kb := (*kbdllHookStruct)(unsafe.Pointer(lParam))
	if kb.VkCode == 'S' && getAsyncKeyState(vkControl) && getAsyncKeyState(vkMenu) {
		if wm == wmKeyDown || wm == wmSysKeyDown {
			c.app.ToggleEnabled()
			return 1
		}
		return 1
	}

	if !c.ctlEnabled.Load() {
		return callNextHookEx(0, int32(nCode), wParam, lParam)
	}

	c.stateMu.Lock()
	remote := c.state == stRemote
	c.stateMu.Unlock()

	// диагностика: логваме само key down (не up — за краткота)
	if wm == wmKeyDown || wm == wmSysKeyDown {
		c.app.logf("KEY vk=%d remote=%v", kb.VkCode, remote)
	}

	if remote && (wm == wmKeyDown || wm == wmKeyUp || wm == wmSysKeyDown || wm == wmSysKeyUp) {
		c.forwardKey(kb.VkCode, kb.ScanCode, kb.Flags&llkhfExtended != 0, wm == wmKeyUp || wm == wmSysKeyUp)
		return 1
	}
	return callNextHookEx(0, int32(nCode), wParam, lParam)
}

func rawWndProc(hwnd, wm, wParam, lParam uintptr) uintptr {
	if wm == wmAppShowCursor {
		showCursor(true)
	}
	if wm == wmInput {
		c := ctlrRef.Load()
		if c != nil && !c.stopped.Load() && c.ctlEnabled.Load() {
			c.rawIn.Add(1)
			if dx, dy, ok := parseRawMouse(lParam); ok {
				c.onRawMove(dx, dy)
			}
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, wm, wParam, lParam)
	return r
}

// ---- преходи ----

// neighbor намира съседен екран в посока dir (1 надясно/надолу, -1 наляво/нагоре).
// Връща индекс в layout и ok, ако съседът е peer и е свързан.
func (c *Controller) neighbor(layout []Screen, idx, dir int) (int, bool) {
	j := idx + dir
	for j >= 0 && j < len(layout) {
		s := layout[j]
		if s.PeerIdx >= 0 && c.app.peerConnected(s.PeerIdx) {
			return j, true
		}
		j += dir
	}
	return 0, false
}

// grab закача курсора и влиза в remote режим към layout[idx].
func (c *Controller) grab(layout []Screen, idx, dirX, relY int) {
	s := layout[idx]
	if s.PeerIdx < 0 || !c.app.peerConnected(s.PeerIdx) {
		return
	}
	area := localArea()
	// влиза се от ръба: при движение надясно — от левия ръб на новия екран
	vx := 0
	if dirX < 0 {
		vx = s.W - 1
	}
	vy := clampInt(mapY(relY, area.H, s.H), 0, s.H-1)

	// Има ли raw input? Ако потребителят вече е местил мишката, но WM_INPUT
	// не е дошъл — значи raw input е блокиран и трябва да ползваме резервния
	// механизъм (LL hook + свободен невидим курсор).
	useRaw := c.rawOK()

	c.stateMu.Lock()
	c.state = stRemote
	c.cur = idx
	c.vx, c.vy = vx, vy
	c.lastSwitch = time.Now()
	c.remoteViaRaw = useRaw
	c.pushStart = time.Time{}
	c.pushLast = time.Time{}
	c.edgeAccX = 0
	c.stateMu.Unlock()

	// скриваме курсора и закачаме физическия на ръба, за да не излиза
	// pin the physical cursor to the edge we came from, so it cannot
	// slip back onto the local screen
	if useRaw {
		if cx, cy, ok := getCursorPos(); ok {
			mon := monitorRectAt(cx, cy)
			if mon.empty() {
				mon = area
			}
			var edge int32
			if dirX > 0 {
				edge = int32(mon.right() - 1)
			} else {
				edge = int32(mon.X)
			}
			clipCursor(&rect{Left: edge, Top: int32(cy), Right: edge + 1, Bottom: int32(cy) + 1})
		}
	} else {
		// резервен режим: курсорът трябва да може да се движи, за да взимаме
		// дилтите от hook-а (само го скриваме).
		clipCursor(nil)
	}
	showCursor(false)
	c.sendMove(s.PeerIdx, vx, vy)
	c.app.logf("→ контролираме екран на %s (raw=%v)", c.app.peerName(s.PeerIdx), c.remoteViaRaw)
}

func (c *Controller) ungrab() {
	c.stateMu.Lock()
	was := c.state
	c.state = stLocal
	c.cur = 0
	c.lastSwitch = time.Now()
	c.remoteViaRaw = false
	c.stateMu.Unlock()
	if was != stRemote {
		return
	}
	clipCursor(nil)
	c.showCursorAsync()
	c.app.logf("→ контролираме локалния екран")
}

// ungrabToPrimary връща курсора на primary екрана на дадена позиция.
func (c *Controller) ungrabToPrimary(relX, relY int) {
	c.ungrab()
	area := localArea()
	setCursorPos(int32(area.X+clampInt(relX, 0, area.W-1)), int32(area.Y+clampInt(relY, 0, area.H-1)))
}

// showCursorAsync shows the cursor from the hook thread - the same thread
// that hid it (ShowCursor keeps a counter, we do not want to depend on
// which thread calls it).
func (c *Controller) showCursorAsync() {
	tid := c.hookTid.Load()
	if tid != 0 && postThreadMessage(tid, wmAppShowCursor) {
		return
	}
	showCursor(true)
}

// onRawMove обработва сурови дилти от WM_INPUT (работят и със закачен курсор).
func (c *Controller) onRawMove(dx, dy int32) {
	c.rawIn.Add(1)
	if dx != 0 || dy != 0 {
		c.rawMove.Add(1)
		c.lastRawMoveAt.Store(time.Now().UnixNano())
	}
	c.stateMu.Lock()
	if c.state == stRemote {
		c.vx += int(dx)
		c.vy += int(dy)
		vx, vy := c.vx, c.vy
		c.stateMu.Unlock()
		c.applyRemotePos(vx, vy)
		return
	}
	c.stateMu.Unlock()

	// local режим: засичаме "натискане" към ръба на екрана.
	// Суровият input дава реални дилти дори когато курсорът вече е закачен
	// на ръба (Windows не генерира WM_MOUSEMOVE там), затова проверката е тук.
	c.checkEdge(dx)
}

// onMouseMoveAbs обработва абсолютната позиция на курсора от low-level
// mouse hook-а. Това е резервният източник за движението: raw input
// (WM_INPUT) може да не пристига при някои конфигурации, а hook-ът
// работи винаги.
//
// В remote режим използваме ТОЧНО ЕДИН източник за дилтите, иначе
// курсорът на peer-а се движи двойно по-бързо:
//   - ако при grab сме разпознали, че raw input работи -> ползваме raw;
//   - иначе -> ползваме дилта от hook-а (курсорът тогава не е clip-нат).
func (c *Controller) onMouseMoveAbs(px, py int32) {
	c.hookMouse.Add(1)
	c.lastHookMoveAt.Store(time.Now().UnixNano())

	c.stateMu.Lock()
	if !c.expectMoveUntil.IsZero() && time.Now().Before(c.expectMoveUntil) {
		// позицията е променена от нас (SetCursorPos при wrap) — игнорирай
		c.expectMoveUntil = time.Time{}
		c.lastPX, c.lastPY = px, py
		c.havePos = true
		c.stateMu.Unlock()
		return
	}
	dx, dy := int32(0), int32(0)
	if c.havePos {
		dx = px - c.lastPX
		dy = py - c.lastPY
	}
	c.lastPX, c.lastPY = px, py
	c.havePos = true
	remote := c.state == stRemote
	viaRaw := c.remoteViaRaw
	c.stateMu.Unlock()

	if remote {
		if viaRaw {
			// Стражи: ако raw входът е спрял да носи движение, но потребителят
			// все така мести мишката (hook-ът го вижда) — превключваме на
			// резервния режим, вместо да замрем на екрана на peer-а.
			now := time.Now()
			lastRaw := time.Unix(0, c.lastRawMoveAt.Load())
			lastHook := time.Unix(0, c.lastHookMoveAt.Load())
			if !lastRaw.IsZero() && now.Sub(lastRaw) > 2*time.Second &&
				now.Sub(lastHook) < 500*time.Millisecond {
				c.stateMu.Lock()
				c.remoteViaRaw = false
				c.stateMu.Unlock()
				clipCursor(nil)
				c.app.logf("⚠️ Raw input спря да носи движение — резервен режим")
			} else {
				return // движението идва от onRawMove
			}
		}
		if dx == 0 && dy == 0 {
			return
		}
		c.stateMu.Lock()
		c.vx += int(dx)
		c.vy += int(dy)
		vx, vy := c.vx, c.vy
		c.stateMu.Unlock()
		c.applyRemotePos(vx, vy)
		c.stateMu.Lock()
		stillRemote := c.state == stRemote
		c.stateMu.Unlock()
		if stillRemote {
			c.wrapLocalCursor()
		}
		return
	}

	// local режим: ако raw input работи, ръбът се следи от него.
	if c.rawOK() {
		return
	}
	c.checkEdgeNoRaw(px, py)
}

// rawOK връща true, ако raw input (WM_INPUT) наистина носи движение.
// Ако потребителят вече е местил мишката (трябва, за да достигне ръба),
// а raw входът все още не е дал нито една ненулева делта — значи той е
// блокиран/изключен и трябва да се ползва резервният механизъм.
func (c *Controller) rawOK() bool {
	return c.rawMove.Load() > 0
}

// wrapLocalCursor предотвратява "засядането" на невидимия локален курсор
// в резервния режим: когато достигне границата на виртуалния екран,
// телепортираме го на противоположната страна. Движението към peer-а
// продължава нормално, защото дилтите идват от hook-а.
func (c *Controller) wrapLocalCursor() {
	area := localArea()
	x, y, ok := getCursorPos()
	if !ok {
		return
	}
	const margin = 64
	var nx, ny int32 = x, y
	if int(x) >= area.right()-margin {
		nx = int32(area.X + margin)
	} else if int(x) <= area.X+margin-1 {
		nx = int32(area.right() - margin - 1)
	}
	if int(y) >= area.bottom()-margin {
		ny = int32(area.Y + margin)
	} else if int(y) <= area.Y+margin-1 {
		ny = int32(area.bottom() - margin - 1)
	}
	if nx == x && ny == y {
		return
	}
	c.stateMu.Lock()
	c.expectMoveUntil = time.Now().Add(150 * time.Millisecond)
	c.stateMu.Unlock()
	setCursorPos(int32(nx), int32(ny))
}

// checkEdgeNoRaw е резервната edge-детекция (когато няма raw input).
// Курсорът трябва да стои на ръба на виртуалния екран, а потребителят
// трябва да натиска непрекъснато (движението на мишката продължава, но
// курсорът не може да се движи — Windows го "задържа" на ръба).
func (c *Controller) checkEdgeNoRaw(px, py int32) {
	area := localArea()
	dirX := 0
	switch {
	case int(px) >= area.right()-1:
		dirX = 1
	case int(px) <= area.X:
		dirX = -1
	}
	if dirX == 0 {
		c.resetEdgeAcc()
		return
	}

	now := time.Now()
	c.stateMu.Lock()
	if c.pushStart.IsZero() || now.Sub(c.pushLast) > edgePushGap {
		c.pushStart = now
	}
	c.pushLast = now
	pressing := now.Sub(c.pushStart) >= edgePushDelay
	c.stateMu.Unlock()

	if !pressing {
		return
	}
	if time.Since(c.lastEdgeLog) > time.Second {
		c.lastEdgeLog = now
		c.app.logf("DBG edge(fallback) x=%d y=%d dir=%d", px, py, dirX)
	}
	c.trySwitch(dirX, int(py)-area.Y)
}

// resetEdgeAcc нулира натрупаното натискане (курсорът не е на ръба).
func (c *Controller) resetEdgeAcc() {
	c.stateMu.Lock()
	c.edgeAccX = 0
	c.pushStart = time.Time{}
	c.pushLast = time.Time{}
	c.stateMu.Unlock()
}

// trySwitch преминава към съседния екран в посока dirX, ако такъв има
// и е свързан.
func (c *Controller) trySwitch(dirX int, relY int) {
	layout := c.app.buildLayout()
	pidx := primaryLayoutIndex(layout)
	if pidx < 0 {
		return
	}
	idx, ok := c.neighbor(layout, pidx, dirX)
	if !ok {
		return
	}
	c.stateMu.Lock()
	cooling := time.Since(c.lastSwitch) < switchCooldown
	c.stateMu.Unlock()
	if cooling {
		return
	}
	c.app.logf("DBG grab dir=%d idx=%d", dirX, idx)
	c.grab(layout, idx, dirX, relY)
}

// checkEdge захваща управлението, когато курсорът е на ръба и потребителят
// натиска към него достатъчно. dx е суровата хоризонтална дилта.
//
// Ръбът е ръбът на виртуалния екран (всички монитори), защото в layout-а
// primary-ът заема целия виртуален екран, а peer-ите са вляво/вдясно от него.
// Всички координати са в физически пиксели (процесът е DPI aware).
func (c *Controller) checkEdge(dx int32) {
	area := localArea()

	cx, cy, ok := getCursorPos()
	if !ok {
		return
	}

	// посоката — към ръба, на който стои курсорът
	var dirX int
	switch {
	case int(cx) >= area.right()-1 && dx > 0:
		dirX = 1
	case int(cx) <= area.X && dx < 0:
		dirX = -1
	}
	if dirX == 0 {
		c.resetEdgeAcc()
		return
	}

	// y относително на виртуалния екран (-peer-ът използва същата система)
	relY := int(cy) - area.Y

	c.stateMu.Lock()
	// натрупваме само движение в посока на ръба, на който стоим
	c.edgeAccX += absInt(int(dx))
	accX := c.edgeAccX
	c.stateMu.Unlock()

	if accX < edgeArmedDelta {
		return
	}

	// диагностика: логваме веднъж/сек, когато сме на ръба
	if time.Since(c.lastEdgeLog) > time.Second {
		c.lastEdgeLog = time.Now()
		c.app.logf("DBG edge x=%d y=%d area=%s dir=%d acc=%d", cx, cy, area, dirX, accX)
	}

	c.trySwitch(dirX, relY)
}

func (c *Controller) applyRemotePos(vx, vy int) {
	layout := c.app.buildLayout()
	c.stateMu.Lock()
	idx := c.cur
	c.stateMu.Unlock()
	if idx < 0 || idx >= len(layout) {
		c.ungrab()
		return
	}
	s := layout[idx]

	// преминаване към съседен екран при излизане извън границите
	if vx >= s.W {
		if idx+1 < len(layout) {
			nb := layout[idx+1]
			if nb.PeerIdx >= 0 && c.app.peerConnected(nb.PeerIdx) {
				c.switchTo(layout, idx+1, 0, vy)
				return
			}
			// наляво-разположен peer, излизане надясно → обратно към primary
			if nb.PeerIdx < 0 {
				ph := nb.H
				c.ungrabToPrimary(0, mapY(vy, s.H, ph))
				return
			}
		}
		vx = s.W - 1 // bounce на ръба
	}
	if vx < 0 {
		if idx-1 >= 0 && layout[idx-1].PeerIdx >= 0 && c.app.peerConnected(layout[idx-1].PeerIdx) {
			ns := layout[idx-1]
			c.switchTo(layout, idx-1, ns.W-1, vy)
			return
		}
		// отляво от primary → връщане към локалния екран (десен ръб)
		if idx-1 >= 0 && idx-1 == primaryLayoutIndex(layout) {
			area := localArea()
			c.ungrabToPrimary(area.W-1, mapY(vy, s.H, area.H))
			return
		}
		vx = 0
	}
	if vy >= s.H {
		vy = s.H - 1
	}
	if vy < 0 {
		vy = 0
	}

	c.stateMu.Lock()
	c.vx, c.vy = vx, vy
	c.stateMu.Unlock()
	c.sendMove(s.PeerIdx, vx, vy)
}

func (c *Controller) switchTo(layout []Screen, idx, nx, ny int) {
	s := layout[idx]
	if s.PeerIdx < 0 || !c.app.peerConnected(s.PeerIdx) {
		return
	}
	vy := clampInt(ny, 0, s.H-1)
	c.stateMu.Lock()
	c.state = stRemote
	c.cur = idx
	c.vx, c.vy = nx, vy
	c.lastSwitch = time.Now()
	c.stateMu.Unlock()
	c.sendMove(s.PeerIdx, nx, vy)
	c.app.logf("→ контролираме екран на %s (raw=%v)", c.app.peerName(s.PeerIdx), c.remoteViaRaw)
}

// ---- препращане към текущия peer ----

func (c *Controller) sendMove(peerIdx, x, y int) {
	c.app.sendToPeer(peerIdx, msg{T: "move", X: x, Y: y})
	c.sends.Add(1)
}

// forwardTarget връща peer индекса за текущия екран (или -1).
// cur е индекс в layout-а ([left…][primary][right…]), а sendToPeer очаква
// индекс в cfg.Peers — пряката употреба пращаше бутони/клавиши към грешен
// пиър или извън диапазона (мълчаливо drop), докато движението ползваше
// layout[cur].PeerIdx и затова работеше.
func (c *Controller) forwardTarget(what string) int {
	c.stateMu.Lock()
	idx := c.cur
	remote := c.state == stRemote
	c.stateMu.Unlock()
	if !remote {
		return -1
	}
	layout := c.app.buildLayout()
	peer := -1
	if idx >= 0 && idx < len(layout) {
		peer = layout[idx].PeerIdx
	}
	if peer >= 0 {
		return peer
	}
	c.stateMu.Lock()
	warned := time.Since(c.lastFwdWarn) > 5*time.Second
	if warned {
		c.lastFwdWarn = time.Now()
	}
	c.stateMu.Unlock()
	if warned {
		c.app.logf("⚠️ %s: екран %d не е peer (layout len=%d) — пропуснато", what, idx, len(layout))
	}
	return -1
}

func (c *Controller) forwardButton(b int, down bool) {
	peer := c.forwardTarget("button")
	if peer < 0 {
		return
	}
	if n := c.fwdBtn.Add(1); n <= 6 || n%50 == 0 {
		c.app.logf("DBG sendBtn#%d b=%d down=%v peer=%d", n, b, down, peer)
	}
	c.app.sendToPeer(peer, msg{T: "button", B: b, Down: down})
}

func (c *Controller) forwardWheel(delta int) {
	peer := c.forwardTarget("wheel")
	if peer < 0 {
		return
	}
	if n := c.fwdWl.Add(1); n <= 6 || n%50 == 0 {
		c.app.logf("DBG sendWheel#%d d=%d peer=%d", n, delta, peer)
	}
	c.app.sendToPeer(peer, msg{T: "wheel", Delta: delta})
}

func (c *Controller) forwardKey(vk, scan uint32, ext, up bool) {
	peer := c.forwardTarget("key")
	if peer < 0 {
		return
	}
	if !up {
		if n := c.fwdKey.Add(1); n <= 6 || n%50 == 0 {
			c.app.logf("DBG sendKey#%d vk=%d scan=%d peer=%d", n, vk, scan, peer)
		}
	}
	c.app.sendToPeer(peer, msg{T: "key", VK: int(vk), Scan: int(scan), Up: up, Ext: ext})
}

// ---- helpers ----

func (c *Controller) CurrentScreen() (name string, remote bool) {
	c.stateMu.Lock()
	remote = c.state == stRemote
	idx := c.cur
	c.stateMu.Unlock()
	if !remote {
		return "Локален екран", false
	}
	layout := c.app.buildLayout()
	if idx >= 0 && idx < len(layout) && layout[idx].PeerIdx >= 0 {
		return c.app.peerName(layout[idx].PeerIdx), true
	}
	return "Локален екран", false
}

func primaryLayoutIndex(layout []Screen) int {
	for i, s := range layout {
		if s.PeerIdx < 0 {
			return i
		}
	}
	return -1
}

func mapY(y, fromH, toH int) int {
	if fromH <= 0 || toH <= 0 {
		return y
	}
	return clampInt(y*toH/fromH, 0, toH-1)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
