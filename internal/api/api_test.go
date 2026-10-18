package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
