package main

import "fmt"

func main() {
	a := ^uint64(0)
	var b uint64
	b = 2
	fmt.Println(b)
	b = b + a
	fmt.Println(b)
}
