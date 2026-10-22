package main

// Wire protocol: newline-delimited JSON over TCP.
// primary -> peer  : hello{name}, move{x,y}, button{b,down}, wheel{delta}, key{vk,scan,up,ext}, ping
// peer  -> primary : hello{name,w,h}, pong
//
// Ръкопожатие: primary-ът изпраща hello веднага след свързване, peer-ът
// отговаря със своя hello (име + размер на екрана). Преди първия hello
// peer-ът не приема команди и не сменя активната връзка — така сканерът
// на мрежата може да попита {"t":"probe"} без да изkick-ва истинския
// primary. На "probe" peer-ът отговаря със hello, но остава пасивен.

type msg struct {
	T     string `json:"t"`
	Name  string `json:"name,omitempty"`
	W     int    `json:"w,omitempty"`
	H     int    `json:"h,omitempty"`
	X     int    `json:"x,omitempty"`
	Y     int    `json:"y,omitempty"`
	B     int    `json:"b,omitempty"`
	Down  bool   `json:"down,omitempty"`
	Delta int    `json:"delta,omitempty"`
	VK    int    `json:"vk,omitempty"`
	Scan  int    `json:"scan,omitempty"`
	Up    bool   `json:"up,omitempty"`
	Ext   bool   `json:"ext,omitempty"`
}

// Mouse buttons (wire values)
const (
	btnLeft   = 1
	btnMiddle = 2
	btnRight  = 3
	btnX1     = 4
	btnX2     = 5
)
