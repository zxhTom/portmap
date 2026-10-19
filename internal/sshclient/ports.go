package sshclient

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net"
	"time"

	"github.com/zxhtom/portmap/internal/config"
	"golang.org/x/crypto/ssh"
)

// RandomPort selects a cryptographically random five-digit TCP port.
func RandomPort() (int, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(65536-10000))
	if err != nil {
		return 0, err
	}
	return 10000 + int(n.Int64()), nil
}

// connect authenticates and verifies host keys exactly as a forwarding tunnel does.
func connect(remote config.Remote) (*ssh.Client, net.Conn, error) {
	auth, err := buildAuth(remote.Auth)
	if err != nil {
		return nil, nil, fmt.Errorf("auth: %w", err)
	}
	cb, err := loadKnownHosts()
	if err != nil {
		if remote.StrictHost {
			return nil, nil, fmt.Errorf("strict host key: %w", err)
		}
		cb = ssh.InsecureIgnoreHostKey()
	}
	cfg := &ssh.ClientConfig{User: remote.User, Auth: auth, HostKeyCallback: cb, Timeout: 10 * time.Second}
	conn, err := net.DialTimeout("tcp", remote.Addr(), cfg.Timeout)
	if err != nil {
		return nil, nil, fmt.Errorf("SSH connect %s: %w", remote.Addr(), err)
	}
	conn.SetDeadline(time.Now().Add(cfg.Timeout))
	c, chans, reqs, err := ssh.NewClientConn(conn, remote.Addr(), cfg)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("SSH handshake %s: %w", remote.Addr(), err)
	}
	conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), conn, nil
}

// AllocatePort tests a real remote bind, rather than relying on a TCP connect
// (a firewall could hide an occupied port). The final tunnel bind remains
// authoritative: this short-lived probe does not reserve ports while stopped.
func AllocatePort(remote config.Remote, requested int, blocked func(int) bool) (int, error) {
	if requested < 0 || requested > 65535 {
		return 0, fmt.Errorf("port must be 1–65535, or 0 for random allocation")
	}
	if requested != 0 && blocked(requested) {
		return 0, fmt.Errorf("中转站端口 %d 已被其他映射占用", requested)
	}
	client, conn, err := connect(remote)
	if err != nil {
		return 0, fmt.Errorf("无法检查中转站端口: %w", err)
	}
	defer client.Close()
	// Bound all forwarding requests, including a non-responsive relay.
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	for attempt := 0; attempt < 128; attempt++ {
		port := requested
		if port == 0 {
			port, err = RandomPort()
			if err != nil {
				return 0, err
			}
		}
		if blocked(port) {
			continue
		}
		ln, listenErr := client.Listen("tcp", net.JoinHostPort("0.0.0.0", fmt.Sprint(port)))
		if listenErr == nil {
			if err := ln.Close(); err != nil {
				return 0, fmt.Errorf("release remote port probe: %w", err)
			}
			return port, nil
		}
		if requested != 0 {
			return 0, fmt.Errorf("中转站 %s 的端口 %d 不可用：已被占用或 SSH 转发策略禁止监听（%w）", remote.Host, port, listenErr)
		}
		select {
		case <-time.After(time.Millisecond):
		}
	}
	return 0, fmt.Errorf("未能找到中转站可用的随机五位数端口，请检查 SSH 转发权限或稍后重试")
}
