package main

import (
	"github.com/urfave/cli"
	"utp/utp"
	"log"
	"os"
	"time"
	"encoding/csv"
	"fmt"
	"net"
	"github.com/golang/snappy"
	"io"
	"github.com/xtaci/smux"
	"encoding/binary"
	"strconv"
	"utp/mux"
)

const (
	idType  = 0 // address type index
	idIP0   = 1 // ip addres start index
	idDmLen = 1 // domain address length index
	idDm0   = 2 // domain address start index

	typeIPv4 = 1 // type is ipv4 address
	typeDm   = 3 // type is domain address
	typeIPv6 = 4 // type is ipv6 address

	lenIPv4     = net.IPv4len + 2 // ipv4 + 2port
	lenIPv6     = net.IPv6len + 2 // ipv6 + 2port
	lenDmBase   = 2               // 1addrLen + 2port, plus addrLen
	lenHmacSha1 = 10
)

// 流压缩类
type compStream struct {
	conn net.Conn // UTP连接
	w    *snappy.Writer
	r    *snappy.Reader
}

func (c *compStream) Read(p []byte) (n int, err error) {
	return c.r.Read(p)
}
func (c *compStream) Write(p []byte) (n int, err error) {
	n, err = c.w.Write(p)
	err = c.w.Flush()
	return n, err
}
func (c *compStream) Close() error {
	return c.conn.Close()
}
func newCompStream(conn net.Conn) *compStream {
	c := new(compStream)
	c.conn = conn
	c.w = snappy.NewBufferedWriter(conn)
	c.r = snappy.NewReader(conn)
	return c
}

func main1() {
	app := cli.NewApp()
	app.Name = "STC"
	app.Usage = "server(with SMUX)"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "listen,l",
			Value: ":29900",
			Usage: "kcp server listen address",
		},
		cli.StringFlag{
			Name:  "target, t",
			Value: "127.0.0.1:12948",
			Usage: "target server address",
		},
		cli.IntFlag{
			Name:  "mtu",
			Value: 1350,
			Usage: "set maximum transmission unit for UDP packets",
		},
		cli.IntFlag{
			Name:  "sndwnd",
			Value: 1024,
			Usage: "set send window size(num of packets)",
		},
		cli.IntFlag{
			Name:  "rcvwnd",
			Value: 1024,
			Usage: "set receive window size(num of packets)",
		},
		cli.BoolFlag{
			Name:  "nocomp",
			Usage: "disable compression",
		},
		cli.BoolFlag{
			Name:   "acknodelay",
			Usage:  "flush ack immediately when a packet is received",
			Hidden: true,
		},
		cli.IntFlag{
			Name:   "nodelay",
			Value:  0,
			Hidden: true,
		},
		cli.IntFlag{
			Name:   "interval",
			Value:  50,
			Hidden: true,
		},
		cli.IntFlag{
			Name:   "resend",
			Value:  0,
			Hidden: true,
		},
		cli.IntFlag{
			Name:   "nc",
			Value:  0,
			Hidden: true,
		},
		cli.IntFlag{
			Name:   "sockbuf",
			Value:  4194304, // 4MB
			Hidden: true,
		},
		cli.IntFlag{
			Name:   "keepalive",
			Value:  10, // nat keepalive interval in seconds
			Hidden: true,
		},
		cli.StringFlag{
			Name:  "snmplog",
			Value: "",
			Usage: "collect snmp to file, aware of timeformat in golang, like: ./snmp-20060102.log",
		},
		cli.IntFlag{
			Name:  "snmpperiod",
			Value: 60,
			Usage: "snmp collect period, in seconds",
		},
		cli.BoolFlag{
			Name:  "pprof",
			Usage: "start profiling server on :6060",
		},
		cli.StringFlag{
			Name:  "c",
			Value: "", // when the value is not empty, the config path must exists
			Usage: "config from json file, which will override the command from shell",
		},
	}
	app.Action = func(c *cli.Context) {
		config := Config{}
		config.Listen = c.String("listen")
		config.Target = c.String("target")
		config.MTU = c.Int("mtu")
		config.SndWnd = c.Int("sndwnd")
		config.RcvWnd = c.Int("rcvwnd")
		config.NoComp = c.Bool("nocomp")
		config.AckNodelay = c.Bool("acknodelay")
		config.NoDelay = c.Int("nodelay")
		config.Interval = c.Int("interval")
		config.Resend = c.Int("resend")
		config.NoCongestion = c.Int("nc")
		config.SockBuf = c.Int("sockbuf")
		config.KeepAlive = c.Int("keepalive")
		config.SnmpLog = c.String("snmplog")
		config.SnmpPeriod = c.Int("snmpperiod")
		config.Pprof = c.Bool("pprof")
		if c.String("c") != "" {
			err := parseJSONConfig(&config, c.String("c"))
			checkError(err)
		}
		//normal
		//config.NoDelay, config.Interval, config.Resend, config.NoCongestion = 0, 40, 2, 1
		//fast
		//config.NoDelay, config.Interval, config.Resend, config.NoCongestion = 0, 30, 2, 1
		//fast2
		//config.NoDelay, config.Interval, config.Resend, config.NoCongestion = 1, 20, 2, 1
		//fast3
		config.NoDelay, config.Interval, config.Resend, config.NoCongestion = 1, 10, 2, 1
		lis, err := utp.ListenWithOptions(config.Listen, nil, 0, 0)
		checkError(err)
		log.Println("listening on:", lis.Addr())
		log.Println("target:", config.Target)
		log.Println("nodelay parameters:", config.NoDelay, config.Interval, config.Resend, config.NoCongestion)
		log.Println("sndwnd:", config.SndWnd, "rcvwnd:", config.RcvWnd)
		log.Println("compression:", !config.NoComp)
		log.Println("mtu:", config.MTU)
		log.Println("acknodelay:", config.AckNodelay)
		log.Println("sockbuf:", config.SockBuf)
		log.Println("keepalive:", config.KeepAlive)
		log.Println("snmplog:", config.SnmpLog)
		log.Println("snmpperiod:", config.SnmpPeriod)
		log.Println("pprof:", config.Pprof)
		if err := lis.SetReadBuffer(config.SockBuf); err != nil {
			log.Println("SetReadBuffer:", err)
		}
		if err := lis.SetWriteBuffer(config.SockBuf); err != nil {
			log.Println("SetWriteBuffer:", err)
		}
		go snmpLogger(config.SnmpLog, config.SnmpPeriod)
		for {
			if conn, err := lis.AcceptUTP(); err == nil {
				log.Println("remote address:", conn.RemoteAddr())
				conn.SetStreamMode(true)
				conn.SetWriteDelay(true)
				conn.SetNoDelay(config.NoDelay, config.Interval, config.Resend, config.NoCongestion)
				conn.SetMtu(config.MTU)
				conn.SetWindowSize(config.SndWnd, config.RcvWnd)
				conn.SetACKNoDelay(config.AckNodelay)
				if config.NoComp {
					go handleMux(conn, &config)
				} else {
					go handleMux(newCompStream(conn), &config)
				}
			}
		}
	}
	app.Run(os.Args)

}

