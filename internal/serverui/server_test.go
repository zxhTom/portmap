package serverui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/zxhtom/portmap/internal/relay"
	"github.com/zxhtom/portmap/internal/relayprotocol"
)

func TestConsoleAuthenticationAndDisableOnly(t *testing.T) {
	dir := t.TempDir()
	i, err := relay.NewInventory(dir)
	if err != nil {
		t.Fatal(err)
	}
	client := "0123456789abcdef"
	h := sha256.Sum256([]byte(client + "\x00web\x00app"))
	owner := hex.EncodeToString(h[:])
	_, err = i.Snapshot(client, "client", []relayprotocol.MappingReport{{Owner: owner, Group: "web", Name: "app", ListenPort: 12345, Status: "running"}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(dir, i)
	if err != nil {
		t.Fatal(err)
	}
	password, err := os.ReadFile(filepath.Join(dir, "server-web-initial-password"))
	if err != nil {
		t.Fatal(err)
	}
	if stat, err := os.Stat(filepath.Join(dir, "server-web-initial-password")); err != nil || stat.Mode().Perm() != 0600 {
		t.Fatalf("password permissions: %v, %v", stat, err)
	}
	call := func(method, path string, body any, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
		t.Helper()
		var b []byte
		if body != nil {
			b, _ = json.Marshal(body)
		}
		r := httptest.NewRequest(method, "http://localhost"+path, bytes.NewReader(b))
		if method == http.MethodPost {
			r.Header.Set("Origin", "http://localhost")
			r.Header.Set("X-CSRF-Token", csrf)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	if got := call("GET", "/api/tunnels", nil, nil, "").Code; got != 401 {
		t.Fatalf("unauthenticated list: %d", got)
	}
	login := call("POST", "/auth/login", map[string]string{"username": "admin", "password": string(bytes.TrimSpace(password))}, nil, "")
	if login.Code != 200 || len(login.Result().Cookies()) != 1 {
		t.Fatalf("login: %d %s", login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	sess := call("GET", "/auth/session", nil, cookie, "")
	var token struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(sess.Body.Bytes(), &token); err != nil || token.CSRF == "" {
		t.Fatalf("session: %v %s", err, sess.Body.String())
	}
	if got := call("POST", "/api/tunnels/"+owner+"/disable", nil, cookie, "").Code; got != 403 {
		t.Fatalf("missing CSRF: %d", got)
	}
	if got := call("POST", "/api/tunnels", nil, cookie, token.CSRF).Code; got != 405 {
		t.Fatalf("create route: %d", got)
	}
	if got := call("POST", "/api/tunnels/"+owner+"/disable", nil, cookie, token.CSRF).Code; got != 200 {
		t.Fatalf("disable: %d", got)
	}
	if disabled, _ := i.Check(owner); !disabled {
		t.Fatal("disable policy not saved")
	}
	if got := call("POST", "/api/tunnels/"+owner+"/enable", nil, cookie, token.CSRF).Code; got != 200 {
		t.Fatalf("enable: %d", got)
	}
}

func TestHTTPSReverseProxyOriginAndCookie(t *testing.T) {
	dir := t.TempDir()
	i, err := relay.NewInventory(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(dir, i)
	if err != nil {
		t.Fatal(err)
	}
	password, err := os.ReadFile(filepath.Join(dir, "server-web-initial-password"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": string(bytes.TrimSpace(password))})
	r := httptest.NewRequest(http.MethodPost, "http://server.port.toms.chat/auth/login", bytes.NewReader(body))
	r.RemoteAddr = "127.0.0.1:40000"
	r.Header.Set("Origin", "https://server.port.toms.chat")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Real-IP", "203.0.113.50")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 || len(w.Result().Cookies()) != 1 || !w.Result().Cookies()[0].Secure {
		t.Fatalf("proxied login: %d, cookies=%v", w.Code, w.Result().Cookies())
	}
	r.RemoteAddr = "203.0.113.20:40000"
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("accepted forwarded origin from untrusted peer: %d", w.Code)
	}
}
