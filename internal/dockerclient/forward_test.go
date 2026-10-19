package dockerclient_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/dockerclient"
	"github.com/zxhtom/portmap/internal/forward"
)

func TestDockerClient_RollbackErrorHandling(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/target-rb/json"):
			_ = json.NewEncoder(w).Encode(types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					ID:    "target-rb",
					State: &types.ContainerState{Running: true},
				},
				NetworkSettings: &types.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"dummy-net": {
							NetworkID: "net-rb",
							IPAddress: "172.20.0.9",
						},
					},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/images/json"):
			_ = json.NewEncoder(w).Encode([]image.Summary{{ID: "img-ok"}})
		case strings.HasSuffix(r.URL.Path, "/networks/create"):
			_ = json.NewEncoder(w).Encode(network.CreateResponse{ID: "net-rb"})
		case strings.HasSuffix(r.URL.Path, "/networks/net-rb/connect"):
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			// Fail here to trigger rollback
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "helper create fail"})
		case strings.HasSuffix(r.URL.Path, "/networks/net-rb/disconnect"):
			// Fail disconnect during rollback
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "fail disconnect"})
		case strings.HasSuffix(r.URL.Path, "/networks/net-rb") && r.Method == http.MethodDelete:
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

	dClient := dockerclient.NewWithClient(cli, "inst-rb")
	resMgr := dockerclient.NewResourceManager(dClient, "inst-rb", "test-g", "rb-map")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err = resMgr.SetupResources(ctx, "target-rb", "img-ok")
	if err == nil {
		t.Fatalf("expected SetupResources to fail")
	}
	// Must contain rollback failed error message!
	if !strings.Contains(err.Error(), "rollback failed") {
		t.Fatalf("expected error to mention rollback failure, got: %v", err)
	}
}

