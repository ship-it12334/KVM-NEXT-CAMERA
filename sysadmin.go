package main

// Административни помощници: защитна стена, автостарт, повишени права.
//
// Всичко тук работи без външни зависимости (само netsh/reg от Windows).

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	fwRuleName = "MultiKVM"
	runKeyPath = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
	runKeyName = "MultiKVM"
)

// firewallInfo описва състоянието на правилото в защитната стена.
type firewallInfo struct {
	RuleExists bool   `json:"ruleExists"`
	PortOpen   bool   `json:"portOpen"`
	Ports      []int  `json:"ports"`
	Detail     string `json:"detail"`
	Elevated   bool   `json:"elevated"`
}

// runHidden изпълнява команда без да показва конзола.
func runHidden(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// firewallStatus проверява дали в защитната стена има правило за MultiKVM
// и дали то покрива дадения порт.
func firewallStatus(ports ...int) firewallInfo {
	info := firewallInfo{Ports: ports, Elevated: isElevated()}
	out, err := runHidden("netsh", "advfirewall", "firewall", "show", "rule", "name="+fwRuleName)
	info.Detail = strings.TrimSpace(out)
	if err != nil {
		// "No rules match the specified criteria." -> няма правило
		return info
	}
	info.RuleExists = true
	for _, p := range ports {
		if p > 0 && strings.Contains(out, strconv.Itoa(p)) {
			info.PortOpen = true
		}
	}
	return info
}

// firewallRuleArgs връща аргументите за netsh за отваряне на портовете.
func firewallRuleArgs(ports []int) string {
	var ps []string
	for _, p := range ports {
		if p > 0 {
			ps = append(ps, strconv.Itoa(p))
		}
	}
	return "advfirewall firewall add rule name=" + fwRuleName +
		" dir=in action=allow protocol=TCP localport=" + strings.Join(ps, ",") +
		" profile=any enable=yes"
}

// addFirewallRules добавя/подменя правилото в защитната стена.
// Изисква администраторски права — при нужда се повишава автоматично
// (появява се UAC промпт) и се чака да завърши.
func addFirewallRules(ports []int) error {
	var ps []string
	for _, p := range ports {
		if p > 0 {
			ps = append(ps, strconv.Itoa(p))
		}
	}
	if len(ps) == 0 {
		return fmt.Errorf("няма портове за отваряне")
	}

	if isElevated() {
		// изтриваме старото правило (ако е за друг порт) и слагаме ново
		runHidden("netsh", "advfirewall", "firewall", "delete", "rule", "name="+fwRuleName)
		out, err := runHidden("netsh", strings.Fields(firewallRuleArgs(ports))...)
		if err != nil {
			return fmt.Errorf("netsh: %v: %s", err, strings.TrimSpace(out))
		}
		return nil
	}

	// не сме администратори -> повишаваме правата
	code, err := runElevated("netsh.exe", firewallRuleArgs(ports))
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("netsh завърши с код %d (отказан ли е UAC?)", code)
	}
	return nil
}

// removeFirewallRules премахва правилото (изисква администратор).
func removeFirewallRules() error {
	if isElevated() {
		runHidden("netsh", "advfirewall", "firewall", "delete", "rule", "name="+fwRuleName)
		return nil
	}
	code, err := runElevated("netsh.exe", "advfirewall firewall delete rule name="+fwRuleName)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("netsh завърши с код %d", code)
	}
	return nil
}

// firewallPorts връща портовете, които трябва да са отворети за текущия конфиг.
func (a *App) firewallPorts() []int {
	cfg := a.cfg.snapshot()
	ports := []int{}
	if cfg.Role == "peer" {
		ports = append(ports, cfg.Port)
	}
	if cfg.LANUI {
		ports = append(ports, cfg.UIPort)
	}
	return ports
}

// ---- автостарт при вход в Windows ----

// autostartEnabled връща true, ако MultiKVM е регистриран за автостарт.
func autostartEnabled() bool {
	out, err := runHidden("reg", "query", runKeyPath, "/v", runKeyName)
	return err == nil && strings.Contains(out, runKeyName)
}

