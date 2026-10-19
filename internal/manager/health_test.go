package manager_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/forward"
	"github.com/zxhtom/portmap/internal/manager"
)

type unhealthyBackend struct {
	fakeBackend
	fail bool
}

func (b *unhealthyBackend) Check(context.Context) error {
	if b.fail {
		return fmt.Errorf("destination refused")
	}
	return nil
}

func TestHealthCircuitBreakerSurvivesRestartAndManualReset(t *testing.T) {
	m, dir, _ := setupRecovery(t)
	attempts := 0
	factory := func(_ string, _ config.Mapping, _ config.Remote, _ config.DockerConfig, _ string) (forward.Backend, error) {
		attempts++
		return &unhealthyBackend{fail: true}, nil
	}
	m.SetBackendFactory(factory)
	if err := m.StartMapping("web", "app"); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 11; n++ {
		if err := m.Reconcile(); err != nil {
			t.Fatal(err)
		}
		row := m.ListMappings()[0]
		if row.ConsecutiveFailures != n {
			t.Fatalf("round %d: %+v", n, row)
		}
		if n <= 10 && row.Health != "重试中" {
			t.Fatalf("premature invalidation: %+v", row)
		}
	}
	if row := m.ListMappings()[0]; row.Status != "invalid" || row.Health != "作废" {
		t.Fatalf("not invalid: %+v", row)
	}
	before := attempts
	m.Reconcile()
	if attempts != before {
		t.Fatal("invalid tunnel retried")
	}
	m.Shutdown()
	next := manager.New(dir)
	next.SetBackendFactory(factory)
	if err := next.Init(); err != nil {
		t.Fatal(err)
	}
	defer next.Shutdown()
	if attempts != before {
		t.Fatal("restart retried invalid tunnel")
	}
	if row := next.ListMappings()[0]; row.Health != "作废" || row.ConsecutiveFailures != 11 {
		t.Fatalf("lost persisted state: %+v", row)
	}
	next.SetBackendFactory(func(_ string, _ config.Mapping, _ config.Remote, _ config.DockerConfig, _ string) (forward.Backend, error) {
		return &unhealthyBackend{}, nil
	})
	if err := next.StartMapping("web", "app"); err != nil {
		t.Fatal(err)
	}
	if err := next.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if row := next.ListMappings()[0]; row.Health != "正常" || row.ConsecutiveFailures != 0 {
		t.Fatalf("manual reset failed: %+v", row)
	}
	if err := next.StopMapping("web", "app"); err != nil {
		t.Fatal(err)
	}
	next.Reconcile()
	if row := next.ListMappings()[0]; row.Health != "已停止" || row.ConsecutiveFailures != 0 {
		t.Fatalf("stopped tunnel checked: %+v", row)
	}
}

func TestHealthSuccessfulRoundResetsFailures(t *testing.T) {
	m, _, _ := setupRecovery(t)
	fail := true
	var current *unhealthyBackend
	m.SetBackendFactory(func(_ string, _ config.Mapping, _ config.Remote, _ config.DockerConfig, _ string) (forward.Backend, error) {
		current = &unhealthyBackend{fail: fail}
		return current, nil
	})
	if err := m.StartMapping("web", "app"); err != nil {
		t.Fatal(err)
	}
	m.Reconcile()
	if m.ListMappings()[0].ConsecutiveFailures != 1 {
		t.Fatal("failure not counted")
	}
	fail = false
	current.fail = false
	m.Reconcile()
	if row := m.ListMappings()[0]; row.ConsecutiveFailures != 0 || row.Health != "正常" {
		t.Fatalf("healthy round did not reset: %+v", row)
	}
}
