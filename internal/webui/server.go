// Package webui serves the authenticated browser console separately from the local CLI API.
package webui

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/manager"
	"golang.org/x/crypto/bcrypt"
)

//go:embed static/*
var assets embed.FS

type credentials struct {
	Username string `json:"username"`
	Hash     string `json:"password_hash"`
}
type session struct {
	Expires time.Time
	CSRF    string
}
type attempt struct {
	Count int
	Since time.Time
}
type Server struct {
	mu       sync.Mutex
	dir      string
	creds    credentials
	sessions map[[32]byte]session
	attempts map[string]attempt
	api      http.Handler
	mgr      *manager.Manager
	http     *http.Server
}

func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func writePrivate(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".web-*")
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
	return os.Rename(name, path)
}

func New(mgr *manager.Manager, api http.Handler) (*Server, error) {
	s := &Server{dir: mgr.DataDir(), mgr: mgr, api: api, sessions: make(map[[32]byte]session), attempts: make(map[string]attempt)}
	path := filepath.Join(s.dir, "web-auth.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		password, err := randomToken()
		if err != nil {
			return nil, err
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return nil, err
		}
		s.creds = credentials{Username: "admin", Hash: string(hash)}
		data, _ = json.MarshalIndent(s.creds, "", "  ")
		if err := writePrivate(filepath.Join(s.dir, "web-initial-password"), []byte(password+"\n")); err != nil {
			return nil, err
		}
		if err := writePrivate(path, data); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if err = json.Unmarshal(data, &s.creds); err != nil {
		return nil, fmt.Errorf("invalid web credentials")
	}
	if s.creds.Username == "" {
		return nil, fmt.Errorf("empty web username")
	}
	if _, err := bcrypt.Cost([]byte(s.creds.Hash)); err != nil {
		return nil, fmt.Errorf("invalid web password hash")
	}
	return s, nil
}

func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, message string) {
	jsonResponse(w, status, map[string]string{"error": message})
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
func sameOrigin(r *http.Request) bool {
	u, err := url.Parse(r.Header.Get("Origin"))
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return err == nil && u.Scheme == scheme && u.Host == r.Host && u.User == nil
}

// Cookies are not isolated by TCP port. Namespace by the browser-facing host
// so local and SSH-forwarded consoles can remain logged in side by side.
func (s *Server) cookieName(r *http.Request) string {
	digest := sha256.Sum256([]byte(strings.ToLower(r.Host)))
	return "portmap_" + hex.EncodeToString(digest[:6])
}
func (s *Server) authenticated(r *http.Request) (session, bool) {
	c, err := r.Cookie(s.cookieName(r))
	if err != nil {
		return session{}, false
	}
	key := sha256.Sum256([]byte(c.Value))
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.sessions[key]
	if !ok || !time.Now().Before(v.Expires) {
		delete(s.sessions, key)
		return session{}, false
	}
	return v, true
}
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	if r.URL.Path == "/auth/login" {
		if r.Method != http.MethodPost {
			fail(w, 405, "method not allowed")
			return
		}
		if !sameOrigin(r) {
			fail(w, 403, "invalid origin")
			return
		}
		s.login(w, r)
		return
	}
	if r.URL.Path == "/" || r.URL.Path == "/login" || strings.HasPrefix(r.URL.Path, "/assets/") {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			fail(w, 405, "method not allowed")
			return
		}
		path := "index.html"
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			path = strings.TrimPrefix(r.URL.Path, "/assets/")
		}
		if path != "index.html" && path != "app.js" && path != "styles.css" {
			http.NotFound(w, r)
			return
		}
		data, err := assets.ReadFile("static/" + path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, path, time.Time{}, bytes.NewReader(data))
		return
	}
	sess, ok := s.authenticated(r)
	if !ok {
		fail(w, 401, "请先登录")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if !sameOrigin(r) || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(sess.CSRF)) != 1 {
			fail(w, 403, "invalid request token")
			return
		}
	}
	switch r.URL.Path {
	case "/auth/session":
		if r.Method != http.MethodGet {
			fail(w, 405, "method not allowed")
			return
		}
		s.mu.Lock()
		name := s.creds.Username
		s.mu.Unlock()
		hostname, _ := os.Hostname()
		jsonResponse(w, 200, map[string]any{"username": name, "csrf_token": sess.CSRF, "expires_at": sess.Expires, "hostname": hostname})
	case "/auth/logout":
		if r.Method != http.MethodPost {
			fail(w, 405, "method not allowed")
			return
		}
		c, _ := r.Cookie(s.cookieName(r))
		s.mu.Lock()
		delete(s.sessions, sha256.Sum256([]byte(c.Value)))
		s.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: s.cookieName(r), Value: "", Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		jsonResponse(w, 200, map[string]bool{"ok": true})
	case "/auth/password":
		if r.Method != http.MethodPut {
			fail(w, 405, "method not allowed")
			return
		}
		s.changePassword(w, r)
	case "/api/remotes":
		if r.Method == http.MethodGet {
			jsonResponse(w, 200, s.mgr.RemoteSummaries())
			return
		}
		if r.Method != http.MethodPost {
			fail(w, 405, "method not allowed")
			return
		}
		var remote config.Remote
		if err := decode(w, r, &remote); err != nil {
			fail(w, 400, "服务器参数格式不正确")
			return
		}
		if remote.Name == "" || strings.Contains(remote.Name, "/") || remote.Host == "" || remote.User == "" || remote.Port < 1 || remote.Port > 65535 || (remote.Auth.Method != "key" && remote.Auth.Method != "password") {
			fail(w, 400, "请检查节点名称、地址、端口和认证方式")
			return
		}
		if err := s.mgr.AddRemote(remote); err != nil {
			fail(w, 400, err.Error())
			return
		}
		jsonResponse(w, 200, map[string]string{"message": "server added"})
	default:
		// Do not expose process identities, filesystem paths or future unreviewed endpoints.
		if r.URL.Path == "/api/settings/domain-zone" || r.URL.Path == "/api/mappings" || strings.HasPrefix(r.URL.Path, "/api/mappings/") || strings.HasPrefix(r.URL.Path, "/api/remotes/") || strings.HasPrefix(r.URL.Path, "/api/groups/") || r.URL.Path == "/api/docker/containers" || r.URL.Path == "/api/multipass/instances" {
			r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
			s.api.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &req); err != nil || len(req.Password) > 72 {
		fail(w, 400, "请输入有效的账号和密码")
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for key, a := range s.attempts {
		if now.Sub(a.Since) > 15*time.Minute {
			delete(s.attempts, key)
		}
	}
	a := s.attempts[ip]
	if a.Count >= 5 {
		w.Header().Set("Retry-After", "900")
		fail(w, 429, "尝试次数过多，请 15 分钟后重试")
		return
	}
	if len(s.attempts) > 1024 {
		fail(w, 429, "请稍后重试")
		return
	}
	passwordOK := bcrypt.CompareHashAndPassword([]byte(s.creds.Hash), []byte(req.Password)) == nil
	if req.Username != s.creds.Username || !passwordOK {
		if a.Count == 0 {
			a.Since = now
		}
		a.Count++
		s.attempts[ip] = a
		fail(w, 401, "账号或密码不正确")
		return
	}
	delete(s.attempts, ip)
	for key, v := range s.sessions {
		if !now.Before(v.Expires) {
			delete(s.sessions, key)
		}
	}
	if len(s.sessions) >= 32 {
		fail(w, 429, "登录会话过多，请稍后重试")
		return
	}
	token, err := randomToken()
	if err != nil {
		fail(w, 500, "无法创建会话")
		return
	}
	csrf, err := randomToken()
	if err != nil {
		fail(w, 500, "无法创建会话")
		return
	}
	s.sessions[sha256.Sum256([]byte(token))] = session{Expires: now.Add(12 * time.Hour), CSRF: csrf}
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(r), Value: token, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 12 * 3600})
	jsonResponse(w, 200, map[string]bool{"ok": true})
}
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := decode(w, r, &req); err != nil || len(req.New) < 12 || len(req.New) > 72 {
		fail(w, 400, "新密码须为 12–72 字节")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if bcrypt.CompareHashAndPassword([]byte(s.creds.Hash), []byte(req.Current)) != nil {
		fail(w, 400, "当前密码不正确")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.New), bcrypt.DefaultCost)
	if err != nil {
		fail(w, 500, "无法更新密码")
		return
	}
	next := credentials{Username: s.creds.Username, Hash: string(hash)}
	data, _ := json.MarshalIndent(next, "", "  ")
	if err := writePrivate(filepath.Join(s.dir, "web-auth.json"), data); err != nil {
		fail(w, 500, "无法保存密码")
		return
	}
	s.creds = next
	s.sessions = make(map[[32]byte]session)
	os.Remove(filepath.Join(s.dir, "web-initial-password"))
	jsonResponse(w, 200, map[string]bool{"ok": true})
}
func (s *Server) Listen(addr, cert, key string) error {
	if addr == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if cert == "" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("public web listener requires --web-cert and --web-key (or use loopback with SSH forwarding)")
	}
	if (cert == "") != (key == "") {
		return fmt.Errorf("both web certificate and key are required")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.http = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	if cert != "" { // Validate before starting the goroutine.
		ln.Close()
		return s.listenTLS(addr, cert, key)
	}
	go s.http.Serve(ln)
	return nil
}
func (s *Server) Close() {
	if s.http != nil {
		s.http.Close()
	}
}

// ResetPassword is an offline recovery tool. The running daemon must be restarted
// to load it, which also revokes all in-memory sessions.
func ResetPassword(dir, password string) error {
	if len(password) < 12 || len(password) > 72 {
		return fmt.Errorf("password must be 12–72 bytes")
	}
	data, err := os.ReadFile(filepath.Join(dir, "web-auth.json"))
	if err != nil {
		return err
	}
	var c credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	c.Hash = string(hash)
	data, _ = json.MarshalIndent(c, "", "  ")
	if err := writePrivate(filepath.Join(dir, "web-auth.json"), data); err != nil {
		return err
	}
	os.Remove(filepath.Join(dir, "web-initial-password"))
	return nil
}
