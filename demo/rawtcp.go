package main

import (
	"fmt"
	"net"
)

func main() {
	addr, _ := net.ResolveTCPAddr("tcp", ":12948")
	listener, _ := net.ListenTCP("tcp", addr)
	//buf := make([]byte,1024)
	for {
		c, _ := listener.Accept()
		buf := make([]byte, 1024)
		n, _ := c.Read(buf)
		fmt.Println(n, buf[:n])
	}
	//netaddr, _ := net.ResolveIPAddr("ipv4", "127.0.0.1")
	//conn, err := net.ListenIP("ip4:tcp", netaddr)
	//if err != nil {
	//	log.Fatalf("ListenIP: %s\n", err)
	//}
	//fmt.Println(conn.ReadMsgIP())
	//addr, _ := net.ResolveTCPAddr("tcp", ":12948")
	//net.ListenTCP("tcp", addr)
}