func checkError(err error) {
	if err != nil {
		log.Printf("%+v\n", err)
		os.Exit(-1)
	}
}

func snmpLogger(path string, interval int) {
	if path == "" || interval == 0 {
		return
	}
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			f, err := os.OpenFile(time.Now().Format(path), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
			if err != nil {
				log.Println(err)
				return
			}
			w := csv.NewWriter(f)
			// write header in empty file
			if stat, err := f.Stat(); err == nil && stat.Size() == 0 {
				if err := w.Write(append([]string{"Unix"}, utp.DefaultSnmp.Header()...)); err != nil {
					log.Println(err)
				}
			}
			if err := w.Write(append([]string{fmt.Sprint(time.Now().Unix())}, utp.DefaultSnmp.ToSlice()...)); err != nil {
				log.Println(err)
			}
			utp.DefaultSnmp.Reset()
			w.Flush()
			f.Close()
		}
	}
}

func handleMux(conn io.ReadWriteCloser, config *Config) {
	// stream multiplex
	log.Println("handleMux")
	smuxConfig := smux.DefaultConfig()
	smuxConfig.MaxReceiveBuffer = config.SockBuf
	smuxConfig.KeepAliveInterval = time.Duration(config.KeepAlive) * time.Second
	mux, err := smux.Server(conn, smuxConfig)
	if err != nil {
		log.Println(err)
		return
	}
	defer mux.Close()
	for {
		p1, err := mux.AcceptStream()
		log.Println("received client request")
		if err != nil {
			log.Println(err)
			return
		}
		go handleClient(p1, false)
	}
}

func handleClient(p1 io.ReadWriteCloser, quiet bool) {
	if !quiet {
		log.Println("stream opened")
		defer log.Println("stream closed")
	}
	defer p1.Close()
	dd := make([]byte, 1024)
	n, _ := p1.Read(dd)
	host, e := getRequest(dd[:n], n)
	if e != nil {
		return
	}
	p2, _ := net.DialTimeout("tcp", host, 5*time.Second)
	defer p2.Close()

	// start tunnel
	p1die := make(chan struct{})
	go func() { io.Copy(p1, p2); close(p1die) }()

	p2die := make(chan struct{})
	go func() { io.Copy(p2, p1); close(p2die) }()

	// wait for tunnel termination
	select {
	case <-p1die:
	case <-p2die:
	}
}

func getRequest(buf []byte, n int) (host string, err error) {
	addrType := buf[idType]
	switch addrType {
	case typeIPv4:
		host = net.IP(buf[idIP0:idIP0+net.IPv4len]).String()
	case typeIPv6:
		host = net.IP(buf[idIP0:idIP0+net.IPv6len]).String()
	case typeDm:
		host = string(buf[idDm0 : idDm0+int(buf[idDmLen])])
	}
	port := binary.BigEndian.Uint16(buf[n-2 : n])
	host = net.JoinHostPort(host, strconv.Itoa(int(port)))
	return
}

func main() {
	lis, err := utp.ListenWithOptions(":29900", nil, 0, 0)
	checkError(err)
	lis.SetDSCP(46)
	for {
		if conn, err := lis.AcceptUTP(); err == nil {
			log.Println("remote address:", conn.RemoteAddr())
			go handleProxy(conn)
		}
	}
}

func handleProxy(conn io.ReadWriteCloser) {
	smuxConfig := mux.DefaultConfig()
	m, err := mux.Server(conn, smuxConfig)
	if err != nil {
		log.Println(err)
		return
	}
	defer m.Close()
	for {
		com, err := m.AcceptStream()
		if err != nil {
			log.Println(err)
			return
		}
		if com != nil {
			p1die := make(chan struct{})
			p2die := make(chan struct{})
			p1 := com.GetStream()
			p2 := com.GetConn()

			go func() { io.Copy(p1, p2); close(p2die) }()
			go func() { io.Copy(p2, p1); close(p1die) }()
			select {
			case <-p1die:
			case <-p2die:
			}
		}

	}
}
