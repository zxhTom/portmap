package sshclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/forward"
	"github.com/zxhtom/portmap/internal/relayprotocol"
	"golang.org/x/crypto/ssh"
)

// RelayRequest uses authenticated SSH and a private Unix socket on the VPS;
// no public control port or separately distributed administrator token exists.
func RelayRequest(remote config.Remote, req relayprotocol.Request) (relayprotocol.Response, error) {
	client, _, err := connect(remote)
	if err != nil {
		return relayprotocol.Response{}, err
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	return requestRelay(ctx, client, req)
}

// ServerRequest is optional for ordinary SSH port forwarding. A VPS without
// portmap server returns an error; callers may keep using plain SSH forwarding.
func ServerRequest(remote config.Remote, req relayprotocol.Request) (relayprotocol.Response, error) {
	client, _, err := connect(remote)
	if err != nil {
		return relayprotocol.Response{}, err
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	return requestControl(ctx, client, req, `exec "$HOME/.local/bin/portmap" server request`)
}

func requestRelay(ctx context.Context, client *ssh.Client, req relayprotocol.Request) (relayprotocol.Response, error) {
	return requestControl(ctx, client, req, `exec "$HOME/.local/bin/portmap" relay request`)
}

func requestControl(ctx context.Context, client *ssh.Client, req relayprotocol.Request, command string) (relayprotocol.Response, error) {
	type result struct {
		response relayprotocol.Response
		err      error
	}
	done := make(chan result, 1)
	go func() {
		session, err := client.NewSession()
		if err != nil {
			done <- result{err: err}
			return
		}
		defer session.Close()
		data, _ := json.Marshal(req)
		session.Stdin = bytes.NewReader(data)
		var out, stderr bytes.Buffer
		session.Stdout = &out
		session.Stderr = &stderr
		if err := session.Run(command); err != nil {
			done <- result{err: fmt.Errorf("中转域名服务不可用，请先部署 portmap relay：%w (%s)", err, stderr.String())}
			return
		}
		var response relayprotocol.Response
		err = json.Unmarshal(out.Bytes(), &response)
		if err == nil && response.Error != "" {
			err = fmt.Errorf("%s", response.Error)
		}
		done <- result{response, err}
	}()
	select {
	case result := <-done:
		return result.response, result.err
	case <-ctx.Done():
		client.Close()
		return relayprotocol.Response{}, ctx.Err()
	}
}

type domainLease struct {
	mu          sync.Mutex
	id          string
	fingerprint string
}

func (t *Tunnel) domainRequest(operation string) relayprotocol.Request {
	zone := t.remote.DomainZone
	if zone == "" {
		zone = relayprotocol.DefaultZone
	}
	t.domain.mu.Lock()
	id := t.domain.id
	t.domain.mu.Unlock()
	return relayprotocol.Request{LeaseID: id, Operation: operation, Owner: t.mapping.DomainOwner, Prefixes: append([]string(nil), t.mapping.DomainPrefixes...), Port: t.mapping.ListenPort, Scheme: t.mapping.UpstreamScheme, Zone: zone}
}
func (t *Tunnel) reserveDomains(client *ssh.Client) error {
	if t.mapping.DomainOwner == "" {
		return fmt.Errorf("missing persistent domain owner identity")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := requestRelay(ctx, client, t.domainRequest("reserve"))
	return err
}
func (t *Tunnel) activateDomains(client *ssh.Client) error {
	var leaseID [16]byte
	if _, err := rand.Read(leaseID[:]); err != nil {
		return err
	}
	t.domain.mu.Lock()
	t.domain.id = hex.EncodeToString(leaseID[:])
	t.domain.mu.Unlock()
	fingerprint := ""
	if t.mapping.UpstreamScheme == "https" {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 8 * time.Second}, "tcp", t.TargetAddr, &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true})
		if err != nil {
			return fmt.Errorf("获取目标 HTTPS 证书失败：%w", err)
		}
		state := conn.ConnectionState()
		conn.Close()
		if len(state.PeerCertificates) == 0 {
			return fmt.Errorf("目标 HTTPS 服务未提供证书")
		}
		pin := sha256.Sum256(state.PeerCertificates[0].Raw)
		fingerprint = hex.EncodeToString(pin[:])
	}
	t.domain.mu.Lock()
	t.domain.fingerprint = fingerprint
	t.domain.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req := t.domainRequest("activate")
	req.Fingerprint = fingerprint
	_, err := requestRelay(ctx, client, req)
	return err
}
func (t *Tunnel) renewDomainLease(ctx context.Context, client *ssh.Client) error {
	if t.mapping.UpstreamScheme == "https" {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", t.TargetAddr, &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true})
		if err != nil {
			return err
		}
		cs := conn.ConnectionState()
		conn.Close()
		if len(cs.PeerCertificates) == 0 {
			return fmt.Errorf("missing upstream TLS certificate")
		}
		sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
		t.domain.mu.Lock()
		t.domain.fingerprint = hex.EncodeToString(sum[:])
		t.domain.mu.Unlock()
	}

	t.domain.mu.Lock()
	pin := t.domain.fingerprint
	t.domain.mu.Unlock()
	req := t.domainRequest("renew")
	req.Fingerprint = pin
	_, err := requestRelay(ctx, client, req)
	return err
}
func (t *Tunnel) renewDomains(client *ssh.Client, closing, done <-chan struct{}) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-closing:
			return
		case <-done:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			err := t.renewDomainLease(ctx, client)
			cancel()
			if err != nil {
				t.setStatus(forward.StateFailed, err.Error())
				client.Close()
				return
			}
		}
	}
}
