package main

import (
	"github.com/codegangsta/cli"
	"os"
	"net"
	"log"
	"time"
	"encoding/csv"
	"fmt"
	"utp/utp"
	//"utp/smux"
	"github.com/pkg/errors"
	"github.com/golang/snappy"
	"io"
	"encoding/binary"
	"strconv"
	"utp/mux"
)

type compStream struct {
	conn net.Conn
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

func main2() {
	app := cli.NewApp()
	app.Name = "STC"
	app.Usage = "client(with SMUX)"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "localaddr,l",
			Value: "local:12948",
			Usage: "utp local address",
		},
		cli.StringFlag{
			Name:  "remoteaddr, r",
			Value: "remote:29900",
			Usage: "utp remote address",
		},
		cli.IntFlag{
			Name:  "mtu",
			Value: 1350,
			Usage: "set maximum transmission unit for UDP packets",
		},
		cli.IntFlag{
			Name:  "sndwnd",
			Value: 128,
			Usage: "set send window size(num of packets)",
		},
		cli.IntFlag{
			Name:  "rcvwnd",
			Value: 512,
			Usage: "set receive window size(num of packets)",
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
		cli.BoolFlag{
			Name:  "nocomp",
			Usage: "disable compression",
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
		cli.StringFlag{
			Name:  "c",
			Value: "", // when the value is not empty, the config path must exists
			Usage: "config from json file, which will override the command from shell",
		},
	}
	app.Action = func(c *cli.Context) error {
		config := Config{}
		config.LocalAddr = c.String("localaddr")   // 代理本地地址
		config.RemoteAddr = c.String("remoteaddr") // 代理对端地址
		config.MTU = c.Int("mtu")                  // UDP报文最大传输单元，默认1350
		config.SndWnd = c.Int("sndwnd")            // 发送窗口大小（发送数据包数量，默认128）
		config.RcvWnd = c.Int("rcvwnd")            // 接收窗口大小（接收数据包数量，默认512）
		config.NoComp = c.Bool("nocomp")           // true关闭压缩
		config.AckNodelay = c.Bool("acknodelay")   // 是否开启acknodelay
		config.NoDelay = c.Int("nodelay")          // 是否启用 nodelay模式，0不启用；1启用
		config.Interval = c.Int("interval")        // 协议内部工作的 interval，单位毫秒，比如 10ms或者 20ms
		config.Resend = c.Int("resend")            // 快速重传模式，默认0关闭，可以设置2（2次ACK跨越将会直接重传）
		config.NoCongestion = c.Int("nc")          // 是否关闭流控，默认是0代表不关闭，1代表关闭
		config.SockBuf = c.Int("sockbuf")          // 缓冲区大小
		config.KeepAlive = c.Int("keepalive")      // keepalive报文时间间隔
		config.SnmpLog = c.String("snmplog")       // 收集snmp的日志的文件
		config.SnmpPeriod = c.Int("snmpperiod")    // snmp收集的时间间隔 (默认60s)

		if c.String("c") != "" {
			err := parseJSONConfig(&config, c.String("c"))
			checkError(err)
		}
		//"normal":
		//config.NoDelay, config.Interval, config.Resend, config.NoCongestion = 0, 40, 2, 1
		//"fast":
		//config.NoDelay, config.Interval, config.Resend, config.NoCongestion = 0, 30, 2, 1
		//"fast2":
		config.NoDelay, config.Interval, config.Resend, config.NoCongestion = 1, 20, 2, 1
		//"fast3":
		//config.NoDelay, config.Interval, config.Resend, config.NoCongestion = 1, 10, 2, 1
		addr, err := net.ResolveTCPAddr("tcp", config.LocalAddr) // 代理服务
		checkError(err)
		listener, err := net.ListenTCP("tcp", addr)
		checkError(err)
		log.Println("listening on:", listener.Addr())
		log.Println("nodelay parameters:", config.NoDelay, config.Interval, config.Resend, config.NoCongestion)
		log.Println("remote address:", config.RemoteAddr)
		log.Println("sndwnd:", config.SndWnd, "rcvwnd:", config.RcvWnd)
		log.Println("compression:", !config.NoComp)
		log.Println("mtu:", config.MTU)
		log.Println("acknodelay:", config.AckNodelay)
		log.Println("sockbuf:", config.SockBuf)
		log.Println("keepalive:", config.KeepAlive)
		log.Println("snmplog:", config.SnmpLog)
		log.Println("snmpperiod:", config.SnmpPeriod)

		smuxConfig := smux.DefaultConfig()
		smuxConfig.MaxReceiveBuffer = config.SockBuf
		smuxConfig.KeepAliveInterval = time.Duration(config.KeepAlive) * time.Second

		numconn := 1
		muxes := make([]struct {
			session *smux.Session
			ttl     time.Time
		}, numconn)

		createConn := func() (*smux.Session, error) {
			utpconn, err := utp.DialWithOptions(config.RemoteAddr, nil, 0, 0)
			if err != nil {
				return nil, errors.Wrap(err, "createConn()")
			}
			utpconn.SetStreamMode(true)
			utpconn.SetWriteDelay(true)
			utpconn.SetNoDelay(config.NoDelay, config.Interval, config.Resend, config.NoCongestion)
			utpconn.SetWindowSize(config.SndWnd, config.RcvWnd)
			utpconn.SetMtu(config.MTU)
			utpconn.SetACKNoDelay(config.AckNodelay)
			if err := utpconn.SetReadBuffer(config.SockBuf); err != nil {
				log.Println("SetReadBuffer:", err)
			}
			if err := utpconn.SetWriteBuffer(config.SockBuf); err != nil {
				log.Println("SetWriteBuffer:", err)
			}

			// stream multiplex
			var session *smux.Session
			if config.NoComp {
				session, err = smux.Client(utpconn, smuxConfig)
			} else {
				session, err = smux.Client(newCompStream(utpconn), smuxConfig)
			}
			if err != nil {
				return nil, errors.Wrap(err, "createConn()")
			}
			log.Println("connection:", utpconn.LocalAddr(), "->", utpconn.RemoteAddr())
			return session, nil
		}

		waitConn := func() *smux.Session {
			for {
				if session, err := createConn(); err == nil {
					return session
				} else {
					log.Println("re-connecting:", err)
					time.Sleep(time.Second)
				}
			}
		}

		for k := range muxes {
			muxes[k].session = waitConn()
			muxes[k].ttl = time.Now().Add(time.Duration(0) * time.Second)
		}

		go snmpLogger(config.SnmpLog, config.SnmpPeriod)

		utpconn, err := utp.DialWithOptions(config.RemoteAddr, nil, 0, 0)
		if err != nil {
			log.Println("createConn() error")
		}
		utpconn.SetStreamMode(true)
		utpconn.SetWriteDelay(true)
		utpconn.SetNoDelay(config.NoDelay, config.Interval, config.Resend, config.NoCongestion)
		utpconn.SetWindowSize(config.SndWnd, config.RcvWnd)
		utpconn.SetMtu(config.MTU)
		utpconn.SetACKNoDelay(config.AckNodelay)
		if err := utpconn.SetReadBuffer(config.SockBuf); err != nil {
			log.Println("SetReadBuffer:", err)
		}
		if err := utpconn.SetWriteBuffer(config.SockBuf); err != nil {
			log.Println("SetWriteBuffer:", err)
		}

		count := 1
		for {
			utpconn.Write([]byte("hello" + string(count)))
			buf := make([]byte, 1024)
			if n, e := utpconn.Read(buf); e == nil && n != 0 {
				log.Println(buf[:n])
			}
			count++
			//p1, err := listener.AcceptTCP()
			//if err != nil {
			//	log.Fatalln(err)
			//}
			//checkError(err)
			//
			//if muxes[0].session.IsClosed() {
			//	//chScavenger <- muxes[idx].session
			//	muxes[0].session = waitConn()
			//	muxes[0].ttl = time.Now().Add(time.Duration(0) * time.Second)
			//}
			//go handleClient(muxes[0].session, p1, false)
		}
		return nil
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

func handleClient(sess *smux.Session, p1 net.Conn, quiet bool) {
	if !quiet {
		log.Println("stream opened")
		defer log.Println("stream closed")
	}
	//defer p1.Close()
	p2, err := sess.OpenStream()
	if err != nil {
		return
	}
	//defer p2.Close()
	var n int
	var e error
	n, e = p2.Write([]byte("abc"))
	log.Println("send abc,res is: ", n, e)
	n, e = p2.Write([]byte("def"))
	log.Println("send def,res is: ", n, e)
	n, e = p2.Write([]byte("ghi"))
	log.Println("send hig,res is: ", n, e)
	//time.Sleep(2*time.Second)
	//n, e = p2.Write([]byte("hello world"))
	buf := make([]byte, 1024)
	n, e = p2.Read(buf)
	if e == nil {
		log.Println("client received ", buf[:n])
	} else {
		log.Println("client error ", e)
	}
	log.Println("client received finished")

	/*
	// handshake with client
	if err = handShake(p1); err != nil {
		log.Println("socks handshake:", err)
		return
	}
	rawaddr, _ := getRequest(p1)
	_, err = p1.Write([]byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x08, 0x43})
	log.Println("raw addr is: ")
	log.Println(rawaddr)
	p2.Write(rawaddr) // type + address
	d := make([]byte, 1024)
	n,_ :=p2.Read(d) // wait for connection established
	log.Println("received ",d[:n])
	// start tunnel
	p1die := make(chan struct{})
	go func() {
		//buf := make([]byte, 1024)
		//n, _ := p2.Read(buf)
		//log.Println("received ",buf[:n])
		io.Copy(p1, p2); //p2 -> p1
		close(p1die)
	}()

	p2die := make(chan struct{})
	go func() {
		//for {
		//	b := make([]byte,1024)
		//	n,_:=p1.Read(b)
		//	if n != 0 {
		//		log.Println(b[:n])
		//		p2.Write(b[:n])
		//	}
		//}
		io.Copy(p2, p1); //p1->p2
		close(p2die)
	}()

	// wait for tunnel termination
	select {
	case <-p1die:
	case <-p2die:
	}
	log.Println("finished")
	*/
}

const (
	socksVer5       = 5
	socksCmdConnect = 1
)

var (
	errAddrType      = errors.New("socks addr type not supported")
	errVer           = errors.New("socks version not supported")
	errMethod        = errors.New("socks only support 1 method now")
	errAuthExtraData = errors.New("socks authentication get extra data")
	errReqExtraData  = errors.New("socks request get extra data")
	errCmd           = errors.New("socks command not supported")
)

func getRequest(conn net.Conn) (rawaddr []byte, err error) {
	/**
	  +----+-----+-------+------+----------+----------+
	  |VER | CMD |  RSV  | ATYP | DST.ADDR | DST.PORT |
	  +----+-----+-------+------+----------+----------+
	  | 1  |  1  | X'00' |  1   | Variable |    2     |
	  +----+-----+-------+------+----------+----------+
	*/
	const (
		idVer   = 0
		idCmd   = 1
		idType  = 3 // address type index
		idIP0   = 4 // ip addres start index
		idDmLen = 4 // domain address length index
		idDm0   = 5 // domain address start index

		typeIPv4 = 1 // type is ipv4 address
		typeDm   = 3 // type is domain address
		typeIPv6 = 4 // type is ipv6 address

		lenIPv4   = 3 + 1 + net.IPv4len + 2 // 3(ver+cmd+rsv) + 1addrType + ipv4 + 2port
		lenIPv6   = 3 + 1 + net.IPv6len + 2 // 3(ver+cmd+rsv) + 1addrType + ipv6 + 2port
		lenDmBase = 3 + 1 + 1 + 2           // 3 + 1addrType + 1addrLen + 2port, plus addrLen
	)
	// refer to getRequest in server.go for why set buffer size to 263
	buf := make([]byte, 263)
	//var n int
	// read till we get possible domain length field
	if _, err = io.ReadAtLeast(conn, buf, idDmLen+1); err != nil {
		return
	}

	// check version and cmd
	if buf[idVer] != socksVer5 {
		err = errVer
		return
	}
	if buf[idCmd] != socksCmdConnect {
		err = errCmd
		return
	}
	reqLen := -1
	switch buf[idType] {
	case typeIPv4:
		reqLen = lenIPv4
	case typeIPv6:
		reqLen = lenIPv6
	case typeDm:
		reqLen = int(buf[idDmLen]) + lenDmBase
	default:
		err = errAddrType
		return
	}

	rawaddr = buf[idType:reqLen]
	var host string
	switch buf[idType] {
	case typeIPv4:
		host = net.IP(buf[idIP0:idIP0+net.IPv4len]).String()
	case typeIPv6:
		host = net.IP(buf[idIP0:idIP0+net.IPv6len]).String()
	case typeDm:
		//ipAddr,_ := net.ResolveIPAddr("ip",string(buf[idDm0 : idDm0+buf[idDmLen]]))
		//host = ipAddr.IP
		host = string(buf[idDm0 : idDm0+buf[idDmLen]])
	}
	port := binary.BigEndian.Uint16(buf[reqLen-2 : reqLen])
	host = net.JoinHostPort(host, strconv.Itoa(int(port)))
	log.Println("target host is", host)

	//port = binary.BigEndian.Uint16(buf[reqLen-2 : reqLen])
	//host = net.JoinHostPort(host, strconv.Itoa(int(port)))

	return
}

func handShake(conn net.Conn) (err error) {
	log.Println("accept connection")
	/**
	  +----+----------+----------+
	  |VER | NMETHODS | METHODS  |
	  +----+----------+----------+
	  | 1  |    1     | 1 to 255 |
	  +----+----------+----------+
	*/
	const (
		idVer     = 0
		idNmethod = 1
	)
	buf := make([]byte, 258)

	if _, err = io.ReadAtLeast(conn, buf, idNmethod+1); err != nil {
		return
	}
	if buf[idVer] != socksVer5 {
		return errVer
	}

	/**
	  +----+--------+
	  |VER | METHOD |
	  +----+--------+
	  | 1  |   1    |
	  +----+--------+
	*/
	//if msgLen == n{}
	_, err = conn.Write([]byte{socksVer5, 0})
	return

	//var n int
	//nmethod := int(buf[idNmethod])
	//msgLen := nmethod + 2
}

func main3() {
	conn, e := utp.DialWithOptions("127.0.0.1:29900", nil, 0, 0)
	checkError(e)
	addr, err := net.ResolveTCPAddr("tcp", ":12948") // 代理服务
	checkError(err)
	listener, err := net.ListenTCP("tcp", addr)
	checkError(err)
	p1, err := listener.AcceptTCP()
	if err != nil {
		log.Fatalln(err)
	}
	if err = handShake(p1); err != nil {
		log.Println("socks handshake:", err)
		return
	}
	rawaddr, _ := getRequest(p1)
	_, err = p1.Write([]byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x08, 0x43})
	conn.Write(rawaddr)
	d := make([]byte, 1024)
	conn.Read(d)

	p1die := make(chan struct{})
	go func() {
		io.Copy(conn, p1)
		close(p1die)
	}()

	p2die := make(chan struct{})
	go func() {
		io.Copy(p1, conn)
		close(p2die)
	}()
	select {
	case <-p1die:
	case <-p2die:
	}
	log.Println("finished")
	//for{
	//	//conn.Write([]byte("hello "+string(count)))
	//	buf := make([]byte, 1024)
	//	n,e:=conn.Read(buf)
	//	if e == nil && n!=0 {
	//		log.Println(buf[:n])
	//	}
	//	//count++
	//}
}

func main() {
	// 1. 创建underlay的utp连接
	// 2. 使用mux封装utp连接，为进一步的复用做准备
	//utpconn, e := utp.DialWithOptions("127.0.0.1:29900", nil, 0, 0)
	//utpconn, e := utp.DialWithOptions("192.168.97.204:29900", nil, 0, 0)
	utpconn, e := utp.DialWithOptions("223.3.70.214:29900", nil, 0, 0)
	utpconn.SetDSCP(46) // 0x1010 1110 最高位使用IP保留的1， 其他设置为46
	checkError(e)
	var session *mux.Session
	smuxConfig := mux.DefaultConfig()
	session, e = mux.Client(utpconn, smuxConfig)
	checkError(e)
	/*
	addr, e := net.ResolveTCPAddr("tcp", ":12948") // 域内代理
	checkError(e)
	listener, err := net.ListenTCP("tcp", addr)
	checkError(err)
	*/
	netaddr, err := net.ResolveIPAddr("ip4", ":12948")
	checkError(err)
	conn, err := net.ListenIP("ip4:tcp", netaddr)
	for {
		buf := make([]byte, 1024)
		numRead, raddr, err := conn.ReadFrom(buf)
		if err != nil {
			log.Fatalf("ReadFrom: %s\n", err)
		}
		log.Printf("numRead:%d raddr:%s", numRead, raddr)
		/*
		if raddr.String() != remoteAddress {
			// this is not the packet we are looking for
			continue
		}
		*/

		/*
		p1, err := listener.AcceptTCP()
		if err != nil {
			log.Fatalln(err)
		}
		go handleProxy(session, p1)
		*/
	}

}

func handleProxy(sess *mux.Session, p1 *net.TCPConn) {
	defer p1.Close()
	p2, err := sess.OpenStream(p1)
	if err != nil {
		return
	}
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
