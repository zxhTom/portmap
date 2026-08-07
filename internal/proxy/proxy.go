package proxy

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
)

type Proxy struct {
	listenAddr string
	targetAddr string
	listener   net.Listener
	active     atomic.Int64
	closing    atomic.Bool
}

func New(listenAddr, targetAddr string) *Proxy {
	return &Proxy{
		listenAddr: listenAddr,
		targetAddr: targetAddr,
	}
}

func (p *Proxy) Start() error {
	if p.listener != nil {
		return fmt.Errorf("proxy already started on %s", p.listenAddr)
	}
	ln, err := net.Listen("tcp", p.listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", p.listenAddr, err)
	}
	p.listener = ln
	p.closing.Store(false)
	slog.Info("proxy started", "listen", p.listenAddr, "target", p.targetAddr)
	go p.acceptLoop()
	return nil
}

func (p *Proxy) Stop() {
	if p.listener == nil {
		return
	}
	p.closing.Store(true)
	p.listener.Close()
	slog.Info("proxy stopped", "listen", p.listenAddr, "target", p.targetAddr)
}

func (p *Proxy) ActiveConns() int64 {
	return p.active.Load()
}

func (p *Proxy) acceptLoop() {
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			if !p.closing.Load() {
				slog.Error("accept error", "listen", p.listenAddr, "err", err)
			}
			return
		}
		p.active.Add(1)
		go p.handle(conn)
	}
}

func (p *Proxy) handle(client net.Conn) {
	defer p.active.Add(-1)
	defer client.Close()

	target, err := net.Dial("tcp", p.targetAddr)
	if err != nil {
		slog.Error("dial target", "addr", p.targetAddr, "err", err)
		return
	}
	defer target.Close()

	slog.Debug("connection", "listen", p.listenAddr, "target", p.targetAddr)
	done := make(chan struct{}, 2)
	go func() { io.Copy(target, client); done <- struct{}{} }()
	go func() { io.Copy(client, target); done <- struct{}{} }()
	<-done
}
