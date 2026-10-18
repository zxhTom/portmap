package sshclient

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/forward"
)

type Tunnel struct {
	RemoteName string
	RemoteAddr string
	ListenAddr string
	TargetAddr string

	remote  config.Remote
	mapping config.Mapping

	mu      sync.Mutex
	status  forward.Status
	client  *ssh.Client
	ln      net.Listener
	closing chan struct{}
	closed  bool
}

func NewTunnel(remote config.Remote, mapping config.Mapping) *Tunnel {
	target := net.JoinHostPort(mapping.LocalHost, fmt.Sprintf("%d", mapping.LocalPort))
	return &Tunnel{
		RemoteName: remote.Name,
		RemoteAddr: remote.Addr(),
		ListenAddr: fmt.Sprintf("0.0.0.0:%d", mapping.ListenPort),
		TargetAddr: target,
		remote:     remote,
		mapping:    mapping,
		status:     forward.Status{State: forward.StateStopped},
		closing:    make(chan struct{}),
	}
}

func (t *Tunnel) Status() forward.Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

func (t *Tunnel) Start() error {
	return t.Dial(t.remote)
}

func (t *Tunnel) Dial(remote config.Remote) error {
	t.mu.Lock()
	if t.closed {
		t.closing = make(chan struct{})
		t.closed = false
	}
	t.status = forward.Status{State: forward.StateStarting}
	t.mu.Unlock()

	authMethods, err := buildAuth(remote.Auth)
	if err != nil {
		t.setStatus(forward.StateFailed, fmt.Sprintf("auth: %v", err))
		return fmt.Errorf("auth: %w", err)
	}

	hostKeyCb, err := loadKnownHosts()
	if err != nil {
		if remote.StrictHost {
			t.setStatus(forward.StateFailed, fmt.Sprintf("strict host key verification required: %v", err))
			return fmt.Errorf("strict host key: %w", err)
		}
		slog.Warn("known_hosts not available, host key verification disabled", "err", err)
		hostKeyCb = ssh.InsecureIgnoreHostKey()
	}

	cfg := &ssh.ClientConfig{
		User:            remote.User,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCb,
		Timeout:         10 * time.Second,
	}

	if remote.Port == 0 {
		remote.Port = 22
	}
	addr := fmt.Sprintf("%s:%d", remote.Host, remote.Port)

	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		t.setStatus(forward.StateFailed, fmt.Sprintf("ssh dial: %v", err))
		return fmt.Errorf("ssh dial %s: %w", addr, err)
	}

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		client.Close()
		return fmt.Errorf("tunnel closed while connecting")
	}
	t.client = client
	t.mu.Unlock()
	slog.Info("ssh connected", "remote", remote.Name, "addr", addr)

	ln, err := client.Listen("tcp", t.ListenAddr)
	if err != nil {
		t.mu.Lock()
		t.client = nil
		t.mu.Unlock()
		client.Close()
		t.setStatus(forward.StateFailed, fmt.Sprintf("listen on remote %s failed: %v", t.ListenAddr, err))
		return fmt.Errorf("remote listen %s: %w", t.ListenAddr, err)
	}

	t.mu.Lock()
	if t.closed {
		t.client = nil
		t.mu.Unlock()
		ln.Close()
		client.Close()
		return fmt.Errorf("tunnel closed while listening")
	}
	t.ln = ln
	t.status = forward.Status{State: forward.StateRunning}
	t.mu.Unlock()

	go t.serveAndSupervise(ln)
	return nil
}

func (t *Tunnel) setStatus(state forward.State, errStr string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status = forward.Status{
		State: state,
		Error: errStr,
	}
}

func (t *Tunnel) serveAndSupervise(initialLn net.Listener) {
	ln := initialLn
	for {
		err := t.serve(ln)
		if ln != nil {
			ln.Close()
		}

		select {
		case <-t.closing:
			return
		default:
		}

		t.setStatus(forward.StateFailed, fmt.Sprintf("remote listener disconnected: %v", err))
		slog.Warn("listener closed, attempting reconnection", "addr", t.ListenAddr, "err", err)

		// Wait before retrying
		select {
		case <-t.closing:
			return
		case <-time.After(2 * time.Second):
		}

		t.mu.Lock()
		client := t.client
		t.mu.Unlock()

		if client == nil {
			return
		}

		newLn, err := client.Listen("tcp", t.ListenAddr)
		if err != nil {
			t.setStatus(forward.StateFailed, fmt.Sprintf("remote listen failed: %v", err))
			continue
		}

		t.mu.Lock()
		t.ln = newLn
		t.status = forward.Status{State: forward.StateRunning}
		t.mu.Unlock()
		ln = newLn
	}
}

func (t *Tunnel) serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go t.pipe(conn)
	}
}

func (t *Tunnel) pipe(remote net.Conn) {
	defer remote.Close()

	local, err := net.DialTimeout("tcp", t.TargetAddr, 10*time.Second)
	if err != nil {
		slog.Error("dial local target", "addr", t.TargetAddr, "err", err)
		return
	}
	defer local.Close()

	slog.Debug("tunnel session", "remote", t.RemoteName, "listen", t.ListenAddr, "target", t.TargetAddr)
	done := make(chan struct{}, 2)
	go func() { io.Copy(remote, local); done <- struct{}{} }()
	go func() { io.Copy(local, remote); done <- struct{}{} }()
	<-done
}

func (t *Tunnel) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	close(t.closing)
	ln := t.ln
	t.ln = nil
	client := t.client
	t.client = nil
	t.status = forward.Status{State: forward.StateStopped}
	t.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	if client != nil {
		_ = client.Close()
	}
	return nil
}

func buildAuth(auth config.AuthConfig) ([]ssh.AuthMethod, error) {
	switch strings.ToLower(auth.Method) {
	case "password":
		return []ssh.AuthMethod{ssh.Password(auth.Password)}, nil
	case "key":
		return keyAuth(auth)
	default:
		if auth.Password != "" {
			return []ssh.AuthMethod{ssh.Password(auth.Password)}, nil
		}
		if auth.KeyPath != "" {
			return keyAuth(auth)
		}
		return nil, fmt.Errorf("no auth method: set method to password or key")
	}
}

func keyAuth(auth config.AuthConfig) ([]ssh.AuthMethod, error) {
	path := auth.KeyPath
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, path[2:])
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key %s: %w", path, err)
	}
	var signer ssh.Signer
	if auth.Passphrase != "" {
		signer, err = ssh.ParsePrivateKeyWithPassphrase(key, []byte(auth.Passphrase))
	} else {
		signer, err = ssh.ParsePrivateKey(key)
	}
	if err != nil {
		return nil, fmt.Errorf("parse key: %w", err)
	}
	return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
}

func loadKnownHosts() (ssh.HostKeyCallback, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return knownhosts.New(filepath.Join(home, ".ssh", "known_hosts"))
}
