package tunnel

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zxhtom/portmap/internal/protocol"
)

type TunnelInfo struct {
	MappingID  string `json:"mapping_id"`
	TargetHost string `json:"target_host"`
	TargetPort int    `json:"target_port"`
	LocalPort  int    `json:"local_port"`
}

type Client struct {
	relayAddr string
	user      string
	password  string
	localDir  string

	mux         *protocol.MuxConn
	mu          sync.Mutex
	running     bool
	tunnels     []TunnelInfo
	handlers    map[uint32]net.Conn
	handlersL   sync.Mutex
	pendingData map[uint32][][]byte
	closing     chan struct{}
}

func NewClient(relayAddr, user, password, localDir string) *Client {
	return &Client{
		relayAddr:   relayAddr,
		user:        user,
		password:    password,
		localDir:    localDir,
		handlers:    make(map[uint32]net.Conn),
		pendingData: make(map[uint32][][]byte),
	}
}

func (c *Client) Start(tunnels []TunnelInfo) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tunnels = tunnels
	c.closing = make(chan struct{})
	return c.connect()
}

func (c *Client) connect() error {
	if c.mux != nil {
		c.mux.Conn().Close()
		c.mux = nil
	}

	dialer := net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.Dial("tcp", c.relayAddr)
	if err != nil {
		return fmt.Errorf("connect to relay %s: %w", c.relayAddr, err)
	}

	mux := protocol.NewMuxConn(conn)
	c.mux = mux

	mux.StartReadLoop(func(streamID uint32, data []byte) {
		c.handleStreamData(streamID, data)
	})

	if c.user != "" || c.password != "" {
		mux.SendCtrl(protocol.CtrlMsg{Type: "auth", User: c.user, Password: c.password})
		if _, ok := c.waitCtrl("auth_ok"); !ok {
			conn.Close()
			return fmt.Errorf("authentication failed")
		}
	}

	for _, t := range c.tunnels {
		mux.SendCtrl(protocol.CtrlMsg{
			Type:       "register",
			MappingID:  t.MappingID,
			TargetHost: t.TargetHost,
			TargetPort: t.TargetPort,
		})
		_, ok := c.waitCtrl("registered")
		if !ok {
			conn.Close()
			return fmt.Errorf("failed to register %s", t.MappingID)
		}
		slog.Info("tunnel registered", "mapping", t.MappingID, "target", fmt.Sprintf("%s:%d", t.TargetHost, t.TargetPort))
	}

	go c.ctrlLoop()
	return nil
}

func (c *Client) ctrlLoop() {
	for {
		select {
		case msg, ok := <-c.mux.CtrlChan():
			if !ok {
				return
			}
			switch msg.Type {
			case "stream_open":
				go c.handleStreamOpen(msg)
			case "stream_close":
				go c.handleStreamClose(msg)
			case "closed":
				slog.Warn("relay connection lost, reconnecting...")
				c.reconnect()
				return
			case "error":
				slog.Error("relay error", "msg", msg.Message)
			}
		case <-c.closing:
			return
		}
	}
}

func (c *Client) reconnect() {
	c.mu.Lock()
	tunnels := c.tunnels
	c.mu.Unlock()
	for {
		select {
		case <-c.closing:
			return
		default:
		}
		slog.Info("reconnecting to relay...")
		c.mu.Lock()
		err := c.connect()
		c.mu.Unlock()
		if err != nil {
			slog.Error("reconnect failed", "err", err)
			time.Sleep(5 * time.Second)
			continue
		}
		c.mu.Lock()
		c.tunnels = tunnels
		c.mu.Unlock()
		return
	}
}

func (c *Client) handleStreamOpen(msg protocol.CtrlMsg) {
	target := fmt.Sprintf("%s:%d", msg.TargetHost, msg.TargetPort)
	conn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		slog.Error("dial target failed", "target", target, "err", err)
		c.mux.SendCtrl(protocol.CtrlMsg{Type: "stream_close", StreamID: msg.StreamID})
		return
	}

	c.handlersL.Lock()
	c.handlers[msg.StreamID] = conn
	pending := c.pendingData[msg.StreamID]
	delete(c.pendingData, msg.StreamID)
	c.handlersL.Unlock()

	for _, data := range pending {
		conn.Write(data)
	}

	slog.Debug("stream opened", "stream_id", msg.StreamID, "target", target)

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if werr := c.mux.WriteFrame(msg.StreamID, buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				c.mux.SendCtrl(protocol.CtrlMsg{Type: "stream_close", StreamID: msg.StreamID})
				break
			}
		}
		c.handlersL.Lock()
		delete(c.handlers, msg.StreamID)
		c.handlersL.Unlock()
		conn.Close()
	}()
}

func (c *Client) handleStreamClose(msg protocol.CtrlMsg) {
	c.handlersL.Lock()
	conn, ok := c.handlers[msg.StreamID]
	if ok {
		delete(c.handlers, msg.StreamID)
	}
	c.handlersL.Unlock()
	if ok {
		conn.Close()
		slog.Debug("stream closed", "stream_id", msg.StreamID)
	}
}

func (c *Client) handleStreamData(streamID uint32, data []byte) {
	c.handlersL.Lock()
	conn, ok := c.handlers[streamID]
	if !ok {
		c.pendingData[streamID] = append(c.pendingData[streamID], data)
		c.handlersL.Unlock()
		return
	}
	c.handlersL.Unlock()
	conn.Write(data)
}

func (c *Client) waitCtrl(expectedType string) (protocol.CtrlMsg, bool) {
	select {
	case msg, ok := <-c.mux.CtrlChan():
		if !ok {
			return msg, false
		}
		if msg.Type == expectedType {
			return msg, true
		}
		if msg.Type == "error" {
			slog.Error("relay error", "msg", msg.Message)
			return msg, false
		}
		return msg, false
	case <-time.After(10 * time.Second):
		slog.Error("timeout waiting for", "type", expectedType)
		return protocol.CtrlMsg{}, false
	}
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing != nil {
		close(c.closing)
	}
	if c.mux != nil {
		c.mux.Conn().Close()
		c.mux = nil
	}
	return nil
}

func (c *Client) Stop() {
	c.Close()
}

func LoadClientConfig(dir string) (string, string, string, []TunnelInfo, error) {
	path := filepath.Join(dir, "tunnel.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", "", nil, err
	}
	var cfg struct {
		RelayAddr string       `json:"relay_addr"`
		User      string       `json:"user,omitempty"`
		Password  string       `json:"password,omitempty"`
		Tunnels   []TunnelInfo `json:"tunnels"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", "", "", nil, err
	}
	return cfg.RelayAddr, cfg.User, cfg.Password, cfg.Tunnels, nil
}

func SaveClientConfig(dir, relayAddr, user, password string, tunnels []TunnelInfo) error {
	cfg := struct {
		RelayAddr string       `json:"relay_addr"`
		User      string       `json:"user,omitempty"`
		Password  string       `json:"password,omitempty"`
		Tunnels   []TunnelInfo `json:"tunnels"`
	}{RelayAddr: relayAddr, User: user, Password: password, Tunnels: tunnels}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "tunnel.json")
	tmp := path + ".tmp"
	os.WriteFile(tmp, data, 0o600) //nolint:errcheck
	return os.Rename(tmp, path)
}
