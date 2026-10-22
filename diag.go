package main

// Диагностичен отчет — за отстраняване на проблеми (особено при
// "не работи на другия компютър"). Генерира се от уеб панела и може
// да се копира и изпрати.

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
)

const appVersion = "1.2"

// diagReport връща текстов отчет с цялата информация за текущото състояние.
func (a *App) diagReport() string {
	cfg := a.cfg.snapshot()
	var b strings.Builder
	w := func(f string, args ...any) { fmt.Fprintf(&b, f, args...) }

	host, _ := os.Hostname()
	w("=== MultiKVM %s — отчет за диагностика ===\n", appVersion)
	w("Време      : %s\n", time.Now().Format("2006-01-02 15:04:05"))
	w("Компютър   : %s (%s/%s)\n", host, runtime.GOOS, runtime.GOARCH)
	w("Изпълняем  : %s\n\n", exePath())

	w("--- Приложение ---\n")
	w("Роля              : %s\n", roleName(cfg.Role))
	w("Име               : %s\n", cfg.Name)
	w("Peer порт         : %d\n", cfg.Port)
	w("UI порт           : %d\n", cfg.UIPort)
	w("Споделяне         : %v\n", cfg.Enabled)
	w("Администратор     : %v\n", isElevated())
	w("DPI awareness     : %s\n", dpiAwarenessMode)
	w("Мащабиране        : %d%%\n", systemScale())
	w("Достъп от мрежата : %v\n", cfg.LANUI)
	w("Автостарт         : %v\n", autostartEnabled())
	w("Конфиг            : %s\n\n", cfg.path)

	w("--- Екрани (физически пиксели) ---\n")
	area := localArea()
	w("Виртуален екран   : %s\n", area)
	w("Основен монитор   : %s\n", primaryMonitorRect())
	mons := allMonitorRects()
	if len(mons) == 0 {
		w("Монитори          : (не се четат)\n")
	}
	for i, m := range mons {
		w("  Монитор %d       : %s\n", i+1, m)
	}
	w("\n")

	w("--- Мрежа ---\n")
	for _, ip := range a.localIPs() {
		w("IP адрес          : %s\n", ip)
	}
	w("Профил на стената : %s\n", firstLine(networkProfile()))
	ports := a.firewallPorts()
	if len(ports) == 0 {
		w("Отворени портове  : (няма какво да се отваря за тази роля)\n")
	} else {
		st := firewallStatus(ports...)
		w("Правило в стената : %v (портове %v)\n", st.RuleExists, ports)
		w("Портове покрити   : %v\n", st.PortOpen)
		if !st.RuleExists {
			w("  -> ВНИМАНИЕ: няма правило! Стартирай като администратор или натисни\n")
			w("     \"Разреши в защитната стена\" в уеб панела.\n")
		} else if !st.PortOpen {
			w("  -> ВНИМАНИЕ: правилото не покрива порта %v!\n", ports)
		}
	}
	w("\n")

	w("--- Състояние ---\n")
	if cfg.Role == "primary" {
		w("Hook-ове          : %s\n", hookState(a))
		name, remote := a.ctl.CurrentScreen()
		w("Управлявам        : %s (remote=%v)\n", name, remote)
	} else {
		a.peerLnMu.Lock()
		listening := a.peerLn != nil
		a.peerLnMu.Unlock()
		w("Слушам на порт    : %v (порт %d)\n", listening, cfg.Port)
	}
	w("Raw input (WM_INPUT): %d съобщения\n", a.ctl.rawIn.Load())
	w("Hook мишка        : %d събития\n", a.ctl.hookMouse.Load())
	if a.ctl.rawIn.Load() == 0 && a.ctl.hookMouse.Load() > 0 {
		w("  -> Raw input не пристига. Програмата използва резервен режим\n")
		w("     (движение по позиция от hook-а). Ако преминаването между\n")
		w("     екрани е бавно — виж 'натискане на ръба' в РЪКОВОДСТВО.\n")
	}
	w("\n")

	w("--- Компютри (peers) ---\n")
	if len(cfg.Peers) == 0 {
		w("(няма добавени)\n")
	}
	a.peerMu.Lock()
	for i, p := range cfg.Peers {
		line := fmt.Sprintf("  %d. %-14s %-22s %-7s", i+1, p.Name, p.Addr, p.Dir)
		if i < len(a.peers) {
			ps := a.peers[i]
			if ps.connected.Load() {
				line += fmt.Sprintf("свързан  rtt=%dms  екран=%dx%d", ps.rttMS.Load(), ps.screenW.Load(), ps.screenH.Load())
			} else {
				err := ""
				if e, ok := ps.lastErr.Load().(string); ok {
					err = e
				}
				line += "НЕ е свързан"
				if err != "" {
					line += "  (" + err + ")"
				}
			}
		} else {
			line += "НЕ е свързан"
		}
		w("%s\n", line)
	}
	a.peerMu.Unlock()
	w("\n")

	w("--- Последни съобщения ---\n")
	for _, l := range a.LogLines() {
		w("%s\n", l)
	}
	return b.String()
}

func roleName(r string) string {
	if r == "primary" {
		return "PRIMARY (имам мишката)"
	}
	return "PEER (слушам)"
}

func exePath() string {
	p, err := os.Executable()
	if err != nil {
		return "?"
	}
	return p
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// hookState описва състоянието на hook-овете (за диагностиката).
func hookState(a *App) string {
	c := a.ctl
	if !c.running() {
		return "НЕ са инсталирани (споделянето е изключено или ролята е peer)"
	}
	return fmt.Sprintf("инсталирани (mouse=%d key=%d rawWnd=%d)", c.mouseHook, c.keyHook, c.rawWnd)
}
