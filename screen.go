package main

// Геометрия на екраните. Всички стойности са в ФИЗИЧЕСКИ пиксели.
//
// Важно: преди да се чете каквото и да е за екрана, процесът трябва да е
// Per-Monitor DPI aware (setDpiAwareness се вика първо в main).
// При не-DPI-aware процес Windows виртуализира GetSystemMetrics/GetCursorPos:
// при 200% мащабиране 2880x1800 се връща като 1440x900 и ръбът на екрана
// се намира в средата на дисплея -> преходът към друг компютър не се
// задейства правилно. Това е причината програмата да "не работи" на
// машини с мащабиране (и при 125%/150%).

import (
	"fmt"
	"syscall"
	"unsafe"
)

// screenRect е правоъгълник в физически екранни координати.
type screenRect struct {
	X, Y, W, H int
}

func (r screenRect) right() int  { return r.X + r.W }
func (r screenRect) bottom() int { return r.Y + r.H }
func (r screenRect) empty() bool { return r.W <= 0 || r.H <= 0 }

func (r screenRect) String() string {
	return fmt.Sprintf("%dx%d @ %d,%d", r.W, r.H, r.X, r.Y)
}

// virtualScreenRect връща обединението на всички монитори (физически пиксели).
func virtualScreenRect() screenRect {
	x := getSystemMetrics(smXVirtualScreen)
	y := getSystemMetrics(smYVirtualScreen)
	w := getSystemMetrics(smCXVirtualScreen)
	h := getSystemMetrics(smCYVirtualScreen)
	if w <= 0 || h <= 0 {
		// fallback: само основният монитор
		x, y = 0, 0
		w = getSystemMetrics(smCxScreen)
		h = getSystemMetrics(smCyScreen)
	}
	if w <= 0 {
		w = 1920
	}
	if h <= 0 {
		h = 1080
	}
	return screenRect{X: x, Y: y, W: w, H: h}
}

// primaryMonitorRect връща физическия rect на основния монитор.
func primaryMonitorRect() screenRect {
	h := monitorFromPoint(0, 0)
	if h != 0 {
		if mi, ok := getMonitorInfo(h); ok {
			return rectToScreen(mi.RcMonitor)
		}
	}
	return screenRect{X: 0, Y: 0, W: getSystemMetrics(smCxScreen), H: getSystemMetrics(smCyScreen)}
}

// monitorRectAt връща физическия rect на монитора, който съдържа точката.
func monitorRectAt(x, y int32) screenRect {
	h := monitorFromPoint(x, y)
	if h != 0 {
		if mi, ok := getMonitorInfo(h); ok {
			return rectToScreen(mi.RcMonitor)
		}
	}
	return screenRect{}
}

func rectToScreen(r rect) screenRect {
	return screenRect{
		X: int(r.Left),
		Y: int(r.Top),
		W: int(r.Right - r.Left),
		H: int(r.Bottom - r.Top),
	}
}

// monitorRects е временен буфер при извикване на EnumDisplayMonitors.
var monitorRects []screenRect

func enumMonitorsProc(hMonitor, hdc, lprc, lParam uintptr) uintptr {
	var mi monitorInfo
	mi.CbSize = uint32(unsafe.Sizeof(monitorInfo{}))
	if r, _, _ := procGetMonitorInfoW.Call(hMonitor, uintptr(unsafe.Pointer(&mi))); r != 0 {
		monitorRects = append(monitorRects, rectToScreen(mi.RcMonitor))
	}
	return 1
}

// allMonitorRects връща rect-а на всеки монитор (за диагностиката).
func allMonitorRects() []screenRect {
	if procEnumDisplayMonitors.Find() != nil {
		return nil
	}
	monitorRects = nil
	cb := syscall.NewCallback(enumMonitorsProc)
	procEnumDisplayMonitors.Call(0, 0, cb, 0)
	return monitorRects
}

// systemScale връща процента на мащабиране на системата (100 = 1:1).
func systemScale() int {
	dpi := getDpiForSystem()
	if dpi <= 0 {
		dpi = 96
	}
	return (dpi*100 + 48) / 96
}

// localArea е областта на екрана на primary-а, използвана в layout-а.
// Използваме виртуалния екран (всички монитори), за да може
// преминаването да стане от ръба на КОИТО и да е монитор е бил курсорът.
func localArea() screenRect {
	return virtualScreenRect()
}

// virtualScreenSize връща размерите на виртуалния екран (всички монитори).
func virtualScreenSize() (int, int) {
	r := virtualScreenRect()
	return r.W, r.H
}
