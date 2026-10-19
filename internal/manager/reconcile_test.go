package manager_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/forward"
	"github.com/zxhtom/portmap/internal/manager"
	"github.com/zxhtom/portmap/internal/multipassclient"
)

func setupRecovery(t *testing.T) (*manager.Manager, string, *[]*fakeBackend) {
	t.Helper()
	dir := t.TempDir()
	m := manager.New(dir)
	made := []*fakeBackend{}
	m.SetBackendFactory(func(_ string, _ config.Mapping, _ config.Remote, _ config.DockerConfig, _ string) (forward.Backend, error) {
		b := &fakeBackend{}
		made = append(made, b)
		return b, nil
	})
	if err := m.AddRemote(config.Remote{Name: "vps", Host: "127.0.0.1", Port: 22}); err != nil {
		t.Fatal(err)
	}
	if err := m.AddMapping("web", config.Mapping{Name: "app", Remote: "vps", ListenPort: 19001, LocalHost: "127.0.0.1", LocalPort: 80}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	return m, dir, &made
}

func TestReconcileRecoversFailureAndHonorsStop(t *testing.T) {
	m, dir, made := setupRecovery(t)
	attempts := 0
	m.SetBackendFactory(func(_ string, _ config.Mapping, _ config.Remote, _ config.DockerConfig, _ string) (forward.Backend, error) {
		attempts++
		b := &fakeBackend{failStart: attempts == 1}
		*made = append(*made, b)
		return b, nil
	})
	if err := m.StartMapping("web", "app"); err == nil {
		t.Fatal("expected initial failure")
	}
	state, err := config.LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Mappings["web/app"].Running {
		t.Fatal("initial failure lost desired state")
	}
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if got := m.ListMappings()[0].Status; got != "running" {
		t.Fatalf("recovery status %s", got)
	}
	b := (*made)[len(*made)-1]
	b.mu.Lock()
	b.started = false
	b.mu.Unlock()
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("backend failure did not create fresh backend: %d", attempts)
	}
	if err := m.StopMapping("web", "app"); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatal("manual stop was restarted")
	}
}

func TestReconcileReloadChangesOnlyAffectedMapping(t *testing.T) {
	m, dir, made := setupRecovery(t)
	if err := m.StartMapping("web", "app"); err != nil {
		t.Fatal(err)
	}
	initial := (*made)[0]
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(*made) != 1 {
		t.Fatal("unchanged config restarted backend")
	}
	c, err := config.LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	c.Groups[0].Mappings[0].LocalPort = 81
	c.Groups[0].Mappings = append(c.Groups[0].Mappings, config.Mapping{Name: "new", Remote: "vps", ListenPort: 19002, LocalHost: "127.0.0.1", LocalPort: 82})
	if err := config.SaveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if !initial.closed || len(*made) != 2 {
		t.Fatal("changed mapping was not replaced")
	}
	for _, info := range m.ListMappings() {
		if info.Name == "app" && (info.LocalPort != 81 || info.Status != "running") {
			t.Fatalf("changed mapping: %+v", info)
		}
		if info.Name == "new" && info.Status != "stopped" {
			t.Fatal("new mapping started without explicit request")
		}
	}
	c.Remotes[0].Host = "127.0.0.2"
	if err := config.SaveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(*made) != 3 {
		t.Fatal("remote update did not reconnect active mapping")
	}
	c.Groups = nil
	if err := config.SaveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(m.ListMappings()) != 0 || !(*made)[2].closed {
		t.Fatal("deleted mapping retained resources")
	}
}

