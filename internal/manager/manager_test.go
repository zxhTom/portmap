package manager_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/forward"
	"github.com/zxhtom/portmap/internal/manager"
)

type fakeBackend struct {
	mu          sync.Mutex
	failStart   bool
	failClose   bool
	started     bool
	closed      bool
	enteredChan chan struct{}
	blockChan   chan struct{}
}

func (f *fakeBackend) Start() error {
	if f.enteredChan != nil {
		select {
		case f.enteredChan <- struct{}{}:
		default:
		}
	}
	if f.blockChan != nil {
		<-f.blockChan
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failStart {
		return errors.New("simulated start failure")
	}
	f.started = true
	return nil
}

func (f *fakeBackend) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failClose {
		return errors.New("simulated close failure")
	}
	f.closed = true
	f.started = false
	return nil
}

func (f *fakeBackend) Status() forward.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.started {
		return forward.Status{State: forward.StateRunning}
	}
	if f.failStart {
		return forward.Status{State: forward.StateFailed, Error: "simulated start failure"}
	}
	return forward.Status{State: forward.StateStopped}
}

func TestManager_FakeBackendLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := manager.New(tmpDir)

	mgr.SetBackendFactory(func(group string, mp config.Mapping, remote config.Remote, dockerCfg config.DockerConfig, instanceID string) (forward.Backend, error) {
		fb := &fakeBackend{}
		if mp.Name == "failing" {
			fb.failStart = true
		}
		return fb, nil
	})

	if err := mgr.AddRemote(config.Remote{Name: "vps1", Host: "127.0.0.1", Port: 22}); err != nil {
		t.Fatalf("AddRemote: %v", err)
	}

	// Add failing mapping
	err := mgr.AddMapping("default", config.Mapping{
		Name:       "failing",
		Remote:     "vps1",
		ListenPort: 8080,
		LocalHost:  "127.0.0.1",
		LocalPort:  80,
	})
	if err != nil {
		t.Fatalf("AddMapping failing: %v", err)
	}

	// Add normal mapping
	err = mgr.AddMapping("default", config.Mapping{
		Name:       "normal",
		Remote:     "vps1",
		ListenPort: 8081,
		LocalHost:  "127.0.0.1",
		LocalPort:  81,
	})
	if err != nil {
		t.Fatalf("AddMapping normal: %v", err)
	}

	// Start normal
	if err := mgr.StartMapping("default", "normal"); err != nil {
		t.Fatalf("StartMapping normal failed: %v", err)
	}

	// Start failing
	err = mgr.StartMapping("default", "failing")
	if err == nil {
		t.Fatalf("expected StartMapping failing to return error")
	}

	// Check mapping statuses
	list := mgr.ListMappings()
	var normalFound, failingFound bool
	for _, m := range list {
		if m.Name == "normal" {
			normalFound = true
			if m.Status != string(forward.StateRunning) {
				t.Fatalf("expected normal mapping running, got %s", m.Status)
			}
		}
		if m.Name == "failing" {
			failingFound = true
			if m.Status != string(forward.StateFailed) {
				t.Fatalf("expected failing mapping failed, got %s", m.Status)
			}
			if m.Error == "" {
				t.Fatalf("expected non-empty error message for failing mapping")
			}
		}
	}
	if !normalFound || !failingFound {
		t.Fatalf("mappings not found in list: %+v", list)
	}

	// Manual stop normal clears running expectation
	if err := mgr.StopMapping("default", "normal"); err != nil {
		t.Fatalf("StopMapping normal failed: %v", err)
	}

	// Test non-blocking concurrency: a slow starting backend shouldn't block ListMappings or another mapping
	slowBackend := &fakeBackend{blockChan: make(chan struct{})}
	mgr.SetBackendFactory(func(group string, mp config.Mapping, remote config.Remote, dockerCfg config.DockerConfig, instanceID string) (forward.Backend, error) {
		return slowBackend, nil
	})

	startDone := make(chan error, 1)
	go func() {
		startDone <- mgr.StartMapping("default", "normal")
	}()

	// Ensure ListMappings does not hang while start is blocked
	time.Sleep(50 * time.Millisecond)
	listDone := make(chan struct{})
	go func() {
		_ = mgr.ListMappings()
		close(listDone)
	}()

	select {
	case <-listDone:
		// success: list didn't hang
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("ListMappings was blocked by concurrent StartMapping")
	}

	// Unblock start
	close(slowBackend.blockChan)
	if err := <-startDone; err != nil {
		t.Fatalf("slow start failed: %v", err)
	}

	mgr.Shutdown()
}