func TestDockerClient_Forward_LifecycleAndFailureMonitoring(t *testing.T) {
	for _, scenario := range []string{"helper-stop", "target-stop", "target-replaced", "address-changed"} {
		t.Run(scenario, func(t *testing.T) {
			var targetRunning = true
			var helperRunning = true
			targetID := "target-live"
			targetIP := "172.20.0.15"
			var mu sync.Mutex

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/containers/target-live/json"):
					mu.Lock()
					tr := targetRunning
					id, ip := targetID, targetIP
					mu.Unlock()
					_ = json.NewEncoder(w).Encode(types.ContainerJSON{
						ContainerJSONBase: &types.ContainerJSONBase{
							ID:    id,
							State: &types.ContainerState{Running: tr},
						},
						NetworkSettings: &types.NetworkSettings{
							Networks: map[string]*network.EndpointSettings{
								"dummy-net": {
									NetworkID: "net-live",
									IPAddress: ip,
								},
							},
						},
					})
				case strings.HasSuffix(r.URL.Path, "/containers/helper-live/json"):
					mu.Lock()
					hr := helperRunning
					mu.Unlock()
					_ = json.NewEncoder(w).Encode(types.ContainerJSON{
						ContainerJSONBase: &types.ContainerJSONBase{
							ID:    "helper-live",
							State: &types.ContainerState{Running: hr},
						},
					})
				case strings.HasSuffix(r.URL.Path, "/images/json"):
					_ = json.NewEncoder(w).Encode([]image.Summary{{ID: "img-live"}})
				case strings.HasSuffix(r.URL.Path, "/networks/create"):
					_ = json.NewEncoder(w).Encode(network.CreateResponse{ID: "net-live"})
				case strings.HasSuffix(r.URL.Path, "/networks/net-live/connect"):
					_ = json.NewEncoder(w).Encode(map[string]string{})
				case strings.HasSuffix(r.URL.Path, "/containers/create"):
					_ = json.NewEncoder(w).Encode(container.CreateResponse{ID: "helper-live"})
				case strings.HasSuffix(r.URL.Path, "/containers/helper-live/start"):
					_ = json.NewEncoder(w).Encode(map[string]string{})
				case strings.HasSuffix(r.URL.Path, "/containers/helper-live/stop"):
					_ = json.NewEncoder(w).Encode(map[string]string{})
				case strings.HasSuffix(r.URL.Path, "/containers/helper-live") && r.Method == http.MethodDelete:
					_ = json.NewEncoder(w).Encode(map[string]string{})
				case strings.HasSuffix(r.URL.Path, "/networks/net-live/disconnect"):
					_ = json.NewEncoder(w).Encode(map[string]string{})
				case strings.HasSuffix(r.URL.Path, "/networks/net-live") && r.Method == http.MethodDelete:
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

			dClient := dockerclient.NewWithClient(cli, "inst-fwd")

			// Get a free localhost port
			testLn, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			freePort := testLn.Addr().(*net.TCPAddr).Port
			_ = testLn.Close()

			mapping := config.Mapping{
				Name:          "fwd-map",
				Type:          config.TypeDocker,
				Container:     "target-live",
				ContainerPort: 80,
				ListenPort:    freePort,
				BindHost:      "127.0.0.1",
			}

			fwd := dockerclient.NewForward(dClient, mapping, "test-g", "inst-fwd", "img-live")
			if fwd.Status().State != forward.StateStopped {
				t.Fatalf("expected initial state stopped, got: %s", fwd.Status().State)
			}

			if err := fwd.Start(); err != nil {
				t.Fatalf("fwd.Start failed: %v", err)
			}
			if fwd.Status().State != forward.StateRunning {
				t.Fatalf("expected state running, got: %s", fwd.Status().State)
			}

			// Simulate helper abnormal stop
			mu.Lock()
			switch scenario {
			case "helper-stop":
				helperRunning = false
			case "target-stop":
				targetRunning = false
			case "target-replaced":
				targetID = "replacement-id"
			case "address-changed":
				targetIP = "172.20.0.16"
			}
			mu.Unlock()

			// Wait for monitorLifecycle to detect failure
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if fwd.Status().State == forward.StateFailed {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if fwd.Status().State != forward.StateFailed {
				t.Fatalf("expected state to turn failed after helper stopped, got: %s", fwd.Status().State)
			}

			// Idempotent Close
			if err := fwd.Close(); err != nil {
				t.Fatalf("Close failed: %v", err)
			}
			if fwd.Status().State != forward.StateStopped {
				t.Fatalf("expected state stopped after Close, got: %s", fwd.Status().State)
			}
		})
	}
}

func TestDockerClient_Forward_IPv6Bind(t *testing.T) {
	testLn, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 [::1] not available on this host: %v", err)
	}
	port := testLn.Addr().(*net.TCPAddr).Port
	_ = testLn.Close()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/target-v6/json"):
			_ = json.NewEncoder(w).Encode(types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					ID:    "target-v6",
					State: &types.ContainerState{Running: true},
				},
				NetworkSettings: &types.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"dummy": {NetworkID: "net-v6", IPAddress: "172.20.0.88"},
					},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/images/json"):
			_ = json.NewEncoder(w).Encode([]image.Summary{{ID: "img-v6"}})
		case strings.HasSuffix(r.URL.Path, "/networks/create"):
			_ = json.NewEncoder(w).Encode(network.CreateResponse{ID: "net-v6"})
		case strings.HasSuffix(r.URL.Path, "/networks/net-v6/connect"):
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			_ = json.NewEncoder(w).Encode(container.CreateResponse{ID: "helper-v6"})
		case strings.HasSuffix(r.URL.Path, "/containers/helper-v6/start"):
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/containers/helper-v6/stop"):
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/containers/helper-v6") && r.Method == http.MethodDelete:
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/networks/net-v6/disconnect"):
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/networks/net-v6") && r.Method == http.MethodDelete:
			_ = json.NewEncoder(w).Encode(map[string]string{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	cli, err := client.NewClientWithOpts(client.WithHost(ts.URL), client.WithHTTPClient(ts.Client()), client.WithVersion("1.45"))
	if err != nil {
		t.Fatalf("NewClientWithOpts: %v", err)
	}

	dClient := dockerclient.NewWithClient(cli, "inst-v6")
	mapping := config.Mapping{
		Name:          "v6-map",
		Type:          config.TypeDocker,
		Container:     "target-v6",
		ContainerPort: 80,
		ListenPort:    port,
		BindHost:      "::1",
	}

	fwd := dockerclient.NewForward(dClient, mapping, "test-g", "inst-v6", "img-v6")
	if err := fwd.Start(); err != nil {
		t.Fatalf("Start with IPv6 [::1] failed: %v", err)
	}
	defer fwd.Close()

	if fwd.Status().State != forward.StateRunning {
		t.Fatalf("expected running state on ::1, got %s", fwd.Status().State)
	}
}

func TestDockerClient_Forward_HijackBidirectionalAndHalfClose(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/target-echo/json"):
			_ = json.NewEncoder(w).Encode(types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					ID:    "target-echo",
					State: &types.ContainerState{Running: true},
				},
				NetworkSettings: &types.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"dummy": {NetworkID: "net-echo", IPAddress: "172.20.0.99"},
					},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/images/json"):
			_ = json.NewEncoder(w).Encode([]image.Summary{{ID: "img-echo"}})
		case strings.HasSuffix(r.URL.Path, "/networks/create"):
			_ = json.NewEncoder(w).Encode(network.CreateResponse{ID: "net-echo"})
		case strings.HasSuffix(r.URL.Path, "/networks/net-echo/connect"):
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			_ = json.NewEncoder(w).Encode(container.CreateResponse{ID: "helper-echo"})
		case strings.HasSuffix(r.URL.Path, "/containers/helper-echo/start"):
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/containers/helper-echo/exec"):
			_ = json.NewEncoder(w).Encode(types.IDResponse{ID: "exec-echo"})
		case strings.HasSuffix(r.URL.Path, "/exec/exec-echo/start"):
			_ = r.Body.Close()
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "hijack not supported", http.StatusInternalServerError)
				return
			}
			conn, bufrw, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()

			// Send 101 Switching Protocols response
			_, _ = bufrw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = bufrw.Flush()

			// Read client data until EOF (CloseWrite), then reply with stdout frame
			clientInput, _ := io.ReadAll(bufrw)

			reply := append([]byte("ECHO:"), clientInput...)
			frame := make([]byte, 8+len(reply))
			frame[0] = 1 // stdout
			binary.BigEndian.PutUint32(frame[4:8], uint32(len(reply)))
			copy(frame[8:], reply)

			_, _ = bufrw.Write(frame)
			_ = bufrw.Flush()
			if cw, ok := conn.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
			return
		case strings.HasSuffix(r.URL.Path, "/containers/helper-echo/stop"):
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/containers/helper-echo") && r.Method == http.MethodDelete:
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/networks/net-echo/disconnect"):
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/networks/net-echo") && r.Method == http.MethodDelete:
			_ = json.NewEncoder(w).Encode(map[string]string{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	cli, err := client.NewClientWithOpts(
		client.WithHost("tcp://"+ts.Listener.Addr().String()),
		client.WithVersion("1.45"),
	)
	if err != nil {
		t.Fatalf("NewClientWithOpts: %v", err)
	}

	testLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := testLn.Addr().(*net.TCPAddr).Port
	_ = testLn.Close()

	dClient := dockerclient.NewWithClient(cli, "inst-echo")
	mapping := config.Mapping{
		Name:          "echo-map",
		Type:          config.TypeDocker,
		Container:     "target-echo",
		ContainerPort: 80,
		ListenPort:    port,
		BindHost:      "127.0.0.1",
	}

	fwd := dockerclient.NewForward(dClient, mapping, "test-g", "inst-echo", "img-echo")
	if err := fwd.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer fwd.Close()

	// Connect client to host port
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("dial host: %v", err)
	}
	defer conn.Close()

	// Write payload with arbitrary and null bytes
	payload := []byte("bin\x00data\xfftest")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	// Half-close client write side: CloseWrite()
	tcpConn := conn.(*net.TCPConn)
	if err := tcpConn.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite failed: %v", err)
	}

	// Read echoed response
	resp, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read response failed: %v", err)
	}

	expected := append([]byte("ECHO:"), payload...)
	if !bytes.Equal(resp, expected) {
		t.Fatalf("expected binary response %q, got %q", expected, resp)
	}
}

