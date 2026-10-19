package multipassclient

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/zxhtom/portmap/internal/config"
	"golang.org/x/crypto/ssh"
)

func TestMultipassReverseSSHForward(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	target, cleanup := echoTarget(t, "127.0.0.1")
	defer cleanup()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	connected := make(chan *ssh.ServerConn, 1)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
		if err != nil {
			return
		}
		defer sc.Close()
		connected <- sc
		go func() {
			for ch := range chans {
				ch.Reject(ssh.Prohibited, "unsupported")
			}
		}()
		for req := range reqs {
			req.Reply(req.Type == "tcpip-forward", nil)
		}
	}()
	host, portText, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	fmt.Sscanf(portText, "%d", &port)
	remote := config.Remote{Name: "vps", Host: host, Port: port, User: "test", Auth: config.AuthConfig{Method: "password", Password: "test"}}
	mapping := config.Mapping{Type: config.TypeMultipass, Name: "vm-public", Instance: "vm", InstancePort: target, ListenPort: freePort(t), Remote: "vps"}
	f := NewForward(&changingResolver{ip: "127.0.0.1"}, mapping, remote)
	defer func() {
		f.Close()
		ln.Close()
		select {
		case <-serverDone:
		case <-time.After(time.Second):
			t.Error("SSH server did not exit")
		}
	}()
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	var sc *ssh.ServerConn
	select {
	case sc = <-connected:
	case <-time.After(time.Second):
		t.Fatal("SSH not connected")
	}
	channel, requests, err := sc.OpenChannel("forwarded-tcpip", ssh.Marshal(struct {
		Addr       string
		Port       uint32
		OriginAddr string
		OriginPort uint32
	}{"0.0.0.0", uint32(mapping.ListenPort), "127.0.0.1", 12345}))
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	go ssh.DiscardRequests(requests)
	done := make(chan error, 1)
	go func() {
		if _, err := channel.Write([]byte("hello")); err != nil {
			done <- err
			return
		}
		buf := make([]byte, 5)
		_, err := io.ReadFull(channel, buf)
		if err == nil && string(buf) != "hello" {
			err = fmt.Errorf("unexpected payload %q", buf)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reverse SSH did not reach instance target")
	}
}