func TestManager_BlockedStart_RacingWithStopDeleteShutdown(t *testing.T) {
	// Case 1: Start blocked, Stop called in background before Start completes.
	{
		tmpDir := t.TempDir()
		mgr := manager.New(tmpDir)
		_ = mgr.AddRemote(config.Remote{Name: "vps1", Host: "127.0.0.1", Port: 22})
		_ = mgr.AddMapping("default", config.Mapping{Name: "m1", Remote: "vps1", ListenPort: 9001, LocalHost: "127.0.0.1", LocalPort: 80})

		gate := make(chan struct{})
		entered := make(chan struct{}, 1)
		mgr.SetBackendFactory(func(group string, mp config.Mapping, remote config.Remote, dockerCfg config.DockerConfig, instanceID string) (forward.Backend, error) {
			return &fakeBackend{blockChan: gate, enteredChan: entered}, nil
		})

		startErrCh := make(chan error, 1)
		go func() {
			startErrCh <- mgr.StartMapping("default", "m1")
		}()

		// Wait until backend.Start() is entered
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for backend.Start() to enter")
		}

		stopErrCh := make(chan error, 1)
		go func() {
			stopErrCh <- mgr.StopMapping("default", "m1")
		}()

		// Give StopMapping time to mark stopRequested and wait on startMu
		time.Sleep(50 * time.Millisecond)

		// Now unblock gate
		close(gate)

		select {
		case <-startErrCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for StartMapping")
		}

		select {
		case stopErr := <-stopErrCh:
			if stopErr != nil {
				t.Fatalf("StopMapping failed: %v", stopErr)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for StopMapping")
		}

		// State must remain stopped!
		mappings := mgr.ListMappings()
		if len(mappings) != 1 || mappings[0].Status == string(forward.StateRunning) {
			t.Fatalf("expected mapping to be stopped, got status: %s", mappings[0].Status)
		}
		mgr.Shutdown()
	}

	// Case 2: Start blocked, Delete called before Start completes.
	{
		tmpDir := t.TempDir()
		mgr := manager.New(tmpDir)
		_ = mgr.AddRemote(config.Remote{Name: "vps1", Host: "127.0.0.1", Port: 22})
		_ = mgr.AddMapping("default", config.Mapping{Name: "m2", Remote: "vps1", ListenPort: 9002, LocalHost: "127.0.0.1", LocalPort: 80})

		gate := make(chan struct{})
		entered := make(chan struct{}, 1)
		mgr.SetBackendFactory(func(group string, mp config.Mapping, remote config.Remote, dockerCfg config.DockerConfig, instanceID string) (forward.Backend, error) {
			return &fakeBackend{blockChan: gate, enteredChan: entered}, nil
		})

		startErrCh := make(chan error, 1)
		go func() {
			startErrCh <- mgr.StartMapping("default", "m2")
		}()

		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for backend.Start() to enter")
		}

		deleteDone := make(chan error, 1)
		go func() {
			deleteDone <- mgr.DeleteMapping("default", "m2")
		}()

		time.Sleep(50 * time.Millisecond)
		close(gate)

		select {
		case <-startErrCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for StartMapping")
		}

		select {
		case err := <-deleteDone:
			if err != nil {
				t.Fatalf("DeleteMapping failed: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for DeleteMapping")
		}

		mappings := mgr.ListMappings()
		if len(mappings) != 0 {
			t.Fatalf("expected mapping to be deleted, got: %+v", mappings)
		}
		mgr.Shutdown()
	}

	// Case 3: Start blocked, Shutdown called. Shutdown must wait for Start to finish before releasing instance lock.
	{
		tmpDir := t.TempDir()
		mgr := manager.New(tmpDir)
		if err := mgr.Init(); err != nil {
			t.Fatalf("Init failed: %v", err)
		}
		_ = mgr.AddRemote(config.Remote{Name: "vps1", Host: "127.0.0.1", Port: 22})
		_ = mgr.AddMapping("default", config.Mapping{Name: "m3", Remote: "vps1", ListenPort: 9003, LocalHost: "127.0.0.1", LocalPort: 80})

		gate := make(chan struct{})
		entered := make(chan struct{}, 1)
		mgr.SetBackendFactory(func(group string, mp config.Mapping, remote config.Remote, dockerCfg config.DockerConfig, instanceID string) (forward.Backend, error) {
			return &fakeBackend{blockChan: gate, enteredChan: entered}, nil
		})

		startErrCh := make(chan error, 1)
		go func() {
			startErrCh <- mgr.StartMapping("default", "m3")
		}()

		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for backend.Start() to enter")
		}

		shutdownDone := make(chan struct{})
		go func() {
			mgr.Shutdown()
			close(shutdownDone)
		}()

		// Verify Shutdown has not released the lock yet while gate is closed
		time.Sleep(50 * time.Millisecond)
		select {
		case <-shutdownDone:
			t.Fatalf("Shutdown completed before in-flight Start unblocked")
		default:
		}

		// Unblock start
		close(gate)

		select {
		case <-startErrCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for StartMapping")
		}

		select {
		case <-shutdownDone:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for Shutdown")
		}

		// Now lock should be cleanly released and a new manager can acquire it
		mgr2 := manager.New(tmpDir)
		if err := mgr2.Init(); err != nil {
			t.Fatalf("second manager failed to acquire instance lock: %v", err)
		}
		mgr2.Shutdown()
	}
}

