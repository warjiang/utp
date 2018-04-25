package main

import (
	//"github.com/codegangsta/cli"
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
	utpconn, e := utp.DialWithOptions("223.3.98.56:29900", nil, 0, 0)
	//utpconn, e := utp.DialWithOptions("223.3.94.193:29900", nil, 0, 0)
	utpconn.SetDSCP(46) // 0x1010 1110 最高位使用IP保留的1， 其他设置为46
	checkError(e)
	var session *mux.Session
	smuxConfig := mux.DefaultConfig()
	session, e = mux.Client(utpconn, smuxConfig)
	checkError(e)
	addr, e :=  net.ResolveTCPAddr("tcp",":12948")
	checkError(e)
	listener,err := net.ListenTCP("tcp",addr)
	checkError(err)
	//buf := make([]byte,1024)
	for {
		p1, err := listener.AcceptTCP()
		//a := p1.File().(net.IPConn)
		//a,_ := p1.SyscallConn()
		//io.ReadAtLeast(p1,buf,3)
		//fmt.Println(buf[:3])


		//n,_ :=p1.Read(buf)
		//fmt.Println(n,buf[:n])
		//f,_ := p1.File()
		//b := net.IPConn{conn{a}}

		if err != nil {
			log.Fatalln(err)
		}
		go handleProxy(session, p1)
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
