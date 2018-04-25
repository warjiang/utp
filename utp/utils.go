package utp

import (
	"time"
	"encoding/binary"
)

// 工具包函数
// 以ms的形式返回当前的时间戳
func currentMs() uint32 {
	return uint32(time.Now().UnixNano() / int64(time.Millisecond))
}
func min(a, b uint32) uint32 {
	// 求a,b最小值
	if a <= b {
		return a
	}
	return b
}
func max(a, b uint32) uint32 {
	// 求a,b最大值
	if a >= b {
		return a
	}
	return b
}
func bound(lower, middle, upper uint32) uint32 {
	// bound(a,b,c) = min(a,max(b,c)) 求b、c最大值，在与a求最小值
	return min(max(lower, middle), upper)
}
func timediff(later, earlier uint32) int32 {
	// 求a - b之差
	return (int32)(later - earlier)
}
// 编解码工具函数
func utp_encode8u(p []byte, c byte) []byte {
	/* encode 8 bits */
	p[0] = c
	return p[1:]
}
func utp_decode8u(p []byte, c *byte) []byte {
	/* decode 8 bits */
	*c = p[0]
	return p[1:]
}
func utp_encode16u(p []byte, w uint16) []byte {
	/* encode 16 bits */
	binary.LittleEndian.PutUint16(p, w)
	return p[2:]
}
func utp_decode16u(p []byte, w *uint16) []byte {
	/* decode 16 bits */
	*w = binary.LittleEndian.Uint16(p)
	return p[2:]
}
func utp_encode32u(p []byte, l uint32) []byte {
	/* encode 32 bits */
	binary.LittleEndian.PutUint32(p, l)
	return p[4:]
}
func utp_decode32u(p []byte, l *uint32) []byte {
	/* decode 32 bits */
	*l = binary.LittleEndian.Uint32(p)
	return p[4:]
}
