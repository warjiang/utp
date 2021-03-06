package main

import (
	"encoding/json"
	"os"
)

// Config for client
type Config struct {
	LocalAddr    string `json:"localaddr"`
	RemoteAddr   string `json:"remoteaddr"`
	MTU          int    `json:"mtu"`
	SndWnd       int    `json:"sndwnd"`
	RcvWnd       int    `json:"rcvwnd"`
	AckNodelay   bool   `json:"acknodelay"`
	NoDelay      int    `json:"nodelay"`
	Interval     int    `json:"interval"`
	NoComp       bool   `json:"nocomp"`
	Resend       int    `json:"resend"`
	NoCongestion int    `json:"nc"`
	SockBuf      int    `json:"sockbuf"`
	KeepAlive    int    `json:"keepalive"`
	SnmpLog      string `json:"snmplog"`
	SnmpPeriod   int    `json:"snmpperiod"`
}

func parseJSONConfig(config *Config, path string) error {
	file, err := os.Open(path) // For read access.
	if err != nil {
		return err
	}
	defer file.Close()

	return json.NewDecoder(file).Decode(config)
}
