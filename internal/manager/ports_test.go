package manager_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/manager"
)

func TestCreateMappingRandomPortPersistedAndAliasesReserved(t *testing.T) {
	m, dir, _ := setupRecovery(t)
	if err := m.AddRemote(config.Remote{Name: "same-host-alias", Host: "127.0.0.1", Port: 22, User: "other"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	m.SetRelayPortAllocator(func(_ config.Remote, requested int, blocked func(int) bool) (int, error) {
		calls++
		if !blocked(19001) {
			t.Fatal("existing mapping not reserved")
		}
		if requested == 0 {
			return 41763, nil
		}
		return requested, nil
	})
	mp, err := m.CreateMapping("public", config.Mapping{Name: "web", Remote: "same-host-alias", LocalHost: "127.0.0.1", LocalPort: 9877})
	if err != nil {
		t.Fatal(err)
	}
	if mp.ListenPort != 41763 {
		t.Fatalf("random allocation lost: %+v", mp)
	}
	if _, err := m.CreateMapping("other", config.Mapping{Name: "conflict", Remote: "vps", LocalHost: "127.0.0.1", LocalPort: 22, ListenPort: 41763}); err == nil || !strings.Contains(err.Error(), "public/web") {
		t.Fatalf("alias conflict not reported: %v", err)
	}
	if calls != 1 {
		t.Fatal("configured conflict should not contact relay")
	}
	m.Shutdown()
	next := manager.New(dir)
	if err := next.Init(); err != nil {
		t.Fatal(err)
	}
	defer next.Shutdown()
	found := false
	for _, row := range next.ListMappings() {
		if row.Name == "web" {
			found = true
			if row.ListenPort != 41763 {
				t.Fatal("restart changed random port")
			}
		}
	}
	if !found {
		t.Fatal("random mapping missing")
	}
}
func TestCreateMappingRejectsUnavailableWithoutSavingAndSerializes(t *testing.T) {
	m, _, _ := setupRecovery(t)
	m.SetRelayPortAllocator(func(config.Remote, int, func(int) bool) (int, error) {
		return 0, fmt.Errorf("中转站端口已被占用")
	})
	if _, err := m.CreateMapping("test", config.Mapping{Name: "failed", Remote: "vps", LocalHost: "127.0.0.1", LocalPort: 22}); err == nil {
		t.Fatal("failed allocation saved")
	}
	if len(m.ListMappings()) != 1 {
		t.Fatal("failed probe polluted config")
	}
	m.SetRelayPortAllocator(func(_ config.Remote, _ int, blocked func(int) bool) (int, error) {
		for p := 30000; p < 30010; p++ {
			if !blocked(p) {
				return p, nil
			}
		}
		return 0, fmt.Errorf("no ports")
	})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := m.CreateMapping("test", config.Mapping{Name: fmt.Sprintf("parallel-%d", i), Remote: "vps", LocalHost: "127.0.0.1", LocalPort: 22})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[int]bool{}
	for _, row := range m.ListMappings() {
		if seen[row.ListenPort] {
			t.Fatal("concurrent allocation reused port")
		}
		seen[row.ListenPort] = true
	}
	if _, err := m.CreateMapping("test", config.Mapping{Name: "local-missing", Type: config.TypeDocker, Container: "test", ContainerPort: 80}); err == nil {
		t.Fatal("local missing listen accepted")
	}
}
