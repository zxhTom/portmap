package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/zxhtom/portmap/internal/api"
	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/manager"
)

func TestAPI_MethodRestrictions(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := manager.New(tmpDir)
	_ = mgr.AddRemote(config.Remote{Name: "vps1", Host: "127.0.0.1", Port: 22})
	_ = mgr.AddMapping("default", config.Mapping{
		Name:       "m1",
		Remote:     "vps1",
		ListenPort: 9001,
		LocalHost:  "127.0.0.1",
		LocalPort:  80,
	})

	srv := api.NewServer(mgr)

	// GET on start mapping must return 405
	req := httptest.NewRequest(http.MethodGet, "/api/mappings/m1/start", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 on GET /api/mappings/m1/start, got %d", rr.Code)
	}

	// GET on stop mapping must return 405
	req = httptest.NewRequest(http.MethodGet, "/api/mappings/m1/stop", nil)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 on GET /api/mappings/m1/stop, got %d", rr.Code)
	}

	// GET on group start must return 405
	req = httptest.NewRequest(http.MethodGet, "/api/groups/default/start", nil)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 on GET /api/groups/default/start, got %d", rr.Code)
	}

	// GET on group stop must return 405
	req = httptest.NewRequest(http.MethodGet, "/api/groups/default/stop", nil)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 on GET /api/groups/default/stop, got %d", rr.Code)
	}
}

func TestAPI_DockerMappingAndContainers(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := manager.New(tmpDir)
	srv := api.NewServer(mgr)

	// Add docker mapping via POST /api/mappings
	body := map[string]interface{}{
		"name":           "nginx-d",
		"type":           "docker",
		"group":          "default",
		"container":      "web",
		"container_port": 80,
		"listen_port":    8088,
		"bind_host":      "127.0.0.1",
	}
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/mappings", bytes.NewReader(bodyBytes))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on add docker mapping, got %d: %s", rr.Code, rr.Body.String())
	}

	// Check mapping list
	req = httptest.NewRequest(http.MethodGet, "/api/mappings", nil)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on get mappings, got %d", rr.Code)
	}
	var list []manager.MappingInfo
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatalf("failed to decode mappings: %v", err)
	}
	if len(list) != 1 || list[0].Type != "docker" || list[0].Container != "web" || list[0].ContainerPort != 80 {
		t.Fatalf("unexpected mappings response: %+v", list)
	}

	// GET /api/docker/containers when Docker daemon is not running should return 503 Service Unavailable
	req = httptest.NewRequest(http.MethodGet, "/api/docker/containers", nil)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable && rr.Code != http.StatusOK {
		t.Fatalf("expected 503 (or 200 if daemon up), got %d: %s", rr.Code, rr.Body.String())
	}

	// POST /api/docker/containers should return 405
	req = httptest.NewRequest(http.MethodPost, "/api/docker/containers", nil)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 on POST /api/docker/containers, got %d", rr.Code)
	}
}