func TestDockerClient_ControlledAttach_HangingDaemonAbortedOnClose(t *testing.T) {
	connObserved := make(chan struct{})
	connClosed := make(chan struct{})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/target-hang/json"):
			_ = json.NewEncoder(w).Encode(types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					ID:    "target-hang",
					State: &types.ContainerState{Running: true},
				},
				NetworkSettings: &types.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"dummy": {NetworkID: "net-hang", IPAddress: "172.20.0.99"},
					},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/images/json"):
			_ = json.NewEncoder(w).Encode([]image.Summary{{ID: "img-hang"}})
		case strings.HasSuffix(r.URL.Path, "/networks/create"):
			_ = json.NewEncoder(w).Encode(network.CreateResponse{ID: "net-hang"})
		case strings.HasSuffix(r.URL.Path, "/networks/net-hang/connect"):
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			_ = json.NewEncoder(w).Encode(container.CreateResponse{ID: "helper-hang"})
		case strings.HasSuffix(r.URL.Path, "/containers/helper-hang/start"):
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/containers/helper-hang/exec"):
			_ = json.NewEncoder(w).Encode(types.IDResponse{ID: "exec-hang"})
		case strings.HasSuffix(r.URL.Path, "/exec/exec-hang/start"):
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "hijack not supported", http.StatusInternalServerError)
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			close(connObserved)

			// Intentionally do NOT reply with 101; wait until client closes raw socket
			buf := make([]byte, 128)
			for {
				_, err := conn.Read(buf)
				if err != nil {
					close(connClosed)
					return
				}
			}
		case strings.HasSuffix(r.URL.Path, "/containers/helper-hang/stop"):
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/containers/helper-hang") && r.Method == http.MethodDelete:
			_ = json.NewEncoder(w).Encode(map[string]string{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	cli, err := client.NewClientWithOpts(
		client.WithHost("tcp://"+ts.Listener.Addr().String()),
		client.WithVersion("1.45"),
	)
	if err != nil {
		t.Fatalf("NewClientWithOpts: %v", err)
	}

	testLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := testLn.Addr().(*net.TCPAddr).Port
	_ = testLn.Close()

	dClient := dockerclient.NewWithClient(cli, "inst-hang")
	mapping := config.Mapping{
		Name:          "hang-map",
		Type:          config.TypeDocker,
		Container:     "target-hang",
		ContainerPort: 80,
		ListenPort:    port,
		BindHost:      "127.0.0.1",
	}

	fwd := dockerclient.NewForward(dClient, mapping, "test-g", "inst-hang", "img-hang")
	if err := fwd.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Dial host port to initiate forward session
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("dial host: %v", err)
	}
	defer conn.Close()

	// Wait until daemon receives upgrade connection
	select {
	case <-connObserved:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for daemon to receive exec/start")
	}

	// Close forward; must abort hanging upgrade handshake and close socket
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- fwd.Close()
	}()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("fwd.Close timed out while daemon was hanging on 101")
	}

	// Daemon side must observe connection closed promptly
	select {
	case <-connClosed:
	case <-time.After(3 * time.Second):
		t.Fatalf("daemon did not observe socket closed after fwd.Close()")
	}
}

