// Package testrelay supplies an isolated real-bind SSH relay for acceptance tests.
package testrelay

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"strconv"
	"sync"

	"golang.org/x/crypto/ssh"
)

type Relay struct {
	ln     net.Listener
	mu     sync.Mutex
	conns  map[net.Conn]bool
	closed bool
	wg     sync.WaitGroup
}

func Start(addr string) (*Relay, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	r := &Relay{ln: ln, conns: make(map[net.Conn]bool)}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			r.mu.Lock()
			if r.closed {
				r.mu.Unlock()
				c.Close()
				return
			}
			r.conns[c] = true
			r.wg.Add(1)
			r.mu.Unlock()
			go func() {
				defer r.wg.Done()
				defer c.Close()
				defer func() { r.mu.Lock(); delete(r.conns, c); r.mu.Unlock() }()
				serve(c, cfg)
			}()
		}
	}()
	return r, nil
}
func (r *Relay) Addr() string { return r.ln.Addr().String() }
func (r *Relay) Close() {
	r.mu.Lock()
	r.closed = true
	r.ln.Close()
	for c := range r.conns {
		c.Close()
	}
	r.mu.Unlock()
	r.wg.Wait()
}
func serve(c net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go func() {
		for ch := range chans {
			ch.Reject(ssh.Prohibited, "not supported in test relay")
		}
	}()
	listeners := make(map[string]net.Listener)
	defer func() {
		for _, ln := range listeners {
			ln.Close()
		}
	}()
	for req := range reqs {
		var payload struct {
			Host string
			Port uint32
		}
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			req.Reply(false, nil)
			continue
		}
		addr := net.JoinHostPort(payload.Host, strconv.Itoa(int(payload.Port)))
		switch req.Type {
		case "tcpip-forward":
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				req.Reply(false, nil)
				continue
			}
			actual := ln.Addr().(*net.TCPAddr).Port
			listeners[net.JoinHostPort(payload.Host, strconv.Itoa(actual))] = ln
			req.Reply(true, ssh.Marshal(struct{ Port uint32 }{uint32(actual)}))
			// Port allocation tests need real binds, not data forwarding. Close any probe.
			go func() {
				for {
					accepted, err := ln.Accept()
					if err != nil {
						return
					}
					accepted.Close()
				}
			}()
		case "cancel-tcpip-forward":
			ln := listeners[addr]
			if ln == nil {
				req.Reply(false, nil)
				continue
			}
			ln.Close()
			delete(listeners, addr)
			req.Reply(true, nil)
		default:
			req.Reply(false, nil)
		}
	}
}
func (r *Relay) String() string { return fmt.Sprint(r.Addr()) }