func TestAPI_InvalidMappingValidation(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := manager.New(tmpDir)
	srv := api.NewServer(mgr)

	tests := []struct {
		name string
		body map[string]interface{}
	}{
		{
			name: "invalid type",
			body: map[string]interface{}{
				"name":        "m_invalid_type",
				"type":        "unknown_type",
				"listen_port": 8080,
			},
		},
		{
			name: "missing name",
			body: map[string]interface{}{
				"name":        "",
				"type":        "ssh",
				"listen_port": 8080,
			},
		},
		{
			name: "invalid listen port negative",
			body: map[string]interface{}{
				"name":        "m_port_neg",
				"type":        "ssh",
				"remote":      "vps1",
				"local_host":  "127.0.0.1",
				"local_port":  80,
				"listen_port": -1,
			},
		},
		{
			name: "invalid listen port overflow",
			body: map[string]interface{}{
				"name":        "m_port_overflow",
				"type":        "ssh",
				"remote":      "vps1",
				"local_host":  "127.0.0.1",
				"local_port":  80,
				"listen_port": 70000,
			},
		},
		{
			name: "missing ssh remote",
			body: map[string]interface{}{
				"name":        "m_ssh_no_remote",
				"type":        "ssh",
				"local_host":  "127.0.0.1",
				"local_port":  80,
				"listen_port": 8080,
			},
		},
		{
			name: "invalid docker container port",
			body: map[string]interface{}{
				"name":           "m_docker_port",
				"type":           "docker",
				"container":      "web",
				"container_port": 0,
				"listen_port":    8080,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bodyBytes, _ := json.Marshal(tc.body)
			req := httptest.NewRequest(http.MethodPost, "/api/mappings", bytes.NewReader(bodyBytes))
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 Bad Request for %s, got %d: %s", tc.name, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestAPI_MultipassMappingAndDiscovery(t *testing.T) {
	dir := t.TempDir()
	mgr := manager.New(dir)
	defer mgr.Shutdown()
	binary := filepath.Join(dir, "multipass")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' '{\"list\":[{\"name\":\"vm\",\"state\":\"Running\",\"ipv4\":[\"10.1.2.3\"]}]}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PORTMAP_MULTIPASS_BINARY", binary)
	srv := api.NewServer(mgr)
	body := `{"name":"vm-ssh","type":"multipass","instance":"vm","instance_port":22,"listen_port":2222}`
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/mappings", bytes.NewBufferString(body)))
	if rr.Code != 200 {
		t.Fatalf("add mapping: %d %s", rr.Code, rr.Body.String())
	}
	mappings := mgr.ListMappings()
	if len(mappings) != 1 || mappings[0].Instance != "vm" || mappings[0].InstancePort != 22 || mappings[0].BindHost != "127.0.0.1" {
		t.Fatalf("mapping lost fields: %+v", mappings)
	}
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/multipass/instances", nil))
	if rr.Code != 200 || !bytes.Contains(rr.Body.Bytes(), []byte(`"name":"vm"`)) {
		t.Fatalf("discovery: %d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/multipass/instances", nil))
	if rr.Code != 405 {
		t.Fatalf("discovery allowed mutation: %d", rr.Code)
	}
	// A VPS-bound Multipass mapping must reference an existing SSH remote.
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/mappings", bytes.NewBufferString(`{"name":"public","type":"multipass","instance":"vm","instance_port":22,"listen_port":1922,"remote":"missing"}`)))
	if rr.Code != 400 {
		t.Fatalf("missing remote accepted: %d", rr.Code)
	}
}

func TestAPI_MappingDescription(t *testing.T) {
	m := manager.New(t.TempDir())
	defer m.Shutdown()
	s := api.NewServer(m)
	req := httptest.NewRequest(http.MethodPost, "/api/mappings", bytes.NewBufferString(`{"name":"vm","type":"multipass","instance":"vm","instance_port":22,"listen_port":2222,"description":"测试机 SSH"}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 || m.ListMappings()[0].Description != "测试机 SSH" {
		t.Fatalf("add: %d %s", rr.Code, rr.Body.String())
	}
	for _, desc := range []string{"日常开发入口", ""} {
		body, _ := json.Marshal(map[string]string{"description": desc})
		rr = httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPut, "/api/mappings/vm/description", bytes.NewReader(body)))
		if rr.Code != 200 || m.ListMappings()[0].Description != desc {
			t.Fatalf("update: %d %s", rr.Code, rr.Body.String())
		}
	}
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPut, "/api/mappings/vm/description", bytes.NewBufferString(`{}`)))
	if rr.Code != 400 {
		t.Fatal("missing description accepted")
	}
}

func TestAPIExpiryValidationAndClearing(t *testing.T) {
	mgr := manager.New(t.TempDir())
	defer mgr.Shutdown()
	mgr.AddRemote(config.Remote{Name: "vps", Host: "127.0.0.1", Port: 22})
	mgr.AddMapping("default", config.Mapping{Name: "app", Remote: "vps", ListenPort: 19001, LocalHost: "127.0.0.1", LocalPort: 80})
	handler := api.NewServer(mgr).Handler()
	cases := []struct {
		method, body string
		status       int
	}{
		{http.MethodGet, `{}`, 405},
		{http.MethodPut, `{}`, 400},
		{http.MethodPut, `{"expires_at":"bad"}`, 400},
		{http.MethodPut, `{"expires_at":"2000-01-01T00:00:00Z"}`, 400},
		{http.MethodPut, `{"expires_at":"2099-01-01T00:00:00Z"}`, 200},
		{http.MethodPut, `{"expires_at":""}`, 200},
	}
	for _, c := range cases {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(c.method, "/api/mappings/app/expiry", bytes.NewBufferString(c.body)))
		if rr.Code != c.status {
			t.Fatalf("%s %s: %d %s", c.method, c.body, rr.Code, rr.Body.String())
		}
	}
	if row := mgr.ListMappings()[0]; row.ExpiresAt != "" || row.Status != "stopped" {
		t.Fatalf("clear did not preserve stopped state: %+v", row)
	}
}

func TestAPIReturnsAssignedRelayPortAndConflictDetails(t *testing.T) {
	mgr := manager.New(t.TempDir())
	defer mgr.Shutdown()
	mgr.AddRemote(config.Remote{Name: "vps", Host: "127.0.0.1", Port: 22})
	mgr.SetRelayPortAllocator(func(_ config.Remote, requested int, _ func(int) bool) (int, error) {
		if requested == 0 {
			return 53719, nil
		}
		return requested, nil
	})
	srv := api.NewServer(mgr)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/mappings", bytes.NewBufferString(`{"name":"auto","remote":"vps","local_port":9877}`)))
	if rr.Code != 200 || !bytes.Contains(rr.Body.Bytes(), []byte(`"listen_port":53719`)) {
		t.Fatalf("actual allocation not returned: %d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/mappings", bytes.NewBufferString(`{"name":"conflict","remote":"vps","local_port":22,"listen_port":53719}`)))
	if rr.Code != 400 || !bytes.Contains(rr.Body.Bytes(), []byte("default/auto")) {
		t.Fatalf("conflict detail missing: %d %s", rr.Code, rr.Body.String())
	}
}
