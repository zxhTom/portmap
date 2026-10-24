package serverui

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zxhtom/portmap/internal/relay"
	"github.com/zxhtom/portmap/internal/relayprotocol"
	"github.com/zxhtom/portmap/internal/websession"
	"golang.org/x/crypto/bcrypt"
)

//go:embed static/*
var assets embed.FS

type session struct {
	Expires time.Time
	CSRF    string
}
type attempt struct {
	Count int
	Since time.Time
}
type Server struct {
	control        func(relayprotocol.Request) (relayprotocol.Response, error)
	mu             sync.Mutex
	dir            string
	inventory      *relay.Inventory
	username, hash string
	sm             *websession.Manager
	attempts       map[string]attempt
	http           *http.Server
	ln             net.Listener
}

func token() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func privateFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".server-web-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
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
	return os.Rename(f.Name(), path)
}

func New(dir string, inventory *relay.Inventory) (*Server, error) {
	s := &Server{dir: dir, inventory: inventory, attempts: map[string]attempt{}}
	path := filepath.Join(dir, "server-web-auth.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		password, err := token()
		if err != nil {
			return nil, err
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return nil, err
		}
		data, _ = json.Marshal(map[string]string{"username": "admin", "password_hash": string(hash)})
		if err = privateFile(filepath.Join(dir, "server-web-initial-password"), []byte(password+"\n")); err != nil {
			return nil, err
		}
		if err = privateFile(path, data); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	var creds struct {
		Username string `json:"username"`
		Hash     string `json:"password_hash"`
	}
	if err = json.Unmarshal(data, &creds); err != nil {
		return nil, err
	}
	if creds.Username == "" {
		return nil, fmt.Errorf("empty server web username")
	}
	if _, err = bcrypt.Cost([]byte(creds.Hash)); err != nil {
		return nil, fmt.Errorf("invalid server web password hash")
	}
	s.username, s.hash = creds.Username, creds.Hash
	fp := websession.CredentialFingerprint(s.username, s.hash)
	sm, err := websession.New(filepath.Join(dir, "server-remember-sessions.json"), fp)
	if err != nil {
		return nil, fmt.Errorf("init server web session manager: %w", err)
	}
	s.sm = sm
	return s, nil
}

func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]string{"error": message})
}
func trustedProxy(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	return err == nil && net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}
func browserHTTPS(r *http.Request) bool {
	return r.TLS != nil || trustedProxy(r) && r.Header.Get("X-Forwarded-Proto") == "https"
}
func originOK(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	scheme := "http"
	if browserHTTPS(r) {
		scheme = "https"
	}
	return origin == scheme+"://"+r.Host
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("invalid JSON body")
	}
	return nil
}
func (s *Server) authenticated(r *http.Request) (websession.Session, bool) {
	c, err := r.Cookie("portmap_server")
	if err != nil {
		return websession.Session{}, false
	}
	return s.sm.Get(c.Value)
}

