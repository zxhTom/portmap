package sshclient_test

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/forward"
	"github.com/zxhtom/portmap/internal/sshclient"
)

func generateTestSigner(t *testing.T) ssh.Signer {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa generate: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	return signer
}

func startMockSSHServer(t *testing.T, signer ssh.Signer) (string, func()) {
	cfg := &ssh.ServerConfig{
		NoClientAuth: true,
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ssh server listen: %v", err)
	}

	var running sync.WaitGroup
	quit := make(chan struct{})

	running.Add(1)
	go func() {
		defer running.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				sConn, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				defer sConn.Close()
				go func() {
					for req := range reqs {
						if req.Type == "tcpip-forward" {
							req.Reply(true, nil)
						} else {
							req.Reply(false, nil)
						}
					}
				}()
				for {
					select {
					case <-quit:
						return
					case newChan, ok := <-chans:
						if !ok {
							return
						}
						newChan.Reject(ssh.Prohibited, "not supported in test")
					}
				}
			}(conn)
		}
	}()

	teardown := func() {
		close(quit)
		ln.Close()
		running.Wait()
	}
	return ln.Addr().String(), teardown
}

func TestSSHTunnel_LifecycleAndSafeClose(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	signer := generateTestSigner(t)
	addr, cleanup := startMockSSHServer(t, signer)
	defer cleanup()

	host, portStr, _ := net.SplitHostPort(addr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	remote := config.Remote{
		Name: "test-vps",
		Host: host,
		Port: port,
		User: "test",
		Auth: config.AuthConfig{Method: "password", Password: "any"},
	}

	mapping := config.Mapping{
		Name:       "test-map",
		ListenPort: 19999,
		LocalHost:  "127.0.0.1",
		LocalPort:  80,
	}

	tunnel := sshclient.NewTunnel(remote, mapping)
	if tunnel.Status().State != forward.StateStopped {
		t.Fatalf("expected initial state stopped, got %s", tunnel.Status().State)
	}

	// Dial to mock server (no known_hosts in tempHome -> falls back to insecure unless StrictHost=true)
	err := tunnel.Dial(remote)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}

	// Idempotent Close test - close multiple times concurrently without panic
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = tunnel.Close()
		}()
	}
	wg.Wait()

	if tunnel.Status().State != forward.StateStopped {
		t.Fatalf("expected state stopped after close, got %s", tunnel.Status().State)
	}

	// StrictHost test: when strict_host is true and known_hosts does not exist, Dial must fail
	remoteStrict := remote
	remoteStrict.StrictHost = true
	tunnelStrict := sshclient.NewTunnel(remoteStrict, mapping)
	if err := tunnelStrict.Dial(remoteStrict); err == nil {
		t.Fatalf("expected Dial with StrictHost to fail when known_hosts is missing")
	}

	// When known_hosts exists, even with default StrictHost=false, mismatching host key MUST be rejected!
	sshDir := filepath.Join(tempHome, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("mkdir .ssh: %v", err)
	}
	// Write a known_hosts entry with a different (bogus) key
	fakeSigner := generateTestSigner(t)
	fakePub := fakeSigner.PublicKey()
	khLine := knownhosts.Line([]string{addr}, fakePub)
	if err := os.WriteFile(filepath.Join(sshDir, "known_hosts"), []byte(khLine+"\n"), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}

	remoteMismatch := remote
	remoteMismatch.StrictHost = false // default
	tunnelMismatch := sshclient.NewTunnel(remoteMismatch, mapping)
	if err := tunnelMismatch.Dial(remoteMismatch); err == nil {
		t.Fatalf("expected Dial with mismatching hostkey in known_hosts to be rejected even when StrictHost=false")
	}
}