func TestDockerClient_Forward_CloseRetryDeletesResources(t *testing.T) {
	var helperDeleteAttempts int
	var networkDeleteAttempts int
	var failDelete = true
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/target-cr/json"):
			_ = json.NewEncoder(w).Encode(types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					ID:    "target-cr",
					State: &types.ContainerState{Running: true},
				},
				NetworkSettings: &types.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"net-cr": {
							NetworkID: "net-cr-id",
							IPAddress: "172.20.0.88",
						},
					},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/images/json"):
			_ = json.NewEncoder(w).Encode([]image.Summary{{ID: "img-cr"}})
		case strings.HasSuffix(r.URL.Path, "/networks/create"):
			_ = json.NewEncoder(w).Encode(network.CreateResponse{ID: "net-cr-id"})
		case strings.HasSuffix(r.URL.Path, "/networks/net-cr-id/connect"):
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			_ = json.NewEncoder(w).Encode(container.CreateResponse{ID: "helper-cr-id"})
		case strings.HasSuffix(r.URL.Path, "/containers/helper-cr-id/start"):
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/containers/helper-cr-id/stop"):
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/containers/helper-cr-id") && r.Method == http.MethodDelete:
			mu.Lock()
			helperDeleteAttempts++
			shouldFail := failDelete
			mu.Unlock()
			if shouldFail {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "helper delete error"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/networks/net-cr-id/disconnect"):
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/networks/net-cr-id") && r.Method == http.MethodDelete:
			mu.Lock()
			networkDeleteAttempts++
			shouldFail := failDelete
			mu.Unlock()
			if shouldFail {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "network delete error"})
				return
			}
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

	testLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := testLn.Addr().(*net.TCPAddr).Port
	_ = testLn.Close()

	dClient := dockerclient.NewWithClient(cli, "inst-cr")
	mapping := config.Mapping{
		Name:          "cr-map",
		Type:          config.TypeDocker,
		Container:     "target-cr",
		ContainerPort: 80,
		ListenPort:    port,
		BindHost:      "127.0.0.1",
	}

	fwd := dockerclient.NewForward(dClient, mapping, "test-g", "inst-cr", "img-cr")
	if err := fwd.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// First Close: should fail because delete endpoints return 500
	err = fwd.Close()
	if err == nil {
		t.Fatalf("expected first Close to fail")
	}

	mu.Lock()
	if helperDeleteAttempts != 1 {
		t.Fatalf("expected 1 helper delete attempt, got %d", helperDeleteAttempts)
	}
	// Stop failing deletes
	failDelete = false
	mu.Unlock()

	// Second Close: must retry Rollback and actually call Docker delete endpoints again
	err = fwd.Close()
	if err != nil {
		t.Fatalf("expected second Close to succeed, got: %v", err)
	}

	mu.Lock()
	if helperDeleteAttempts != 2 {
		t.Fatalf("expected 2 helper delete attempts, got %d", helperDeleteAttempts)
	}
	if networkDeleteAttempts < 1 {
		t.Fatalf("expected network delete attempts >= 1, got %d", networkDeleteAttempts)
	}
	mu.Unlock()
}

