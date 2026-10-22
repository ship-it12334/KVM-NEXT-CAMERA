package main

// App ядро: държи конфига, peer състоянията,лог-а и уеб UI сървъра.

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type App struct {
	cfg *Config

	stopping atomic.Bool

	// peer състояния (гледката на primary)
	peerMu sync.Mutex
	peers  []*peerState

	// peer listener (гледката на peer)
	peerLnMu      sync.Mutex
	peerLn        net.Listener
	primaryConn   net.Conn
	primaryConnMu sync.Mutex

	ctl *Controller

	logMu sync.Mutex
	logs  []string

	// диагностика на входящите събития (peer роля)
	inBtn atomic.Int64
	inKey atomic.Int64
	inBad atomic.Int64

	// диагностика на входящите движения (peer роля)
	inMove   atomic.Int64
	moveFail atomic.Int64
	moveX    atomic.Int32
	moveY    atomic.Int32

	peerScreenMu sync.Mutex
	peerScreens  map[int][2]int

	// допълнителен слушател за достъп от мрежата (LAN режим)
	lanLnMu sync.Mutex
	lanLn   net.Listener

	// кеш на статуса на защитната стена (netsh е бавен subprocess)
	fwMu     sync.Mutex
	fwAt     time.Time
	fwInfo   firewallInfo
	fwCached bool
}

// firewallStatusCached връща статуса на защитната стена, но не стартира
// netsh по-често от веднъж на 3 секунди (панелът обновява състоянието
// на всеки 1,5 сек).
func (a *App) firewallStatusCached(ports []int) firewallInfo {
	a.fwMu.Lock()
	defer a.fwMu.Unlock()
	if a.fwCached && time.Since(a.fwAt) < 3*time.Second && len(a.fwInfo.Ports) == len(ports) {
		return a.fwInfo
	}
	info := firewallStatus(ports...)
	a.fwInfo = info
	a.fwAt = time.Now()
	a.fwCached = true
	return info
}

// invalidateFirewallCache изчиства кеша (след промяна на правилото).
func (a *App) invalidateFirewallCache() {
	a.fwMu.Lock()
	a.fwCached = false
	a.fwMu.Unlock()
}

const maxLog = 120

func NewApp(cfg *Config) *App {
	a := &App{cfg: cfg, peerScreens: map[int][2]int{}}
	a.ctl = NewController(a)
	return a
}

