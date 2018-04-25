package utp

import (
	"sync"
	"net"
	"github.com/pkg/errors"
	"sync/atomic"
	"time"
	"golang.org/x/net/ipv4"
	"crypto/rand"
	"encoding/binary"
	"log"
	"fmt"
)

const (
	mtuLimit      = 1500
	acceptBacklog = 128
	qlen          = 128
)

const (
	errBrokenPipe       = "broken pipe"
	errInvalidOperation = "invalid operation"
)

var (
	// 全局缓存
	xmitBuf sync.Pool
)

func init() {
	xmitBuf.New = func() interface{} {
		return make([]byte, mtuLimit)
	}
}

type (
	errTimeout struct {
		error
	}

	sessionKey struct {
		addr   string
		convID uint32
	}

	UDPSession struct {
		updaterIdx int            // record slice index in updater
		conn       net.PacketConn // the underlying packet connection
		utp        *UTP           // UTP protocol
		l          *Listener      // point to the Listener if it's accepted by Listener
		block      BlockCrypt     // block encryption
		recvbuf    []byte         // utp receiving is based on packets recvbuf turns packets into stream
		bufptr     []byte         // buff pointer
		ext        []byte         // extended output buffer(with header)

		// settings
		remote     net.Addr  // remote peer address
		rd         time.Time // read deadline
		wd         time.Time // write deadline
		headerSize int       // the overall header size added before KCP frame
		ackNoDelay bool      // send ack immediately for each incoming packet
		writeDelay bool      // delay kcp.flush() for Write() for bulk transfer
		dup        int       // duplicate udp packets

		// notifications
		die          chan struct{} // notify session has Closed
		chReadEvent  chan struct{} // notify Read() can be called without blocking
		chWriteEvent chan struct{} // notify Write() can be called without blocking
		chErrorEvent chan error    // notify Read() have an error
		isClosed     bool          // flag the session has Closed
		mu           sync.Mutex
	}

	// Listener defines a server listening for connections
	Listener struct {
		block           BlockCrypt                 // block encryption
		dataShards      int                        // FEC data shard
		parityShards    int                        // FEC parity shard
		conn            net.PacketConn             // the underlying packet connection
		sessions        map[sessionKey]*UDPSession // all sessions accepted by this Listener
		chAccepts       chan *UDPSession           // Listen() backlog
		chSessionClosed chan sessionKey            // session close queue
		headerSize      int                        // the overall header size added before KCP frame
		die             chan struct{}              // notify the listener has closed
		rd              atomic.Value               // read deadline for Accept()
		wd              atomic.Value
		queryCallback   func(data []byte) // query消息回调函数
		finishCallback  func(data []byte) // finish消息回调函数
	}

	// connectedUDPConn is a wrapper for net.UDPConn which converts WriteTo syscalls
	// to Write syscalls that are 4 times faster on some OS'es. This should only be
	// used for connections that were produced by a net.Dial* call.
	connectedUDPConn struct{ *net.UDPConn }

	// incoming packet
	inPacket struct {
		from net.Addr
		data []byte
	}

	setReadBuffer interface {
		SetReadBuffer(bytes int) error
	}

	setWriteBuffer interface {
		SetWriteBuffer(bytes int) error
	}
)

// 实现error接口
func (errTimeout) Timeout() bool   { return true }
func (errTimeout) Temporary() bool { return true }
func (errTimeout) Error() string   { return "i/o timeout" }

