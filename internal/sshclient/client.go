package sshclient

import (
	"context"
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
	domain     domainLease
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
	listenHost := "0.0.0.0"
	if len(mapping.DomainPrefixes) > 0 {
		listenHost = "127.0.0.1"
	}
	return &Tunnel{
		RemoteName: remote.Name,
		RemoteAddr: remote.Addr(),
		ListenAddr: net.JoinHostPort(listenHost, fmt.Sprint(mapping.ListenPort)),
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
	addr := remote.Addr()

	// Bound both TCP establishment and the SSH handshake.
	conn, err := net.DialTimeout("tcp", addr, cfg.Timeout)
	if err != nil {
		t.setStatus(forward.StateFailed, fmt.Sprintf("ssh dial: %v", err))
		return fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(cfg.Timeout))
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
	} else {
		_ = conn.SetDeadline(time.Time{})
	}
	var client *ssh.Client
	if err == nil {
		client = ssh.NewClient(c, chans, reqs)
	}
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

	_ = conn.SetDeadline(time.Now().Add(cfg.Timeout))
	if len(t.mapping.DomainPrefixes) > 0 {
		if err := t.reserveDomains(client); err != nil {
			client.Close()
			t.setStatus(forward.StateFailed, err.Error())
			return err
		}
	}
	ln, err := client.Listen("tcp", t.ListenAddr)
	_ = conn.SetDeadline(time.Time{})
	if err != nil {
		t.mu.Lock()
		t.client = nil
		t.mu.Unlock()
		client.Close()
		t.setStatus(forward.StateFailed, fmt.Sprintf("listen on remote %s failed: %v", t.ListenAddr, err))
		return fmt.Errorf("中转站端口 %d 不可用：已被占用或 SSH 转发策略禁止监听（%w）", t.mapping.ListenPort, err)
	}

	if len(t.mapping.DomainPrefixes) > 0 {
		if err := t.activateDomains(client); err != nil {
			client.Close()
			ln.Close()
			t.setStatus(forward.StateFailed, err.Error())
			return err
		}
	}
	t.mu.Lock()
	if t.closed {
		t.client = nil
		t.mu.Unlock()
		client.Close()
		ln.Close()
		return fmt.Errorf("tunnel closed while listening")
	}
	t.ln = ln
	t.status = forward.Status{State: forward.StateRunning}
	closing := t.closing
	t.mu.Unlock()

	go t.serveAndSupervise(ln, client, closing)
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

// Manager supervision establishes a fresh authenticated connection after failure.
func (t *Tunnel) serveAndSupervise(ln net.Listener, client *ssh.Client, closing <-chan struct{}) {
	done := make(chan struct{})
	defer close(done)
	go t.keepAlive(client, closing, done)
	if len(t.mapping.DomainPrefixes) > 0 {
		go t.renewDomains(client, closing, done)
	}
	err := t.serve(ln)
	_ = client.Close()
	_ = ln.Close()
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed && t.client == client {
		t.status = forward.Status{State: forward.StateFailed, Error: fmt.Sprintf("SSH connection lost: %v", err)}
	}
}

func (t *Tunnel) keepAlive(client *ssh.Client, closing, done <-chan struct{}) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-closing:
			return
		case <-done:
			return
		case <-ticker.C:
			result := make(chan error, 1)
			go func() { _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); result <- err }()
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-closing:
				timer.Stop()
				return
			case <-done:
				timer.Stop()
				return
			case err := <-result:
				timer.Stop()
				if err != nil {
					_ = client.Close()
					return
				}
			case <-timer.C:
				_ = client.Close()
				return
			}
		}
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

	if client != nil && len(t.mapping.DomainPrefixes) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		requestRelay(ctx, client, t.domainRequest("deactivate"))
		cancel()
	}
	// Close the transport first: listener cancellation waits for a remote reply
	// and must not block shutdown when the peer is unresponsive.
	if client != nil {
		_ = client.Close()
	}
	if ln != nil {
		_ = ln.Close()
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

// Check verifies both the destination and the public relay listener.
func (t *Tunnel) Check(ctx context.Context) error {
	if len(t.mapping.DomainPrefixes) > 0 {
		if err := forward.CheckTCP(ctx, t.TargetAddr); err != nil {
			return err
		}
		t.mu.Lock()
		client := t.client
		t.mu.Unlock()
		if client == nil {
			return fmt.Errorf("SSH connection unavailable")
		}
		return t.renewDomainLease(ctx, client)
	}
	return forward.CheckTCP(ctx, t.TargetAddr, net.JoinHostPort(t.remote.Host, fmt.Sprint(t.mapping.ListenPort)))
}
