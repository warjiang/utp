package mux

import (
	"sync"
	"sync/atomic"
	"io"
	"github.com/pkg/errors"
	"encoding/binary"
	"net"
	"log"
	"strconv"
	"time"
)

const (
	errBrokenPipe        = "broken pipe"
	errInvalidProtocol   = "invalid protocol version"
	errGoAway            = "stream id overflows, should start a new connection"
	defaultAcceptBacklog = 1024
)

const (
	socksVer5       = 5
	socksCmdConnect = 1
	idVer           = 0 // version从0开始，读取1个字节
	idNmethod       = 1 // METHODS从1开始，读取1个字节
)

var (
	errAddrType      = errors.New("socks addr type not supported")
	errVer           = errors.New("socks version not supported")
	errMethod        = errors.New("socks only support 1 method now")
	errAuthExtraData = errors.New("socks authentication get extra data")
	errReqExtraData  = errors.New("socks request get extra data")
	errCmd           = errors.New("socks command not supported")
)

// 在UTP连接的底层基础上，往上层定义session的复用
type (
	Session struct {
		conn               io.ReadWriteCloser     // 实现io.ReadWriteCloser的ServeConn（Listener对象）或者NewConn（UTPSession对象）
		config             *Config                // 复用参数配置
		nextStreamID       uint32                 // 下一条流的ID
		nextStreamIDLock   sync.Mutex             // 流ID自增锁
		bucket             int32                  // token bucket 令牌通
		bucketNotify       chan struct{}          // used for waiting for tokens 令牌通channel
		streams            map[uint32]*Stream     // 所有接入的TCP流按照{streamid:stream}的形式存放在map中
		streamLock         sync.Mutex             // stream map锁
		die                chan struct{}          // 标记session已经关闭
		dieLock            sync.Mutex             // die channel锁
		chAccepts          chan *Stream           // 接受stream的channel
		chStreamAndTCPConn chan *StreamAndTCPConn // 同时返回stream和conn的channel
		dataReady          int32                  // 标记数据已经准备好了
		goAway             int32                  // 标记stream id已经耗尽（超过uint32的最大值）
		deadline           atomic.Value
		writes             chan writeRequest
	}
	// 封装发送数据
	writeRequest struct {
		frame  Frame
		result chan writeResult
	}
	// 封装发送数据的返回结果
	writeResult struct {
		n   int
		err error
	}

	StreamAndTCPConn struct {
		stream *Stream
		conn   net.Conn
	}
)

