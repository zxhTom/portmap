package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zxhtom/portmap/internal/relayprotocol"
)

func newTestRelay(t *testing.T) *Server {
	t.Helper()
	s, err := New(t.TempDir(), relayprotocol.DefaultZone)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}
func requestFor(owner, prefix string, port int) relayprotocol.Request {
	return relayprotocol.Request{Operation: "reserve", Owner: strings.Repeat(owner, 32), Prefixes: []string{prefix}, Port: port, Scheme: "http", LeaseID: "session-identity-0001"}
}
func TestCentralOwnershipAtomicConcurrentAndPersistent(t *testing.T) {
	s := newTestRelay(t)
	var wg sync.WaitGroup
	results := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := requestFor(fmt.Sprintf("owner-%d", i), "unique", 32123)
			_, err := s.Apply(req)
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !strings.Contains(err.Error(), "不能重复绑定") {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("domain claimed %d times", success)
	}
	s.mu.RLock()
	owner := s.bindings["unique."+s.zone].Owner
	s.mu.RUnlock()
	dir := s.dir
	s.Close()
	next, err := New(dir, relayprotocol.DefaultZone)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if next.bindings["unique."+next.zone].Owner != owner {
		t.Fatal("registry forgot owner on restart")
	}
	if _, err := next.Apply(requestFor("foreign", "unique", 32124)); err == nil {
		t.Fatal("restart allowed domain takeover")
	}
	if _, err := next.Apply(relayprotocol.Request{Operation: "release", Owner: owner}); err != nil {
		t.Fatal(err)
	}
	if _, err := next.Apply(requestFor("foreign", "unique", 32124)); err != nil {
		t.Fatal(err)
	}
}
func TestHostRoutingUnknownInactiveExpiryAndLeaseGeneration(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, r.Host+"|"+r.URL.Path) }))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	_, portText, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portText)
	s := newTestRelay(t)
	req := requestFor("owner", "app", port)
	if _, err := s.Apply(req); err != nil {
		t.Fatal(err)
	}
	call := func(host string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "https://"+host+"/path", nil)
		r.Host = host
		w := httptest.NewRecorder()
		s.ProxyHandler().ServeHTTP(w, r)
		return w
	}
	for _, host := range []string{"47.121.182.177", "unbound." + s.zone, "app." + s.zone + ".evil.test", "nested.app." + s.zone} {
		if w := call(host); w.Code != 404 {
			t.Fatalf("unbound host routed: %s %d", host, w.Code)
		}
	}
	if w := call("app." + s.zone); w.Code != 503 {
		t.Fatal("inactive route forwarded")
	}
	req.Operation = "activate"
	if _, err := s.Apply(req); err != nil {
		t.Fatal(err)
	}
	if w := call("app." + s.zone); w.Code != 200 || w.Body.String() != "app."+s.zone+"|/path" {
		t.Fatalf("wrong forwarding %d %s", w.Code, w.Body.String())
	}
	newer := req
	newer.LeaseID = "session-identity-0002"
	if _, err := s.Apply(newer); err != nil {
		t.Fatal(err)
	}
	req.Operation = "deactivate"
	s.Apply(req)
	if w := call("app." + s.zone); w.Code != 200 {
		t.Fatal("old tunnel deactivated new lease")
	}
	newer.Operation = "deactivate"
	s.Apply(newer)
	newer.Operation = "renew"
	if _, err := s.Apply(newer); err == nil {
		t.Fatal("revoked lease renewed")
	}
	newer.Operation = "activate"
	newer.LeaseID = "session-identity-0003"
	s.Apply(newer)
	s.mu.Lock()
	b := s.bindings["app."+s.zone]
	b.Expires = time.Now().Add(-time.Second)
	s.bindings["app."+s.zone] = b
	s.mu.Unlock()
	if w := call("app." + s.zone); w.Code != 503 {
		t.Fatal("expired lease still forwarded")
	}
}
func TestRejectPublicBackendAndPinHTTPSCertificate(t *testing.T) {
	public, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer public.Close()
	s := newTestRelay(t)
	req := requestFor("owner", "private", public.Addr().(*net.TCPAddr).Port)
	s.Apply(req)
	req.Operation = "activate"
	if _, err := s.Apply(req); err == nil || !strings.Contains(err.Error(), "公开监听") {
		t.Fatalf("public backend activated: %v", err)
	}
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "secure-pinned-target") }))
	defer target.Close()
	u, _ := url.Parse(target.URL)
	_, p, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(p)
	req = requestFor("tlsowner", "secure", port)
	req.Scheme = "https"
	s.Apply(req)
	req.Operation = "activate"
	if _, err := s.Apply(req); err == nil {
		t.Fatal("HTTPS activated without a pin")
	}
	digest := sha256.Sum256(target.Certificate().Raw)
	req.Fingerprint = hex.EncodeToString(digest[:])
	if _, err := s.Apply(req); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "https://secure."+s.zone+"/", nil)
	w := httptest.NewRecorder()
	s.ProxyHandler().ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "secure-pinned-target" {
		t.Fatalf("pin forwarding failed: %d %s", w.Code, w.Body.String())
	}
	s.mu.Lock()
	b := s.bindings["secure."+s.zone]
	b.Fingerprint = strings.Repeat("00", 32)
	s.bindings["secure."+s.zone] = b
	s.mu.Unlock()
	w = httptest.NewRecorder()
	s.ProxyHandler().ServeHTTP(w, r)
	io.Copy(io.Discard, w.Result().Body)
	if w.Code != 502 {
		t.Fatal("wrong upstream certificate accepted")
	}
}
func TestPrefixValidationAndAtomicReplacement(t *testing.T) {
	s := newTestRelay(t)
	one := requestFor("one", "first", 32123)
	s.Apply(one)
	two := requestFor("two", "second", 32124)
	s.Apply(two)
	one.Prefixes = []string{"third", "second"}
	if _, err := s.Apply(one); err == nil {
		t.Fatal("partial conflicting replacement accepted")
	}
	if s.bindings["first."+s.zone].Owner != one.Owner {
		t.Fatal("failed replacement lost original")
	}
	one.Prefixes = []string{"third", "fourth"}
	if _, err := s.Apply(one); err != nil {
		t.Fatal(err)
	}
	if _, exists := s.bindings["first."+s.zone]; exists {
		t.Fatal("old binding retained after replacement")
	}
	for _, prefix := range []string{"*", "../evil", "x.port.zxhtom.store", "-bad", "bad-", "bad;return 200", "a_b", strings.Repeat("a", 64)} {
		req := requestFor("invalid", prefix, 32125)
		if _, err := s.Apply(req); err == nil {
			t.Fatalf("invalid prefix %q accepted", prefix)
		}
	}
}

func TestHTTPRedirectRequiresBoundHostname(t *testing.T) {
	s := newTestRelay(t)
	s.Apply(requestFor("owner", "registered", 31234))
	for _, test := range []struct {
		host string
		want int
	}{{"registered." + s.zone, 308}, {"unknown." + s.zone, 404}} {
		r := httptest.NewRequest("GET", "http://"+test.host+"/hello?q=1", nil)
		r.Header.Set("X-Forwarded-Proto", "http")
		w := httptest.NewRecorder()
		s.ProxyHandler().ServeHTTP(w, r)
		if w.Code != test.want {
			t.Fatalf("%s: %d", test.host, w.Code)
		}
		if test.want == 308 && w.Header().Get("Location") != "https://"+test.host+"/hello?q=1" {
			t.Fatal("wrong canonical domain redirect")
		}
	}
}
