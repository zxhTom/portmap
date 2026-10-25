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

func TestManualWakeStartsStoppedAndInvalidMapping(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			m, dir, made := setupRecovery(t)
			if err := m.Init(); err != nil {
				t.Fatal(err)
			}
			state := config.DefaultState()
			state.Mappings["web/app"] = config.MappingState{Running: invalid, Invalid: invalid, Failures: 11}
			if err := config.SaveState(dir, state); err != nil {
				t.Fatal(err)
			}
			if err := m.Reconcile(); err != nil {
				t.Fatal(err)
			}
			m.SetServerRequester(func(_ config.Remote, r relayprotocol.Request) (relayprotocol.Response, error) {
				if r.Operation == "tunnel_check" {
					return relayprotocol.Response{}, nil
				}
				if r.RecoveryProtocol < 2 {
					t.Fatal("manual wake protocol not advertised")
				}
				return relayprotocol.Response{RecoveryCommands: []relayprotocol.RecoveryCommand{{ID: "manual-wake", Owner: r.Mappings[0].Owner, Action: "start", ExpiresAt: time.Now().Add(time.Minute)}}}, nil
			})
			m.SyncServers()
			if len(*made) != 1 || m.ListMappings()[0].Status != "running" {
				t.Fatal("manual wake did not start stopped/invalid tunnel")
			}
			saved, _ := config.LoadState(dir)
			st := saved.Mappings["web/app"]
			if !st.Running || st.Invalid || st.Failures != 0 {
				t.Fatalf("breaker not reset: %+v", st)
			}
			m.SyncServers()
			if len(*made) != 1 {
				t.Fatal("manual command executed twice")
			}
		})
	}
}

func TestNewClientFallsBackToLegacyServerSnapshot(t *testing.T) {
	m, _, _ := setupRecovery(t)
	if err := m.Init(); err != nil {
		t.Fatal(err)
	}
	calls := 0
	m.SetServerRequester(func(_ config.Remote, r relayprotocol.Request) (relayprotocol.Response, error) {
		if r.Operation == "tunnel_check" {
			return relayprotocol.Response{}, nil
		}
		calls++
		if r.RecoveryProtocol > 0 {
			return relayprotocol.Response{}, fmt.Errorf(`legacy CLI rejected: json: unknown field "recovery_protocol"`)
		}
		return relayprotocol.Response{}, nil
	})
	m.SyncServers()
	if calls != 2 {
		t.Fatal("new capability field broke old-server synchronization")
	}
}