func (a *App) logf(format string, args ...any) {
	line := fmt.Sprintf("%s  %s", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	log.Println(line)
	a.logMu.Lock()
	a.logs = append(a.logs, line)
	if len(a.logs) > maxLog {
		a.logs = a.logs[len(a.logs)-maxLog:]
	}
	a.logMu.Unlock()
}

func (a *App) LogLines() []string {
	a.logMu.Lock()
	defer a.logMu.Unlock()
	out := make([]string, len(a.logs))
	copy(out, a.logs)
	return out
}

func (a *App) syncPeers() {
	cfg := a.cfg.snapshot()
	a.peerMu.Lock()
	// разширяваме/свиваме списъка
	for len(a.peers) < len(cfg.Peers) {
		ps := &peerState{quit: make(chan struct{})}
		a.peers = append(a.peers, ps)
	}
	if len(a.peers) > len(cfg.Peers) {
		for _, ps := range a.peers[len(cfg.Peers):] {
			ps.quitOnce.Do(func() { close(ps.quit) })
			ps.closeConn()
		}
		a.peers = a.peers[:len(cfg.Peers)]
	}
	// стартираме connect-ващи goroutine-и за несвързаните
	for i, ps := range a.peers {
		if !ps.connected.Load() {
			select {
			case <-ps.quit:
			default:
				go a.connectPeer(i)
			}
		}
	}
	a.peerMu.Unlock()
}

func (a *App) peerConnected(i int) bool {
	a.peerMu.Lock()
	defer a.peerMu.Unlock()
	if i < 0 || i >= len(a.peers) {
		return false
	}
	return a.peers[i].connected.Load()
}

func (a *App) peerName(i int) string {
	cfg := a.cfg.snapshot()
	if i < len(cfg.Peers) {
		if cfg.Peers[i].Name != "" {
			return cfg.Peers[i].Name
		}
		return cfg.Peers[i].Addr
	}
	return "peer"
}

func (a *App) peerScreenSize(i int) (int, int) {
	a.peerMu.Lock()
	if i < len(a.peers) && a.peers[i].screenW.Load() > 0 {
		w := int(a.peers[i].screenW.Load())
		h := int(a.peers[i].screenH.Load())
		a.peerMu.Unlock()
		if h <= 0 {
			h = 1080
		}
		return w, h
	}
	a.peerMu.Unlock()
	a.peerScreenMu.Lock()
	s := a.peerScreens[i]
	a.peerScreenMu.Unlock()
	if s[0] > 0 {
		return s[0], s[1]
	}
	return 1920, 1080
}

// ---- стартиране/спиране на ролята ----

func (a *App) StartRole() {
	cfg := a.cfg.snapshot()
	ctl := a.ctl
	ctl.ctlEnabled.Store(cfg.Enabled)
	if cfg.Role == "primary" {
		a.syncPeers()
		go a.pingLoop()
		if cfg.Enabled {
			ctl.Start()
		}
		a.logf("Роля: PRIMARY (споделям мишка/клавиатура към %d пиъра)", len(cfg.Peers))
	} else {
		if err := a.startPeerListener(); err != nil {
			a.logf("Грешка при стартиране на listener: %v", err)
		}
		overlay.setEnabled(true)
		startOverlay()
		a.logf("Роля: PEER — слушам на порт %d", cfg.Port)
	}
}

func (a *App) ToggleEnabled() {
	cur := a.cfg.snapshot().Enabled
	a.cfg.update(func(c *Config) { c.Enabled = !cur })
	a.applyEnabled()
}

func (a *App) applyEnabled() {
	cfg := a.cfg.snapshot()
	if cfg.Role != "primary" {
		return
	}
	if cfg.Enabled {
		a.ctl.ctlEnabled.Store(true)
		if !a.ctl.running() {
			a.ctl.Start()
		}
		a.logf("Споделянето е ВКЛЮЧЕНО (горещ клавиш Ctrl+Alt+S за изкл.)")
	} else {
		a.ctl.ungrab()
		a.ctl.ctlEnabled.Store(false)
		a.logf("Споделянето е ИЗКЛЮЧЕНО")
	}
}

// Restart сменя ролята/настройките и рестартира съответните компоненти.
func (a *App) Restart() {
	// спиране на старото
	a.ctl.Stop()
	a.stopPeerListener()
	a.peerMu.Lock()
	for _, ps := range a.peers {
		select {
		case <-ps.quit:
		default:
			close(ps.quit)
		}
		ps.closeConn()
	}
	a.peers = nil
	a.peerMu.Unlock()
	time.Sleep(100 * time.Millisecond)
	a.StartRole()
}

// localIPs връща IPv4 адресите на активните интерфейси (без loopback).
func (a *App) localIPs() []string {
	var ips []string
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, ad := range addrs {
			if ipnet, ok := ad.(*net.IPNet); ok && ipnet.IP.To4() != nil {
				ips = append(ips, ipnet.IP.String())
			}
		}
	}
	return ips
}

// uiAddr връща адреса, на който слуша основният уеб панел.
// Винаги само 127.0.0.1 — нищо не се показва навън. Достъпът от
// мрежата е отделен слушател (виж setLAN), изискващ токен.
func (a *App) uiAddr() string {
	cfg := a.cfg.snapshot()
	return fmt.Sprintf("127.0.0.1:%d", cfg.UIPort)
}

// setLAN включва/изключва допълнителния слушател за достъп от мрежата.
func (a *App) setLAN(enabled bool) {
	cfg := a.cfg.snapshot()
	addr := fmt.Sprintf("0.0.0.0:%d", cfg.UIPort)

	a.lanLnMu.Lock()
	defer a.lanLnMu.Unlock()

	if !enabled {
		if a.lanLn != nil {
			a.lanLn.Close()
			a.lanLn = nil
			a.logf("Достъпът от мрежата е изключен")
		}
		return
	}
	if a.lanLn != nil {
		return
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		a.logf("⚠️ Не може да се отвори слушател на %s: %v", addr, err)
		return
	}
	a.lanLn = ln
	go func() {
		srv := &http.Server{Handler: a.routes()}
		if err := srv.Serve(ln); err != nil {
			a.logf("LAN слушателя спря: %v", err)
		}
	}()
	a.logf("Достъп от мрежата е включен: %s", a.lanURL())
}

// localURL е адресът за достъп от самата машина.
func (a *App) localURL() string {
	cfg := a.cfg.snapshot()
	return fmt.Sprintf("http://127.0.0.1:%d/", cfg.UIPort)
}

