package main

// Уеб UI (само на 127.0.0.1): конфигурация + live статус.

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"time"
)

//go:embed web
var webFS embed.FS

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()

	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))

	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(a.stateJSON())
	})

	mux.HandleFunc("/api/role", func(w http.ResponseWriter, r *http.Request) {
		role := r.FormValue("role")
		if role != "primary" && role != "peer" {
			http.Error(w, "bad role", 400)
			return
		}
		a.cfg.update(func(c *Config) { c.Role = role })
		a.Restart()
		json.NewEncoder(w).Encode(a.stateJSON())
	})

	mux.HandleFunc("/api/name", func(w http.ResponseWriter, r *http.Request) {
		a.cfg.update(func(c *Config) { c.Name = r.FormValue("name") })
		json.NewEncoder(w).Encode(a.stateJSON())
	})

	mux.HandleFunc("/api/port", func(w http.ResponseWriter, r *http.Request) {
		p, err := strconv.Atoi(r.FormValue("port"))
		if err != nil || p < 1 || p > 65535 {
			http.Error(w, "bad port", 400)
			return
		}
		a.cfg.update(func(c *Config) { c.Port = p })
		a.Restart()
		json.NewEncoder(w).Encode(a.stateJSON())
	})

	mux.HandleFunc("/api/peer/add", func(w http.ResponseWriter, r *http.Request) {
		name := r.FormValue("name")
		addr := r.FormValue("addr")
		dir := r.FormValue("dir")
		if addr == "" {
			http.Error(w, "addr required", 400)
			return
		}
		if dir != "left" && dir != "right" {
			dir = "right"
		}
		if name == "" {
			name = addr
		}
		a.cfg.update(func(c *Config) {
			c.Peers = append(c.Peers, Peer{Name: name, Addr: addr, Dir: dir})
		})
		a.syncPeers()
		json.NewEncoder(w).Encode(a.stateJSON())
	})

	mux.HandleFunc("/api/peer/remove", func(w http.ResponseWriter, r *http.Request) {
		i, err := strconv.Atoi(r.FormValue("i"))
		if err != nil {
			http.Error(w, "bad index", 400)
			return
		}
		a.cfg.update(func(c *Config) {
			if i >= 0 && i < len(c.Peers) {
				c.Peers = append(c.Peers[:i], c.Peers[i+1:]...)
			}
		})
		a.syncPeers()
		json.NewEncoder(w).Encode(a.stateJSON())
	})

	mux.HandleFunc("/api/peer/move", func(w http.ResponseWriter, r *http.Request) {
		i, _ := strconv.Atoi(r.FormValue("i"))
		d, _ := strconv.Atoi(r.FormValue("d")) // -1 нагоре, +1 надолу
		a.cfg.update(func(c *Config) {
			j := i + d
			if i >= 0 && i < len(c.Peers) && j >= 0 && j < len(c.Peers) {
				c.Peers[i], c.Peers[j] = c.Peers[j], c.Peers[i]
			}
		})
		a.syncPeers()
		json.NewEncoder(w).Encode(a.stateJSON())
	})

	mux.HandleFunc("/api/toggle", func(w http.ResponseWriter, r *http.Request) {
		a.ToggleEnabled()
		json.NewEncoder(w).Encode(a.stateJSON())
	})

	// Еднократна проверка на връзка към даден адрес (диагностика).
	mux.HandleFunc("/api/testconn", func(w http.ResponseWriter, r *http.Request) {
		addr := r.FormValue("addr")
		start := time.Now()
		conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			a.logf("❌ Тест връзка %s: %v", addr, err)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "err": err.Error()})
			return
		}
		conn.Close()
		a.logf("✅ Тест връзка %s: успешна (%d ms)", addr, time.Since(start).Milliseconds())
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "ms": time.Since(start).Milliseconds()})
	})

	// IP адреси на локалните интерфейси — за да ги въведеш на primary-а.
	mux.HandleFunc("/api/ips", func(w http.ResponseWriter, r *http.Request) {
		port := a.cfg.snapshot().Port
		var ips []string
		for _, ip := range a.localIPs() {
			ips = append(ips, ip+":"+strconv.Itoa(port))
		}
		json.NewEncoder(w).Encode(ips)
	})

	// ---- Защитна стена ----

	// Проверка/добавяне/премахване на правилото в защитната стена.
	// При добавяне, ако процесът не е администратор, се показва UAC промпт.
	mux.HandleFunc("/api/firewall", func(w http.ResponseWriter, r *http.Request) {
		action := r.FormValue("action")
		if action == "" {
			action = "check"
		}
		ports := a.firewallPorts()
		if p := r.FormValue("port"); p != "" {
			if n, err := strconv.Atoi(p); err == nil && n > 0 && n < 65536 {
				ports = []int{n}
			}
		}
		switch action {
		case "add":
			a.logf("Отваряне на портовете %v в защитната стена...", ports)
			if err := addFirewallRules(ports); err != nil {
				a.logf("❌ Защитна стена: %v", err)
				a.invalidateFirewallCache()
				json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
				return
			}
			a.invalidateFirewallCache()
			st := waitForFirewallRule(ports, 10*time.Second)
			a.logf("✅ Защитна стена: портове %v са отворени", ports)
			json.NewEncoder(w).Encode(map[string]any{"ok": st.RuleExists && st.PortOpen, "status": st})
		case "remove":
			if err := removeFirewallRules(); err != nil {
				a.invalidateFirewallCache()
				json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
				return
			}
			a.invalidateFirewallCache()
			a.logf("Правилото за защитната стена е премахнато")
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": firewallStatus(ports...)})
		default:
			json.NewEncoder(w).Encode(a.firewallStatusCached(ports))
		}
	})

	// ---- Сканиране на мрежата за отворения порт ----
	mux.HandleFunc("/api/scan", func(w http.ResponseWriter, r *http.Request) {
		port, _ := strconv.Atoi(r.FormValue("port"))
		if port == 0 {
			port = a.cfg.snapshot().Port
		}
		cidr := r.FormValue("cidr")
		resp := scanNetwork(port, cidr)
		a.logf("Сканиране на мрежата за порт %d: намерени %d (за %.1f сек)", port, len(resp.Hosts), float64(resp.Duration)/1000)
		json.NewEncoder(w).Encode(resp)
	})

	// ---- Диагностика ----
	mux.HandleFunc("/api/diag", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(a.diagReport()))
	})

	// ---- Автостарт ----
	mux.HandleFunc("/api/autostart", func(w http.ResponseWriter, r *http.Request) {
		on := r.FormValue("on") == "1"
		if err := setAutostart(on); err != nil {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error(), "enabled": autostartEnabled()})
			return
		}
		a.logf("Автостарт при вход в Windows: %v", on)
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "enabled": autostartEnabled()})
	})

	// ---- Достъп от мрежата ----
	mux.HandleFunc("/api/lan", func(w http.ResponseWriter, r *http.Request) {
		on := r.FormValue("on") == "1"
		a.cfg.update(func(c *Config) {
			c.LANUI = on
			if on && c.Token == "" {
				c.Token = newToken()
			}
		})
		a.setLAN(on)
		fwOK := true
		if on {
			// портът на уеб панела трябва да е отворит, иначе от другата
			// машина няма да се свърже (ако вече не е отворит — UAC).
			ports := a.firewallPorts()
			st := a.firewallStatusCached(ports)
			if !(st.RuleExists && st.PortOpen) {
				if err := addFirewallRules(ports); err != nil {
					fwOK = false
					a.logf("⚠️ Защитна стена: %v", err)
				} else {
					a.invalidateFirewallCache()
					waitForFirewallRule(ports, 10*time.Second)
					a.logf("✅ Защитна стена: отворени портове %v", ports)
				}
			}
		}
		a.logf("Достъп от мрежата: %v", on)
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "firewall": fwOK, "enabled": on, "url": a.lanURL()})
	})

	// ---- Скриптове за инсталиране ----
	mux.HandleFunc("/api/script/install.bat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="MultiKVM-Install.bat"`)
		w.Write([]byte(a.installScript()))
	})

	mux.HandleFunc("/api/script/startup.bat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="MultiKVM-Startup.bat"`)
		w.Write([]byte(a.startupScript()))
	})

	return withAuth(a, mux)
}

