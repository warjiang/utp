package utp

import (
	"sync/atomic"
	"log"
)

/*
UTP 报文格式 (扩展 UDP payload部分)
0             2             4 (BYTE)
+---------------------------+
|           sn              |
+---------------------------+4
|           ack             |
+---------------------------+8
| t(2)+wnd(14) |     len    |
+--------------+------------+12
|                           |
|      DATA (optional)      |
|                           |
+---------------------------+
// 发送过程
// 用户调用Send->sendQueue->sendBuf->output
// 接受过程
// 接受UDP报文Input->receiveBuf->receiveQueue->用户调用Receive函数
*/

const (
	UTP_MTU_DEF     = 1400 // 默认MTU
	UTP_OVERHEAD    = 12   // 头部长度
	UTP_WND_SND     = 32   // 默认发送窗口大小
	UTP_WND_RCV     = 32   // 默认接收窗口大小
	UTP_RTO_DEF     = 200  // 默认RTO
	UTP_INTERVAL    = 100  // UTP内部工作时间间隔 100ms
	UTP_DEADLINK    = 20   // 报文传输(xmit>20次)仍然不成功则表示该链路已经断开
	UTP_CMD_ACK     = 0    // ack报文
	UTP_CMD_DATA    = 1    // 数据报文
	UTP_CMD_AWND    = 2    // 数据报文
	UTP_CMD_TWND    = 3    // 数据报文
	UTP_THRESH_INIT = 2    // 慢启动门限
	UTP_THRESH_MIN  = 2    // 慢启动门限最小值
	UTP_RTO_MIN     = 100  // normal min rto
)

// UTP segment
type segment struct {
	sn       uint32 // 报文序列号(4B)
	ack      uint32 // 小于ack的报文均已经收到(4B)
	cmd      uint16 // 报文cmd(type)标志(4bit=>2bit) 分别表示ACK0、DATA1、AWND2、TWND3
	wnd      uint16 // 窗口大小(12bit=>14bit,最大表示2^12=>2^14=16384)
	rto      uint32 // 超时定时器
	xmit     uint32 // 报文发送次数
	resendts uint32 // 重传时间戳
	fastack  uint32 // fastack次数
	data     []byte // 报文数据区域
}

func NewSegment(sn, ack uint32, cmd, wnd uint16, data []byte) (seg segment) {
	seg.sn = sn
	seg.ack = ack
	seg.cmd = cmd
	seg.wnd = wnd
	seg.rto = UTP_RTO_MIN
	seg.xmit = 1
	seg.resendts = 0
	seg.fastack = 0
	seg.data = data
	return
}
func DecodeSegment(data []byte) (sn, ack uint32, cmd, wnd, length uint16) {
	// 从data中按字节解析对应字段
	var cmdWnd uint16
	data = utp_decode32u(data, &sn)
	data = utp_decode32u(data, &ack)
	data = utp_decode16u(data, &cmdWnd)
	data = utp_decode16u(data, &length)
	cmd = cmdWnd >> 14
	wnd = cmdWnd & 0x3FFF
	return
}
func (seg *segment) EncodeSegment(ptr []byte) []byte {
	// 将segment编码到ptr对应的内存空间中，编码完成返回新的ptr
	ptr = utp_encode32u(ptr, seg.sn)                     // 序列号
	ptr = utp_encode32u(ptr, seg.ack)                    // ack号
	ptr = utp_encode16u(ptr, seg.cmd<<14+seg.wnd&0x3FFF) // cmd<<14 + wnd
	ptr = utp_encode16u(ptr, uint16(len(seg.data)))      // 数据部分len
	atomic.AddUint64(&DefaultSnmp.OutSegs, 1)
	return ptr
}

// UTP连接的callback函数
type outputCallback func(buf []byte, size int)