func TestDockerClient_ControlledAttachFailure_NoFallback(t *testing.T) {
	var execStartCalled bool
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/target-no-fb/json"):
			_ = json.NewEncoder(w).Encode(types.ContainerJSON{
				ContainerJSONBase: &types.ContainerJSONBase{
					ID:    "target-no-fb",
					State: &types.ContainerState{Running: true},
				},
				NetworkSettings: &types.NetworkSettings{
					Networks: map[string]*network.EndpointSettings{
						"net-no-fb": {
							NetworkID: "net-no-fb-id",
							IPAddress: "172.20.0.99",
						},
					},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/images/json"):
			_ = json.NewEncoder(w).Encode([]image.Summary{{ID: "img-no-fb"}})
		case strings.HasSuffix(r.URL.Path, "/networks/create"):
			_ = json.NewEncoder(w).Encode(network.CreateResponse{ID: "net-no-fb-id"})
		case strings.HasSuffix(r.URL.Path, "/networks/net-no-fb-id/connect"):
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			_ = json.NewEncoder(w).Encode(container.CreateResponse{ID: "helper-no-fb-id"})
		case strings.HasSuffix(r.URL.Path, "/containers/helper-no-fb-id/start"):
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/containers/helper-no-fb-id/stop"):
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/containers/helper-no-fb-id") && r.Method == http.MethodDelete:
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/networks/net-no-fb-id/disconnect"):
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/networks/net-no-fb-id") && r.Method == http.MethodDelete:
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case strings.HasSuffix(r.URL.Path, "/containers/helper-no-fb-id/exec"):
			_ = json.NewEncoder(w).Encode(types.IDResponse{ID: "exec-no-fb-id"})
		case strings.Contains(r.URL.Path, "/exec/exec-no-fb-id/start"):
			mu.Lock()
			execStartCalled = true
			mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
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

	testLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := testLn.Addr().(*net.TCPAddr).Port
	_ = testLn.Close()

	// Using NewWithClient: cli does NOT implement dialerProvider, so ControlledAttach will fail!
	dClient := dockerclient.NewWithClient(cli, "inst-no-fb")
	mapping := config.Mapping{
		Name:          "no-fb-map",
		Type:          config.TypeDocker,
		Container:     "target-no-fb",
		ContainerPort: 80,
		ListenPort:    port,
		BindHost:      "127.0.0.1",
	}

	fwd := dockerclient.NewForward(dClient, mapping, "test-g", "inst-no-fb", "img-no-fb")
	if err := fwd.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer fwd.Close()

	// Dial to trigger handleSession
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Write dummy data and wait for close
	_, _ = conn.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	buf := make([]byte, 1024)
	_, _ = conn.Read(buf)
	_ = conn.Close()

	// Wait a moment for handleSession to complete
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	called := execStartCalled
	mu.Unlock()

	if called {
		t.Fatalf("expected NO fallback ContainerExecAttach/exec-start when ControlledAttach fails")
	}
}
