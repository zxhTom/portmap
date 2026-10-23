package websession

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWebSessionPersistence(t *testing.T) {
	dir := t.TempDir()
	sessFile := filepath.Join(dir, "test-session.json")
	fp := CredentialFingerprint("admin", "hash1")

	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	nowFunc := func() time.Time { return now }

	mgr, err := New(sessFile, fp, WithNowFunc(nowFunc))
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	// 1. 普通登录（不记住）：TTL = 12h，不落盘
	tok1, _, exp1, maxAge1, err := mgr.CreateSession(false)
	if err != nil {
		t.Fatalf("CreateSession normal: %v", err)
	}
	if maxAge1 != 12*3600 {
		t.Fatalf("expected maxAge 12h, got %d", maxAge1)
	}
	if exp1.Sub(now) != DefaultTTL {
		t.Fatalf("expected exp 12h, got %v", exp1.Sub(now))
	}
	if _, err := os.Stat(sessFile); !os.IsNotExist(err) {
		t.Fatalf("normal session should not create file on disk")
	}

	// 2. 记住登录：TTL = 30天，落盘
	tok2, csrf2, exp2, maxAge2, err := mgr.CreateSession(true)
	if err != nil {
		t.Fatalf("CreateSession remember: %v", err)
	}
	if maxAge2 != 30*86400 {
		t.Fatalf("expected maxAge 30d, got %d", maxAge2)
	}
	if exp2.Sub(now) != RememberTTL {
		t.Fatalf("expected exp 30d, got %v", exp2.Sub(now))
	}

	// 检查磁盘文件权限与内容
	info, err := os.Stat(sessFile)
	if err != nil {
		t.Fatalf("stat session file: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("expected 0600 perms, got %v", info.Mode().Perm())
	}
	fileBytes, _ := os.ReadFile(sessFile)
	if string(fileBytes) == "" {
		t.Fatalf("session file empty")
	}
	// 验证磁盘上没有 raw token
	if bytesContains := func(b []byte, s string) bool {
		for i := 0; i+len(s) <= len(b); i++ {
			if string(b[i:i+len(s)]) == s {
				return true
			}
		}
		return false
	}; bytesContains(fileBytes, tok2) {
		t.Fatalf("raw token found in session file!")
	}

	// 3. 重启恢复：重建 Manager，tok2 仍有效，tok1 丢失
	mgr2, err := New(sessFile, fp, WithNowFunc(nowFunc))
	if err != nil {
		t.Fatalf("reboot manager: %v", err)
	}
	if _, ok := mgr2.Get(tok1); ok {
		t.Fatalf("normal session tok1 should not survive reboot")
	}
	s2, ok := mgr2.Get(tok2)
	if !ok {
		t.Fatalf("remember session tok2 should survive reboot")
	}
	if s2.CSRF != csrf2 {
		t.Fatalf("csrf mismatch")
	}

	// 4. 时间流逝测试：12小时后，tok1在mgr中自然过期（即使在同一进程中）
	now = now.Add(13 * time.Hour)
	if _, ok := mgr.Get(tok1); ok {
		t.Fatalf("tok1 should be expired after 13h")
	}
	// tok2 仍有效
	if _, ok := mgr.Get(tok2); !ok {
		t.Fatalf("tok2 should still be valid after 13h")
	}

	// 5. 30天边界测试：29天23小时仍有效，30天1秒过期
	now = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC).Add(29*24*time.Hour + 23*time.Hour)
	if _, ok := mgr2.Get(tok2); !ok {
		t.Fatalf("tok2 should be valid before 30d boundary")
	}
	now = now.Add(2 * time.Hour)
	if _, ok := mgr2.Get(tok2); ok {
		t.Fatalf("tok2 should expire after 30d boundary")
	}

	// 6. Logout：销毁 session 并清理持久化（持久空记录）
	now = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	mgr3, _ := New(sessFile, fp, WithNowFunc(nowFunc))
	tok3, _, _, _, _ := mgr3.CreateSession(true)
	if _, err := os.Stat(sessFile); os.IsNotExist(err) {
		t.Fatalf("session file should exist for tok3")
	}
	if err := mgr3.Delete(tok3); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, ok := mgr3.Get(tok3); ok {
		t.Fatalf("tok3 should be deleted")
	}
	// 重启后不应复活（磁盘文件应存有空记录）
	mgr4, err := New(sessFile, fp, WithNowFunc(nowFunc))
	if err != nil {
		t.Fatalf("reboot after delete: %v", err)
	}
	if _, ok := mgr4.Get(tok3); ok {
		t.Fatalf("tok3 should not resurrect after reboot")
	}

	// 7. 凭据变更 / 指纹不匹配测试
	tok5, _, _, _, _ := mgr4.CreateSession(true)
	fpChanged := CredentialFingerprint("admin", "new_hash")
	mgr5, err := New(sessFile, fpChanged, WithNowFunc(nowFunc))
	if err != nil {
		t.Fatalf("New with new fp: %v", err)
	}
	if _, ok := mgr5.Get(tok5); ok {
		t.Fatalf("tok5 should be invalid under changed fingerprint")
	}

	// 8. 损坏文件 fail-closed 测试（非json、版本不对、多余垃圾、超30天等）
	_ = os.WriteFile(sessFile, []byte("broken json content"), 0600)
	if _, err := New(sessFile, fp, WithNowFunc(nowFunc)); err == nil {
		t.Fatalf("expected error loading corrupted session file, got nil")
	}

	// 尾部垃圾
	_ = os.WriteFile(sessFile, []byte(`{"version":1,"fingerprint":"`+fp+`","sessions":[]} {"extra":1}`), 0600)
	if _, err := New(sessFile, fp, WithNowFunc(nowFunc)); err == nil {
		t.Fatalf("expected error loading trailing garbage, got nil")
	}

	// 版本不为1
	_ = os.WriteFile(sessFile, []byte(`{"version":2,"fingerprint":"`+fp+`","sessions":[]}`), 0600)
	if _, err := New(sessFile, fp, WithNowFunc(nowFunc)); err == nil {
		t.Fatalf("expected error loading version 2, got nil")
	}

	// 有效期超过30天拒绝
	_ = os.WriteFile(sessFile, []byte(`{"version":1,"fingerprint":"`+fp+`","sessions":[{"token_hash":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","csrf":"c","expires_at":"2026-12-01T00:00:00Z"}]}`), 0600)
	if _, err := New(sessFile, fp, WithNowFunc(nowFunc)); err == nil {
		t.Fatalf("expected error loading session > 30 days, got nil")
	}
}