func TestManager_BlockedStart_RacingWithStopGroup(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := manager.New(tmpDir)
	_ = mgr.AddRemote(config.Remote{Name: "vps1", Host: "127.0.0.1", Port: 22})
	_ = mgr.AddMapping("g1", config.Mapping{Name: "m1", Remote: "vps1", ListenPort: 9011, LocalHost: "127.0.0.1", LocalPort: 80})

	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	mgr.SetBackendFactory(func(group string, mp config.Mapping, remote config.Remote, dockerCfg config.DockerConfig, instanceID string) (forward.Backend, error) {
		return &fakeBackend{blockChan: gate, enteredChan: entered}, nil
	})

	startErrCh := make(chan error, 1)
	go func() {
		startErrCh <- mgr.StartMapping("g1", "m1")
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for backend.Start() to enter")
	}

	stopGroupDone := make(chan error, 1)
	go func() {
		stopGroupDone <- mgr.StopGroup("g1")
	}()

	time.Sleep(50 * time.Millisecond)
	close(gate)

	select {
	case <-startErrCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for StartMapping")
	}

	select {
	case err := <-stopGroupDone:
		if err != nil {
			t.Fatalf("StopGroup failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for StopGroup")
	}

	mappings := mgr.ListMappings()
	if len(mappings) != 1 || mappings[0].Status == string(forward.StateRunning) {
		t.Fatalf("expected mapping stopped after StopGroup, got: %+v", mappings)
	}
	mgr.Shutdown()
}

func TestManager_FailingStart_ThenDelete(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := manager.New(tmpDir)
	_ = mgr.AddRemote(config.Remote{Name: "vps1", Host: "127.0.0.1", Port: 22})
	_ = mgr.AddMapping("default", config.Mapping{Name: "fail-del", Remote: "vps1", ListenPort: 9021, LocalHost: "127.0.0.1", LocalPort: 80})

	mgr.SetBackendFactory(func(group string, mp config.Mapping, remote config.Remote, dockerCfg config.DockerConfig, instanceID string) (forward.Backend, error) {
		return &fakeBackend{failStart: true}, nil
	})

	err := mgr.StartMapping("default", "fail-del")
	if err == nil {
		t.Fatalf("expected StartMapping to fail")
	}

	// DeleteMapping should cleanly delete the mapping
	if err := mgr.DeleteMapping("default", "fail-del"); err != nil {
		t.Fatalf("DeleteMapping failed: %v", err)
	}

	mappings := mgr.ListMappings()
	if len(mappings) != 0 {
		t.Fatalf("expected 0 mappings, got %d", len(mappings))
	}
	mgr.Shutdown()
}

func TestManager_CloseFailure_Retries(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := manager.New(tmpDir)
	_ = mgr.AddRemote(config.Remote{Name: "vps1", Host: "127.0.0.1", Port: 22})
	_ = mgr.AddMapping("default", config.Mapping{Name: "close-fail", Remote: "vps1", ListenPort: 9031, LocalHost: "127.0.0.1", LocalPort: 80})

	fb := &fakeBackend{failClose: true}
	mgr.SetBackendFactory(func(group string, mp config.Mapping, remote config.Remote, dockerCfg config.DockerConfig, instanceID string) (forward.Backend, error) {
		return fb, nil
	})

	if err := mgr.StartMapping("default", "close-fail"); err != nil {
		t.Fatalf("StartMapping failed: %v", err)
	}

	// StopMapping fails because Close fails
	if err := mgr.StopMapping("default", "close-fail"); err == nil {
		t.Fatalf("expected StopMapping to fail when Close fails")
	}

	// Mapping should still exist and still have backend
	mappings := mgr.ListMappings()
	if len(mappings) != 1 {
		t.Fatalf("expected 1 mapping, got %d", len(mappings))
	}

	// DeleteMapping also fails when Close fails
	if err := mgr.DeleteMapping("default", "close-fail"); err == nil {
		t.Fatalf("expected DeleteMapping to fail when Close fails")
	}

	// Mapping must still exist in manager
	mappings = mgr.ListMappings()
	if len(mappings) != 1 {
		t.Fatalf("expected 1 mapping retained after failed delete, got %d", len(mappings))
	}

	// Now fix Close failure and retry Delete
	fb.mu.Lock()
	fb.failClose = false
	fb.mu.Unlock()

	if err := mgr.DeleteMapping("default", "close-fail"); err != nil {
		t.Fatalf("retry DeleteMapping failed: %v", err)
	}

	mappings = mgr.ListMappings()
	if len(mappings) != 0 {
		t.Fatalf("expected 0 mappings after successful delete, got %d", len(mappings))
	}
	mgr.Shutdown()
}

func TestManager_Stop_ThenRestart(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := manager.New(tmpDir)
	_ = mgr.AddRemote(config.Remote{Name: "vps1", Host: "127.0.0.1", Port: 22})
	_ = mgr.AddMapping("default", config.Mapping{Name: "restart-m", Remote: "vps1", ListenPort: 9041, LocalHost: "127.0.0.1", LocalPort: 80})

	var createdBackends []*fakeBackend
	var beMu sync.Mutex
	mgr.SetBackendFactory(func(group string, mp config.Mapping, remote config.Remote, dockerCfg config.DockerConfig, instanceID string) (forward.Backend, error) {
		fb := &fakeBackend{}
		beMu.Lock()
		createdBackends = append(createdBackends, fb)
		beMu.Unlock()
		return fb, nil
	})

	if err := mgr.StartMapping("default", "restart-m"); err != nil {
		t.Fatalf("initial StartMapping failed: %v", err)
	}

	if err := mgr.StopMapping("default", "restart-m"); err != nil {
		t.Fatalf("StopMapping failed: %v", err)
	}

	// Restart
	if err := mgr.StartMapping("default", "restart-m"); err != nil {
		t.Fatalf("restart StartMapping failed: %v", err)
	}

	mappings := mgr.ListMappings()
	if len(mappings) != 1 || mappings[0].Status != string(forward.StateRunning) {
		t.Fatalf("expected mapping running after restart, got: %+v", mappings)
	}

	mgr.Shutdown()
}

func TestManager_DockerOverridesAndCleanupOrder(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Write an initial config file with standard docker config
	initCfg := config.DefaultConfig()
	initCfg.Docker.Host = "unix:///var/run/custom.sock"
	initCfg.Docker.HelperImage = "custom-helper:latest"
	if err := config.SaveConfig(tmpDir, initCfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	mgr := manager.New(tmpDir)

	// Apply overrides: only override host, keep image nil
	overrideHost := "unix:///var/run/override.sock"
	mgr.SetDockerOverrides(&overrideHost, nil)

	if err := mgr.Init(); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	dc := mgr.DockerConfig()
	if dc.Host != overrideHost {
		t.Fatalf("expected Docker Host %q, got %q", overrideHost, dc.Host)
	}
	if dc.HelperImage != "custom-helper:latest" {
		t.Fatalf("expected Docker HelperImage %q, got %q", "custom-helper:latest", dc.HelperImage)
	}
	mgr.Shutdown()
}

func TestManager_StartFail_CloseFail_ThenDeleteRetry(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := manager.New(tmpDir)
	_ = mgr.AddRemote(config.Remote{Name: "vps1", Host: "127.0.0.1", Port: 22})
	_ = mgr.AddMapping("default", config.Mapping{
		Name:       "start-fail-close-fail",
		Remote:     "vps1",
		ListenPort: 9051,
		LocalHost:  "127.0.0.1",
		LocalPort:  80,
	})

	fb := &fakeBackend{
		failStart: true,
		failClose: true,
	}

	mgr.SetBackendFactory(func(group string, mp config.Mapping, remote config.Remote, dockerCfg config.DockerConfig, instanceID string) (forward.Backend, error) {
		return fb, nil
	})

	err := mgr.StartMapping("default", "start-fail-close-fail")
	if err == nil {
		t.Fatalf("expected StartMapping to fail")
	}

	// Because Close failed during Start cleanup, Backend must NOT have been discarded.
	// DeleteMapping should call Close() on the retained backend, which initially fails.
	if err := mgr.DeleteMapping("default", "start-fail-close-fail"); err == nil {
		t.Fatalf("expected DeleteMapping to fail because backend.Close fails")
	}

	// Mapping must still exist in manager
	mappings := mgr.ListMappings()
	if len(mappings) != 1 {
		t.Fatalf("expected 1 mapping retained after failed delete, got %d", len(mappings))
	}

	// Now clear the Close failure on the backend and retry Delete
	fb.mu.Lock()
	fb.failClose = false
	fb.mu.Unlock()

	if err := mgr.DeleteMapping("default", "start-fail-close-fail"); err != nil {
		t.Fatalf("retry DeleteMapping failed: %v", err)
	}

	// Mapping is now deleted
	mappings = mgr.ListMappings()
	if len(mappings) != 0 {
		t.Fatalf("expected 0 mappings after successful delete retry, got %d", len(mappings))
	}

	mgr.Shutdown()
}

func TestManager_StartFail_CloseFail_ThenStopRetry(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := manager.New(tmpDir)
	_ = mgr.AddRemote(config.Remote{Name: "vps1", Host: "127.0.0.1", Port: 22})
	_ = mgr.AddMapping("default", config.Mapping{
		Name:       "start-fail-stop-retry",
		Remote:     "vps1",
		ListenPort: 9061,
		LocalHost:  "127.0.0.1",
		LocalPort:  80,
	})

	fb := &fakeBackend{
		failStart: true,
		failClose: true,
	}

	mgr.SetBackendFactory(func(group string, mp config.Mapping, remote config.Remote, dockerCfg config.DockerConfig, instanceID string) (forward.Backend, error) {
		return fb, nil
	})

	err := mgr.StartMapping("default", "start-fail-stop-retry")
	if err == nil {
		t.Fatalf("expected StartMapping to fail")
	}

	// State.Running is false, but Backend is retained because Close failed.
	// Calling StopMapping should NOT reject with "mapping is not running", but attempt Close.
	if err := mgr.StopMapping("default", "start-fail-stop-retry"); err == nil {
		t.Fatalf("expected StopMapping to fail because backend.Close fails")
	}

	// Now fix Close and retry StopMapping
	fb.mu.Lock()
	fb.failClose = false
	fb.mu.Unlock()

	if err := mgr.StopMapping("default", "start-fail-stop-retry"); err != nil {
		t.Fatalf("retry StopMapping failed: %v", err)
	}

	// Backend is now cleared
	mgr.Shutdown()
}

func TestManager_Init_StalledDockerDaemon(t *testing.T) {
	tmpDir := t.TempDir()

	// Use /private/tmp directly for Unix socket to avoid path length limits on macOS
	sockDir, err := os.MkdirTemp("/private/tmp", "pm-stall-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(sockDir)
	})
	sockPath := filepath.Join(sockDir, "s.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	defer ln.Close()

	// Accept connections and hang
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Hang the connection
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1024)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	mgr := manager.New(tmpDir)
	mgr.SetCleanupTimeout(300 * time.Millisecond)

	// Configure mgr with the stalled docker host
	stalledHost := "unix://" + sockPath
	mgr.SetDockerOverrides(&stalledHost, nil)

	// Init should detect stalled timeout, warn, and NOT return fatal error.
	// SSH / API must still be allowed to run.
	if err := mgr.Init(); err != nil {
		t.Fatalf("expected Init to succeed despite stalled Docker daemon, got: %v", err)
	}

	// Add an SSH mapping and ensure SSH mapping works
	err = mgr.AddRemote(config.Remote{Name: "vps1", Host: "127.0.0.1", Port: 22})
	if err != nil {
		t.Fatalf("AddRemote failed: %v", err)
	}
	err = mgr.AddMapping("default", config.Mapping{
		Name:       "ssh-ok",
		Remote:     "vps1",
		ListenPort: 9071,
		LocalHost:  "127.0.0.1",
		LocalPort:  80,
	})
	if err != nil {
		t.Fatalf("AddMapping failed: %v", err)
	}

	mgr.SetBackendFactory(func(group string, mp config.Mapping, remote config.Remote, dockerCfg config.DockerConfig, instanceID string) (forward.Backend, error) {
		return &fakeBackend{}, nil
	})

	if err := mgr.StartMapping("default", "ssh-ok"); err != nil {
		t.Fatalf("StartMapping for SSH failed while Docker daemon stalled: %v", err)
	}

	list := mgr.ListMappings()
	if len(list) != 1 || list[0].Status != string(forward.StateRunning) {
		t.Fatalf("expected SSH mapping running, got: %+v", list)
	}

	mgr.Shutdown()
}




