package upgrade_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zxhtom/portmap/internal/upgrade"
)

func TestAtomicReplacementBackupAndRollback(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "portmap")
	source := filepath.Join(dir, "candidate")
	if err := os.WriteFile(target, []byte("old-binary"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("new-binary"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := []byte(`{"description":"中文用途","unknown_future_field":true}`)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), cfg, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"mappings":{"web/app":{"running":false}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	running, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close()
	backup, err := upgrade.Backup(target, dir)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := upgrade.Digest(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := upgrade.ReplaceVerified(source, target, sum); err != nil {
		t.Fatal(err)
	}
	actual, _ := os.ReadFile(target)
	if string(actual) != "new-binary" {
		t.Fatal("candidate not installed")
	}
	old := make([]byte, 10)
	if _, err := running.Read(old); err != nil {
		t.Fatal(err)
	}
	if string(old) != "old-binary" {
		t.Fatal("running executable inode was modified")
	}
	actual, _ = os.ReadFile(filepath.Join(dir, "config.json"))
	if string(actual) != string(cfg) {
		t.Fatal("configuration was modified")
	}
	mode, _ := os.Stat(target)
	if mode.Mode().Perm() != 0755 {
		t.Fatal("executable mode changed")
	}
	mode, _ = os.Stat(filepath.Join(backup, "config.json"))
	if mode.Mode().Perm() != 0600 {
		t.Fatal("backup leaked configuration")
	}
	if err := upgrade.Replace(filepath.Join(backup, "portmap"), target); err != nil {
		t.Fatal(err)
	}
	actual, _ = os.ReadFile(target)
	if string(actual) != "old-binary" {
		t.Fatal("rollback failed")
	}
}

func TestRejectsChangedCandidateAndSymlinkWithoutTouchingTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "portmap")
	source := filepath.Join(dir, "source")
	os.WriteFile(target, []byte("old"), 0755)
	os.WriteFile(source, []byte("new"), 0755)
	if err := upgrade.ReplaceVerified(source, target, "wrong-checksum"); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "old" {
		t.Fatal("checksum failure modified target")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := upgrade.Replace(source, link); err == nil {
		t.Fatal("symlink target accepted")
	}
	if err := upgrade.Replace(filepath.Join(dir, "absent"), target); err == nil {
		t.Fatal("missing candidate accepted")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".portmap-update-*"))
	if len(matches) != 0 {
		t.Fatal("staging files leaked")
	}
}
func TestUpgradeLockExcludesConcurrentUpdates(t *testing.T) {
	dir := t.TempDir()
	release, err := upgrade.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := upgrade.Lock(dir); err == nil {
		second()
		t.Fatal("concurrent update accepted")
	}
	release()
	second, err := upgrade.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	second()
}
