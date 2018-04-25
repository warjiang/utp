package mux

import (
	"encoding/binary"
	"fmt"
)

/*
Frame 结构定义
STREAM ID(4B) | TYPE(1B) | RWND(2B) | LENGTH(2B) |  DATA(LENGTH)
*/

const (
	sizeOfSid    = 4 // 会话ID
	sizeOfCmd    = 1 // frame类型
	sizeOfRWND   = 2 // 窗口大小
	sizeOfLength = 2 // 数据长度2B
	headerSize   = 9 // 报头长度 （sizeOfSid + sizeOfCmd + sizeOfRWND + sizeOfLength）
)
const (
	cmdSYN byte = iota // stream open
	cmdFIN             // stream close, a.k.a EOF mark
	cmdPSH             // data push
	cmdNOP             // no operation
)

type Frame struct {
	sid  uint32 // 会话id
	cmd  byte   // 类型
	rwnd uint16 // 窗口大小
	data []byte // 数据部分
}

func newFrame(sid uint32, cmd byte, rwnd uint16) Frame {
	// 根据命令类型，会话id 创建Frame对象
	return Frame{sid: sid, cmd: cmd, rwnd: rwnd}
}

type rawHeader []byte

func (h rawHeader) StreamID() uint32 {
	// 返回会话id（协议中第0、1、2、3字节）
	return binary.LittleEndian.Uint32(h[0:])
}
func (h rawHeader) Cmd() byte {
	// 返回frame类型（协议第4字节）
	return h[4]
}
func (h rawHeader) Rwnd() uint16 {
	// 返回frame类型（协议第5、6字节）
	return binary.LittleEndian.Uint16(h[5:])
}
func (h rawHeader) Length() uint16 {
	// 返回frame中数据的长度（协议中第7、8字节）
	return binary.LittleEndian.Uint16(h[7:])
}
func (h rawHeader) String() string {
	// toString方法
	return fmt.Sprintf("StreamID:%d Cmd:%d Rwnd:%d Length:%d", h.StreamID(), h.Cmd(), h.Rwnd(), h.Length())
}
