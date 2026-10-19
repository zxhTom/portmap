package main

import (
	"fmt"
	"github.com/zxhtom/portmap/internal/testrelay"
	"os"
	"os/signal"
)

func main() {
	addr := "127.0.0.1:0"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}
	r, err := testrelay.Start(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer r.Close()
	fmt.Println(r.Addr())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	<-ch
}
