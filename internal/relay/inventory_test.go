package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/zxhtom/portmap/internal/relayprotocol"
)

func report(clientID, group, name string) relayprotocol.MappingReport {
	h := sha256.Sum256([]byte(clientID + "\x00" + group + "\x00" + name))
	return relayprotocol.MappingReport{Owner: hex.EncodeToString(h[:]), Group: group, Name: name, Type: "ssh", ListenPort: 12000, Target: "127.0.0.1:80", Status: "running", DesiredRunning: true}
}

func TestInventorySnapshotDisableAndPersistence(t *testing.T) {
	dir := t.TempDir()
	i, err := NewInventory(dir)
	if err != nil {
		t.Fatal(err)
	}
	client := "0123456789abcdef"
	mp := report(client, "web", "app")
	if _, err := i.Snapshot(client, "host", []relayprotocol.MappingReport{mp}); err != nil {
		t.Fatal(err)
	}
	if got := i.List(); len(got) != 1 || !got[0].Online {
		t.Fatalf("initial inventory: %+v", got)
	}
	if err := i.SetDisabled(mp.Owner, true); err != nil {
		t.Fatal(err)
	}
	if disabled, err := i.Check(mp.Owner); err != nil || !disabled {
		t.Fatalf("disabled=%v, err=%v", disabled, err)
	}
	owners, err := i.Snapshot(client, "host", []relayprotocol.MappingReport{mp})
	if err != nil || len(owners) != 1 || owners[0] != mp.Owner {
		t.Fatalf("owners=%v, err=%v", owners, err)
	}
	restarted, err := NewInventory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if disabled, err := restarted.Check(mp.Owner); err != nil || !disabled {
		t.Fatalf("disabled after restart=%v, err=%v", disabled, err)
	}
	if _, err := restarted.Snapshot(client, "host", []relayprotocol.MappingReport{mp}); err != nil {
		t.Fatal(err)
	}
	if err := restarted.SetDisabled(mp.Owner, false); err != nil {
		t.Fatal(err)
	}
	if disabled, _ := restarted.Check(mp.Owner); disabled {
		t.Fatal("enable did not clear policy")
	}
}

func TestInventoryRejectsForeignIdentityWithoutReplacingSnapshot(t *testing.T) {
	i, err := NewInventory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := "0123456789abcdef"
	mp := report(client, "web", "app")
	if _, err := i.Snapshot(client, "host", []relayprotocol.MappingReport{mp}); err != nil {
		t.Fatal(err)
	}
	other := report("abcdef0123456789", "web", "app")
	if _, err := i.Snapshot(client, "host", []relayprotocol.MappingReport{other}); err == nil {
		t.Fatal("accepted another client's owner")
	}
	if got := i.List(); len(got) != 1 || got[0].Owner != mp.Owner {
		t.Fatalf("mutated on invalid snapshot: %+v", got)
	}
}
