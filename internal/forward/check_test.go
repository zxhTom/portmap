package forward

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestCheckTCPRejectsUnreachableTargetDespiteOpenEntrance(t *testing.T) {
	entrance, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer entrance.Close()
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := target.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := CheckTCP(ctx, entrance.Addr().String(), addr); err != nil {
		t.Fatal(err)
	}
	target.Close()
	if err := CheckTCP(ctx, entrance.Addr().String(), addr); err == nil {
		t.Fatal("open entrance concealed closed destination")
	}
}
