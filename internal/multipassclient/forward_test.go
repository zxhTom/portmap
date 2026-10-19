package multipassclient

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zxhtom/portmap/internal/config"
)

type changingResolver struct {
	mu  sync.Mutex
	ip  string
	err error
}

func (r *changingResolver) Resolve(ctx context.Context, _ string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ip, r.err
}
func (r *changingResolver) set(ip string, err error) {
	r.mu.Lock()
	r.ip = ip
	r.err = err
	r.mu.Unlock()
}

func echoTarget(t *testing.T, host string) (int, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	conns := map[net.Conn]bool{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns[c] = true
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				_, _ = io.Copy(c, c)
				mu.Lock()
				delete(conns, c)
				mu.Unlock()
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() {
		ln.Close()
		mu.Lock()
		for c := range conns {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	}
}
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}
func assertEcho(t *testing.T, port int) {
	t.Helper()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("forwarded data %q %v", buf, err)
	}
}

func TestForwardTCPDetectsAddressChangeAndClosesSessions(t *testing.T) {
	target, cleanup := echoTarget(t, "127.0.0.1")
	defer cleanup()
	r := &changingResolver{ip: "127.0.0.1"}
	mp := config.Mapping{Type: config.TypeMultipass, Name: "test", Instance: "vm", InstancePort: target, ListenPort: freePort(t)}
	f := NewForward(r, mp, config.Remote{})
	f.interval = 10 * time.Millisecond
	defer f.Close()
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	assertEcho(t, mp.ListenPort)
	active, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", mp.ListenPort))
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	r.set("127.0.0.2", nil)
	deadline := time.Now().Add(time.Second)
	for f.Status().State != "failed" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.Status().State != "failed" || !strings.Contains(f.Status().Error, "address changed") {
		t.Fatalf("change not detected: %+v", f.Status())
	}
	active.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1)
	if _, err := active.Read(buf); err == nil {
		t.Fatal("old session retained")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	r.set("127.0.0.1", nil)
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	assertEcho(t, mp.ListenPort)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestForwardOfflineAndStop(t *testing.T) {
	r := &changingResolver{err: fmt.Errorf("instance stopped")}
	mp := config.Mapping{Type: config.TypeMultipass, Name: "test", Instance: "vm", InstancePort: 22, ListenPort: freePort(t)}
	f := NewForward(r, mp, config.Remote{})
	defer f.Close()
	if err := f.Start(); err == nil {
		t.Fatal("offline instance accepted")
	}
	if f.Status().State != "failed" {
		t.Fatal("failure not reported")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	r.set("127.0.0.1", nil)
	f.interval = 10 * time.Millisecond
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	r.set("", fmt.Errorf("instance stopped"))
	deadline := time.Now().Add(time.Second)
	for f.Status().State != "failed" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.Status().State != "failed" {
		t.Fatal("instance stop not detected")
	}
}
