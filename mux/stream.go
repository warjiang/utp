package mux

import (
	"bytes"
	"sync"
	"sync/atomic"
	"time"
	"net"
	"io"
	"errors"
)

// 每个stream对应了一个域内的TCP连接
type Stream struct {
	id            uint32        // stream id
	rstflag       int32         // rst 标记，rst对应断开域内TCP连接
	sess          *Session      // session对象的引用
	buffer        bytes.Buffer  // stream维护独立的buffer
	bufferLock    sync.Mutex    // buffer读写锁
	frameSize     int           // frame的大小（字节）
	chReadEvent   chan struct{} // Stream 可读
	die           chan struct{} // Stream 断开
	dieLock       sync.Mutex    // die 锁
	readDeadline  atomic.Value  // 读deadline
	writeDeadline atomic.Value  // 写deadline
}

// Stream 实现了 net.Conn 接口
func (s *Stream) Read(b []byte) (n int, err error) {
	// 实现net.Conn中的Read方法
	if len(b) == 0 {
		select {
		case <-s.die:
			return 0, errors.New(errBrokenPipe)
		default:
			return 0, nil
		}
	}
	var deadline <-chan time.Time // 超时定时器
	if d, ok := s.readDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}
READ:
	s.bufferLock.Lock() // 从s.buffer中阻塞读取数据
	n, err = s.buffer.Read(b)
	s.bufferLock.Unlock()
	if n > 0 { // 如果读取到了数据
		return n, nil
	} else if atomic.LoadInt32(&s.rstflag) == 1 {
		_ = s.Close()
		return 0, io.EOF
	}
	// 在deadline内，如果收到chReadEvent的话，则跳到READ继续尝试读取数据
	select {
	case <-s.chReadEvent:
		goto READ
	case <-deadline:
		return n, errTimeout
	case <-s.die:
		return 0, errors.New(errBrokenPipe)
	}
}
func (s *Stream) Write(b []byte) (n int, err error) {
	// 实现net.Conn的Write方法，不保证一定能够把b内的数据都发送出去，而是采用尽可能的多发
	// 当某个stream的写入时间过长时，将该stream踢出去，出发timeout，不让其继续写入
	var deadline <-chan time.Time
	if d, ok := s.writeDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}
	// 这里的select在stream活跃情况下，默认会通过，如果stream已经关闭，则直接返回broken pipe
	select {
	case <-s.die:
		return 0, errors.New(errBrokenPipe)
	default:
	}
	// 将待发送数据按照进行分割，组装成frames数组
	frames := s.split(b, cmdPSH, s.id)
	sent := 0
	for k := range frames {
		req := writeRequest{
			frame:  frames[k],
			result: make(chan writeResult, 1),
		}
		// 发送frame
		select {
		case s.sess.writes <- req: // 往s.sess.writes写入数据
		case <-s.die: // 如果stream已经关闭，直接返回broken pipe
			return sent, errors.New(errBrokenPipe)
		case <-deadline: // 当前stream已经写入超时，即每个stream只能写入一段时间，否则会产生timeout，此时返回已经发送了多少数据
			return sent, errTimeout
		}

		// 等待frame的应答结果
		select {
		case result := <-req.result: // 读取发送结果
			sent += result.n         // 已发送数据更新
			if result.err != nil {
				return sent, result.err
			}
		case <-s.die:
			return sent, errors.New(errBrokenPipe)
		case <-deadline: // 防止stream饥饿
			return sent, errTimeout
		}
	}
	return sent, nil
}
func (s *Stream) Close() error {
	s.dieLock.Lock()
	select {
	case <-s.die:
		s.dieLock.Unlock()
		return errors.New(errBrokenPipe) // 提前关闭，返回broken pipe的错误
	default:
		close(s.die)              // 正常关闭，关闭s.die channel
		s.dieLock.Unlock()        // 开锁
		s.sess.streamClosed(s.id) // s.sess.streamClosed(s.id)，通知session从stream map中移除当前的stream
		//_, err := s.sess.writeFrame(newFrame(cmdFIN, s.id)) // 往底层写入一个cmdFIN报文
		s.sess.conn.Write([]byte{}) // todo
		//return err
		return nil
	}
}
func (s *Stream) SetReadDeadline(t time.Time) error {
	// 实现net.Conn的SetReadDeadline方法
	s.readDeadline.Store(t)
	return nil
}
func (s *Stream) SetWriteDeadline(t time.Time) error {
	// 实现net.Conn的SetWriteDeadline方法
	s.writeDeadline.Store(t)
	return nil
}
func (s *Stream) SetDeadline(t time.Time) error {
	// 实现net.Conn的SetDeadline方法，内部同时调用SetReadDeadline和SetWriteDeadline
	if err := s.SetReadDeadline(t); err != nil {
		return err
	}
	if err := s.SetWriteDeadline(t); err != nil {
		return err
	}
	return nil
}
func (s *Stream) LocalAddr() net.Addr {
	if ts, ok := s.sess.conn.(interface {
		LocalAddr() net.Addr
	}); ok {
		return ts.LocalAddr()
	}
	return nil
}
func (s *Stream) RemoteAddr() net.Addr {
	if ts, ok := s.sess.conn.(interface {
		RemoteAddr() net.Addr
	}); ok {
		return ts.RemoteAddr()
	}
	return nil
}
func (s *Stream) ID() uint32 {
	// 返回唯一stream id
	return s.id
}
func (s *Stream) split(bts []byte, cmd byte, sid uint32) []Frame {
	// len(bts)/s.frameSize 表示bts需要拆分介个小frame
	// 创建大小为（len(bts)/s.frameSize）的frame数组
	frames := make([]Frame, 0, len(bts)/s.frameSize+1)
	for len(bts) > s.frameSize {
		frame := newFrame(sid, cmd,0)
		frame.data = bts[:s.frameSize]
		bts = bts[s.frameSize:]
	}
	if len(bts) > 0 {
		frame := newFrame(sid, cmd,0)
		frame.data = bts
		frames = append(frames, frame)
	}
	return frames // 返回frame数组
}
func (s *Stream) recycleTokens() (n int) {
	// 负责获取buffer中剩余数据个数，即token个数，清空buffer
	s.bufferLock.Lock()
	n = s.buffer.Len()
	s.buffer.Reset()
	s.bufferLock.Unlock()
	return
}
func (s *Stream) pushBytes(p []byte) {
	// 将p写入到s.buffer中
	s.bufferLock.Lock()
	s.buffer.Write(p)
	s.bufferLock.Unlock()
}
func (s *Stream) sessionClose() {
	// 提供给session关闭stream
	s.dieLock.Lock()
	defer s.dieLock.Unlock()
	select {
	case <-s.die:
	default:
		close(s.die)
	}
}
func (s *Stream) markRST() {
	// 将该条流标记为RESET
	atomic.StoreInt32(&s.rstflag, 1)
}
func (s *Stream) notifyReadEvent() {
	// 往s.chReadEvent写入数据，通知读取数据
	select {
	case s.chReadEvent <- struct{}{}:
	default:
	}
}

// 创建stream对象
func newStream(id uint32, frameSize int, sess *Session) *Stream {
	s := new(Stream)                       // 创建stream对象
	s.id = id                              // stream id赋值
	s.chReadEvent = make(chan struct{}, 1) // read event channel
	s.frameSize = frameSize                // 缓冲区大小
	s.sess = sess                          // 应用session对象
	s.die = make(chan struct{})            // 标记stream 关闭
	return s
}

var errTimeout error = &timeoutError{}

type timeoutError struct{}

func (e *timeoutError) Error() string   { return "i/o timeout" }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return true }
