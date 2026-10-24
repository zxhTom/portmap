package manager_test

import (
	"fmt"
	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/relayprotocol"
	"testing"
	"time"
)

func TestServerCommandRebuildsOnlyDesiredMappingAndDeduplicates(t *testing.T) {
	m, _, made := setupRecovery(t)
	if err := m.Init(); err != nil {
		t.Fatal(err)
	}
	var command relayprotocol.RecoveryCommand
	var acknowledged bool
	m.SetServerRequester(func(_ config.Remote, r relayprotocol.Request) (relayprotocol.Response, error) {
		if r.Operation == "tunnel_check" {
			return relayprotocol.Response{}, nil
		}
		if r.Operation != "tunnel_snapshot" {
			return relayprotocol.Response{}, fmt.Errorf("unexpected")
		}
		if len(r.RecoveryResults) > 0 {
			acknowledged = true
		}
		if command.ID == "" {
			command = relayprotocol.RecoveryCommand{ID: "test-command", Owner: r.Mappings[0].Owner, ExpiresAt: time.Now().Add(time.Minute)}
		}
		return relayprotocol.Response{RecoveryCommands: []relayprotocol.RecoveryCommand{command}}, nil
	})
	if err := m.StartMapping("web", "app"); err != nil {
		t.Fatal(err)
	}
	m.SyncServers()
	if len(*made) != 2 {
		t.Fatal("server recovery did not recreate backend")
	}
	m.SyncServers()
	if len(*made) != 2 || !acknowledged {
		t.Fatal("command replayed or not acknowledged")
	}
	if err := m.StopMapping("web", "app"); err != nil {
		t.Fatal(err)
	}
	command.ID = "second-command"
	m.SyncServers()
	if len(*made) != 2 {
		t.Fatal("manual stop overridden")
	}
}
