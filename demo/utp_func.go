package main

import (
	"utp/utp"
	"fmt"
)

func TestEncode() {
	buf := make([]byte, 1024)
	ptr := buf
	seg := utp.NewSegment(1, 2, 0, 4096, []byte("abcd"))
	ptr = seg.EncodeSegment(ptr)
	//fmt.Println(len(buf)-len(ptr))
	size := len(buf) - len(ptr)
	sn, ack, cmd, wnd, length := utp.DecodeSegment(buf[:size])
	fmt.Println("sn", sn, "ack", ack, "cmd", cmd, "wnd", wnd, "length", length)
}

func TestUTP() {
	var buf []string
	buf = append([]string{}, utp.DefaultSnmp.Header()...)
	fmt.Println(buf)
}

func TestDecode() {
	buf := []byte{0, 0, 0, 0, 0, 0, 0, 0, 16, 0, 33, 0}
	//buf := []byte{0, 0, 0, 0, 0, 0, 0, 0, 32, 0, 3, 0}
	sn, ack, cmd, wnd, length := utp.DecodeSegment(buf)
	fmt.Println(sn, ack, cmd, wnd, length)
}

func TestMath() {
	var cmd,wnd uint16
	cmd, wnd = 0, 4096-1
	cmdWnd := cmd << 12 + wnd
	//fmt.Printf("cmd<<12 %X\n",cmd<<12)
	//fmt.Printf("cmd<<12+wnd %X\n",cmd<<12+wnd)
	//fmt.Printf("cmdWnd %X\n",cmdWnd)
	//fmt.Printf("cmdWnd>>12 %X\n",cmdWnd>>12)
	//fmt.Printf("cmdWnd&0xF000 %X\n",cmdWnd&0xF000)
	//fmt.Printf("cmdWnd&0xF000>>12 %X\n",cmdWnd&0xF000>>12)
	//fmt.Printf("cmdWnd&0x0FFF %X\n",cmdWnd&0x0FFF)
	fmt.Println("cmd",cmdWnd>>12,"wnd",cmdWnd&0x0FFF)

}

func main() {
	//TestEncode()
	//TestUTP()
	//TestDecode()
	TestMath()
}
