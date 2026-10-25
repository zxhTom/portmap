package relay

import (
	"github.com/zxhtom/portmap/internal/relayprotocol"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManualWakeOfflineInvalidPersistsUntilDelivery(t *testing.T) {
	dir := t.TempDir()
	i, err := NewInventory(dir)
	if err != nil {
		t.Fatal(err)
	}
	client := strings.Repeat("a", 16)
	mp := recoveryReport(client, "asleep")
	mp.Invalid = true
	mp.DesiredRunning = false
	mp.Status = "invalid"
	i.Snapshot(client, "offline-host", []relayprotocol.MappingReport{mp}, 2)
	record := i.records[mp.Owner]
	record.LastSeen = time.Now().Add(-time.Hour)
	i.records[mp.Owner] = record
	if err = i.RequestRecovery(mp.Owner); err != nil {
		t.Fatalf("offline manual wake rejected: %v", err)
	}
	if len(i.RecoveryCommands(client)) != 0 {
		t.Fatal("delivered while control channel offline")
	}
	if !i.recovery[mp.Owner].Command.ExpiresAt.IsZero() {
		t.Fatal("expiry started before client connected")
	}
	i, err = NewInventory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(i.List()) != 1 || i.List()[0].RecoveryStatus != "waiting_client" {
		t.Fatal("offline request lost on server restart")
	}
	i.Snapshot(client, "offline-host", []relayprotocol.MappingReport{mp}, 2)
	commands := i.RecoveryCommands(client)
	if len(commands) != 1 || commands[0].Action != "start" || !time.Now().Before(commands[0].ExpiresAt) {
		t.Fatalf("manual wake not delivered: %+v", commands)
	}
	i.Acknowledge(client, []relayprotocol.RecoveryResult{{ID: commands[0].ID, Owner: mp.Owner, Status: "restarted"}})
	reloaded, err := NewInventory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.RecoveryCommands(client)) != 0 {
		t.Fatal("executed manual wake replayed after restart")
	}
}

func TestManualWakeWaitsForOldClientUpgrade(t *testing.T) {
	i, _ := NewInventory(t.TempDir())
	client := strings.Repeat("b", 16)
	mp := recoveryReport(client, "legacy")
	mp.Invalid = true
	i.Snapshot(client, "old-client", []relayprotocol.MappingReport{mp})
	if err := i.RequestRecovery(mp.Owner); err != nil {
		t.Fatal(err)
	}
	if len(i.RecoveryCommands(client)) != 0 || i.List()[0].RecoveryStatus != "waiting_upgrade" {
		t.Fatal("old client received unsupported manual command")
	}
	i.Snapshot(client, "new-client", []relayprotocol.MappingReport{mp}, 2)
	if len(i.RecoveryCommands(client)) != 1 {
		t.Fatal("upgrade did not unlock queued wake")
	}
}

func TestManualWakeOldClientStillReceivesSupportedAutomaticRestart(t *testing.T) {
	i, _ := NewInventory(t.TempDir())
	client := strings.Repeat("c", 16)
	mp := recoveryReport(client, "legacy-running")
	i.Snapshot(client, "old-client", []relayprotocol.MappingReport{mp})
	if err := i.RequestRecovery(mp.Owner); err != nil {
		t.Fatal(err)
	}
	commands := i.RecoveryCommands(client)
	if len(commands) != 1 || commands[0].Action != "" {
		t.Fatal("manual request blocked supported legacy recovery")
	}
}

func TestManualWakePersistenceFailureDoesNotStartDeliveryClock(t *testing.T) {
	dir := t.TempDir()
	i, _ := NewInventory(dir)
	client := strings.Repeat("d", 16)
	mp := recoveryReport(client, "durable")
	mp.Invalid = true
	i.Snapshot(client, "client", []relayprotocol.MappingReport{mp}, 2)
	if err := i.RequestRecovery(mp.Owner); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(bad, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	i.dir = bad
	if len(i.RecoveryCommands(client)) != 0 {
		t.Fatal("delivered without durable execution state")
	}
	if !i.recovery[mp.Owner].Command.ExpiresAt.IsZero() {
		t.Fatal("lost delivery window after persistence failure")
	}
	i.dir = dir
	commands := i.RecoveryCommands(client)
	if len(commands) != 1 || time.Until(commands[0].ExpiresAt) < 55*time.Second {
		t.Fatal("delivery did not receive full execution window")
	}
}
