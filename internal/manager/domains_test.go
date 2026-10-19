package manager_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/relay"
	"github.com/zxhtom/portmap/internal/relayprotocol"
)

func TestDomainsSetPreservesMetadataAndStopAndReleasesOnDelete(t *testing.T) {
	m, _, _ := setupRecovery(t)
	central, err := relay.New(t.TempDir(), relayprotocol.DefaultZone)
	if err != nil {
		t.Fatal(err)
	}
	defer central.Close()
	m.SetDomainRequester(func(_ config.Remote, r relayprotocol.Request) (relayprotocol.Response, error) {
		return central.Apply(r)
	})
	if err := m.DescribeMapping("web", "app", "keep this description"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetDomains("web", "app", []string{"application", "api"}, "http"); err != nil {
		t.Fatal(err)
	}
	row := m.ListMappings()[0]
	if row.Status != "stopped" || len(row.Domains) != 2 || row.Domains[0] != "application.port.toms.chat" || row.Description != "keep this description" {
		t.Fatalf("domain edit lost metadata or running intent: %+v", row)
	}
	foreign := relayprotocol.Request{Operation: "reserve", Owner: strings.Repeat("foreign", 8), Prefixes: []string{"application"}, Port: 31333, Scheme: "http"}
	if _, err := central.Apply(foreign); err == nil {
		t.Fatal("domain not centrally reserved")
	}
	if err := m.DeleteMapping("web", "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := central.Apply(foreign); err != nil {
		t.Fatalf("delete retained domain ownership: %v", err)
	}
}
func TestDomainConflictDoesNotInterruptRunningMapping(t *testing.T) {
	m, _, made := setupRecovery(t)
	m.StartMapping("web", "app")
	m.SetDomainRequester(func(_ config.Remote, r relayprotocol.Request) (relayprotocol.Response, error) {
		return relayprotocol.Response{}, fmt.Errorf("域名已绑定其他隧道")
	})
	if err := m.SetDomains("web", "app", []string{"claimed"}, "http"); err == nil {
		t.Fatal("domain conflict accepted")
	}
	b := (*made)[0]
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if closed || m.ListMappings()[0].Status != "running" {
		t.Fatal("failed domain preflight interrupted healthy tunnel")
	}
}

func TestGlobalDomainZoneChecksRelayAndPersists(t *testing.T) {
	m, dir, _ := setupRecovery(t)
	m.SetDomainRequester(func(_ config.Remote, req relayprotocol.Request) (relayprotocol.Response, error) {
		if req.Operation == "info" {
			return relayprotocol.Response{Zone: "old.example"}, nil
		}
		return relayprotocol.Response{}, nil
	})
	if err := m.SetDomains("web", "app", []string{"application"}, "http"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetDomainZone("NEW.EXAMPLE."); err == nil {
		t.Fatal("accepted mismatched relay")
	}
	if got := m.DomainZone(); got != relayprotocol.DefaultZone {
		t.Fatalf("changed zone on failure: %s", got)
	}
	m.SetDomainRequester(func(_ config.Remote, req relayprotocol.Request) (relayprotocol.Response, error) {
		if req.Operation == "info" {
			return relayprotocol.Response{Zone: "new.example"}, nil
		}
		return relayprotocol.Response{}, nil
	})
	if err := m.SetDomainZone("NEW.EXAMPLE."); err != nil {
		t.Fatal(err)
	}
	if got := m.ListMappings()[0].Domains[0]; got != "application.new.example" {
		t.Fatalf("wrong domain: %s", got)
	}
	c, err := config.LoadConfig(dir)
	if err != nil || c.DomainZone != "new.example" {
		t.Fatalf("zone not persisted: %+v, %v", c, err)
	}
}
