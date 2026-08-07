package sshclient

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/zxhtom/portmap/internal/config"
)

type Tunnel struct {
	RemoteName string
	RemoteAddr string
	ListenAddr string
	TargetAddr string

	client  *ssh.Client
	closing chan struct{}
}

func NewTunnel(remote config.Remote, mapping config.Mapping) *Tunnel {
	target := net.JoinHostPort(mapping.LocalHost, fmt.Sprintf("%d", mapping.LocalPort))
	return &Tunnel{
		RemoteName: remote.Name,
		RemoteAddr: remote.Addr(),
		ListenAddr: fmt.Sprintf("0.0.0.0:%d", mapping.ListenPort),
		TargetAddr: target,
		closing:    make(chan struct{}),
	}
}

func (t *Tunnel) Dial(remote config.Remote) error {
	authMethods, err := buildAuth(remote.Auth)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	hostKeyCb, err := loadKnownHosts()
	if err != nil {
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
		return fmt.Errorf("ssh dial %s: %w", addr, err)
	}

	t.client = client
	slog.Info("ssh connected", "remote", remote.Name, "addr", addr)

	go t.keepListenAlive()
	return nil
}

func (t *Tunnel) keepListenAlive() {
	for {
		ln, err := t.client.Listen("tcp", t.ListenAddr)
		if err != nil {
			select {
			case <-t.closing:
				return
			default:
			}
			slog.Error("remote listen failed", "addr", t.ListenAddr, "err", err)
			return
		}
		slog.Info("reverse tunnel listening", "remote", t.RemoteName, "listen", t.ListenAddr, "target", t.TargetAddr)

		err = t.serve(ln)
		ln.Close()

		select {
		case <-t.closing:
			return
		default:
			slog.Warn("listener closed, retrying", "addr", t.ListenAddr, "err", err)
			time.Sleep(2 * time.Second)
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

func (t *Tunnel) Close() {
	close(t.closing)
	if t.client != nil {
		t.client.Close()
		t.client = nil
	}
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
