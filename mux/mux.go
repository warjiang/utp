package mux

import (
	"time"
	"io"
)

// mux 配置对象
type Config struct {
	KeepAliveInterval time.Duration // 向对方发送nop frame的事件间隔
	KeepAliveTimeout  time.Duration // KeepAliveTimeout内没有收到数据，则关闭session
	MaxFrameSize      int           // 控制发送到对端frame的frame size的最大值
	MaxReceiveBuffer  int           // 控制缓存池大小
}

// 默认配置
func DefaultConfig() *Config {
	return &Config{
		KeepAliveInterval: 10 * time.Second, // 每隔10s发送一次nop报文
		KeepAliveTimeout:  30 * time.Second, // 30s内没有收到任何报文，则关闭session
		MaxFrameSize:      4096,             // frame size要控制在4096内
		MaxReceiveBuffer:  4194304,          // 缓存池大小为4MB（4194304=4*1024*1024）
	}
}


// 创建server一侧的链接复用
func Server(conn io.ReadWriteCloser, config *Config) (*Session, error) {
	if config == nil {
		config = DefaultConfig()
	}
	//if err := VerifyConfig(config); err != nil {
	//	return nil, err
	//}
	return newSession(config, conn, false), nil
}

// 创建client一侧的链接复用
func Client(conn io.ReadWriteCloser, config *Config) (*Session, error) {
	if config == nil {
		config = DefaultConfig()
	}

	//if err := VerifyConfig(config); err != nil {
	//	return nil, err
	//}
	return newSession(config, conn, true), nil
}
