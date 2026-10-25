// Package relay maintains the central domain registry and HTTP routing table.
// Its control API is available only through a private Unix socket, via SSH.
package relay

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/relayprotocol"
)

const LeaseDuration = 90 * time.Second

type Binding struct {
	LeaseID     string    `json:"-"`
	Owner       string    `json:"owner"`
	Port        int       `json:"port"`
	Scheme      string    `json:"scheme"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	Active      bool      `json:"-"`
	Expires     time.Time `json:"-"`
}
type Server struct {
	mu            sync.RWMutex
	zone, dir     string
	zones         []string
	bindings      map[string]Binding
	inventory     *Inventory
	control, http *http.Server
	cancel        context.CancelFunc
	lock          *config.InstanceLock
	checkPort     func(int) error
}

func New(dir, zone string) (*Server, error) {
	var err error
	zone, err = relayprotocol.ValidateZone(zone)
	if err != nil {
		return nil, err
	}
	_, lock, err := config.AcquireInstanceLock(dir)
	if err != nil {
		return nil, err
	}
	inventory, err := NewInventory(dir)
	if err != nil {
		lock.Release()
		return nil, err
	}
	s := &Server{zone: zone, zones: []string{zone}, dir: dir, bindings: map[string]Binding{}, inventory: inventory, lock: lock, checkPort: LoopbackOnly}
	if err := s.loadZones(); err != nil {
		lock.Release()
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "domains.json"))
	if err == nil {
		err = json.Unmarshal(data, &s.bindings)
	}
	if err != nil && !os.IsNotExist(err) {
		lock.Release()
		return nil, err
	}
	for host, b := range s.bindings {
		if !s.acceptsHost(host) || b.Owner == "" {
			lock.Release()
			return nil, fmt.Errorf("invalid saved domain registry")
		}
		b.Active = false
		b.Expires = time.Time{}
		s.bindings[host] = b
	}
	return s, nil
}
func (s *Server) save(next map[string]Binding) error {
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".domains-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(s.dir, "domains.json"))
}
func (s *Server) Apply(req relayprotocol.Request) (relayprotocol.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	response := relayprotocol.Response{Zone: s.zone, Zones: append([]string(nil), s.zones...)}
	if req.Operation == "configure_zones" {
		if err := s.configureZones(req.Zones, req.Zone); err != nil {
			return response, err
		}
		return relayprotocol.Response{Zone: s.zone, Zones: append([]string(nil), s.zones...)}, nil
	}
	if req.Operation == "info" {
		return response, nil
	}
	if req.Operation == "tunnel_check" {
		var err error
		response.Disabled, err = s.inventory.Check(req.Owner)
		return response, err
	}
	if req.Operation == "tunnel_snapshot" {
		var err error
		response.DisabledOwners, err = s.inventory.Snapshot(req.ClientID, req.ClientName, req.Mappings, req.RecoveryProtocol)
		if err == nil {
			err = s.inventory.Acknowledge(req.ClientID, req.RecoveryResults)
		}
		if err == nil {
			response.RecoveryCommands = s.inventory.RecoveryCommands(req.ClientID)
		}
		return response, err
	}
	if len(req.Owner) < 16 || len(req.Owner) > 512 {
		return response, fmt.Errorf("invalid mapping owner")
	}
	zone := req.Zone
	if zone == "" {
		zone = s.zone
	}
	if !s.acceptsZone(zone) {
		return response, fmt.Errorf("中转站域名区域为 %s，与客户端配置不一致", s.zone)
	}
	prefixes, err := relayprotocol.Normalize(req.Prefixes)
	if err != nil {
		return response, err
	}
	hosts := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		hosts = append(hosts, prefix+"."+zone)
	}
	response.Domains = hosts
	switch req.Operation {
	case "check", "reserve":
		if len(hosts) == 0 {
			return response, fmt.Errorf("at least one domain prefix is required")
		}
		if req.Port < 1 || req.Port > 65535 {
			return response, fmt.Errorf("invalid upstream port")
		}
		if req.Scheme != "http" && req.Scheme != "https" {
			return response, fmt.Errorf("upstream scheme must be http or https")
		}
		for _, host := range hosts {
			if old, exists := s.bindings[host]; exists && old.Owner != req.Owner {
				return response, fmt.Errorf("域名 %s 已绑定其他隧道，不能重复绑定", host)
			}
		}
		if req.Operation == "check" {
			return response, nil
		}
		next := make(map[string]Binding, len(s.bindings)+len(hosts))
		for host, b := range s.bindings {
			if b.Owner != req.Owner {
				next[host] = b
			}
		}
		for _, host := range hosts {
			b := Binding{Owner: req.Owner, Port: req.Port, Scheme: req.Scheme}
			if old, exists := s.bindings[host]; exists && old.Owner == req.Owner && old.Port == req.Port && old.Scheme == req.Scheme {
				b = old
			}
			next[host] = b
		}
		if err := s.save(next); err != nil {
			return response, err
		}
		s.bindings = next
	case "activate", "renew":
		if len(req.LeaseID) < 16 {
			return response, fmt.Errorf("missing lease identity")
		}
		if len(hosts) == 0 {
			return response, fmt.Errorf("no domains to activate")
		}
		for _, host := range hosts {
			b, exists := s.bindings[host]
			if !exists || b.Owner != req.Owner || b.Port != req.Port || b.Scheme != req.Scheme {
				return response, fmt.Errorf("域名 %s 未由当前隧道持有，或隧道配置已改变", host)
			}
		}
		if req.Operation == "renew" {
			for _, host := range hosts {
				if s.bindings[host].LeaseID != "" && s.bindings[host].LeaseID != req.LeaseID {
					return response, fmt.Errorf("domain lease replaced")
				}
			}
		}
		if err := s.checkPort(req.Port); err != nil {
			return response, err
		}
		if req.Scheme == "https" {
			fingerprint, err := hex.DecodeString(req.Fingerprint)
			if err != nil || len(fingerprint) != 32 {
				return response, fmt.Errorf("HTTPS upstream requires a SHA-256 certificate pin")
			}
		}
		for _, host := range hosts {
			b := s.bindings[host]
			b.LeaseID = req.LeaseID
			b.Active = true
			b.Expires = time.Now().Add(LeaseDuration)
			b.Fingerprint = req.Fingerprint
			s.bindings[host] = b
		}
	case "deactivate":
		for host, b := range s.bindings {
			if b.Owner == req.Owner && b.LeaseID == req.LeaseID {
				b.Active = false
				b.LeaseID = "revoked:" + req.LeaseID
				b.Expires = time.Time{}
				s.bindings[host] = b
			}
		}
	case "release":
		next := map[string]Binding{}
		for host, b := range s.bindings {
			if b.Owner != req.Owner {
				next[host] = b
			}
		}
		if err := s.save(next); err != nil {
			return response, err
		}
		s.bindings = next
	default:
		return response, fmt.Errorf("unknown relay operation")
	}
	return response, nil
}
func (s *Server) ControlHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "POST" || r.URL.Path != "/control" {
			w.WriteHeader(405)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req relayprotocol.Request
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(relayprotocol.Response{Error: "invalid request"})
			return
		}
		if dec.Decode(new(any)) != io.EOF {
			w.WriteHeader(400)
			return
		}
		response, err := s.Apply(req)
		if err != nil {
			response.Error = err.Error()
			w.WriteHeader(409)
		}
		json.NewEncoder(w).Encode(response)
	})
}
func (s *Server) ProxyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		// Exactly the registered host must be used; unknown names and IPs never fall back.
		s.mu.RLock()
		b, exists := s.bindings[host]
		s.mu.RUnlock()
		if !exists {
			http.NotFound(w, r)
			return
		}
		if disabled, _ := s.inventory.Check(b.Owner); disabled {
			http.Error(w, "tunnel disabled", http.StatusForbidden)
			return
		}
		if r.Header.Get("X-Forwarded-Proto") == "http" {
			http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
			return
		}
		if !b.Active || !time.Now().Before(b.Expires) {
			http.Error(w, "tunnel is inactive", http.StatusServiceUnavailable)
			return
		}
		upstream := &url.URL{Scheme: b.Scheme, Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(b.Port))}
		proxy := httputil.NewSingleHostReverseProxy(upstream)
		original := proxy.Director
		proxy.Director = func(out *http.Request) {
			original(out)
			out.Host = r.Host
			out.Header.Del("Forwarded")
			out.Header.Set("X-Forwarded-Host", r.Host)
			if r.Header.Get("X-Forwarded-Proto") == "https" {
				out.Header.Set("X-Forwarded-Proto", "https")
			} else {
				out.Header.Set("X-Forwarded-Proto", "http")
			}
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		transport.ResponseHeaderTimeout = 30 * time.Second
		transport.DisableKeepAlives = true
		if b.Scheme == "https" {
			pin, err := hex.DecodeString(b.Fingerprint)
			if err != nil || len(pin) != 32 {
				http.Error(w, "invalid upstream TLS pin", 502)
				return
			}
			transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return fmt.Errorf("missing upstream certificate")
				}
				got := sha256.Sum256(cs.PeerCertificates[0].Raw)
				if subtle.ConstantTimeCompare(got[:], pin) != 1 {
					return fmt.Errorf("upstream certificate pin mismatch")
				}
				return nil
			}}
		}
		proxy.Transport = transport
		defer transport.CloseIdleConnections()
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("domain upstream unavailable", "host", host, "err", err)
			http.Error(w, "tunnel upstream unavailable", http.StatusBadGateway)
		}
		proxy.ServeHTTP(w, r)
	})
}
func (s *Server) Listen(addr, socket string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("relay HTTP listener must be loopback")
	}
	tcp, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		tcp.Close()
		return err
	}
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			tcp.Close()
			return fmt.Errorf("refusing to replace non-socket")
		}
		os.Remove(socket)
	}
	unix, err := net.Listen("unix", socket)
	if err != nil {
		tcp.Close()
		return err
	}
	if err := os.Chmod(socket, 0600); err != nil {
		tcp.Close()
		unix.Close()
		return err
	}
	s.http = &http.Server{Handler: s.ProxyHandler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	s.control = &http.Server{Handler: s.ControlHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 20 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go s.inventory.Monitor(ctx)
	go s.http.Serve(tcp)
	go s.control.Serve(unix)
	return nil
}
func (s *Server) Close() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.control != nil {
		s.control.Close()
	}
	if s.http != nil {
		s.http.Close()
	}
	if s.lock != nil {
		s.lock.Release()
	}
}

func (s *Server) Inventory() *Inventory { return s.inventory }
func Request(ctx context.Context, socket string, req relayprotocol.Request) (relayprotocol.Response, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	reader, writer := io.Pipe()
	go func() { err := json.NewEncoder(writer).Encode(req); writer.CloseWithError(err) }()
	defer reader.Close()
	r, err := http.NewRequestWithContext(ctx, "POST", "http://unix/control", reader)
	if err != nil {
		return relayprotocol.Response{}, err
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: transport, Timeout: 15 * time.Second}).Do(r)
	if err != nil {
		return relayprotocol.Response{}, err
	}
	defer resp.Body.Close()
	var response relayprotocol.Response
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&response); err != nil {
		return response, err
	}
	if resp.StatusCode != 200 || response.Error != "" {
		return response, fmt.Errorf("%s", response.Error)
	}
	return response, nil
}
