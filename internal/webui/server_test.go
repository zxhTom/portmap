package webui

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zxhtom/portmap/internal/api"
	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/manager"
	"github.com/zxhtom/portmap/internal/relayprotocol"
)

func fixture(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	m := manager.New(dir)
	t.Cleanup(m.Shutdown)
	if err := m.AddRemote(config.Remote{Name: "test", Host: "127.0.0.1", Port: 22, User: "test-user", Auth: config.AuthConfig{Method: "password", Password: "never-return-this-secret", KeyPath: "/private/key"}}); err != nil {
		t.Fatal(err)
	}
	s, err := New(m, api.NewServer(m).Handler())
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "web-initial-password"))
	if err != nil {
		t.Fatal(err)
	}
	return s, strings.TrimSpace(string(b))
}
func call(s *Server, method, path string, body any, cookie *http.Cookie, csrf, origin string) *httptest.ResponseRecorder {
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, "http://localhost"+path, bytes.NewReader(b))
	r.Header.Set("Origin", origin)
	r.Header.Set("X-CSRF-Token", csrf)
	r.RemoteAddr = "127.0.0.1:1234"
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func loginTest(t *testing.T, s *Server, password string) (*http.Cookie, string) {
	t.Helper()
	w := call(s, "POST", "/auth/login", map[string]string{"username": "admin", "password": password}, nil, "", "http://localhost")
	if w.Code != 200 {
		t.Fatalf("login: %d", w.Code)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing cookie")
	}
	c := cookies[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.MaxAge != 43200 {
		t.Fatal("unsafe session cookie")
	}
	w = call(s, "GET", "/auth/session", nil, c, "", "")
	var v map[string]any
	json.Unmarshal(w.Body.Bytes(), &v)
	csrf, _ := v["csrf_token"].(string)
	if csrf == "" {
		t.Fatal("missing csrf token")
	}
	return c, csrf
}
func TestAuthenticationOriginCSRFAndSecretRedaction(t *testing.T) {
	s, pw := fixture(t)
	for _, path := range []string{"/api/mappings", "/api/remotes", "/auth/session", "/api/groups/default/start"} {
		if w := call(s, "GET", path, nil, nil, "", ""); w.Code != 401 {
			t.Fatalf("unprotected %s", path)
		}
	}
	for _, path := range []string{"/", "/login", "/assets/styles.css", "/assets/app.js"} {
		w := call(s, "GET", path, nil, nil, "", "")
		if w.Code != 200 || w.Header().Get("Content-Security-Policy") == "" {
			t.Fatalf("asset %s: %d", path, w.Code)
		}
	}
	if w := call(s, "POST", "/auth/login", map[string]string{"username": "admin", "password": pw}, nil, "", "http://evil.test"); w.Code != 403 {
		t.Fatal("cross-origin login allowed")
	}
	c, csrf := loginTest(t, s, pw)
	for _, params := range [][2]string{{"", "http://localhost"}, {csrf, "http://evil.test"}} {
		if w := call(s, "POST", "/auth/logout", nil, c, params[0], params[1]); w.Code != 403 {
			t.Fatal("mutation bypassed csrf/origin")
		}
	}
	w := call(s, "GET", "/api/remotes", nil, c, "", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "never-return") || strings.Contains(w.Body.String(), "/private/key") {
		t.Fatal("remote secrets leaked")
	}
	w = call(s, "GET", "/api/health", nil, c, "", "")
	if w.Code != 404 {
		t.Fatal("internal process endpoint exposed")
	}
	w = call(s, "POST", "/auth/logout", nil, c, csrf, "http://localhost")
	if w.Code != 200 {
		t.Fatal("logout failed")
	}
	if w = call(s, "GET", "/api/mappings", nil, c, "", ""); w.Code != 401 {
		t.Fatal("logout left session active")
	}
}

func TestDomainZoneSettingRequiresSessionAndCSRF(t *testing.T) {
	s, pw := fixture(t)
	if w := call(s, "GET", "/api/settings/domain-zone", nil, nil, "", ""); w.Code != 401 {
		t.Fatalf("unauthenticated setting read: %d", w.Code)
	}
	c, csrf := loginTest(t, s, pw)
	if w := call(s, "PUT", "/api/settings/domain-zone", map[string]string{"domain_zone": "example.test"}, c, "", "http://localhost"); w.Code != 403 {
		t.Fatalf("setting changed without CSRF: %d", w.Code)
	}
	if w := call(s, "PUT", "/api/settings/domain-zone", map[string]string{"domain_zone": "example.test"}, c, csrf, "http://localhost"); w.Code != 200 {
		t.Fatalf("setting change failed: %d %s", w.Code, w.Body.String())
	}
	if w := call(s, "GET", "/api/settings/domain-zone", nil, c, "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "example.test") {
		t.Fatalf("setting not readable: %d %s", w.Code, w.Body.String())
	}
}
func TestPasswordChangeInvalidatesSessionsAndPersists(t *testing.T) {
	s, pw := fixture(t)
	c, csrf := loginTest(t, s, pw)
	c2, _ := loginTest(t, s, pw)
	w := call(s, "PUT", "/auth/password", map[string]string{"current_password": pw, "new_password": "new-console-password-2026"}, c, csrf, "http://localhost")
	if w.Code != 200 {
		t.Fatalf("password change %d", w.Code)
	}
	for _, c := range []*http.Cookie{c, c2} {
		if w := call(s, "GET", "/auth/session", nil, c, "", ""); w.Code != 401 {
			t.Fatal("old session survives password change")
		}
	}
	if _, err := os.Stat(filepath.Join(s.dir, "web-initial-password")); !os.IsNotExist(err) {
		t.Fatal("plaintext bootstrap password retained")
	}
	data, _ := os.ReadFile(filepath.Join(s.dir, "web-auth.json"))
	if bytes.Contains(data, []byte("new-console-password")) {
		t.Fatal("password stored in plaintext")
	}
	st, _ := os.Stat(filepath.Join(s.dir, "web-auth.json"))
	if st.Mode().Perm() != 0600 {
		t.Fatal("credentials not private")
	}
	restarted, err := New(s.mgr, s.api)
	if err != nil {
		t.Fatal(err)
	}
	loginTest(t, restarted, "new-console-password-2026")
	w = call(restarted, "POST", "/auth/login", map[string]string{"username": "admin", "password": pw}, nil, "", "http://localhost")
	if w.Code != 401 {
		t.Fatal("old password still works")
	}
}
func TestRateLimitAndSessionExpiry(t *testing.T) {
	s, pw := fixture(t)
	c, _ := loginTest(t, s, pw)
	_ = s.sm.Reset()
	if w := call(s, "GET", "/auth/session", nil, c, "", ""); w.Code != 401 {
		t.Fatal("expired session accepted")
	}
	for i := 0; i < 5; i++ {
		w := call(s, "POST", "/auth/login", map[string]string{"username": "admin", "password": "wrong"}, nil, "", "http://localhost")
		if w.Code != 401 {
			t.Fatal("unexpected auth response")
		}
	}
	w := call(s, "POST", "/auth/login", map[string]string{"username": "admin", "password": pw}, nil, "", "http://localhost")
	if w.Code != 429 {
		t.Fatal("login not rate limited")
	}
}
func TestPublicHTTPRejectedAndRemoteCreationNeverReflectsPassword(t *testing.T) {
	s, pw := fixture(t)
	if err := s.Listen("0.0.0.0:0", "", ""); err == nil {
		s.Close()
		t.Fatal("public cleartext listener accepted")
	}
	c, csrf := loginTest(t, s, pw)
	remote := config.Remote{Name: "added", Host: "127.0.0.2", Port: 22, User: "test", Auth: config.AuthConfig{Method: "password", Password: "do-not-reflect"}}
	w := call(s, "POST", "/api/remotes", remote, c, csrf, "http://localhost")
	if w.Code != 200 || strings.Contains(w.Body.String(), "do-not-reflect") {
		t.Fatal("remote creation leaked secret or failed")
	}
	w = call(s, "GET", "/assets/../../web-auth.json", nil, c, "", "")
	b, _ := io.ReadAll(w.Result().Body)
	if bytes.Contains(b, []byte("password_hash")) {
		t.Fatal("path traversal exposed credentials")
	}
}

func TestOfflinePasswordRecovery(t *testing.T) {
	s, _ := fixture(t)
	if err := ResetPassword(s.dir, "short"); err == nil {
		t.Fatal("weak reset password accepted")
	}
	if err := ResetPassword(s.dir, "recovered-password-2026"); err != nil {
		t.Fatal(err)
	}
	next, err := New(s.mgr, s.api)
	if err != nil {
		t.Fatal(err)
	}
	loginTest(t, next, "recovered-password-2026")
}

func TestCookieNamespacesKeepForwardedConsolesIndependent(t *testing.T) {
	s, pw := fixture(t)
	cookie, _ := loginTest(t, s, pw)
	r := httptest.NewRequest("GET", "http://localhost:9878/auth/session", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("session cookie leaked between browser-facing ports")
	}
	if s.cookieName(r) == cookie.Name {
		t.Fatal("console cookies share a namespace")
	}
	if w := call(s, "GET", "/auth/session", nil, cookie, "", ""); w.Code != 200 {
		t.Fatal("other console invalidated original session")
	}
}

func TestLoginClearsRateLimitAttemptsOnSuccess(t *testing.T) {
	s, pw := fixture(t)
	// 4次失败尝试
	for i := 0; i < 4; i++ {
		w := call(s, "POST", "/auth/login", map[string]string{"username": "admin", "password": "wrong"}, nil, "", "http://localhost")
		if w.Code != 401 {
			t.Fatalf("expected 401, got %d", w.Code)
		}
	}
	// 正确密码登录成功
	w := call(s, "POST", "/auth/login", map[string]string{"username": "admin", "password": pw}, nil, "", "http://localhost")
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// 验证 attempts 已清空，还可以再错 4 次而不被 429
	for i := 0; i < 4; i++ {
		w := call(s, "POST", "/auth/login", map[string]string{"username": "admin", "password": "wrong"}, nil, "", "http://localhost")
		if w.Code != 401 {
			t.Fatalf("expected 401 after reset, got %d", w.Code)
		}
	}
}

func TestPasswordChangeRollbackConsistency(t *testing.T) {
	s, pw := fixture(t)
	c, csrf := loginTest(t, s, pw)

	// 把 web-auth.json 设为只读或目录锁住写，模拟 writePrivate 失败
	authFile := filepath.Join(s.dir, "web-auth.json")
	origBytes, _ := os.ReadFile(authFile)
	// 将父目录设为只读以使 os.CreateTemp 失败
	_ = os.Chmod(s.dir, 0500)
	defer os.Chmod(s.dir, 0700)

	// 尝试改密码，应返回 500
	w := call(s, "PUT", "/auth/password", map[string]string{"current_password": pw, "new_password": "brand-new-password-1234"}, c, csrf, "http://localhost")
	if w.Code != 500 {
		t.Fatalf("expected 500 when disk write fails, got %d", w.Code)
	}

	// 恢复目录权限
	_ = os.Chmod(s.dir, 0700)
	afterBytes, _ := os.ReadFile(authFile)
	if string(origBytes) != string(afterBytes) {
		t.Fatalf("web-auth.json should remain unchanged after failure")
	}

	// 验证内存中 creds 没有被篡改分裂，旧密码仍可以验证
	s.mu.Lock()
	memHash := s.creds.Hash
	s.mu.Unlock()
	if !strings.Contains(string(origBytes), memHash) {
		t.Fatalf("memory creds desynced from disk web-auth.json")
	}
}

func TestVPSDomainSettingsRequireCSRFAndStayScoped(t *testing.T) {
	s, pw := fixture(t)
	cookie, csrf := loginTest(t, s, pw)
	s.mgr.SetDomainRequester(func(_ config.Remote, r relayprotocol.Request) (relayprotocol.Response, error) {
		return relayprotocol.Response{Zone: r.Zone, Zones: r.Zones}, nil
	})
	body := map[string]any{"domain_zones": []string{"first.example", "second.example"}, "domain_zone": "second.example"}
	if got := call(s, "PUT", "/api/remotes/test/domain-zones", body, cookie, "", "http://localhost").Code; got != 403 {
		t.Fatal("VPS zones bypassed CSRF")
	}
	if got := call(s, "PUT", "/api/remotes/test/domain-zones", body, cookie, csrf, "http://localhost").Code; got != 200 {
		t.Fatalf("VPS zones: %d", got)
	}
	remote, _ := s.mgr.RemoteByName("test")
	if remote.DomainZone != "second.example" || len(remote.DomainZones) != 2 {
		t.Fatal("VPS zones missing")
	}
}
