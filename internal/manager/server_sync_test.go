package manager_test

import (
	"fmt"
	"testing"

	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/relay"
	"github.com/zxhtom/portmap/internal/relayprotocol"
)

func TestServerSyncDisableAndResume(t *testing.T) {
	m, _, made := setupRecovery(t)
	if err := m.Init(); err != nil {
		t.Fatal(err)
	}
	inventory, err := relay.NewInventory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.SetServerRequester(func(_ config.Remote, req relayprotocol.Request) (relayprotocol.Response, error) {
		switch req.Operation {
		case "tunnel_check":
			disabled, err := inventory.Check(req.Owner)
			return relayprotocol.Response{Disabled: disabled}, err
		case "tunnel_snapshot":
			owners, err := inventory.Snapshot(req.ClientID, req.ClientName, req.Mappings)
			return relayprotocol.Response{DisabledOwners: owners}, err
		}
		return relayprotocol.Response{}, fmt.Errorf("unexpected request %s", req.Operation)
	})
	if err := m.StartMapping("web", "app"); err != nil {
		t.Fatal(err)
	}
	m.SyncServers()
	records := inventory.List()
	if len(records) != 1 || !records[0].Online || records[0].Target != "127.0.0.1:80" {
		t.Fatalf("snapshot: %+v", records)
	}
	if err := inventory.SetDisabled(records[0].Owner, true); err != nil {
		t.Fatal(err)
	}
	m.SyncServers()
	if !(*made)[0].closed {
		t.Fatal("disabled mapping stayed open")
	}
	if row := m.ListMappings()[0]; !row.DesiredRunning || row.Status == "running" {
		t.Fatalf("disabled mapping: %+v", row)
	}
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(*made) != 1 {
		t.Fatal("disabled mapping restarted")
	}
	if err := inventory.SetDisabled(records[0].Owner, false); err != nil {
		t.Fatal(err)
	}
	m.SyncServers()
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(*made) != 2 || m.ListMappings()[0].Status != "running" {
		t.Fatal("mapping did not resume")
	}
}

func TestNoServerStillAllowsMapping(t *testing.T) {
	m, _, made := setupRecovery(t)
	if err := m.Init(); err != nil {
		t.Fatal(err)
	}
	m.SetServerRequester(func(_ config.Remote, _ relayprotocol.Request) (relayprotocol.Response, error) {
		return relayprotocol.Response{}, fmt.Errorf("command not found")
	})
	if err := m.StartMapping("web", "app"); err != nil {
		t.Fatal(err)
	}
	m.SyncServers()
	if len(*made) != 1 || m.ListMappings()[0].Status != "running" {
		t.Fatal("plain SSH VPS was blocked")
	}
}
