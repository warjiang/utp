package main

import (
	"time"
	"fmt"
)

func main() {
	var t time.Time
	t = time.Now()
	fmt.Println(t)
	// fmt.Println(t.IsZero())
	//time.Sleep(time.Second)
	//fmt.Println(time.Now().Sub(t))
	// fmt.Println(time.Now().After(t))
	timeout := time.NewTimer(5 * time.Second)
	ticker := time.Tick(10 * time.Second)

	for {
		select {
			case <-timeout.C:
				fmt.Println("execute timeout",time.Now().Sub(t))
			case <-ticker:
				fmt.Println("execute ticker",time.Now().Sub(t))
		}
	}
}
