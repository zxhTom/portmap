package dockerclient_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/dockerclient"
)

// TestDocker_LiveIntegration is an end-to-end acceptance test against a live Docker daemon.
// It is guarded by PORTMAP_DOCKER_INTEGRATION=1 and skipped by default.
func TestDocker_LiveIntegration(t *testing.T) {
	if os.Getenv("PORTMAP_DOCKER_INTEGRATION") != "1" {
		t.Skip("skipping live Docker integration test; set PORTMAP_DOCKER_INTEGRATION=1 to run")
	}

	host := config.DefaultDockerHost
	if envHost := os.Getenv("PORTMAP_DOCKER_HOST"); envHost != "" {
		host = envHost
	}

	cfg := config.DockerConfig{
		Host:        host,
		HelperImage: config.DefaultDockerHelperImage,
	}

	instID := fmt.Sprintf("test-integ-%d", time.Now().UnixNano())

	// Probe daemon connectivity
	dcli, err := dockerclient.NewClient(cfg, instID)
	if err != nil {
		t.Skipf("skipping live Docker test: cannot create docker client: %v", err)
	}

	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelProbe()
	_, err = dcli.CommonClient().Ping(probeCtx)
	if err != nil {
		t.Skipf("skipping live Docker test: docker daemon unreachable at %s: %v", host, err)
	}

	// Pull helper image and an unexposed target image (e.g. hashicorp/http-echo or nginx:alpine or traefik/whoami)
	pullCtx, cancelPull := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelPull()

	if err := dcli.EnsureHelperImage(pullCtx, cfg.HelperImage); err != nil {
		t.Fatalf("EnsureHelperImage failed: %v", err)
	}

	targetImage := "hashicorp/http-echo:latest"
	_, _, err = dcli.CommonClient().ImageInspectWithRaw(pullCtx, targetImage)
	if err != nil {
		reader, err := dcli.CommonClient().ImagePull(pullCtx, targetImage, image.PullOptions{})
		if err != nil {
			t.Fatalf("ImagePull target image %s: %v", targetImage, err)
		}
		_, _ = io.Copy(io.Discard, reader)
		_ = reader.Close()
	}

	// Create test target container without publishing port 80/5678 to host
	targetName := fmt.Sprintf("portmap-target-%s", instID)
	createResp, err := dcli.CommonClient().ContainerCreate(pullCtx,
		&container.Config{
			Image: targetImage,
			Cmd:   []string{"-text=hello-from-portmap-integration"},
			// No PublishedPorts / HostConfig port bindings
		},
		&container.HostConfig{},
		nil,
		nil,
		targetName,
	)
	if err != nil {
		t.Fatalf("create target container: %v", err)
	}
	targetID := createResp.ID

	// Register test cleanup for target container
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stopTimeout := 2
		_ = dcli.CommonClient().ContainerStop(cleanupCtx, targetID, container.StopOptions{Timeout: &stopTimeout})
		_ = dcli.CommonClient().ContainerRemove(cleanupCtx, targetID, container.RemoveOptions{Force: true})
	})

	if err := dcli.CommonClient().ContainerStart(pullCtx, targetID, container.StartOptions{}); err != nil {
		t.Fatalf("start target container: %v", err)
	}

	// Allocate two free local host ports
	getFreePort := func() int {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("allocate free port: %v", err)
		}
		p := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		return p
	}

	port1 := getFreePort()
	port2 := getFreePort()

	m1 := config.Mapping{
		Name:          "fwd-integ-1",
		Type:          config.TypeDocker,
		Container:     targetName,
		ContainerPort: 5678,
		ListenPort:    port1,
		BindHost:      "127.0.0.1",
	}

	m2 := config.Mapping{
		Name:          "fwd-integ-2",
		Type:          config.TypeDocker,
		Container:     targetName,
		ContainerPort: 5678,
		ListenPort:    port2,
		BindHost:      "127.0.0.1",
	}

	fwd1 := dockerclient.NewForward(dcli, m1, "integ-group", instID, cfg.HelperImage)
	fwd2 := dockerclient.NewForward(dcli, m2, "integ-group", instID, cfg.HelperImage)

	t.Cleanup(func() {
		_ = fwd1.Close()
		_ = fwd2.Close()
	})

	if err := fwd1.Start(); err != nil {
		t.Fatalf("fwd1.Start: %v", err)
	}
	if err := fwd2.Start(); err != nil {
		t.Fatalf("fwd2.Start: %v", err)
	}

	fetchHTTP := func(port int) (string, error) {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d", port))
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}

	// Verify both forwards access target HTTP content
	body1, err := fetchHTTP(port1)
	if err != nil || !strings.Contains(body1, "hello-from-portmap-integration") {
		t.Fatalf("fwd1 HTTP response mismatch: err=%v body=%q", err, body1)
	}

	body2, err := fetchHTTP(port2)
	if err != nil || !strings.Contains(body2, "hello-from-portmap-integration") {
		t.Fatalf("fwd2 HTTP response mismatch: err=%v body=%q", err, body2)
	}

	// Stop fwd1; fwd2 must remain functional
	if err := fwd1.Close(); err != nil {
		t.Fatalf("fwd1.Close failed: %v", err)
	}

	body2After, err := fetchHTTP(port2)
	if err != nil || !strings.Contains(body2After, "hello-from-portmap-integration") {
		t.Fatalf("fwd2 after stopping fwd1 failed: err=%v body=%q", err, body2After)
	}

	// Stop fwd2
	if err := fwd2.Close(); err != nil {
		t.Fatalf("fwd2.Close failed: %v", err)
	}

	// Target container must still be running
	inspectCtx, cancelInspect := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelInspect()
	targetInspect, err := dcli.CommonClient().ContainerInspect(inspectCtx, targetID)
	if err != nil || targetInspect.State == nil || !targetInspect.State.Running {
		t.Fatalf("expected target container still running after forwards stopped: %v", err)
	}

	// Assert no helper/network remains for this instanceID using labels list (do not rely solely on CleanupOwned)
	queryCtx, cancelQuery := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelQuery()

	ownedFilter := filters.NewArgs(
		filters.Arg("label", fmt.Sprintf("%s=true", dockerclient.LabelManaged)),
		filters.Arg("label", fmt.Sprintf("%s=%s", dockerclient.LabelInstanceID, instID)),
	)

	remainingContainers, err := dcli.CommonClient().ContainerList(queryCtx, container.ListOptions{
		All:     true,
		Filters: ownedFilter,
	})
	if err != nil {
		t.Fatalf("ContainerList check after Close failed: %v", err)
	}
	if len(remainingContainers) != 0 {
		t.Fatalf("expected 0 helper containers remaining after Close, found %d: %+v", len(remainingContainers), remainingContainers)
	}

	remainingNetworks, err := dcli.CommonClient().NetworkList(queryCtx, network.ListOptions{
		Filters: ownedFilter,
	})
	if err != nil {
		t.Fatalf("NetworkList check after Close failed: %v", err)
	}
	if len(remainingNetworks) != 0 {
		t.Fatalf("expected 0 networks remaining after Close, found %d: %+v", len(remainingNetworks), remainingNetworks)
	}

	// Test Crash / Recovery simulation:
	// Manually create an orphaned network connected to test target container,
	// and an orphaned helper container with our instanceID labels.
	// t.Cleanup ensures fallback recovery of test-created resources.
	orphanNetName := fmt.Sprintf("portmap-net-%s-orphan", instID)
	orphanNetResp, err := dcli.CommonClient().NetworkCreate(queryCtx, orphanNetName, network.CreateOptions{
		Driver: "bridge",
		Labels: map[string]string{
			dockerclient.LabelManaged:    "true",
			dockerclient.LabelInstanceID: instID,
		},
	})
	if err != nil {
		t.Fatalf("create orphan network: %v", err)
	}
	orphanNetID := orphanNetResp.ID

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = dcli.CommonClient().NetworkDisconnect(ctx, orphanNetID, targetID, true)
		_ = dcli.CommonClient().NetworkRemove(ctx, orphanNetID)
	})

	if err := dcli.CommonClient().NetworkConnect(queryCtx, orphanNetID, targetID, nil); err != nil {
		t.Fatalf("connect target to orphan network: %v", err)
	}

	orphanHelperName := fmt.Sprintf("portmap-helper-%s-orphan", instID)
	orphanHelperResp, err := dcli.CommonClient().ContainerCreate(queryCtx,
		&container.Config{
			Image: cfg.HelperImage,
			Labels: map[string]string{
				dockerclient.LabelManaged:    "true",
				dockerclient.LabelInstanceID: instID,
			},
			Cmd: []string{"sleep", "infinity"},
		},
		&container.HostConfig{},
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				orphanNetName: {NetworkID: orphanNetID},
			},
		},
		nil,
		orphanHelperName,
	)
	if err != nil {
		t.Fatalf("create orphan helper container: %v", err)
	}
	orphanHelperID := orphanHelperResp.ID

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stopTimeout := 2
		_ = dcli.CommonClient().ContainerStop(ctx, orphanHelperID, container.StopOptions{Timeout: &stopTimeout})
		_ = dcli.CommonClient().ContainerRemove(ctx, orphanHelperID, container.RemoveOptions{Force: true})
	})

	// Run CleanupOwned to purge orphaned resources
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCleanup()
	if err := dcli.CleanupOwned(cleanupCtx); err != nil {
		t.Fatalf("CleanupOwned orphan cleanup failed: %v", err)
	}

	// Verify orphan helper container is gone
	_, _, err = dcli.CommonClient().ContainerInspectWithRaw(cleanupCtx, orphanHelperID, false)
	if !client.IsErrNotFound(err) {
		t.Fatalf("expected orphan helper container removed by CleanupOwned, got err: %v", err)
	}

	// Verify orphan network is gone
	_, err = dcli.CommonClient().NetworkInspect(cleanupCtx, orphanNetID, network.InspectOptions{})
	if !client.IsErrNotFound(err) {
		t.Fatalf("expected orphan network removed by CleanupOwned, got err: %v", err)
	}

	// Target container must still be running after orphan cleanup
	targetInspectAfter, err := dcli.CommonClient().ContainerInspect(cleanupCtx, targetID)
	if err != nil || targetInspectAfter.State == nil || !targetInspectAfter.State.Running {
		t.Fatalf("expected target container still running after orphan CleanupOwned: %v", err)
	}
}
