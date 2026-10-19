package multipassclient

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/forward"
	"github.com/zxhtom/portmap/internal/sshclient"
)

type Resolver interface {
	Resolve(context.Context, string) (string, error)
}

// Forward binds a host TCP port, or a VPS port when Remote is configured.
// Instance discovery is read-only. IP changes are handled by manager recovery.
type Forward struct {
	opMu     sync.Mutex
	mu       sync.Mutex
	mapping  config.Mapping
	remote   config.Remote
	resolver Resolver
	status   forward.Status
	ctx      context.Context
	cancel   context.CancelFunc
	ln       net.Listener
	tunnel   *sshclient.Tunnel
	ip       string
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	interval time.Duration
}

func NewForward(resolver Resolver, mp config.Mapping, remote config.Remote) *Forward {
	mp.SetDefaults()
	return &Forward{mapping: mp, remote: remote, resolver: resolver, status: forward.Status{State: forward.StateStopped}, interval: 2 * time.Second}
}

func (f *Forward) Status() forward.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status.State == forward.StateRunning && f.tunnel != nil {
		return f.tunnel.Status()
	}
	return f.status
}

func (f *Forward) Start() error {
	f.opMu.Lock()
	defer f.opMu.Unlock()
	f.mu.Lock()
	if f.cancel != nil {
		f.mu.Unlock()
		return fmt.Errorf("Multipass forward already started; close before restarting")
	}
	f.ctx, f.cancel = context.WithCancel(context.Background())
	f.conns = make(map[net.Conn]struct{})
	f.status = forward.Status{State: forward.StateStarting}
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	ip, err := f.resolver.Resolve(ctx, f.mapping.Instance)
	cancel()
	if err != nil {
		f.fail(err.Error())
		return err
	}
	f.ip = ip
	if f.mapping.Remote != "" {
		mp := f.mapping
		mp.Type = config.TypeSSH
		mp.LocalHost = ip
		mp.LocalPort = mp.InstancePort
		tunnel := sshclient.NewTunnel(f.remote, mp)
		if err := tunnel.Start(); err != nil {
			f.fail(err.Error())
			return err
		}
		f.mu.Lock()
		f.tunnel = tunnel
		f.mu.Unlock()
	} else {
		ln, err := net.Listen("tcp", net.JoinHostPort(f.mapping.BindHost, strconv.Itoa(f.mapping.ListenPort)))
		if err != nil {
			f.fail(err.Error())
			return err
		}
		f.mu.Lock()
		f.ln = ln
		f.mu.Unlock()
		f.wg.Add(1)
		go f.serve(ln)
	}
	f.mu.Lock()
	f.status = forward.Status{State: forward.StateRunning}
	f.mu.Unlock()
	f.wg.Add(1)
	go f.monitor()
	return nil
}

func (f *Forward) monitor() {
	defer f.wg.Done()
	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()
	for {
		select {
		case <-f.ctx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
			ip, err := f.resolver.Resolve(ctx, f.mapping.Instance)
			cancel()
			if f.ctx.Err() != nil {
				return
			}
			if err != nil {
				f.fail(err.Error())
				return
			}
			if ip != f.ip {
				f.fail("Multipass instance address changed")
				return
			}
			f.mu.Lock()
			tunnel := f.tunnel
			f.mu.Unlock()
			if tunnel != nil && tunnel.Status().State == forward.StateFailed {
				f.fail("Multipass SSH forwarding connection lost")
				return
			}
		}
	}
}

func (f *Forward) serve(ln net.Listener) {
	defer f.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if f.ctx.Err() == nil {
				f.fail("Multipass listener failed")
			}
			return
		}
		if !f.track(conn) {
			continue
		}
		f.wg.Add(1)
		go f.pipe(conn)
	}
}

func (f *Forward) track(c net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ctx.Err() != nil {
		c.Close()
		return false
	}
	f.conns[c] = struct{}{}
	return true
}
func (f *Forward) release(c net.Conn) { c.Close(); f.mu.Lock(); delete(f.conns, c); f.mu.Unlock() }
func closeWrite(c net.Conn) {
	if w, ok := c.(interface{ CloseWrite() error }); ok {
		_ = w.CloseWrite()
	}
}

func (f *Forward) pipe(source net.Conn) {
	defer f.wg.Done()
	defer f.release(source)
	dialer := net.Dialer{Timeout: 10 * time.Second}
	target, err := dialer.DialContext(f.ctx, "tcp", net.JoinHostPort(f.ip, strconv.Itoa(f.mapping.InstancePort)))
	if err != nil {
		return
	}
	if !f.track(target) {
		return
	}
	defer f.release(target)
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(target, source); closeWrite(target) }()
	_, _ = io.Copy(source, target)
	closeWrite(source)
	// A failed copy must not leave the other direction waiting indefinitely.
	_ = source.Close()
	_ = target.Close()
	<-done
}

func (f *Forward) fail(reason string) {
	f.mu.Lock()
	if f.ctx == nil || f.ctx.Err() != nil {
		f.mu.Unlock()
		return
	}
	f.status = forward.Status{State: forward.StateFailed, Error: reason}
	f.cancel()
	f.mu.Unlock()
	f.closeResources()
}

func (f *Forward) closeResources() {
	f.mu.Lock()
	ln, tunnel := f.ln, f.tunnel
	conns := make([]net.Conn, 0, len(f.conns))
	for c := range f.conns {
		conns = append(conns, c)
	}
	f.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	if tunnel != nil {
		_ = tunnel.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
}

func (f *Forward) Close() error {
	f.opMu.Lock()
	defer f.opMu.Unlock()
	f.mu.Lock()
	if f.cancel != nil {
		f.cancel()
	}
	f.status = forward.Status{State: forward.StateStopped}
	f.mu.Unlock()
	f.closeResources()
	f.wg.Wait()
	f.mu.Lock()
	f.cancel = nil
	f.ln = nil
	f.tunnel = nil
	f.mu.Unlock()
	return nil
}

// Check verifies the current VM target as well as the forwarding listener.
func (f *Forward) Check(ctx context.Context) error {
	f.mu.Lock()
	tunnel := f.tunnel
	f.mu.Unlock()
	if tunnel != nil {
		return tunnel.Check(ctx)
	}
	ip, err := f.resolver.Resolve(ctx, f.mapping.Instance)
	if err != nil {
		return err
	}
	host := f.mapping.BindHost
	if host == "0.0.0.0" || host == "::" || host == "" {
		host = "127.0.0.1"
	}
	return forward.CheckTCP(ctx, net.JoinHostPort(ip, strconv.Itoa(f.mapping.InstancePort)), net.JoinHostPort(host, strconv.Itoa(f.mapping.ListenPort)))
}
