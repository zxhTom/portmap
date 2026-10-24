package manager_test

import (
	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/relay"
	"github.com/zxhtom/portmap/internal/relayprotocol"
	"testing"
)

func TestRemoteZonesPinExistingAndValidateNewMappings(t *testing.T) {
	m, dir, _ := setupRecovery(t)
	if err := m.Init(); err != nil {
		t.Fatal(err)
	}
	central, err := relay.New(t.TempDir(), relayprotocol.DefaultZone)
	if err != nil {
		t.Fatal(err)
	}
	defer central.Close()
	m.SetDomainRequester(func(_ config.Remote, r relayprotocol.Request) (relayprotocol.Response, error) {
		return central.Apply(r)
	})
	if err := m.SetDomains("web", "app", []string{"old"}, "http"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetRemoteDomainZones("vps", []string{relayprotocol.DefaultZone, "second.example"}, "second.example"); err != nil {
		t.Fatal(err)
	}
	if got := m.ListMappings()[0].Domains[0]; got != "old."+relayprotocol.DefaultZone {
		t.Fatalf("old URL changed: %s", got)
	}
	if err := m.SetDomains("web", "app", []string{"edited"}, "http", ""); err != nil {
		t.Fatal(err)
	}
	if got := m.ListMappings()[0].Domains[0]; got != "edited."+relayprotocol.DefaultZone {
		t.Fatalf("legacy caller moved suffix: %s", got)
	}

	m.SetRelayPortAllocator(func(_ config.Remote, requested int, _ func(int) bool) (int, error) { return requested, nil })
	mp := config.Mapping{Name: "new", Remote: "vps", LocalHost: "127.0.0.1", LocalPort: 80, ListenPort: 31002, DomainPrefixes: []string{"new"}}
	created, err := m.CreateMapping("web", mp)
	if err != nil || created.DomainZone != "second.example" {
		t.Fatalf("primary not selected: %+v %v", created, err)
	}
	mp.Name = "bad"
	mp.ListenPort = 31003
	mp.DomainZone = "other.example"
	if _, err = m.CreateMapping("web", mp); err == nil {
		t.Fatal("accepted unbound suffix")
	}
	stored, err := config.LoadConfig(dir)
	if err != nil || stored.Remotes[0].DomainZone != "second.example" {
		t.Fatal("settings not persisted")
	}
}