// 每个UTP连接会维护一个UTP对象，负责调用UTP协议处理报文
type UTP struct {
	mtu, mss, state uint32
	// 发送窗口管理
	sendUna, cwnd            uint32
	sendNext, sendWnd        uint32
	receiveNext, receiveWnd  uint32
	remoteWnd                uint32
	ssthresh                 uint32
	rxRTO                    uint32
	interval, flushTimestamp uint32
	nodelay, updated         uint32
	deadLink, incr           uint32
	fastResend               int32
	noCwnd, stream           int32
	sendQueue                []segment // 发送队列
	receiveQueue             []segment // 接收队列
	sendBuf                  []segment // 发送缓存
	receiveBuf               []segment // 接收缓存
	ackList                  []uint32  // ack队列
	buffer                   []byte    // 用于报文累加发送，尽量充满mtu(mss)
	output                   outputCallback
}
func NewUTP(output outputCallback) *UTP {
	// 根据callback函数创建UTP对象
	utp := new(UTP)                                     // 创建UTP对象
	utp.sendWnd = UTP_WND_SND                           // 默认发送窗口32
	utp.receiveWnd = UTP_WND_RCV                        // 默认接收窗口32
	utp.remoteWnd = UTP_WND_RCV                         // 默认远端窗口32
	utp.mtu = UTP_MTU_DEF                               // 默认MTU值1400
	utp.mss = utp.mtu - UTP_OVERHEAD                    // 最大报文段为从MTU中排除了UTP头的剩余部分长度
	utp.buffer = make([]byte, (utp.mtu+UTP_OVERHEAD)*3) // buffer用来临时保存数据
	utp.interval = UTP_INTERVAL                         // UTP内部工作间隔 100ms
	utp.flushTimestamp = UTP_INTERVAL                   // 刷新数据时间间隔 100ms
	utp.ssthresh = UTP_THRESH_INIT                      // 慢启动门限初始化为2
	utp.deadLink = UTP_DEADLINK                         // dead link检测阈值
	utp.output = output                                 // output回调函数
	utp.rxRTO = UTP_RTO_DEF                             // 接收RTO 200ms
	return utp
}
func (utp *UTP) newSegment(size int) (seg segment) {
	// 创建segment即从全局的缓存池中取出size大小的缓冲区，作为segment的数据部分
	seg.data = xmitBuf.Get().([]byte)[:size]
	return
}
func (utp *UTP) delSegment(seg segment) {
	// 删除segment即将segment重新放回全局缓存池，等待复用
	xmitBuf.Put(seg.data)
}
func (utp *UTP) SetMtu(mtu int) int {
	// 设置mtu，默认情况下mtu为1400
	if mtu < 50 || mtu < UTP_OVERHEAD {
		return -1
	}
	buffer := make([]byte, (mtu+UTP_OVERHEAD)*3)
	if buffer == nil {
		return -2
	}
	utp.mtu = uint32(mtu)
	utp.mss = utp.mtu - UTP_OVERHEAD
	utp.buffer = buffer
	return 0
}
func (utp *UTP) SetWndSize(sndWnd, rcvWnd int) int {
	// 设置发送窗口和接收窗口，刚开始sendWnd和receiveWnd均采用默认值为32
	if sndWnd > 0 {
		utp.sendWnd = uint32(sndWnd)
	}
	if rcvWnd > 0 {
		utp.receiveWnd = uint32(rcvWnd)
	}
	return 0
}
func (utp *UTP) NoDelay(nodelay, interval, resend, nc int) int {
	// NoDelay options
	// nodelay ：是否启用 nodelay模式，0不启用；1启用。
	// interval ：协议内部工作的 interval，单位毫秒，比如 10ms或者 20ms
	// resend ：快速重传模式，默认0关闭，可以设置2（2次ACK跨越将会直接重传）
	// nc ：是否关闭流控，默认是0代表不关闭，1代表关闭。
	// 普通模式： (0, 40, 0, 0)
	// 极速模式： (1, 10, 2, 1)
	if nodelay >= 0 {
		utp.nodelay = uint32(nodelay)
	}
	if interval >= 0 {
		if interval > 5000 {
			interval = 5000
		} else if interval < 10 {
			interval = 10
		}
		utp.interval = uint32(interval)
	}
	if resend >= 0 {
		utp.fastResend = int32(resend)
	}
	if nc >= 0 {
		utp.noCwnd = int32(nc)
	}
	return 0
}
func (utp *UTP) GetPeekSize() (length int) {
	// 获取接收队列第一个segment 数据部分的字节数
	if len(utp.receiveQueue) == 0 {
		return -1
	}
	seg := &utp.receiveQueue[0]
	return len(seg.data)
}
func (utp *UTP) GetWaitingSegment() int {
	// 返回等待发送segment的数量(sendBuf中等待确认的报文，以及sendQueue中等待发送的报文)
	return len(utp.sendBuf) + len(utp.sendQueue)
}
func (utp *UTP) getUnusedWnd() uint16 {
	// 返回未使用的窗口大小
	// rcv_queue的最大长度为rcv_wnd，wnd_unused返回rcv_queue上的剩余窗口大小
	if len(utp.receiveQueue) < int(utp.receiveWnd) {
		return uint16(int(utp.receiveWnd) - len(utp.receiveQueue))
	}
	return 0
}
func (utp *UTP) removeFront(q []segment, n int) []segment {
	// 从队列中移除前n个元素
	newn := copy(q, q[n:]) // 将前n个元素覆盖
	for i := newn; i < len(q); i++ { // 删除尾部的n个元素
		q[i] = segment{} // 手动GC
	}
	return q[:newn]
}
func (utp *UTP) parseUna(una uint32) {
	// 对（segment.sn<una）的报文进行确认，并从sendBuf中移除
	count := 0
	for k := range utp.sendBuf {
		seg := &utp.sendBuf[k]
		if timediff(una, seg.sn) > 0 {
			utp.delSegment(*seg)
			count++
		} else {
			break
		}
	}
	if count > 0 {
		utp.sendBuf = utp.removeFront(utp.sendBuf, count)
	}
}
func (utp *UTP) parseAck(sn uint32) {
	// 对（segment.sn==sn）的报文进行确认，并从sendBuf中移除
	// sn < utp.sendUna  的报文已经进行过确认
	// sn >= utp.sendNext 的报文还没有发送
	if timediff(sn, utp.sendUna) < 0 || timediff(sn, utp.sendNext) >= 0 {
		return
	}

	for k := range utp.sendBuf {
		seg := &utp.sendBuf[k]
		if sn == seg.sn {
			utp.delSegment(*seg)
			copy(utp.sendBuf[k:], utp.sendBuf[k+1:])
			utp.sendBuf[len(utp.sendBuf)-1] = segment{}
			utp.sendBuf = utp.sendBuf[:len(utp.sendBuf)-1]
			break
		}
		if timediff(sn, seg.sn) < 0 {
			break
		}
	}
}
func (utp *UTP) parseAckList(snList []uint32) {
	// 对ack list进行批量处理
	for _, sn := range snList {
		// sn < utp.sendUna  的报文已经进行过确认
		// sn >= utp.sendNext 的报文还没有发送
		utp.parseAck(sn)
	}
}
func (utp *UTP) parseFastack(sn uint32) {
	// 将序列号小于sn的报文的fastack参数加1
	if timediff(sn, utp.sendUna) < 0 || timediff(sn, utp.sendNext) >= 0 {
		return
	}
	for _, seg := range utp.sendBuf {
		if timediff(sn, seg.sn) < 0 {
			break
		} else if sn != seg.sn {
			seg.fastack++
		}
	}
}
func (utp *UTP) parseData(newSegment segment) {
	// 在接收过程中对于newSegment的处理流程,送入receiveBuf中，
	// 然后刷新receiveBuf，对于符合条件的执行receiveBuf->receiveQueue
	sn := newSegment.sn
	// utp.receiveNext <= newSegment.sn <= utp.receiveNext+utp.receiveWnd
	if timediff(sn, utp.receiveNext+utp.receiveWnd) >= 0 ||
		timediff(sn, utp.receiveNext) < 0 {
		utp.delSegment(newSegment)
		return
	}

	// 从receiveBuf的尾部开始，根据报文序列号之间的关系，寻找插入位置insertIdx
	// 从后往前找，找到第一个seg.sn < sn的i,插入位置insertIdx = i+1
	n := len(utp.receiveBuf) - 1
	insertIdx := 0
	repeat := false
	for i := n; i >= 0; i-- {
		seg := &utp.receiveBuf[i]
		if seg.sn == sn {
			repeat = true
			atomic.AddUint64(&DefaultSnmp.RepeatSegs, 1)
			break
		}
		if timediff(sn, seg.sn) > 0 {
			insertIdx = i + 1
			break
		}
	}

	if !repeat {
		if insertIdx == n+1 {
			utp.receiveBuf = append(utp.receiveBuf, newSegment)
		} else {
			utp.receiveBuf = append(utp.receiveBuf, segment{})
			copy(utp.receiveBuf[insertIdx+1:], utp.receiveBuf[insertIdx:])
			utp.receiveBuf[insertIdx] = newSegment
		}
	} else {
		utp.delSegment(newSegment)
	}
	utp.sortRecvBuf()
}
func (utp *UTP) sortRecvBuf() {
	// 整理receiveBuf到receiveQueue中
	// 将receiveBuf中满足条件 receiveNext<= segment.sn <=receiveNext+receiveWnd的报文移除并加入到receiveQueue中
	count := 0
	for k := range utp.receiveBuf {
		seg := &utp.receiveBuf[k]
		if seg.sn == utp.receiveNext && len(utp.receiveQueue) < int(utp.receiveWnd) {
			utp.receiveNext++
			count++
		} else {
			break
		}
	}
	if count > 0 {
		utp.receiveQueue = append(utp.receiveQueue, utp.receiveBuf[:count]...)
		utp.receiveBuf = utp.removeFront(utp.receiveBuf, count)
	}
}
func (utp *UTP) updateUna() {
	// 更新sendUna,下次发送给对方，通知对方小于sendUna的报文，己方已经全部收到
	if len(utp.sendBuf) > 0 {
		seg := &utp.sendBuf[0]
		utp.sendUna = seg.sn
	} else {
		utp.sendUna = utp.sendNext
	}
}
func (utp *UTP) Input(data []byte, ackNoDelay bool) int {
	// 当收到一个底层UDP报文后执行Input函数[供外部程序调用
	// 当外部程序收到UDP报文之后，调用Input函数做进一步处理
	sendUna := utp.sendUna // 保存当前的sendUna,后面在执行updateUna的过程会修改utp.sendUna
	if len(data) < UTP_OVERHEAD { // 报文长度小于UTP报文头部
		return -1
	}
	var maxAck uint32 // 最大ack的序列号
	var flag int      // flag介于0、1之间，用于第一次获取maxAck和lastAckTs，以后置为1，通过比较最终的maxack和lastackts
	var inSegs uint64 // 从buffer中读取segment的数量

	for {
		if len(data) < int(UTP_OVERHEAD) {
			break
		}

		sn, ack, cmd, wnd, length := DecodeSegment(data)
		data = data[UTP_OVERHEAD:]
		if len(data) < int(length) { // -2 数据长度不匹配
			return -2
		}
		if cmd != UTP_CMD_ACK && cmd != UTP_CMD_DATA &&
			cmd != UTP_CMD_AWND && cmd != UTP_CMD_TWND { // cmd不支持
			return -3
		}

		utp.remoteWnd = uint32(wnd) // 更新remove window
		utp.parseUna(ack)           // 将sendBuf中报文序号小于ack的报文移除
		utp.updateUna()             // 更新utp.sendUna
		log.Println("packet type is: ", cmd)
		switch cmd {
		case UTP_CMD_ACK:
			// keep alive 报文，处理ack逻辑
			utp.parseAck(sn) // sendBuf中移除segment.sn == sn的报文
			utp.updateUna()  // 更新utp.sendUna
			if flag == 0 { // trick:第一次为0，以后都为1
				flag = 1
				maxAck = sn
			} else if timediff(sn, maxAck) > 0 {
				maxAck = sn
			}
		case UTP_CMD_DATA:
			// 普通数据报文
			if timediff(sn, utp.receiveNext+utp.receiveWnd) < 0 {
				// 1.对方重发报文 此时可能己方发送的ack对方并没有收到，需要将ack加入ackList
				// 2.对方正常发送报文，此时也是需要将己方待发送的ack加入到ackList中
				utp.pushAck(sn, 0)
				if timediff(sn, utp.receiveNext) >= 0 { // 保证segment在[receiveNext,receiveNext+receiveWnd]之间
					seg := utp.newSegment(int(length)) // 构造segment,从全局缓存池中申请length大小的区域保存数据
					seg.cmd = cmd
					seg.wnd = wnd
					seg.sn = sn
					copy(seg.data, data[:length])
					utp.parseData(seg) // 送往receiveBuf中,符合条件送往receiveQueue中
				} else {
					atomic.AddUint64(&DefaultSnmp.RepeatSegs, 1)
				}
			} else {
				atomic.AddUint64(&DefaultSnmp.RepeatSegs, 1)
			}
		case UTP_CMD_AWND:
		case UTP_CMD_TWND:
		default:
			return -3
		}

		inSegs++
		data = data[length:]
	}

	if flag != 0 { // 如果更新出来maxAck
		utp.parseFastack(maxAck) // 将序列号小于maxAck的报文的fastack++
	}

	// AIMD
	// 每收到一个UDP报文执行一次
	if timediff(utp.sendUna, sendUna) > 0 { // utp.sendUna发生了变化，说明发生了报文的ACK
		if utp.cwnd < utp.remoteWnd {
			mss := utp.mss
			if utp.cwnd < utp.ssthresh { // 慢启动
				utp.cwnd++
				utp.incr += mss
			} else { // 拥塞避免
				if utp.incr < mss {
					utp.incr = mss
				}
				utp.incr += (mss*mss)/utp.incr + (mss / 16)
				if (utp.cwnd+1)*mss <= utp.incr {
					utp.cwnd++
				}
			}
			if utp.cwnd > utp.remoteWnd { // 如果计算出来的cwnd的值超过对端发送过来的remoteWnd则直接按照remoteWnd设置
				utp.cwnd = utp.remoteWnd
				utp.incr = utp.remoteWnd * mss
			}
		}
	}

	if ackNoDelay && len(utp.ackList) > 0 { // ack immediately
		utp.flush(true)
	}

	return 0
}
func (utp *UTP) flush(ackOnly bool) {
	// 刷新数据
	// 如果ackOnly为true的话表示此次只是刷新acklist
	// 否则的话，负责从snd_buf
	var seg segment              // 构造segment
	seg.cmd = UTP_CMD_ACK        // 直接把cmd置为UTP_CMD_KEEPALIVE
	seg.wnd = utp.getUnusedWnd() // rcv_queue中空闲窗口大小，通过UTP报文中的wnd通知给发送端
	seg.ack = utp.receiveNext    // 设置segment的ack，通知给发送端，我这边小于utp.rcv_nxt的数据报文都已经收到了
	buffer := utp.buffer         // 刷新acklist，先构造成segment
	ptr := buffer                // buffer和ptr指向同一片内存地址，buffer不动，ptr移动
	for i, ack := range utp.ackList {
		size := len(buffer) - len(ptr)
		if size+UTP_OVERHEAD > int(utp.mtu) { // 剩余空间不够继续构造UTP报文
			utp.output(buffer, size) // 调用构造UTP时候传入的output函数
			ptr = buffer             // ptr和buffer重新指向同一内存地址
		}
		if ack >= utp.receiveNext || len(utp.ackList)-1 == i {
			seg.sn = ack
			ptr = seg.EncodeSegment(ptr)
		}
	}
	utp.ackList = utp.ackList[0:0] // 清空utp.acklist
	if ackOnly { // 将buffer中剩余的ack通过utp.output函数一起出去
		size := len(buffer) - len(ptr)
		if size > 0 {
			utp.output(buffer, size)
		}
		return
	}

	// 计算cwnd的大小
	cwnd := min(utp.sendWnd, utp.remoteWnd)
	if utp.noCwnd == 0 { // 是否启用nc流控，0表示不关闭，1表示关闭
		cwnd = min(utp.cwnd, cwnd)
	}

	// 滑动窗口机制，滑动窗口的范围为[snd_nxt,snd_nxt+cwnd]
	// 将数据从snd_queue中往snd_buf中挪动
	newSegsCount := 0
	for k := range utp.sendQueue {
		// sn小于sendUna的均已经发送完毕，发送方最多能够发送sendUna+cwnd的飞行数据
		// 当发送序号大于sendUna+cwnd表示已经超过发送能力
		if timediff(utp.sendNext, utp.sendUna+cwnd) >= 0 {
			break
		}
		newSegment := utp.sendQueue[k]
		newSegment.cmd = UTP_CMD_DATA
		newSegment.sn = utp.sendNext
		utp.sendBuf = append(utp.sendBuf, newSegment)
		utp.sendNext++
		newSegsCount++
		utp.sendQueue[k].data = nil
	}
	if newSegsCount > 0 {
		utp.sendQueue = utp.removeFront(utp.sendQueue, newSegsCount)
	}
	resent := uint32(utp.fastResend)

	// 报文重传
	current := currentMs()
	var change, lost, lostSegs, fastRetransSegs, earlyRetransSegs uint64
	for _, segment := range utp.sendBuf {
		needsend := false
		if segment.xmit == 0 {
			// xmit表示传输次数，如果报文是首次传输，则遍历到该报文时一定将该报文发送出去
			needsend = true
			segment.rto = utp.rxRTO
			segment.resendts = current + segment.rto
		} else if timediff(current, segment.resendts) >= 0 {
			// RTO发现报文超时未应答，有可能该报文发生了丢包，此时需要重新发送该报文
			needsend = true
			if utp.nodelay == 0 { // 在开启流控的情况下
				segment.rto += utp.rxRTO
			} else { // 在关闭流控的情况下
				segment.rto += utp.rxRTO / 2
			}
			segment.resendts = current + segment.rto
			lost++
			lostSegs++
		} else if segment.fastack >= resent {
			// 当收到fastack次数超过resent（utp.fastResend）的情况下，启用快速重传
			needsend = true
			segment.fastack = 0
			segment.rto = utp.rxRTO
			segment.resendts = current + segment.rto
			change++
			fastRetransSegs++
		} else if segment.fastack > 0 && newSegsCount == 0 {
			// 当snd_buf中没有新的segment时，启用ER
			needsend = true
			segment.fastack = 0
			segment.rto = utp.rxRTO
			segment.resendts = current + segment.rto
			change++
			earlyRetransSegs++
		}

		if needsend {
			segment.xmit++
			segment.wnd = seg.wnd
			segment.ack = seg.ack

			size := len(buffer) - len(ptr)
			need := UTP_OVERHEAD + len(segment.data)
			if size+need > int(utp.mtu) {
				utp.output(buffer, size)
				current = currentMs() // time update for a blocking call
				ptr = buffer
			}

			ptr = segment.EncodeSegment(ptr)
			copy(ptr, segment.data)
			ptr = ptr[len(segment.data):]

			if segment.xmit >= utp.deadLink { // 如果对某个报文的发送次数超过utp.deadLink（UTP_DEADLINK=20)表示当前链路已经是一条无效链路
				utp.state = 0xFFFFFFFF
			}
		}
	}

	// 清空剩余buffer
	size := len(buffer) - len(ptr)
	if size > 0 {
		utp.output(buffer, size)
	}

	sum := lostSegs
	if lostSegs > 0 {
		atomic.AddUint64(&DefaultSnmp.LostSegs, lostSegs)
	}
	if fastRetransSegs > 0 {
		atomic.AddUint64(&DefaultSnmp.FastRetransSegs, fastRetransSegs)
		sum += fastRetransSegs
	}
	if earlyRetransSegs > 0 {
		atomic.AddUint64(&DefaultSnmp.EarlyRetransSegs, earlyRetransSegs)
		sum += earlyRetransSegs
	}
	if sum > 0 {
		atomic.AddUint64(&DefaultSnmp.RetransSegs, sum)
	}

	// update ssthresh
	// rate halving, https://tools.ietf.org/html/rfc6937
	if change > 0 {
		inflight := utp.sendNext - utp.sendUna
		utp.ssthresh = inflight / 2
		if utp.ssthresh < UTP_THRESH_MIN {
			utp.ssthresh = UTP_THRESH_MIN
		}
		utp.cwnd = utp.ssthresh + resent
		utp.incr = utp.cwnd * utp.mss
	}

	// congestion control, https://tools.ietf.org/html/rfc5681
	if lost > 0 {
		utp.ssthresh = cwnd / 2
		if utp.ssthresh < UTP_THRESH_MIN {
			utp.ssthresh = UTP_THRESH_MIN
		}
		utp.cwnd = 1
		utp.incr = utp.mss
	}

	if utp.cwnd < 1 {
		utp.cwnd = 1
		utp.incr = utp.mss
	}

}
func (utp *UTP) pushAck(sn, ts uint32) {
	// 往ackList中增加新的ack
	utp.ackList = append(utp.ackList, sn)
}
func (utp *UTP) Receive(buffer []byte) (n int) {
	// 接收函数，负责将数据读取到buffer中
	if len(utp.receiveQueue) == 0 {
		return -1
	}
	peekSize := utp.GetPeekSize()
	if peekSize < 0 {
		return -2 // -2 表示底层收到的报文出了问题
	}
	if peekSize > len(buffer) {
		return -3 // -3 表示报文的大小超过buffer的容量
	}
	count := 0
	for _, seg := range utp.receiveQueue { // 等价于每次从rcv_queue中取出一个报文送到buffer中
		copy(buffer, seg.data)
		buffer = buffer[len(seg.data):]
		n += len(seg.data)
		count++
		utp.delSegment(seg)
		break // trick
	}
	if count > 0 {
		utp.receiveQueue = utp.removeFront(utp.receiveQueue, count)
	}
	// 尝试将序列号大于rcv_nxt的segment从rcv_buf中移动到rcv_queue中
	count = 0
	for k := range utp.receiveBuf {
		seg := &utp.receiveBuf[k]
		if seg.sn == utp.receiveNext && len(utp.receiveQueue) < int(utp.receiveWnd) {
			utp.receiveNext++
			count++
		} else {
			break
		}
	}

	if count > 0 {
		utp.receiveQueue = append(utp.receiveQueue, utp.receiveBuf[:count]...)
		utp.receiveBuf = utp.removeFront(utp.receiveBuf, count)
	}

	return
}
func (utp *UTP) Send(buffer []byte) int {
	// 发送函数，用户调用Send将数据首先发送到sendQueue中，返回值表示往sendQueue塞了多少数据。
	var count int
	if len(buffer) == 0 {
		return -1
	}
	// 首先将数据尽可能的添加在最后一个segment上
	n := len(utp.sendQueue)
	if n > 0 {
		seg := &utp.sendQueue[n-1] // 发送队列最后一个segment
		if len(seg.data) < int(utp.mss) { // 只要接收队列中数据区域还没有超过mss，则可以继续往最后一个segment塞数据
			capacity := int(utp.mss) - len(seg.data)
			extend := capacity // extend = min(capacity,len(buffer))=(utp.mss - len(seg.data),len(buffer))
			if len(buffer) < capacity {
				extend = len(buffer)
			}
			oldLen := len(seg.data)
			seg.data = seg.data[:oldLen+extend]
			copy(seg.data[oldLen:], buffer)
			buffer = buffer[extend:]
		}
		if len(buffer) == 0 { // 通过往最后一个segment塞数据就已经能够满足Send要求，提前退出
			return 0
		}
	}

	count = (len(buffer)-1)/int(utp.mss) + 1 // ceil(len(buffer)-1/int(utp.mss))
	for i := 0; i < count; i++ {
		var size int
		if len(buffer) > int(utp.mss) { // size = min(len(buffer),utp.mss)
			size = int(utp.mss)
		} else {
			size = len(buffer)
		}
		seg := utp.newSegment(size)
		copy(seg.data, buffer[:size])
		utp.sendQueue = append(utp.sendQueue, seg)
		buffer = buffer[size:]
	}
	return 0
}
