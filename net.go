package main

// Мрежов слой: всеки peer слуша TCP; primary-ът държи една връзка към всеки peer.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	connectRetryDelay = 3 * time.Second
	writeTimeout      = 2 * time.Second
)

// peerState е състоянието на една връзка към peer (гледката на primary).
type peerState struct {
	mu        sync.Mutex
	conn      net.Conn
	sendCh    chan msg
	connected atomic.Bool
	screenW   atomic.Int32
	screenH   atomic.Int32
	rttMS     atomic.Int64
	pingSent  atomic.Int64
	lastErr   atomic.Value // string
	quit      chan struct{}
	quitOnce  sync.Once
	errLogged bool // вече сме логвали неуспешния опит (за да не спамим)
}

// ---- peer listener (роля peer) ----

func (a *App) startPeerListener() error {
	cfg := a.cfg.snapshot()
	addr := fmt.Sprintf(":%d", cfg.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	a.peerLnMu.Lock()
	a.peerLn = ln
	a.peerLnMu.Unlock()
	a.logf("Слушам за връзки на порт %d", cfg.Port)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go a.handlePrimaryConn(conn)
		}
	}()
	return nil
}

func (a *App) stopPeerListener() {
	a.peerLnMu.Lock()
	if a.peerLn != nil {
		a.peerLn.Close()
		a.peerLn = nil
	}
	a.peerLnMu.Unlock()
}

// handlePrimaryConn обработва входяща връзка от primary (режим peer).
// Връзката се "активира" едва след hello от клиента — така сканерът
// (probe) не може да прекъсне работещата връзка с истинския primary.
func (a *App) handlePrimaryConn(conn net.Conn) {
	cfg := a.cfg.snapshot()
	w, h := virtualScreenSize()
	active := false

	defer func() {
		conn.Close()
		if active {
			a.primaryConnMu.Lock()
			if a.primaryConn == conn {
				a.primaryConn = nil
			}
			a.primaryConnMu.Unlock()
			a.logf("Primary връзката е прекъсната")
		}
	}()

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var m msg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		switch m.T {
		case "hello":
			if active {
				continue
			}
			active = true
			a.primaryConnMu.Lock()
			old := a.primaryConn
			a.primaryConn = conn
			a.primaryConnMu.Unlock()
			if old != nil && old != conn {
				old.Close()
			}
			a.logf("Primary свързан: %s", conn.RemoteAddr())
			// отговаряме с името и размера на екрана (физически пиксели)
			writeJSON(conn, msg{T: "hello", Name: cfg.Name, W: w, H: h})
		case "probe":
			// отговаряме за идентификация, но НЕ поемаме ролята на primary
			writeJSON(conn, msg{T: "hello", Name: cfg.Name, W: w, H: h})
		default:
			if !active {
				continue
			}
			a.applyRemoteEvent(m)
		}
	}
}

// inInput бележи входящо събитие; inFail логва (ограничено), когато
// SendInput е блокиран на тази машина.
func (a *App) inInput(kind string, n int64) {
	if n <= 6 || n%50 == 0 {
		a.logf("DBG recv%s#%d", kind, n)
	}
}

func (a *App) inFail(kind string) {
	if n := a.inBad.Add(1); n <= 6 || n%50 == 0 {
		a.logf("⚠️ %s: SendInput върна 0 (#%d) — инжектирането е блокирано (UIPI/elevated процес с фокус?)", kind, n)
	}
}

func (a *App) applyRemoteEvent(m msg) {
	switch m.T {
	case "move":
		// координатите са относителни спрямо виртуалния екран на peer-а
		org := virtualScreenRect()
		setCursorPos(int32(org.X+m.X), int32(org.Y+m.Y))
	case "button":
		a.inInput("Btn", a.inBtn.Add(1))
		var f uint32
		switch m.B {
		case btnLeft:
			if m.Down {
				f = mouseeventfLeftDown
			} else {
				f = mouseeventfLeftUp
			}
		case btnRight:
			if m.Down {
				f = mouseeventfRightDown
			} else {
				f = mouseeventfRightUp
			}
		case btnMiddle:
			if m.Down {
				f = mouseeventfMidDown
			} else {
				f = mouseeventfMidUp
			}
		case btnX1, btnX2:
			xd := uint32(1)
			if m.B == btnX2 {
				xd = 2
			}
			if m.Down {
				f = mouseeventfXDown
			} else {
				f = mouseeventfXUp
			}
			if !sendMouse(f, xd, 0, 0) {
				a.inFail("button")
			}
			return
		default:
			a.logf("⚠️ recvBtn: непознат бутон b=%d", m.B)
			return
		}
		if !sendMouse(f, 0, 0, 0) {
			a.inFail("button")
		}
	case "wheel":
		if !sendMouse(mouseeventfWheel, uint32(int32(m.Delta)), 0, 0) {
			a.inFail("wheel")
		}
	case "key":
		if !m.Up {
			a.inInput("Key", a.inKey.Add(1))
		}
		if !sendKey(uint32(m.VK), uint32(m.Scan), m.Up, m.Ext) {
			a.inFail("key")
		}
	case "ping":
		// отговаряме директно
		a.primaryConnMu.Lock()
		if a.primaryConn != nil {
			writeJSON(a.primaryConn, msg{T: "pong"})
		}
		a.primaryConnMu.Unlock()
	}
}

