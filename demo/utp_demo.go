package main

import (
	"utp/utp"
	"github.com/astaxie/beego/logs"
)

func main() {

	if l, e := utp.Listen(":10000"); e == nil {
		logs.Info("utp server listen at localhost:10000")
		go func() {
			b := make([]byte, 1024)
			if conn, e := l.Accept(); e == nil {
				logs.Info("accept client connection")
				n, _ := conn.Read(b)
				logs.Info("received " + string(b[:n]))
				conn.Write([]byte("server reply " + string(b[:n])))
			}
		}()
	}

	buf := make([]byte, 1024)
	utpconn, _ := utp.DialWithOptions(":10000", nil, 10, 3)
	//logs.Info("utp client send hello world start")
	utpconn.Write([]byte("abc"))
	//logs.Info("utp client send hello world end")
	//logs.Info("utp client try to receive start")
	n, _ := utpconn.Read(buf)
	logs.Info(string(buf[:n]))
	//logs.Info("utp client try to receive end")
}
