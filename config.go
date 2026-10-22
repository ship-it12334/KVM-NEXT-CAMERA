package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type Peer struct {
	Name string `json:"name"`
	Addr string `json:"addr"` // ip:port
	Dir  string `json:"dir"`  // "left" или "right" спрямо този компютър
}

type Config struct {
	path    string
	Role    string `json:"role"` // "primary" или "peer"
	Name    string `json:"name"`
	Port    int    `json:"port"`    // TCP порт на peer-а (слушателя)
	UIPort  int    `json:"uiport"`  // уеб UI порт (само localhost)
	Peers   []Peer `json:"peers"`   // пиърове, добавени от primary
	Enabled bool   `json:"enabled"` // споделянето включено/изключено

	// LANUI отваря уеб панела за достъп от другите машики в мрежата.
	// Изисква Token (генерира се автоматично), защото панелът дава
	// достъп до мишката/клавиатурата на тази машина.
	LANUI bool   `json:"lanui"`
	Token string `json:"token"`

	AutoStart   bool `json:"autostart"`   // стартира при вход в Windows
	OpenBrowser bool `json:"openbrowser"` // отваря панела в браузър при старт
}

// cfgMu пази всички достъпи до Config ( самият Config не носи lock, за да
// може да се копира безопасно за API отговорите ).
var cfgMu sync.Mutex

// configPath връща пътя на конфигурацията. Може да се смени с
// флага -config (полезно за тест с две копии на една машина).
var configFileOverride string

func configPath() string {
	if configFileOverride != "" {
		p, err := filepath.Abs(configFileOverride)
		if err == nil {
			os.MkdirAll(filepath.Dir(p), 0o755)
			return p
		}
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	dir = filepath.Join(dir, "MultiKVM")
	os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "config.json")
}

func defaultConfig() *Config {
	name, _ := os.Hostname()
	if name == "" {
		name = "PC"
	}
	return &Config{
		path:        configPath(),
		Role:        "peer",
		Name:        name,
		Port:        9876,
		UIPort:      9877,
		Peers:       []Peer{},
		OpenBrowser: true,
	}
}

func loadConfig() *Config {
	c := defaultConfig()
	data, err := os.ReadFile(c.path)
	if err != nil {
		return c
	}
	json.Unmarshal(data, c)
	c.path = configPath()
	if c.Port == 0 {
		c.Port = 9876
	}
	if c.UIPort == 0 {
		c.UIPort = 9877
	}
	if c.Role != "primary" && c.Role != "peer" {
		c.Role = "peer"
	}
	if c.Name == "" {
		name, _ := os.Hostname()
		c.Name = name
	}
	if c.LANUI && c.Token == "" {
		c.Token = newToken()
	}
	return c
}

// newToken генерира случайен hex токен за достъп от мрежата.
func newToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// fallback — използваме времето
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

func (c *Config) save() {
	cfgMu.Lock()
	data, _ := json.MarshalIndent(c, "", "  ")
	cfgMu.Unlock()
	os.WriteFile(c.path, data, 0o644)
}

// snapshot връща копие на конфига без заключване (за UI/API).
func (c *Config) snapshot() Config {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	cp := *c
	cp.Peers = append([]Peer(nil), c.Peers...)
	return cp
}

func (c *Config) update(fn func(*Config)) {
	cfgMu.Lock()
	fn(c)
	data, _ := json.MarshalIndent(c, "", "  ")
	cfgMu.Unlock()
	os.WriteFile(c.path, data, 0o644)
}