func writeJSON(conn net.Conn, m msg) {
	conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	b = append(b, '\n')
	conn.Write(b)
}

// ---- primary: връзки към пиърове ----

func (a *App) connectPeer(i int) {
	a.peerMu.Lock()
	if i >= len(a.peers) {
		a.peerMu.Unlock()
		return
	}
	ps := a.peers[i]
	a.peerMu.Unlock()

	cfg := a.cfg.snapshot()
	if i >= len(cfg.Peers) {
		return
	}
	addr := cfg.Peers[i].Addr

	for !a.stopping.Load() && !ps.quitClosed() {
		conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			ps.setErr(err.Error())
			if !ps.errLogged {
				a.logf("❌ Неуспешна връзка с %s (%s): %v — повторен опит на всеки %v", ps.name(cfg, i), addr, err, connectRetryDelay)
				ps.errLogged = true
			}
			time.Sleep(connectRetryDelay)
			continue
		}
		ps.errLogged = false
		ps.setErr("")
		ps.setConn(conn)
		ps.connected.Store(true)
		a.logf("✅ Свързан с %s (%s)", ps.name(cfg, i), addr)
		ps.resetSend()

		// ръкопожатие: казваме кой сме, преди да изпращаме команди
		writeJSON(conn, msg{T: "hello", Name: cfg.Name})

		// writer goroutine
		go ps.writer(conn)

		// reader: очакваме hello (с размери), после pong-ове
		ps.reader(conn, a, i)

		ps.connected.Store(false)
		ps.setConn(nil)
		if !ps.quitClosed() {
			a.logf("Връзката с %s е прекъсната — опитвам отново...", ps.name(cfg, i))
			time.Sleep(connectRetryDelay)
		}
	}
}

func (ps *peerState) quitClosed() bool {
	select {
	case <-ps.quit:
		return true
	default:
		return false
	}
}

func (ps *peerState) name(cfg Config, i int) string {
	if i < len(cfg.Peers) && cfg.Peers[i].Name != "" {
		return cfg.Peers[i].Name
	}
	if i < len(cfg.Peers) {
		return cfg.Peers[i].Addr
	}
	return "peer"
}

func (ps *peerState) setErr(s string) {
	ps.lastErr.Store(s)
}

// setConn / getConn пазят връзката под mutex (пише и чете от различни
// goroutine-и -> без това е data race).
func (ps *peerState) setConn(c net.Conn) {
	ps.mu.Lock()
	ps.conn = c
	ps.mu.Unlock()
}

func (ps *peerState) getConn() net.Conn {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.conn
}

func (ps *peerState) closeConn() {
	ps.mu.Lock()
	c := ps.conn
	ps.conn = nil
	ps.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

func (ps *peerState) resetSend() {
	ps.mu.Lock()
	if ps.sendCh != nil {
		close(ps.sendCh)
	}
	ps.sendCh = make(chan msg, 512)
	ps.mu.Unlock()
}

func (ps *peerState) writer(conn net.Conn) {
	ps.mu.Lock()
	ch := ps.sendCh
	ps.mu.Unlock()
	for m := range ch {
		if conn != ps.getConn() {
			return
		}
		writeJSON(conn, m)
	}
}

func (ps *peerState) reader(conn net.Conn, a *App, i int) {
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var m msg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		switch m.T {
		case "hello":
			if m.W > 0 {
				ps.screenW.Store(int32(m.W))
			}
			if m.H > 0 {
				ps.screenH.Store(int32(m.H))
			}
			a.logf("Peer екран: %dx%d", m.W, m.H)
		case "pong":
			ps.rttMS.Store(time.Now().UnixMilli() - ps.pingSent.Load())
		}
	}
}

// sendToPeer изпраща съобщение към peer по индекс (или логва грешка).
func (a *App) sendToPeer(i int, m msg) {
	a.peerMu.Lock()
	if i < 0 || i >= len(a.peers) {
		a.peerMu.Unlock()
		return
	}
	ps := a.peers[i]
	a.peerMu.Unlock()
	if !ps.connected.Load() {
		return
	}
	ps.mu.Lock()
	ch := ps.sendCh
	ps.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- m:
	default: // buffer пълен — drop, за да не блокира hook thread-ът
	}
}

// pingLoop праща ping на всеки 2s за RTT.
func (a *App) pingLoop() {
	for !a.stopping.Load() {
		time.Sleep(2 * time.Second)
		a.peerMu.Lock()
		for _, ps := range a.peers {
			if ps.connected.Load() {
				ps.pingSent.Store(time.Now().UnixMilli())
				ps.mu.Lock()
				ch := ps.sendCh
				ps.mu.Unlock()
				if ch != nil {
					select {
					case ch <- msg{T: "ping"}:
					default:
					}
				}
			}
		}
		a.peerMu.Unlock()
	}
}