// lanURL е адресът за достъп от други машики (с токен).
func (a *App) lanURL() string {
	cfg := a.cfg.snapshot()
	if !cfg.LANUI {
		return ""
	}
	ip := a.primaryLANIP()
	if ip == "" {
		ip = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%d/?token=%s", ip, cfg.UIPort, cfg.Token)
}

// primaryLANIP връща най-подходящия IPv4 адрес за достъп от мрежата.
// Предпочита адрес в същата подмрежа като на peer-ите, после частен
// адрес (192.168.x / 10.x), накрая първия наличен. Така адресът за
// отваряне на панела от друг компютър е реално достижим.
func (a *App) primaryLANIP() string {
	cfg := a.cfg.snapshot()
	ips := a.localIPs()
	if len(ips) == 0 {
		return ""
	}
	// 1) същата /24 подмрежа с някой от peer-ите
	for _, p := range cfg.Peers {
		host := p.Addr
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if net.ParseIP(host) == nil {
			continue
		}
		for _, my := range ips {
			if sameSubnet24(my, host) {
				return my
			}
		}
	}
	// 2) частен адрес
	for _, my := range ips {
		if strings.HasPrefix(my, "192.168.") || strings.HasPrefix(my, "10.") {
			return my
		}
	}
	return ips[0]
}

// sameSubnet24 връща true, ако двата адреса са в една и съща /24 мрежа.
func sameSubnet24(a, b string) bool {
	pa := strings.Split(a, ".")
	pb := strings.Split(b, ".")
	if len(pa) != 4 || len(pb) != 4 {
		return false
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return false
		}
	}
	return true
}

func main() {
	// Аргументи от командния ред:
	//   -config <файл>   пътят към конфигурацията (за тест с 2 копии)
	//   -role primary|peer
	//   -port <n>        портът на peer-а
	//   -uiport <n>      портът на уеб панела
	//   -name <име>
	//   -nobrowser        не отваря браузър при старт
	flagConfig := flag.String("config", "", "път към конфигурационния файл")
	flagRole := flag.String("role", "", "роля: primary или peer")
	flagPort := flag.Int("port", 0, "TCP порт на peer-а")
	flagUIPort := flag.Int("uiport", 0, "порт на уеб панела")
	flagName := flag.String("name", "", "име на компютъра")
	flagNoBrowser := flag.Bool("nobrowser", false, "не отваря браузър при старт")
	flag.Parse()

	if *flagConfig != "" {
		configFileOverride = *flagConfig
	}

	// Първо DPI awareness — иначе всички екранни координати са
	// виртуализирани и преходът между екрани се чупи при мащабиране.
	dpi := setDpiAwareness()

	// Ако предишен (сривнал се) процес е оставил курсора скрит —
	// ShowCursor броячът е общ за сесията и курсорът остава невидим,
	// макар мишката да работи. Връщаме го при старт.
	if ensureCursorVisible() {
		log.Printf("Курсорът беше скрит от предишен процес — възстановен")
	}

	cfg := loadConfig()
	if *flagRole != "" {
		cfg.Role = *flagRole
	}
	if *flagName != "" {
		cfg.Name = *flagName
	}
	if *flagPort > 0 {
		cfg.Port = *flagPort
	}
	if *flagUIPort > 0 {
		cfg.UIPort = *flagUIPort
	}
	if *flagNoBrowser {
		cfg.OpenBrowser = false
	}
	cfg.save()

	app := NewApp(cfg)
	log.SetOutput(os.Stdout)

	app.logf("MultiKVM стартира (DPI: %s, мащабиране %d%%)", dpi, systemScale())
	if isElevated() {
		app.logf("Администраторски права: да")
	} else {
		app.logf("Администраторски права: НЕ (за отваряне на портовете са нужни)")
	}

	// Ако сме администратори — отваряме портовете автоматично (без UAC).
	app.ensureFirewallRule()

	app.StartRole()

	// Уеб панелът винаги слуша само на 127.0.0.1.
	// При LANUI=true се отваря ДОПЪЛНИТЕЛЕН слушател на 0.0.0.0
	// (изисква токен), без да се налага рестарт на процеса.
	app.setLAN(cfg.LANUI)

	addr := app.uiAddr()
	app.logf("Уеб UI: http://127.0.0.1:%d", cfg.UIPort)
	if cfg.LANUI {
		app.logf("Уеб UI е достъпен и от мрежата: %s", app.lanURL())
	}
	if cfg.OpenBrowser {
		go func() {
			time.Sleep(700 * time.Millisecond)
			openURL(app.localURL())
		}()
	}
	if err := http.ListenAndServe(addr, app.routes()); err != nil {
		app.logf("UI сървърът спря: %v", err)
		os.Exit(1)
	}
}
