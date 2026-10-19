package manager_test

import (
	"fmt"
	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/forward"
	"github.com/zxhtom/portmap/internal/manager"
	"github.com/zxhtom/portmap/internal/multipassclient"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestExpiryClosesPersistsAndRenewsWithoutLosingIntent(t *testing.T) {
	m, dir, made := setupRecovery(t)
	if err := m.StartMapping("web", "app"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(100 * time.Millisecond)
	if err := m.SetExpiry("web", "app", deadline.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if len(*made) != 1 {
		t.Fatal("setting future expiry restarted healthy tunnel")
	}
	time.Sleep(time.Until(deadline) + time.Millisecond)
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	row := m.ListMappings()[0]
	if row.Status != "invalid" || row.InvalidReason != "expired" || row.ExpiresAt == "" {
		t.Fatalf("expiry not exposed: %+v", row)
	}
	b := (*made)[0]
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if !closed {
		t.Fatal("expired backend still open")
	}
	if err := m.StartMapping("web", "app"); err == nil {
		t.Fatal("manual start bypassed expiry")
	}
	m.Reconcile()
	if len(*made) != 1 {
		t.Fatal("expired mapping auto-reconnected")
	}
	m.Shutdown()
	next := manager.New(dir)
	attempts := 0
	next.SetBackendFactory(func(_ string, _ config.Mapping, _ config.Remote, _ config.DockerConfig, _ string) (forward.Backend, error) {
		attempts++
		return &fakeBackend{}, nil
	})
	if err := next.Init(); err != nil {
		t.Fatal(err)
	}
	defer next.Shutdown()
	if attempts != 0 || next.ListMappings()[0].InvalidReason != "expired" {
		t.Fatal("restart lost expiry")
	}
	renewed := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	if err := next.SetExpiry("web", "app", renewed); err != nil {
		t.Fatal(err)
	}
	if row := next.ListMappings()[0]; row.Status != "running" || row.ExpiresAt != renewed || row.InvalidReason != "" {
		t.Fatalf("renewal failed: %+v", row)
	}
	if attempts != 1 {
		t.Fatalf("expected one restart, got %d", attempts)
	}
	if err := next.StopMapping("web", "app"); err != nil {
		t.Fatal(err)
	}
	if err := next.SetExpiry("web", "app", ""); err != nil {
		t.Fatal(err)
	}
	if row := next.ListMappings()[0]; row.Status != "stopped" || row.ExpiresAt != "" {
		t.Fatalf("clear expiry lost manual stop: %+v", row)
	}
}

func TestExpiryRejectsBadDeadlinesAndHotReloadDoesNotRestart(t *testing.T) {
	m, dir, made := setupRecovery(t)
	m.StartMapping("web", "app")
	for _, deadline := range []string{"tomorrow", time.Now().Add(-time.Hour).Format(time.RFC3339), "2026-10-20T12:00:00"} {
		if err := m.SetExpiry("web", "app", deadline); err == nil {
			t.Fatalf("accepted %q", deadline)
		}
	}
	cfg, err := config.LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Groups[0].Mappings[0].ExpiresAt = time.Now().Add(time.Hour).Format(time.RFC3339)
	if err := config.SaveConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(*made) != 1 {
		t.Fatal("expiry metadata edit restarted transport")
	}
	cfg.Groups[0].Mappings[0].ExpiresAt = time.Now().Add(-time.Hour).Format(time.RFC3339)
	config.SaveConfig(dir, cfg)
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if m.ListMappings()[0].InvalidReason != "expired" {
		t.Fatal("reload failed to expire")
	}
}

func TestExpiryActuallyClosesTCPListenerAndRenewReopensIt(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "alive") }))
	defer target.Close()
	host, portText, _ := net.SplitHostPort(strings.TrimPrefix(target.URL, "http://"))
	targetPort, _ := strconv.Atoi(portText)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	m := manager.New(t.TempDir())
	defer m.Shutdown()
	m.SetBackendFactory(func(_ string, mp config.Mapping, r config.Remote, _ config.DockerConfig, _ string) (forward.Backend, error) {
		return multipassclient.NewForward(&vmResolver{ip: host}, mp, r), nil
	})
	if err := m.AddMapping("vm", config.Mapping{Name: "test", Type: config.TypeMultipass, Instance: "test-vm", InstancePort: targetPort, ListenPort: port}); err != nil {
		t.Fatal(err)
	}
	if err := m.StartMapping("vm", "test"); err != nil {
		t.Fatal(err)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	deadline := time.Now().Add(100 * time.Millisecond)
	if err := m.SetExpiry("vm", "test", deadline.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(deadline) + time.Millisecond)
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatal("expired listener accepts connections")
	}
	if err := m.SetExpiry("vm", "test", time.Now().Add(time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	c, err = net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("renewed listener unreachable: %v", err)
	}
	c.Close()
}
