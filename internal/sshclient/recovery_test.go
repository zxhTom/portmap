package sshclient_test

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/manager"
	"github.com/zxhtom/portmap/internal/sshclient"
	"golang.org/x/crypto/ssh"
)

func TestManagerReconnectsWithFreshSSHConnection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	serverConfig := &ssh.ServerConfig{NoClientAuth: true}
	serverConfig.AddHostKey(generateTestSigner(t))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	connections := make(chan *ssh.ServerConn, 8)
	var mu sync.Mutex
	var sockets []net.Conn
	var wg sync.WaitGroup
	defer func() {
		ln.Close()
		mu.Lock()
		for _, c := range sockets {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			sockets = append(sockets, c)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				sc, chans, reqs, err := ssh.NewServerConn(c, serverConfig)
				if err != nil {
					return
				}
				defer sc.Close()
				connections <- sc
				go func() {
					for ch := range chans {
						ch.Reject(ssh.Prohibited, "not supported")
					}
				}()
				for req := range reqs {
					req.Reply(req.Type == "tcpip-forward", nil)
				}
			}()
		}
	}()
	host, portText, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	fmt.Sscanf(portText, "%d", &port)
	m := manager.New(t.TempDir())
	defer m.Shutdown()
	if err := m.AddRemote(config.Remote{Name: "test", Host: host, Port: port, User: "test", Auth: config.AuthConfig{Method: "password", Password: "test"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.AddMapping("web", config.Mapping{Name: "app", Remote: "test", ListenPort: 19123, LocalHost: "127.0.0.1", LocalPort: 80}); err != nil {
		t.Fatal(err)
	}
	if err := m.StartMapping("web", "app"); err != nil {
		t.Fatal(err)
	}
	first := <-connections
	first.Close()
	deadline := time.Now().Add(3 * time.Second)
	for m.ListMappings()[0].Status != "failed" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if m.ListMappings()[0].Status != "failed" {
		t.Fatal("disconnect not detected")
	}
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	select {
	case second := <-connections:
		if first == second {
			t.Fatal("reused broken SSH session")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fresh connection not established")
	}
	if m.ListMappings()[0].Status != "running" {
		t.Fatal("mapping did not recover")
	}
}

// The peer accepts forwarding but never acknowledges keepalive/cancel requests.
func TestUnresponsiveSSHPeerDoesNotBlockCloseAndIsDetected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	scfg := &ssh.ServerConfig{NoClientAuth: true}
	scfg.AddHostKey(generateTestSigner(t))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	var sockets []net.Conn
	var wg sync.WaitGroup
	defer func() {
		ln.Close()
		mu.Lock()
		for _, c := range sockets {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			sockets = append(sockets, c)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				sc, chans, reqs, err := ssh.NewServerConn(c, scfg)
				if err != nil {
					return
				}
				defer sc.Close()
				go func() {
					for ch := range chans {
						ch.Reject(ssh.Prohibited, "unsupported")
					}
				}()
				for req := range reqs {
					if req.Type == "tcpip-forward" {
						req.Reply(true, nil)
					}
				}
			}()
		}
	}()
	host, portText, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	fmt.Sscanf(portText, "%d", &port)
	remote := config.Remote{Name: "test", Host: host, Port: port, User: "test", Auth: config.AuthConfig{Method: "password", Password: "test"}}
	mapping := config.Mapping{Name: "app", ListenPort: 19124, LocalHost: "127.0.0.1", LocalPort: 80}
	t.Run("close", func(t *testing.T) {
		tunnel := sshclient.NewTunnel(remote, mapping)
		if err := tunnel.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- tunnel.Close() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("close waited for unresponsive SSH server")
		}
	})
	t.Run("keepalive", func(t *testing.T) {
		tunnel := sshclient.NewTunnel(remote, mapping)
		if err := tunnel.Start(); err != nil {
			t.Fatal(err)
		}
		defer tunnel.Close()
		deadline := time.Now().Add(18 * time.Second)
		for time.Now().Before(deadline) {
			if tunnel.Status().State == "failed" {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("unresponsive peer was not detected by keepalive")
	})
}
