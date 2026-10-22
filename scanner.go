package main

// Сканиране на локалната мрежа за отворени портове и откриване на
// MultiKVM peer-и. Използва се от уеб панела, когато се добавя нов компютър.

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	scanDialTimeout = 400 * time.Millisecond
	scanProbeWait   = 1200 * time.Millisecond
	scanMaxHosts    = 1024 // safety cap per subnet
	scanWorkers     = 192
)

// scanResult е резултат за един адрес.
type scanResult struct {
	IP       string `json:"ip"`
	Port     int    `json:"port"`
	Open     bool   `json:"open"`
	Latency  int64  `json:"latency"`
	Name     string `json:"name"`
	MultiKVM bool   `json:"multiKvm"`
	PeerName string `json:"peerName"`
	Screen   string `json:"screen"`
	Error    string `json:"error"`
}

// scanResponse е отговорът на /api/scan.
type scanResponse struct {
	Port      int          `json:"port"`
	Subnets   []string     `json:"subnets"`
	Hosts     []scanResult `json:"hosts"`
	Scanned   int          `json:"scanned"`
	Duration  int64        `json:"durationMs"`
	Truncated bool         `json:"truncated"`
	Error     string       `json:"error"`
}

// localSubnets връща мрежите (CIDR), към които има свързани интерфейси.
func localSubnets() []*net.IPNet {
	var out []*net.IPNet
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			out = append(out, ipnet)
		}
	}
	return out
}

// hostRange връща адресите за сканиране от дадената мрежа.
func hostRange(n *net.IPNet) ([]string, bool) {
	ip4 := n.IP.To4()
	if ip4 == nil {
		return nil, false
	}
	ones, bits := n.Mask.Size()
	if bits != 32 {
		return nil, false
	}
	total := 1 << uint(bits-ones)
	if total <= 2 {
		return nil, false
	}
	truncated := false
	if total-2 > scanMaxHosts {
		total = scanMaxHosts + 2
		truncated = true
	}
	base := uint32(ip4[0])<<24 | uint32(ip4[1])<<16 | uint32(ip4[2])<<8 | uint32(ip4[3])
	mask := uint32(0xFFFFFFFF)
	if ones > 0 {
		mask = ^uint32(0) << uint(bits-ones)
	} else {
		mask = 0
	}
	base &= mask
	var out []string
	for i := 1; i < total-1; i++ {
		v := base + uint32(i)
		out = append(out, net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).String())
	}
	return out, truncated
}

// probeHost проверява дали портът е отворен и дали зад него стои MultiKVM.
func probeHost(ip string, port int) scanResult {
	res := scanResult{IP: ip, Port: port}
	start := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(port)), scanDialTimeout)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer conn.Close()
	res.Open = true
	res.Latency = time.Since(start).Milliseconds()

	// Питаме дали насрежда е MultiKVM peer (без да поемем ролята на primary).
	conn.SetDeadline(time.Now().Add(scanProbeWait))
	if b, err := json.Marshal(msg{T: "probe"}); err == nil {
		conn.Write(append(b, '\n'))
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err == nil && n > 0 {
		var m msg
		if json.Unmarshal(firstJSONLine(buf[:n]), &m) == nil && m.T == "hello" {
			res.MultiKVM = true
			res.PeerName = m.Name
			if m.W > 0 && m.H > 0 {
				res.Screen = fmt.Sprintf("%dx%d", m.W, m.H)
			}
		}
	}

	// обратно DNS (име на компютъра) — ако не се намери, остава празно
	if names, err := net.LookupAddr(ip); err == nil && len(names) > 0 {
		res.Name = names[0]
	}
	return res
}

func firstJSONLine(b []byte) []byte {
	for i, c := range b {
		if c == '\n' {
			return b[:i]
		}
	}
	return b
}

// scanNetwork сканира локалните мрежи за отворен порт.
func scanNetwork(port int, cidr string) scanResponse {
	resp := scanResponse{Port: port, Hosts: []scanResult{}}
	if port <= 0 || port > 65535 {
		resp.Error = "невалиден порт"
		return resp
	}
	start := time.Now()

	var nets []*net.IPNet
	if cidr != "" {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			resp.Error = "невалидна мрежа: " + err.Error()
			return resp
		}
		nets = append(nets, n)
	} else {
		nets = localSubnets()
	}
	if len(nets) == 0 {
		resp.Error = "няма активни мрежови интерфейси"
		return resp
	}

	// събираме адресите (без дубликати)
	seen := map[string]bool{}
	var ips []string
	for _, n := range nets {
		resp.Subnets = append(resp.Subnets, n.String())
		list, trunc := hostRange(n)
		if trunc {
			resp.Truncated = true
		}
		for _, ip := range list {
			if !seen[ip] {
				seen[ip] = true
				ips = append(ips, ip)
			}
		}
	}
	resp.Scanned = len(ips)

	// паралелно сканиране
	ch := make(chan string, len(ips))
	for _, ip := range ips {
		ch <- ip
	}
	close(ch)

	var wg sync.WaitGroup
	var mu sync.Mutex
	results := []scanResult{}
	sem := make(chan struct{}, scanWorkers)
	for w := 0; w < scanWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ip := range ch {
				sem <- struct{}{}
				r := probeHost(ip, port)
				<-sem
				if r.Open {
					mu.Lock()
					results = append(results, r)
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool {
		if results[i].MultiKVM != results[j].MultiKVM {
			return results[i].MultiKVM
		}
		return results[i].Latency < results[j].Latency
	})
	resp.Hosts = results
	resp.Duration = time.Since(start).Milliseconds()
	return resp
}