func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	if r.URL.Path == "/auth/login" {
		s.login(w, r)
		return
	}
	if r.URL.Path == "/" || r.URL.Path == "/app.js" || r.URL.Path == "/style.css" || r.URL.Path == "/favicon.svg" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			fail(w, 405, "method not allowed")
			return
		}
		name := r.URL.Path
		if name == "/" {
			name = "/index.html"
		}
		data, err := assets.ReadFile("static" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
		return
	}
	sess, ok := s.authenticated(r)
	if !ok {
		fail(w, 401, "请先登录")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if !originOK(r) || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(sess.CSRF)) != 1 {
			fail(w, 403, "invalid request token")
			return
		}
	}
	switch {
	case r.URL.Path == "/auth/session" && r.Method == http.MethodGet:
		respond(w, 200, map[string]any{"username": s.username, "csrf_token": sess.CSRF, "expires_at": sess.Expires})
	case r.URL.Path == "/auth/logout" && r.Method == http.MethodPost:
		if c, err := r.Cookie("portmap_server"); err == nil {
			if err := s.sm.Delete(c.Value); err != nil {
				fail(w, 500, "退出失败，请稍后重试")
				return
			}
		}
		http.SetCookie(w, &http.Cookie{Name: "portmap_server", Value: "", Path: "/", HttpOnly: true, Secure: browserHTTPS(r), SameSite: http.SameSiteStrictMode, MaxAge: -1})
		respond(w, 200, map[string]bool{"ok": true})
	case r.URL.Path == "/api/domain-zones" && (r.Method == http.MethodGet || r.Method == http.MethodPut):
		if s.control == nil {
			fail(w, 503, "服务端域名配置不可用")
			return
		}
		req := relayprotocol.Request{Operation: "info"}
		if r.Method == http.MethodPut {
			var body struct {
				Zones   []string `json:"domain_zones"`
				Primary string   `json:"domain_zone"`
			}
			if err := decode(w, r, &body); err != nil {
				fail(w, 400, "invalid JSON")
				return
			}
			req.Operation = "configure_zones"
			req.Zones = body.Zones
			req.Zone = body.Primary
		}
		response, err := s.control(req)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		respond(w, 200, map[string]any{"domain_zone": response.Zone, "domain_zones": response.Zones})
	case r.URL.Path == "/api/tunnels" && r.Method == http.MethodGet:
		respond(w, 200, s.inventory.List())
	case strings.HasPrefix(r.URL.Path, "/api/tunnels/") && r.Method == http.MethodPost:
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/tunnels/"), "/")
		if len(parts) != 2 || parts[0] == "" {
			http.NotFound(w, r)
			return
		}
		if parts[1] == "recover" {
			if err := s.inventory.RequestRecovery(parts[0]); err != nil {
				fail(w, 400, err.Error())
				return
			}
			respond(w, 202, map[string]string{"message": "等待客户端接收恢复请求"})
			return
		}
		var disabled bool
		if parts[1] == "disable" {
			disabled = true
		} else if parts[1] != "enable" {
			http.NotFound(w, r)
			return
		}
		if err := s.inventory.SetDisabled(parts[0], disabled); err != nil {
			fail(w, 400, err.Error())
			return
		}
		respond(w, 200, map[string]bool{"disabled": disabled})
	default:
		if r.URL.Path == "/api/tunnels" || strings.HasPrefix(r.URL.Path, "/api/tunnels/") {
			fail(w, 405, "method not allowed")
			return
		}
		http.NotFound(w, r)
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "method not allowed")
		return
	}
	if !originOK(r) {
		fail(w, 403, "invalid origin")
		return
	}
	var body struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		RememberMe bool   `json:"remember_me"`
	}
	if err := decode(w, r, &body); err != nil || len(body.Password) > 72 {
		fail(w, 400, "invalid credentials")
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if trustedProxy(r) {
		if realIP := net.ParseIP(r.Header.Get("X-Real-IP")); realIP != nil {
			ip = realIP.String()
		}
	}
	s.mu.Lock()
	now := time.Now()
	for k, a := range s.attempts {
		if now.Sub(a.Since) > 15*time.Minute {
			delete(s.attempts, k)
		}
	}
	a := s.attempts[ip]
	if a.Count >= 5 || len(s.attempts) > 1024 {
		s.mu.Unlock()
		fail(w, 429, "尝试次数过多")
		return
	}
	if body.Username != s.username || bcrypt.CompareHashAndPassword([]byte(s.hash), []byte(body.Password)) != nil {
		if a.Count == 0 {
			a.Since = now
		}
		a.Count++
		s.attempts[ip] = a
		s.mu.Unlock()
		fail(w, 401, "账号或密码错误")
		return
	}
	delete(s.attempts, ip)
	secret, _, _, maxAge, err := s.sm.CreateSession(body.RememberMe)
	s.mu.Unlock()
	if err != nil {
		if err.Error() == "too many active sessions" {
			fail(w, 429, "登录会话过多，请稍后重试")
			return
		}
		fail(w, 500, "cannot create session")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "portmap_server",
		Value:    secret,
		Path:     "/",
		HttpOnly: true,
		Secure:   browserHTTPS(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   maxAge,
	})
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *Server) Listen(addr, cert, key string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if (cert == "") != (key == "") {
		return fmt.Errorf("both certificate and key are required")
	}
	if cert == "" {
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("server console without TLS must listen on loopback")
		}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	if cert != "" {
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			ln.Close()
			return err
		}
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
	}
	s.ln = ln
	s.http = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	go s.http.Serve(ln)
	return nil
}
func (s *Server) Close() {
	if s.http != nil {
		_ = s.http.Close()
	} else if s.ln != nil {
		_ = s.ln.Close()
	}
}

func (s *Server) SetControl(fn func(relayprotocol.Request) (relayprotocol.Response, error)) {
	s.control = fn
}
