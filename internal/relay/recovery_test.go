package relay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/zxhtom/portmap/internal/relayprotocol"
	"strings"
	"testing"
	"time"
)

func recoveryReport(client, name string) relayprotocol.MappingReport {
	sum := sha256.Sum256([]byte(client + "\x00web\x00" + name))
	return relayprotocol.MappingReport{Owner: hex.EncodeToString(sum[:]), Group: "web", Name: name, ListenHost: "127.0.0.1", ListenPort: 1, DesiredRunning: true, Status: "failed"}
}
func TestRecoveryCommandsScopedAcknowledgedAndBounded(t *testing.T) {
	i, _ := NewInventory(t.TempDir())
	client := strings.Repeat("a", 16)
	mp := recoveryReport(client, "app")
	i.Snapshot(client, "client", []relayprotocol.MappingReport{mp})
	commands := i.RecoveryCommands(client)
	if len(commands) != 1 {
		t.Fatal("missing automatic recovery")
	}
	if got := i.RecoveryCommands(strings.Repeat("b", 16)); len(got) != 0 {
		t.Fatal("cross-client recovery")
	}
	if err := i.Acknowledge(client, []relayprotocol.RecoveryResult{{ID: commands[0].ID, Owner: mp.Owner, Status: "restarted"}}); err != nil {
		t.Fatal(err)
	}
	i.recovery[mp.Owner].Status = "healthy"
	i.Acknowledge(client, []relayprotocol.RecoveryResult{{ID: commands[0].ID, Owner: mp.Owner, Status: "restarted"}})
	if i.recovery[mp.Owner].Status != "healthy" {
		t.Fatal("duplicate ack reverted health")
	}
	if len(i.RecoveryCommands(client)) != 0 {
		t.Fatal("replayed completed command")
	}
	if err := i.RequestRecovery(mp.Owner); err == nil {
		t.Fatal("ignored retry cooldown")
	}
}
func TestRecoveryDoesNotStartStoppedExpiredOrDisabled(t *testing.T) {
	for _, mode := range []string{"stopped", "expired", "disabled", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			i, _ := NewInventory(t.TempDir())
			client := strings.Repeat("a", 16)
			mp := recoveryReport(client, "app")
			switch mode {
			case "stopped":
				mp.DesiredRunning = false
			case "expired":
				mp.ExpiresAt = time.Now().Add(-time.Hour).Format(time.RFC3339)
			case "invalid":
				mp.Invalid = true
			}
			i.Snapshot(client, "client", []relayprotocol.MappingReport{mp})
			if mode == "disabled" {
				i.SetDisabled(mp.Owner, true)
			}
			if len(i.RecoveryCommands(client)) != 0 {
				t.Fatal("ineligible mapping auto-started")
			}
			if err := i.RequestRecovery(mp.Owner); err == nil {
				t.Fatal("ineligible mapping manually queued")
			}
		})
	}
}
func TestServerProbeQueuesMissingListener(t *testing.T) {
	i, _ := NewInventory(t.TempDir())
	client := strings.Repeat("a", 16)
	mp := recoveryReport(client, "app")
	mp.Status = "running"
	i.Snapshot(client, "client", []relayprotocol.MappingReport{mp})
	i.Probe(context.Background())
	if len(i.RecoveryCommands(client)) != 1 {
		t.Fatal("missing listener was not recovered")
	}
	if i.List()[0].Online {
		t.Fatal("missing listener marked online")
	}
}