// 实现net.Conn接口（Read、Write、Close、LocalAddr、RemoteAddr、SetDeadline、SetReadDeadline、SetWriteDeadline）
func (s *UDPSession) Read(b []byte) (n int, err error) {
	for {
		s.mu.Lock()
		if len(s.bufptr) > 0 {
			n = copy(b, s.bufptr)
			s.bufptr = s.bufptr[n:]
			s.mu.Unlock()
			return n, nil
		}

		if s.isClosed {
			s.mu.Unlock()
			return 0, errors.New(errBrokenPipe)
		}

		if size := s.utp.GetPeekSize(); size > 0 { // peek data size from kcp
			atomic.AddUint64(&DefaultSnmp.BytesReceived, uint64(size))
			if len(b) >= size {
				s.utp.Receive(b)
				s.mu.Unlock()
				return size, nil
			}
			if cap(s.recvbuf) < size {
				s.recvbuf = make([]byte, size)
			}
			s.recvbuf = s.recvbuf[:size]
			s.utp.Receive(s.recvbuf)
			n = copy(b, s.recvbuf)
			s.bufptr = s.recvbuf[n:]
			s.mu.Unlock()
			return n, nil
		}

		// read deadline
		var timeout *time.Timer
		var c <-chan time.Time
		if !s.rd.IsZero() {
			if time.Now().After(s.rd) {
				s.mu.Unlock()
				return 0, errTimeout{}
			}

			delay := s.rd.Sub(time.Now())
			timeout = time.NewTimer(delay)
			c = timeout.C
		}
		s.mu.Unlock()

		// wait for read event or timeout
		select {
		case <-s.chReadEvent:
		case <-c:
		case <-s.die:
		case err = <-s.chErrorEvent:
			if timeout != nil {
				timeout.Stop()
			}
			return n, err
		}

		if timeout != nil {
			timeout.Stop()
		}
	}
}
func (s *UDPSession) Write(b []byte) (n int, err error) {
	for {
		s.mu.Lock()
		//if s.isClosed {
		//	s.mu.Unlock()
		//	return 0, errors.New(errBrokenPipe)
		//}

		// api flow control
		if s.utp.GetWaitingSegment() < int(s.utp.sendWnd) {
			n = len(b)
			//if n != 8 {
			//	log.Println("length: ",len(b))
			//	log.Println(b)
			//}
			for {
				if len(b) <= int(s.utp.mss) {
					s.utp.Send(b)
					break
				} else {
					s.utp.Send(b[:s.utp.mss])
					b = b[s.utp.mss:]
				}
			}

			if !s.writeDelay {
				s.utp.flush(false)
			}
			s.mu.Unlock()
			atomic.AddUint64(&DefaultSnmp.BytesSent, uint64(n))
			return n, nil
		}

		// write deadline
		var timeout *time.Timer
		var c <-chan time.Time
		if !s.wd.IsZero() {
			if time.Now().After(s.wd) {
				s.mu.Unlock()
				return 0, errTimeout{}
			}
			delay := s.wd.Sub(time.Now())
			timeout = time.NewTimer(delay)
			c = timeout.C
		}
		s.mu.Unlock()

		// wait for write event or timeout
		select {
		case <-s.chWriteEvent:
		case <-c:
		case <-s.die:
		}

		if timeout != nil {
			timeout.Stop()
		}
	}
}
func (s *UDPSession) Close() error {
	updater.removeSession(s)
	if s.l != nil { // notify listener
		s.l.closeSession(sessionKey{
			addr:   s.remote.String(),
			convID: 1, //tmp test
		})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isClosed {
		return errors.New(errBrokenPipe)
	}
	close(s.die)
	s.isClosed = true
	atomic.AddUint64(&DefaultSnmp.CurrEstab, ^uint64(0))
	if s.l == nil { // client socket close
		return s.conn.Close()
	}
	return nil
}
func (s *UDPSession) LocalAddr() net.Addr  { return s.conn.LocalAddr() }
func (s *UDPSession) RemoteAddr() net.Addr { return s.remote }
func (s *UDPSession) SetDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rd = t
	s.wd = t
	return nil
}
func (s *UDPSession) SetReadDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rd = t
	return nil
}
func (s *UDPSession) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wd = t
	return nil
}
func (s *UDPSession) SetWriteDelay(delay bool) {
	// 开启writeDelay会等到下个更新周期才发送数据
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeDelay = delay
}
func (s *UDPSession) SetWindowSize(sndWnd, rcvWnd int) {
	// 设定窗口大小(发送窗口和接收窗口)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.utp.SetWndSize(sndWnd, rcvWnd)
}
func (s *UDPSession) SetMtu(mtu int) bool {
	// SetMtu sets the maximum transmission unit(not including UDP header)
	if mtu > mtuLimit {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.utp.SetMtu(mtu - s.headerSize)
	return true
}
func (s *UDPSession) SetStreamMode(enable bool) {
	// SetStreamMode toggles the stream mode on/off
	s.mu.Lock()
	defer s.mu.Unlock()
	if enable {
		s.utp.stream = 1
	} else {
		s.utp.stream = 0
	}
}
func (s *UDPSession) SetACKNoDelay(nodelay bool) {
	// SetACKNoDelay changes ack flush option, set true to flush ack immediately,
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ackNoDelay = nodelay
}
func (s *UDPSession) SetDUP(dup int) {
	// SetDUP duplicates udp packets for kcp output, for testing purpose only
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dup = dup
}
func (s *UDPSession) SetNoDelay(nodelay, interval, resend, nc int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.utp.NoDelay(nodelay, interval, resend, nc)
}
func (s *UDPSession) output(buf []byte) {
	// output pipeline entry
	// steps for output data processing:
	// 0. Header extends
	// 1. FEC
	// 2. CRC32
	// 3. Encryption
	// 4. WriteTo kernel
	ext := buf
	nbytes := 0
	npkts := 0
	//fmt.Println("send",buf[(UTP_OVERHEAD+8):])
	//fmt.Println("send",buf)
	for i := 0; i < s.dup+1; i++ {
		if n, err := s.conn.WriteTo(ext, s.remote); err == nil {
			nbytes += n
			npkts++
		}
	}
	atomic.AddUint64(&DefaultSnmp.OutPkts, uint64(npkts))
	atomic.AddUint64(&DefaultSnmp.OutBytes, uint64(nbytes))
}
func (s *UDPSession) SetDSCP(dscp int) error {
	// SetDSCP sets the 6bit DSCP field of IP header, no effect if it's accepted from Listener
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.l == nil {
		if nc, ok := s.conn.(*connectedUDPConn); ok {
			//return ipv4.NewConn(nc.UDPConn).SetTOS(dscp << 2)
			x:= ipv4.NewConn(nc.UDPConn)
			x.SetTOS(3)
			fmt.Println(x.TOS())
			return nil
		} else if nc, ok := s.conn.(net.Conn); ok {
			return ipv4.NewConn(nc).SetTOS(3)
		}
	}
	return errors.New(errInvalidOperation)
}
func (s *UDPSession) notifyReadEvent() {
	select {
	case s.chReadEvent <- struct{}{}:
	default:
	}
}
func (s *UDPSession) notifyWriteEvent() {
	select {
	case s.chWriteEvent <- struct{}{}:
	default:
	}
}
func (s *UDPSession) update() (interval time.Duration) {
	s.mu.Lock()
	s.utp.flush(false)
	if s.utp.GetWaitingSegment() < int(s.utp.sendWnd) {
		s.notifyWriteEvent()
	}
	interval = time.Duration(s.utp.interval) * time.Millisecond
	s.mu.Unlock()
	return
}
func (s *UDPSession) utpInput(data []byte) {
	var utpInErrors uint64
	s.mu.Lock()
	if ret := s.utp.Input(data, s.ackNoDelay); ret != 0 {
		utpInErrors++
	}
	if n := s.utp.GetPeekSize(); n > 0 {
		s.notifyReadEvent()
	}
	s.mu.Unlock()
	atomic.AddUint64(&DefaultSnmp.InPkts, 1)
	atomic.AddUint64(&DefaultSnmp.InBytes, uint64(len(data)))
	if utpInErrors > 0 {
		atomic.AddUint64(&DefaultSnmp.UTPInErrors, utpInErrors)
	}
}
func (s *UDPSession) receiver(ch chan<- []byte) {
	for {
		data := xmitBuf.Get().([]byte)[:mtuLimit]
		if n, _, err := s.conn.ReadFrom(data); err == nil && n >= s.headerSize+UTP_OVERHEAD {
			select {
			case ch <- data[:n]:
			case <-s.die:
				return
			}
		} else if err != nil {
			s.chErrorEvent <- err
			return
		} else {
			atomic.AddUint64(&DefaultSnmp.InErrs, 1)
		}
	}
}
func (s *UDPSession) readLoop() {
	// read loop for client session
	chPacket := make(chan []byte, qlen)
	go s.receiver(chPacket)
	for {
		select {
		case data := <-chPacket:
			raw := data
			s.utpInput(data)
			xmitBuf.Put(raw)
		case <-s.die:
			return
		}
	}

}
func (s *UDPSession) SetReadBuffer(bytes int) error {
	// SetReadBuffer sets the socket read buffer, no effect if it's accepted from Listener
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.l == nil {
		if nc, ok := s.conn.(setReadBuffer); ok {
			return nc.SetReadBuffer(bytes)
		}
	}
	return errors.New(errInvalidOperation)
}
func (s *UDPSession) SetWriteBuffer(bytes int) error {
	// SetWriteBuffer sets the socket write buffer, no effect if it's accepted from Listener
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.l == nil {
		if nc, ok := s.conn.(setWriteBuffer); ok {
			return nc.SetWriteBuffer(bytes)
		}
	}
	return errors.New(errInvalidOperation)
}
func newUDPSession(conv uint32,
	dataShards, parityShards int, l *Listener, conn net.PacketConn, remote net.Addr, block BlockCrypt) *UDPSession {
	sess := new(UDPSession)
	sess.die = make(chan struct{})
	sess.chReadEvent = make(chan struct{}, 1)
	sess.chWriteEvent = make(chan struct{}, 1)
	sess.chErrorEvent = make(chan error, 1)
	sess.remote = remote
	sess.conn = conn
	sess.l = l
	sess.block = block
	sess.recvbuf = make([]byte, mtuLimit)
	if sess.headerSize > 0 {
		sess.ext = make([]byte, mtuLimit)
	}
	sess.utp = NewUTP(func(buf []byte, size int) {
		if size >= UTP_OVERHEAD {
			sess.output(buf[:size])
		}
	})
	sess.utp.SetMtu(UTP_MTU_DEF - sess.headerSize)
	blacklist.add(remote.String(), conv)
	updater.addSession(sess)
	if sess.l == nil {
		go sess.readLoop()
		atomic.AddUint64(&DefaultSnmp.ActiveOpens, 1)
	} else {
		atomic.AddUint64(&DefaultSnmp.PassiveOpens, 1)
	}
	currestab := atomic.AddUint64(&DefaultSnmp.CurrEstab, 1)
	maxconn := atomic.LoadUint64(&DefaultSnmp.MaxConn)
	if currestab > maxconn {
		atomic.CompareAndSwapUint64(&DefaultSnmp.MaxConn, maxconn, currestab)
	}
	return sess
}

// Accept implements the Accept method in the Listener interface; it waits for the next call and returns a generic Conn.
func (l *Listener) Accept() (net.Conn, error) {
	return l.AcceptUTP()
}
func (l *Listener) AcceptUTP() (*UDPSession, error) {
	var timeout <-chan time.Time
	if tdeadline, ok := l.rd.Load().(time.Time); ok && !tdeadline.IsZero() {
		timeout = time.After(tdeadline.Sub(time.Now()))
	}

	select {
	case <-timeout:
		return nil, &errTimeout{}
	case c := <-l.chAccepts:
		return c, nil
	case <-l.die:
		return nil, errors.New(errBrokenPipe)
	}
}
func (l *Listener) SetDeadline(t time.Time) error {
	l.SetReadDeadline(t)
	l.SetWriteDeadline(t)
	return nil
}
func (l *Listener) SetReadDeadline(t time.Time) error {
	l.rd.Store(t)
	return nil
}
func (l *Listener) SetWriteDeadline(t time.Time) error {
	l.wd.Store(t)
	return nil
}
func (l *Listener) Close() error {
	close(l.die)
	return l.conn.Close()
}
func (l *Listener) closeSession(key sessionKey) bool {
	select {
	case l.chSessionClosed <- key:
		return true
	case <-l.die:
		return false
	}
}
func (l *Listener) SetReadBuffer(bytes int) error {
	// SetReadBuffer sets the socket read buffer for the Listener
	if nc, ok := l.conn.(setReadBuffer); ok {
		return nc.SetReadBuffer(bytes)
	}
	return errors.New(errInvalidOperation)
}
func (l *Listener) SetWriteBuffer(bytes int) error {
	// SetWriteBuffer sets the socket write buffer for the Listener
	if nc, ok := l.conn.(setWriteBuffer); ok {
		return nc.SetWriteBuffer(bytes)
	}
	return errors.New(errInvalidOperation)
}
func (l *Listener) Addr() net.Addr { return l.conn.LocalAddr() }
func (l *Listener) receiver(ch chan<- inPacket) {
	for {
		data := xmitBuf.Get().([]byte)[:mtuLimit]
		if n, from, err := l.conn.ReadFrom(data); err == nil && n >= l.headerSize+UTP_OVERHEAD {
			select {
			case ch <- inPacket{from, data[:n]}:
			case <-l.die:
				return
			}
		} else if err != nil {
			return
		} else {
			atomic.AddUint64(&DefaultSnmp.InErrs, 1)
		}
	}
}
func (l *Listener) monitor() {
	// cache last session
	var lastKey sessionKey
	var lastSession *UDPSession
	chPacket := make(chan inPacket, qlen)
	go l.receiver(chPacket)
	for {
		select {
		case p := <-chPacket:
			raw := p.data
			data := p.data
			from := p.from
			conv := uint32(1)
			key := sessionKey{
				addr:   from.String(),
				convID: conv,
			}
			var s *UDPSession
			var ok bool

			if key == lastKey {
				s, ok = lastSession, true
			} else if s, ok = l.sessions[key]; ok {
				lastSession = s
				lastKey = key
			}

			if !ok { // new session
				log.Println("new session from remote")
				if !blacklist.has(from.String(), conv) && len(l.chAccepts) < cap(l.chAccepts) && len(l.sessions) < 4096 { // do not let new session overwhelm accept queue and connection count
					ses := newUDPSession(conv, l.dataShards, l.parityShards, l, l.conn, from, l.block)
					ses.utpInput(data)
					l.sessions[key] = ses
					l.chAccepts <- ses
				}
			} else {
				s.utpInput(data)
			}
			xmitBuf.Put(raw)
		case key := <-l.chSessionClosed:
			if key == lastKey {
				lastKey = sessionKey{}
			}
			delete(l.sessions, key)
		case <-l.die:
			return
		}
	}

}
func (l *Listener) SetDSCP(dscp int) error {
	// SetDSCP sets the 6bit DSCP field of IP header
	if nc, ok := l.conn.(net.Conn); ok {
		//return ipv4.NewConn(nc).SetTOS(dscp << 2)
		return ipv4.NewConn(nc).SetTOS(dscp<<2 + 2)
	}
	return errors.New(errInvalidOperation)
}

// WriteTo redirects all writes to the Write syscall, which is 4 times faster.
func (c *connectedUDPConn) WriteTo(b []byte, addr net.Addr) (int, error) { return c.Write(b) }
func Listen(laddr string) (net.Listener, error) {
	return ListenWithOptions(laddr, nil, 0, 0)
}
func ListenWithOptions(laddr string, block BlockCrypt, dataShards, parityShards int) (*Listener, error) {
	udpaddr, err := net.ResolveUDPAddr("udp", laddr)
	if err != nil {
		return nil, errors.Wrap(err, "net.ResolveUDPAddr")
	}
	conn, err := net.ListenUDP("udp", udpaddr)
	if err != nil {
		return nil, errors.Wrap(err, "net.ListenUDP")
	}
	return ServeConn(block, dataShards, parityShards, conn)
}
func ServeConn(block BlockCrypt, dataShards, parityShards int, conn net.PacketConn) (*Listener, error) {
	l := new(Listener)
	l.conn = conn
	l.sessions = make(map[sessionKey]*UDPSession)
	l.chAccepts = make(chan *UDPSession, acceptBacklog)
	l.chSessionClosed = make(chan sessionKey)
	l.die = make(chan struct{})
	l.dataShards = dataShards
	l.parityShards = parityShards
	l.block = block
	go l.monitor()
	return l, nil
}
func ClientConn(raddr string, block BlockCrypt, dataShards, parityShards int, conn net.PacketConn) (*UDPSession, error) {
	udpaddr, err := net.ResolveUDPAddr("udp", raddr)
	if err != nil {
		return nil, errors.Wrap(err, "net.ResolveUDPAddr")
	}
	var convid uint32
	binary.Read(rand.Reader, binary.LittleEndian, &convid)
	return newUDPSession(convid, dataShards, parityShards, nil, conn, udpaddr, block), nil
}
func Dial(raddr string) (net.Conn, error) {
	// Dial connects to the remote address "raddr" on the network "udp"
	return DialWithOptions(raddr, nil, 0, 0)
}
func DialWithOptions(raddr string, block BlockCrypt, dataShards, parityShards int) (*UDPSession, error) {
	// DialWithOptions connects to the remote address "raddr" on the network "udp" with packet encryption
	udpaddr, err := net.ResolveUDPAddr("udp", raddr)
	if err != nil {
		return nil, errors.Wrap(err, "net.ResolveUDPAddr")
	}

	udpconn, err := net.DialUDP("udp", nil, udpaddr)
	if err != nil {
		return nil, errors.Wrap(err, "net.DialUDP")
	}

	return ClientConn(raddr, block, dataShards, parityShards, &connectedUDPConn{udpconn})
}