// isLoopbackAddr връща true, ако адресът е локален (127.0.0.1 / ::1).
func isLoopbackAddr(addr string) bool {
	if addr == "" {
		return true
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// withAuth изисква токен за заявки, които идват НЕ от тази машина.
// Локалният достъп (127.0.0.1) работи без токен — иначе потребителят
// трябва да го въвежда всеки път на собствения си компютър.
func withAuth(a *App, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := a.cfg.snapshot()
		if !cfg.LANUI || isLoopbackAddr(r.RemoteAddr) {
			next.ServeHTTP(w, r)
			return
		}
		tok := r.Header.Get("X-Auth-Token")
		if tok == "" {
			tok = r.URL.Query().Get("token")
		}
		if tok == "" || tok != cfg.Token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type peerJSON struct {
	Name      string `json:"name"`
	Addr      string `json:"addr"`
	Dir       string `json:"dir"`
	Connected bool   `json:"connected"`
	Screen    string `json:"screen"`
	RTT       int64  `json:"rtt"`
	Error     string `json:"error"`
}

type stateJSON struct {
	Role    string     `json:"role"`
	Name    string     `json:"name"`
	Port    int        `json:"port"`
	Enabled bool       `json:"enabled"`
	Peers   []peerJSON `json:"peers"`
	Current string     `json:"current"`
	Logs    []string   `json:"logs"`

	// нови полета за панела
	IPs       []string     `json:"ips"`
	Admin     bool         `json:"admin"`
	DPI       string       `json:"dpi"`
	Scale     int          `json:"scale"`
	Screen    string       `json:"screen"`
	Firewall  firewallInfo `json:"firewall"`
	LAN       bool         `json:"lan"`
	LANURL    string       `json:"lanurl"`
	AutoStart bool         `json:"autostart"`
	Version   string       `json:"version"`
	Listener  bool         `json:"listener"`
	RawIn     int64        `json:"rawIn"`
	HookMouse int64        `json:"hookMouse"`
	HookKey   int64        `json:"hookKey"`
}

func (a *App) stateJSON() stateJSON {
	cfg := a.cfg.snapshot()
	st := stateJSON{
		Role:      cfg.Role,
		Name:      cfg.Name,
		Port:      cfg.Port,
		Enabled:   cfg.Enabled,
		Peers:     []peerJSON{},
		Logs:      a.LogLines(),
		IPs:       a.localIPs(),
		Admin:     isElevated(),
		DPI:       dpiAwarenessMode,
		Scale:     systemScale(),
		Screen:    localArea().String(),
		Firewall:  a.firewallStatusCached(a.firewallPorts()),
		LAN:       cfg.LANUI,
		LANURL:    a.lanURL(),
		AutoStart: autostartEnabled(),
		Version:   appVersion,
		RawIn:     a.ctl.rawIn.Load(),
		HookMouse: a.ctl.hookMouse.Load(),
		HookKey:   a.ctl.hookKey.Load(),
	}
	a.peerLnMu.Lock()
	st.Listener = a.peerLn != nil
	a.peerLnMu.Unlock()
	a.peerMu.Lock()
	for i, p := range cfg.Peers {
		pj := peerJSON{Name: p.Name, Addr: p.Addr, Dir: p.Dir}
		if i < len(a.peers) {
			ps := a.peers[i]
			pj.Connected = ps.connected.Load()
			pj.RTT = ps.rttMS.Load()
			if e, ok := ps.lastErr.Load().(string); ok {
				pj.Error = e
			}
			w, h := int(ps.screenW.Load()), int(ps.screenH.Load())
			if w > 0 {
				pj.Screen = strconv.Itoa(w) + "×" + strconv.Itoa(h)
			}
		}
		st.Peers = append(st.Peers, pj)
	}
	a.peerMu.Unlock()

	if cfg.Role == "primary" {
		name, _ := a.ctl.CurrentScreen()
		st.Current = name
	} else {
		st.Current = cfg.Name
	}
	return st
}