func TestReconcileRejectsInvalidEditsAndStillRecovers(t *testing.T) {
	m, dir, made := setupRecovery(t)
	if err := m.StartMapping("web", "app"); err != nil {
		t.Fatal(err)
	}
	b := (*made)[0]
	b.mu.Lock()
	b.started = false
	b.mu.Unlock()
	path := filepath.Join(dir, "config.json")
	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"groups":`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	if m.ListMappings()[0].Status != "running" {
		t.Fatal("invalid edit blocked recovery of last valid configuration")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(); err == nil {
		t.Fatal("missing file accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("missing config was recreated")
	}
	if err := os.WriteFile(path, valid, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := config.LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	c.Groups[0].Mappings[0].Remote = "missing"
	if err := config.SaveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(); err == nil {
		t.Fatal("missing remote accepted")
	}
	if m.ListMappings()[0].Status != "running" {
		t.Fatal("invalid edit stopped existing backend")
	}
}

func TestManagerRunCanBeCancelled(t *testing.T) {
	m, _, _ := setupRecovery(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("supervisor did not stop")
	}
}

func TestRestoreRetriesAfterBootFailure(t *testing.T) {
	dir := t.TempDir()
	c := config.DefaultConfig()
	c.Docker.Host = "unix://" + filepath.Join(dir, "no-docker.sock")
	c.Remotes = []config.Remote{{Name: "vps", Host: "127.0.0.1"}}
	c.Groups = []config.Group{{Name: "web", Mappings: []config.Mapping{{Name: "app", Remote: "vps", ListenPort: 19001, LocalPort: 80}}}}
	if err := config.SaveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveState(dir, config.State{Mappings: map[string]config.MappingState{"web/app": {Running: true}}}); err != nil {
		t.Fatal(err)
	}
	m := manager.New(dir)
	defer m.Shutdown()
	tries := 0
	m.SetBackendFactory(func(_ string, _ config.Mapping, _ config.Remote, _ config.DockerConfig, _ string) (forward.Backend, error) {
		tries++
		return &fakeBackend{failStart: tries == 1}, nil
	})
	if err := m.Init(); err != nil {
		t.Fatal(err)
	}
	if m.ListMappings()[0].Status != "failed" {
		t.Fatal("expected boot failure")
	}
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if tries != 2 || m.ListMappings()[0].Status != "running" {
		t.Fatal("boot failure not recovered")
	}
}

func TestReloadPreservesDockerOverrides(t *testing.T) {
	dir := t.TempDir()
	m := manager.New(dir)
	defer m.Shutdown()
	host := "unix://" + filepath.Join(dir, "missing.sock")
	image := "custom-helper"
	m.SetDockerOverrides(&host, &image)
	if err := m.Init(); err != nil {
		t.Fatal(err)
	}
	c, err := config.LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	c.Docker.Host = "unix:///another.sock"
	c.Docker.HelperImage = "changed"
	if err := config.SaveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if got := m.DockerConfig(); got.Host != host || got.HelperImage != image {
		t.Fatalf("lost CLI overrides: %+v", got)
	}
}

func TestRunReloadsConfigurationWithoutManualCall(t *testing.T) {
	m, dir, _ := setupRecovery(t)
	c, err := config.LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	c.Groups[0].Mappings[0].LocalPort = 1234
	if err := config.SaveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if m.ListMappings()[0].LocalPort == 1234 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("background supervisor did not reload config")
}

type vmResolver struct {
	mu      sync.Mutex
	ip      string
	offline bool
}

func (r *vmResolver) Resolve(context.Context, string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.offline {
		return "", fmt.Errorf("instance offline")
	}
	return r.ip, nil
}

func TestMultipassMappingRecoversAfterInstanceChanges(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "vm-service") }))
	defer target.Close()
	host, portText, _ := net.SplitHostPort(strings.TrimPrefix(target.URL, "http://"))
	targetPort, _ := strconv.Atoi(portText)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hostPort := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	resolver := &vmResolver{ip: host, offline: true}
	m := manager.New(t.TempDir())
	defer m.Shutdown()
	m.SetBackendFactory(func(_ string, mp config.Mapping, r config.Remote, _ config.DockerConfig, _ string) (forward.Backend, error) {
		return multipassclient.NewForward(resolver, mp, r), nil
	})
	mp := config.Mapping{Type: config.TypeMultipass, Name: "vm-web", Instance: "vm", InstancePort: targetPort, ListenPort: hostPort}
	if err := m.AddMapping("vm", mp); err != nil {
		t.Fatal(err)
	}
	if err := m.StartMapping("vm", "vm-web"); err == nil {
		t.Fatal("offline instance unexpectedly started")
	}
	resolver.mu.Lock()
	resolver.offline = false
	resolver.mu.Unlock()
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	check := func() {
		t.Helper()
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d", hostPort))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatal(resp.Status)
		}
	}
	check()
	resolver.mu.Lock()
	resolver.ip = "127.0.0.2"
	resolver.mu.Unlock()
	deadline := time.Now().Add(4 * time.Second)
	for m.ListMappings()[0].Status != "failed" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if m.ListMappings()[0].Status != "failed" {
		t.Fatal("instance change not detected")
	}
	resolver.mu.Lock()
	resolver.ip = host
	resolver.mu.Unlock()
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	check()
	if err := m.StopMapping("vm", "vm-web"); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if m.ListMappings()[0].Status != "stopped" {
		t.Fatal("manually stopped Multipass mapping restarted")
	}
}

func TestDescriptionChangesDoNotRestartMapping(t *testing.T) {
	m, dir, made := setupRecovery(t)
	if err := m.StartMapping("web", "app"); err != nil {
		t.Fatal(err)
	}
	desc := "tom 虚拟机 SSH 管理入口"
	if err := m.DescribeMapping("web", "app", desc); err != nil {
		t.Fatal(err)
	}
	c, err := config.LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Groups[0].Mappings[0].Description != desc || m.ListMappings()[0].Description != desc {
		t.Fatal("description not saved or listed")
	}
	c.Groups[0].Mappings[0].Description = "通过配置更新用途"
	if err := config.SaveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(*made) != 1 || (*made)[0].closed || m.ListMappings()[0].Description != "通过配置更新用途" {
		t.Fatal("description-only edit restarted mapping or failed to load")
	}
	if err := m.DescribeMapping("web", "app", ""); err != nil {
		t.Fatal(err)
	}
	if m.ListMappings()[0].Description != "" {
		t.Fatal("description not cleared")
	}
	if err := m.DescribeMapping("web", "missing", desc); err == nil {
		t.Fatal("missing mapping accepted")
	}
}
