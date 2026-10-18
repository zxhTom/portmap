package dockerclient_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/dockerclient"
)

func TestDockerClient_NewClient_UnixSocket(t *testing.T) {
	// Use /tmp directly for Unix socket to avoid long paths on macOS
	sockDir, err := os.MkdirTemp("/tmp", "pm-sock-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(sockDir)
	sockPath := filepath.Join(sockDir, "d.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	defer ln.Close()

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]types.Container{})
		}),
	}
	go srv.Serve(ln)
	defer srv.Close()

	cfg := config.DockerConfig{
		Host: "unix://" + sockPath,
	}
	c, err := dockerclient.NewClient(cfg, "inst-sock")
	if err != nil {
		t.Fatalf("NewClient unix failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	list, err := c.ListContainers(ctx)
	if err != nil {
		t.Fatalf("ListContainers over unix socket failed: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0 containers, got %d", len(list))
	}
}

func TestDockerClient_EnsureHelperImage_StreamJSON(t *testing.T) {
	errStream := `{"status":"Pulling fs layer"}
{"status":"Downloading"}
{"errorDetail":{"message":"manifest not found"},"error":"manifest not found"}
`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/images/json"):
			_ = json.NewEncoder(w).Encode([]image.Summary{})
		case strings.HasSuffix(r.URL.Path, "/images/create"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, errStream)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	cli, err := client.NewClientWithOpts(
		client.WithHost(ts.URL),
		client.WithHTTPClient(ts.Client()),
		client.WithVersion("1.45"),
	)
	if err != nil {
		t.Fatalf("NewClientWithOpts: %v", err)
	}

	dClient := dockerclient.NewWithClient(cli, "inst-img")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err = dClient.EnsureHelperImage(ctx, "nonexistent:tag")
	if err == nil {
		t.Fatalf("expected EnsureHelperImage to return error on stream error")
	}
	if !strings.Contains(err.Error(), "manifest not found") {
		t.Fatalf("expected error to contain 'manifest not found', got: %v", err)
	}
}

func TestDockerClient_CleanupOwned_FullEndpointsAndIsolation(t *testing.T) {
	var disconnectedContainer, removedNet, removedContainer string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			_ = json.NewEncoder(w).Encode([]types.Container{
				{
					ID:    "helper-owned",
					Names: []string{"/portmap-helper-inst-A"},
					Labels: map[string]string{
						dockerclient.LabelManaged:    "true",
						dockerclient.LabelInstanceID: "inst-A",
					},
				},
				{
					ID:    "target-user",
					Names: []string{"/user-nginx"},
					Labels: map[string]string{
						// Target user container does NOT have portmap labels!
					},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/containers/helper-owned/stop"):
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/containers/helper-owned") && r.Method == http.MethodDelete:
			removedContainer = "helper-owned"
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/networks") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]network.Inspect{
				{
					ID:   "net-owned",
					Name: "portmap-net-A",
					Labels: map[string]string{
						dockerclient.LabelManaged:    "true",
						dockerclient.LabelInstanceID: "inst-A",
					},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/networks/net-owned") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(network.Inspect{
				ID:   "net-owned",
				Name: "portmap-net-A",
				Labels: map[string]string{
					dockerclient.LabelManaged:    "true",
					dockerclient.LabelInstanceID: "inst-A",
				},
				Containers: map[string]network.EndpointResource{
					"target-user": {EndpointID: "ep-1"},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/networks/net-owned/disconnect"):
			disconnectedContainer = "target-user"
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/networks/net-owned") && r.Method == http.MethodDelete:
			removedNet = "net-owned"
			_ = json.NewEncoder(w).Encode(map[string]string{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	cli, err := client.NewClientWithOpts(
		client.WithHost(ts.URL),
		client.WithHTTPClient(ts.Client()),
		client.WithVersion("1.45"),
	)
	if err != nil {
		t.Fatalf("NewClientWithOpts: %v", err)
	}

	dClient := dockerclient.NewWithClient(cli, "inst-A")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := dClient.CleanupOwned(ctx); err != nil {
		t.Fatalf("CleanupOwned failed: %v", err)
	}

	if removedContainer != "helper-owned" {
		t.Fatalf("expected helper-owned container removed, got %q", removedContainer)
	}
	if disconnectedContainer != "target-user" {
		t.Fatalf("expected target-user to be disconnected from network, got %q", disconnectedContainer)
	}
	if removedNet != "net-owned" {
		t.Fatalf("expected net-owned removed, got %q", removedNet)
	}
}

func TestDocker_DemuxStdoutStream(t *testing.T) {
	var inBuf bytes.Buffer

	f1Data := []byte("hello ")
	h1 := make([]byte, 8)
	h1[0] = 1
	binary.BigEndian.PutUint32(h1[4:8], uint32(len(f1Data)))
	inBuf.Write(h1)
	inBuf.Write(f1Data)

	f2Data := []byte("debug noise")
	h2 := make([]byte, 8)
	h2[0] = 2
	binary.BigEndian.PutUint32(h2[4:8], uint32(len(f2Data)))
	inBuf.Write(h2)
	inBuf.Write(f2Data)

	f3Data := []byte("world!")
	h3 := make([]byte, 8)
	h3[0] = 1
	binary.BigEndian.PutUint32(h3[4:8], uint32(len(f3Data)))
	inBuf.Write(h3)
	inBuf.Write(f3Data)

	var outBuf bytes.Buffer
	err := dockerclient.DemuxStdoutStream(&inBuf, &outBuf)
	if err != nil && err != io.EOF {
		t.Fatalf("DemuxStdoutStream failed: %v", err)
	}

	if outBuf.String() != "hello world!" {
		t.Fatalf("expected demuxed output 'hello world!', got %q", outBuf.String())
	}
}

func TestDockerClient_NewClient_APIVersionNegotiation(t *testing.T) {
	// Mock server reporting an older Docker API version like 1.40
	var requestedVersion string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Api-Version", "1.40")
		w.Header().Set("Docker-Experimental", "false")
		w.Header().Set("Ostype", "linux")
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/version") {
			_ = json.NewEncoder(w).Encode(types.Version{
				APIVersion: "1.40",
				Version:    "19.03.12",
			})
			return
		}
		if strings.Contains(r.URL.Path, "/containers/json") {
			// Extract version from path e.g. /v1.40/containers/json
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
			if len(parts) > 0 {
				requestedVersion = parts[0]
			}
			_ = json.NewEncoder(w).Encode([]types.Container{})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	cfg := config.DockerConfig{
		Host: ts.URL,
	}

	client, err := dockerclient.NewClient(cfg, "inst-ver")
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// ListContainers triggers API calls; WithAPIVersionNegotiation negotiates with /version
	_, err = client.ListContainers(ctx)
	if err != nil {
		t.Fatalf("ListContainers failed with version negotiation: %v", err)
	}

	if requestedVersion != "v1.40" {
		t.Fatalf("expected negotiated version v1.40, got %q", requestedVersion)
	}
}