func TestWebSessionPersistenceWriteFailureRollback(t *testing.T) {
	dir := t.TempDir()
	sessFile := filepath.Join(dir, "sub", "test-session.json") // directory doesn't exist yet
	fp := CredentialFingerprint("admin", "hash1")

	// If directory doesn't exist, create session (remember=true) should fail and rollback
	mgr, _ := New(sessFile, fp)
	tok, _, _, _, err := mgr.CreateSession(true)
	if err == nil {
		t.Fatalf("expected error writing to nonexistent directory")
	}
	if _, ok := mgr.Get(tok); ok {
		t.Fatalf("session should be rolled back on write failure")
	}
}

func TestWebSessionMaxCapAndSweep(t *testing.T) {
	dir := t.TempDir()
	sessFile := filepath.Join(dir, "test-session-cap.json")
	fp := CredentialFingerprint("admin", "hash1")
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	nowFunc := func() time.Time { return now }

	mgr, _ := New(sessFile, fp, WithNowFunc(nowFunc))
	for i := 0; i < MaxSessions; i++ {
		_, _, _, _, err := mgr.CreateSession(false)
		if err != nil {
			t.Fatalf("create session %d failed: %v", i, err)
		}
	}
	// 第 33 个报错
	_, _, _, _, err := mgr.CreateSession(false)
	if err == nil {
		t.Fatalf("expected error exceeding max sessions, got nil")
	}

	// 过期后 sweep 腾出空间
	now = now.Add(13 * time.Hour)
	_, _, _, _, err = mgr.CreateSession(false)
	if err != nil {
		t.Fatalf("should be able to create session after expiry sweep: %v", err)
	}
}