// setAutostart добавя/премахва записа в Run ключа (HKCU — не изисква админ).
func setAutostart(enabled bool) error {
	if enabled {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		exe, _ = filepath.Abs(exe)
		out, err := runHidden("reg", "add", runKeyPath, "/v", runKeyName, "/t", "REG_SZ",
			"/d", "\""+exe+"\"", "/f")
		if err != nil {
			return fmt.Errorf("reg add: %v: %s", err, strings.TrimSpace(out))
		}
		return nil
	}
	out, err := runHidden("reg", "delete", runKeyPath, "/v", runKeyName, "/f")
	if err != nil {
		// записът може да не е съществувал -> това не е грешка
		if !autostartEnabled() {
			return nil
		}
		return fmt.Errorf("reg delete: %v: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// ---- скрипт за инсталиране ----

// installScript връща .bat файл, който отваря портовете, настройва
// автостарта и стартира програмата. Генерира се на живо, за да са в него
// правилните портове.
func (a *App) installScript() string {
	cfg := a.cfg.snapshot()
	exe, _ := os.Executable()
	if exe == "" {
		exe = "MultiKVM.exe"
	}
	ports := a.firewallPorts()
	if len(ports) == 0 {
		ports = []int{cfg.Port}
	}
	var ps []string
	for _, p := range ports {
		ps = append(ps, strconv.Itoa(p))
	}
	portList := strings.Join(ps, ",")

	var b strings.Builder
	b.WriteString("@echo off\r\n")
	b.WriteString("chcp 65001 >nul\r\n")
	b.WriteString("setlocal\r\n")
	b.WriteString("title MultiKVM - инсталиране\r\n\r\n")
	b.WriteString("echo ============================================\r\n")
	b.WriteString("echo   MultiKVM - отваряне на портове и автостарт\r\n")
	b.WriteString("echo ============================================\r\n")
	b.WriteString("echo.\r\n")
	b.WriteString("echo Портове за отваряне: " + portList + "\r\n")
	b.WriteString("echo.\r\n\r\n")
	b.WriteString("net session >nul 2>&1\r\n")
	b.WriteString("if errorlevel 1 (\r\n")
	b.WriteString("  echo ИЗИСКВА СЕ ПРАВО НА АДМИНИСТРАТОР.\r\n")
	b.WriteString("  echo Стартирай този файл като Администратор и опитай пак.\r\n")
	b.WriteString("  pause\r\n")
	b.WriteString("  exit /b 1\r\n")
	b.WriteString(")\r\n\r\n")
	b.WriteString("echo [1/3] Изтриване на старото правило...\r\n")
	b.WriteString("netsh advfirewall firewall delete rule name=\"" + fwRuleName + "\" >nul 2>&1\r\n")
	b.WriteString("echo [2/3] Отваряне на TCP портовете в защитната стена...\r\n")
	b.WriteString("netsh advfirewall firewall add rule name=\"" + fwRuleName + "\" dir=in action=allow protocol=TCP localport=" + portList + " profile=any enable=yes\r\n")
	b.WriteString("if errorlevel 1 (\r\n")
	b.WriteString("  echo ГРЕШКА при добавяне на правилото!\r\n")
	b.WriteString("  pause\r\n")
	b.WriteString("  exit /b 1\r\n")
	b.WriteString(")\r\n")
	b.WriteString("echo       -> правилото е добавено.\r\n\r\n")
	b.WriteString("echo [3/3] Настройка на автостарт при вход в Windows...\r\n")
	b.WriteString("reg add \"" + runKeyPath + "\" /v \"" + runKeyName + "\" /t REG_SZ /d \"\\\"" + exe + "\\\"\" /f >nul\r\n")
	b.WriteString("echo       -> готов е.\r\n\r\n")
	b.WriteString("echo Стартиране на MultiKVM...\r\n")
	b.WriteString("start \"\" \"" + exe + "\"\r\n")
	b.WriteString("echo.\r\n")
	b.WriteString("echo Готово! Може да затвориш този прозорец.\r\n")
	b.WriteString("pause\r\n")
	b.WriteString("endlocal\r\n")
	return b.String()
}

// startupScript връща .bat, който добавя MultiKVM за автостарт при вход
// в Windows (HKCU\...\Run) и — ако е стартиран като администратор —
// отваря портовете в защитната стена.
func (a *App) startupScript() string {
	cfg := a.cfg.snapshot()
	exe, _ := os.Executable()
	if exe == "" {
		exe = "MultiKVM.exe"
	}
	ports := a.firewallPorts()
	if len(ports) == 0 {
		ports = []int{cfg.Port}
	}
	var ps []string
	for _, p := range ports {
		ps = append(ps, strconv.Itoa(p))
	}
	portList := strings.Join(ps, ",")

	var b strings.Builder
	b.WriteString("@echo off\r\n")
	b.WriteString("chcp 65001 >nul\r\n")
	b.WriteString("setlocal\r\n")
	b.WriteString("title MultiKVM - автостарт\r\n\r\n")
	b.WriteString("echo ==============================================\r\n")
	b.WriteString("echo   MultiKVM - добавяне при стартиране на Windows\r\n")
	b.WriteString("echo ==============================================\r\n")
	b.WriteString("echo.\r\n\r\n")
	b.WriteString("reg add \"" + runKeyPath + "\" /v \"" + runKeyName + "\" /t REG_SZ /d \"\\\"" + exe + "\\\"\" /f\r\n")
	b.WriteString("if errorlevel 1 (\r\n")
	b.WriteString("  echo ГРЕШКА при запис в регистъра!\r\n")
	b.WriteString("  pause\r\n")
	b.WriteString("  exit /b 1\r\n")
	b.WriteString(")\r\n")
	b.WriteString("echo   -> MultiKVM ще се стартира автоматично при вход.\r\n\r\n")
	b.WriteString("rem Ако искаш и портовете да са отворени в защитната стена,\r\n")
	b.WriteString("rem стартирай този файл като Администратор.\r\n")
	b.WriteString("net session >nul 2>&1\r\n")
	b.WriteString("if not errorlevel 1 (\r\n")
	b.WriteString("  netsh advfirewall firewall delete rule name=\"" + fwRuleName + "\" >nul 2>&1\r\n")
	b.WriteString("  netsh advfirewall firewall add rule name=\"" + fwRuleName + "\" dir=in action=allow protocol=TCP localport=" + portList + " profile=any enable=yes\r\n")
	b.WriteString("  echo   -> портовете " + portList + " са отворени в защитната стена.\r\n")
	b.WriteString(") else (\r\n")
	b.WriteString("  echo   -> портовете НЕ са отворени (стартирай като Администратор).\r\n")
	b.WriteString(")\r\n\r\n")
	b.WriteString("echo Готово!\r\n")
	b.WriteString("pause\r\n")
	b.WriteString("endlocal\r\n")
	return b.String()
}

// ---- helper for process ----

// networkProfile връща името на активния профил на защитната стена
// (Private/Public/Domain) за диагностиката.
func networkProfile() string {
	out, _ := runHidden("netsh", "advfirewall", "show", "currentprofile")
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(l), "profile") || strings.Contains(l, "Profile Settings") {
			return l
		}
	}
	return strings.TrimSpace(out)
}

// ensureFirewallRule се опитва да отвори портовете, ако процесът е администратор.
// Извиква се при стартиране — без UAC, само ако вече сме повишени.
func (a *App) ensureFirewallRule() {
	if !isElevated() {
		return
	}
	ports := a.firewallPorts()
	if len(ports) == 0 {
		return
	}
	st := firewallStatus(ports...)
	if st.RuleExists && st.PortOpen {
		return
	}
	if err := addFirewallRules(ports); err != nil {
		a.logf("⚠️ Неуспешно отваряне на портовете: %v", err)
		return
	}
	a.logf("✅ Защитната стена: отворени TCP портове %v", ports)
}

// waitForFirewallRule чака правилото да се появи (след UAC промпт).
func waitForFirewallRule(ports []int, timeout time.Duration) firewallInfo {
	deadline := time.Now().Add(timeout)
	for {
		st := firewallStatus(ports...)
		if st.RuleExists && st.PortOpen {
			return st
		}
		if time.Now().After(deadline) {
			return st
		}
		time.Sleep(500 * time.Millisecond)
	}
}