// 创建一个session
func newSession(config *Config, conn io.ReadWriteCloser, client bool) *Session {
	s := new(Session)                                      // 创建session对象
	s.die = make(chan struct{})                            // 创建die channel用于标记session已经关闭
	s.conn = conn                                          // 底层的UTP连接对象一般为ServeConn或者NewConn返回对象
	s.config = config                                      // 会话复用的配置
	s.streams = make(map[uint32]*Stream)                   // stream map
	s.chAccepts = make(chan *Stream, defaultAcceptBacklog) // 最大可接受的stream数量
	s.chStreamAndTCPConn = make(chan *StreamAndTCPConn, defaultAcceptBacklog)
	s.bucket = int32(config.MaxReceiveBuffer) // 令牌桶
	s.bucketNotify = make(chan struct{}, 1)   // 令牌桶通知的channel
	s.writes = make(chan writeRequest)        // 写数据的channel
	if client { // 客户端的streamID从1开始，依次+2
		s.nextStreamID = 1
	} else { // 服务端的streamID从0开始，依次+2
		s.nextStreamID = 0
	}
	go s.recvLoop() // 循环接收
	go s.sendLoop() // 循环发送
	return s        // 返回session对象
}
func handShake(conn net.Conn) (err error) {
	// client发起请求，代理负责与client之间完成三次握手
	/**
	  +----+----------+----------+
	  |VER | NMETHODS | METHODS  |
	  +----+----------+----------+
	  | 1  |    1     | 1 to 255 |
	  +----+----------+----------+
	*/
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
	_, err = conn.Write([]byte{socksVer5, 0})
	return
}
func getRequest(conn net.Conn) (rawaddr []byte, err error) {
	// 从客户端请求中解析出目标地址
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
		host = string(buf[idDm0 : idDm0+buf[idDmLen]])
	}
	port := binary.BigEndian.Uint16(buf[reqLen-2 : reqLen])
	host = net.JoinHostPort(host, strconv.Itoa(int(port)))
	log.Println("target host is", host)
	// 欺骗客户端已经建立连接
	_, err = conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x08, 0x43})
	return
}
func getTargetRequest(buf []byte, n int) (host string, err error) {
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
func (s *Session) OpenStream(conn *net.TCPConn) (*Stream, error) {
	// 为conn构建对应的stream对象
	if s.IsClosed() {
		return nil, errors.New(errBrokenPipe)
	}
	// 产生下一个stream id，客户端依次为1、3、5、7、9；服务端则为0、2、4、6、8
	// 生成stream id时要保证线程安全使用s.nextStreamIDLock.Lock()与s.nextStreamIDLock.Unlock()来保证同一时间只有一个go routine
	// 能够生成stream id
	s.nextStreamIDLock.Lock()
	if s.goAway > 0 {
		s.nextStreamIDLock.Unlock()
		return nil, errors.New(errGoAway)
	}
	s.nextStreamID += 2
	sid := s.nextStreamID
	if sid == sid%2 {
		// 能够满足sid == sid % 2只有两种情况，一种是sid=0,另一种是sid=1
		// sid从0or1开始，当达到最大值的时候，再+2，则直接变回0or1 此时表示stream-id已经耗尽
		s.goAway = 1
		s.nextStreamIDLock.Unlock()
		return nil, errors.New(errGoAway)
	}
	s.nextStreamIDLock.Unlock()

	// 根据stream id，stream，缓冲区大小以及session对象创建stream
	stream := newStream(sid, s.config.MaxFrameSize, s)
	if err := handShake(conn); err != nil {
		log.Println("socks handshake error:", err)
		return nil, err
	}
	rawaddr, _ := getRequest(conn)
	log.Println("raw address is: ", rawaddr)

	// 新stream，发送UTP.QUERY（payload部分为remote地址）消息通知给对端
	frame := newFrame(sid, cmdSYN, 0)
	frame.data = rawaddr
	if _, err := s.writeFrame(frame); err != nil {
		return nil, errors.Wrap(err, "write query frame")
	}
	s.streamLock.Lock()
	s.streams[sid] = stream
	s.streamLock.Unlock()
	return stream, nil
}
func (s *Session) streamClosed(sid uint32) {
	// 通知session某条stream已经关闭，关闭时需要清除stream中的buffer对象
	s.streamLock.Lock()
	s.streams[sid].recycleTokens() // 归还使用的buffer
	delete(s.streams, sid)         // 从stream map中移除sid对应的stream
	s.streamLock.Unlock()
}
func (s *Session) keepalive() {}
func (s *Session) recvLoop() {
	buffer := make([]byte, (1<<16)+headerSize)
	for {
		if f, err := s.readFrame(buffer); err == nil { // 从UTP连接中读取一个frame
			atomic.StoreInt32(&s.dataReady, 1) // 通知数据已经准备好了
			switch f.cmd {
			case cmdNOP: // nop的时候没有操作
			case cmdSYN: // [QUERY]syn的时候，首先判断stream是否存在，不存在则创建stream，并加入到session的stream map中
				if _, ok := s.streams[f.sid]; !ok {
					s.streamLock.Lock()
					//目的地址 f.data
					r := new(StreamAndTCPConn)
					host, e := getTargetRequest(f.data, len(f.data))
					p2, e := net.DialTimeout("tcp", host, 5*time.Second)
					if e != nil {
						log.Fatal("cannot make connection with remote")
						s.chStreamAndTCPConn <- nil
						return
					}
					stream := newStream(f.sid, s.config.MaxFrameSize, s)
					s.streams[f.sid] = stream
					r.stream = stream
					r.conn = p2
					select {
					//case s.chAccepts <- stream: // 将stream送到s.chAccepts，当调用AcceptStream的时候，将stream返回出去
					case s.chStreamAndTCPConn <- r: // 将stream送到s.chAccepts，当调用AcceptStream的时候，将stream返回出去
					case <-s.die:
					}
					s.streamLock.Unlock()
				}

			case cmdFIN: // [FINISH]fin的时候，
				s.streamLock.Lock()
				if stream, ok := s.streams[f.sid]; ok {
					stream.markRST()         // 将该条流标记为RESET
					stream.notifyReadEvent() // 继续执行recvLoop
				}
				s.streamLock.Unlock()
			case cmdPSH: // 读取到普通的数据报文时
				s.streamLock.Lock()
				if stream, ok := s.streams[f.sid]; ok {
					//atomic.AddInt32(&s.bucket, -int32(len(f.data))) // 更新s.bucket，即更新令牌桶
					stream.pushBytes(f.data) // 将frame送到stream的buffer中
					stream.notifyReadEvent() // 继续执行recvLoop
				}
				s.streamLock.Unlock()
			default:
				s.Close()
				return
			}
		} else {
			s.Close()
			return
		}

	}
}
func (s *Session) sendLoop() {
	buf := make([]byte, (1<<16)+headerSize) // frame中data的length为2B=16bit最大值为2^16=1<<16.缓冲区的大小应该为headerSize+len(data)
	for {
		select {
		case <-s.die:
			return
		case request := <-s.writes:
			binary.LittleEndian.PutUint32(buf[0:], request.frame.sid)               // 往buf中写入stream id [0,1,2,3]
			buf[4] = request.frame.cmd                                              // 往buf中写入cmd类型    [4]
			binary.LittleEndian.PutUint16(buf[5:], request.frame.rwnd)              // 往buf中写入rwnd      [5,6]
			binary.LittleEndian.PutUint16(buf[7:], uint16(len(request.frame.data))) // 往buf中写入数据长度   [7,8]
			copy(buf[headerSize:], request.frame.data)                              // 往buf中写入数据部分
			n, err := s.conn.Write(buf[:headerSize+len(request.frame.data)])        // 调用底层的UTP连接负责写入数据
			//log.Println(buf[headerSize:headerSize+len(request.frame.data)])
			//log.Println(buf[headerSize+len(request.frame.data)])
			log.Println(buf[:headerSize+len(request.frame.data)])
			n -= headerSize // 减去frame头部长度，剩下表示实际写入多少数据
			if n < 0 {
				n = 0
			}
			result := writeResult{
				n:   n,
				err: err,
			}
			request.result <- result // 调用底层UTP连接写入数据成功后，将写入结果通过request.result回发回去
			close(request.result)    // 关闭request.result这条channel
		}
	}
}
func (s *Session) readFrame(buffer []byte) (f Frame, err error) {
	// 从底层UTP连接中读取数据，构造frame对象并返回出来
	if _, err := io.ReadFull(s.conn, buffer[:headerSize]); err != nil {
		return f, errors.Wrap(err, "readFrame")
	}
	// 将buffer强制转换成rawHeader，方便做进一步解析
	dec := rawHeader(buffer)
	// 封装frame对象
	f.sid = dec.StreamID()
	f.cmd = dec.Cmd()
	f.rwnd = dec.Rwnd()
	if length := dec.Length(); length > 0 {
		if _, err := io.ReadFull(s.conn, buffer[headerSize:headerSize+length]); err != nil {
			return f, errors.Wrap(err, "readFrame")
		}
		f.data = buffer[headerSize : headerSize+length] // 将数据包保存到frame对象中的data部分
	}
	return f, nil
}
func (s *Session) writeFrame(f Frame) (n int, err error) {
	// 将frame写入s.write，并阻塞等待writeResult在写入成功的情况下返回
	// 构造writeRequest,result部分是一个writeResult
	req := writeRequest{
		frame:  f,
		result: make(chan writeResult, 1),
	}
	select {
	case <-s.die:
		return 0, errors.New(errBrokenPipe)
	case s.writes <- req: // 将请求报文发送到s.writes
	}
	result := <-req.result // 等待writeResult
	return result.n, result.err
}
func (s *Session) IsClosed() bool {
	// 检查底层的链接是否关闭，默认返回false
	select {
	case <-s.die:
		return true
	default:
		return false
	}
}
func (s *Session) Close() (err error) {
	// 关闭所有的会话和stream
	s.dieLock.Lock()
	select {
	case <-s.die:
		s.dieLock.Unlock()
		return errors.New(errBrokenPipe)
	default:
		close(s.die) // 关闭session的die channel
		s.dieLock.Unlock()
		s.streamLock.Lock()
		for k := range s.streams { //  遍历所有的stream
			s.streams[k].sessionClose() // 对每个stream执行sessionClose()
		}
		s.streamLock.Unlock()
		return s.conn.Close()
	}
}
func (s *Session) AcceptStream() (*StreamAndTCPConn, error) {
	var deadline <-chan time.Time
	if d, ok := s.deadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}
	select {
	//case stream := <-s.chAccepts:
	//	return stream, nil
	case r := <-s.chStreamAndTCPConn:
		return r, nil
	case <-deadline:
		return nil, errTimeout
	case <-s.die:
		return nil, errors.New(errBrokenPipe)
	}
}

func (s *StreamAndTCPConn) GetStream() *Stream {
	return s.stream
}
func (s *StreamAndTCPConn) GetConn() net.Conn {
	return s.conn
}
