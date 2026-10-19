// Package upgrade stages executable updates atomically, with private backups.
package upgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

func Digest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func Lock(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "upgrade.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another update is in progress")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

// Replace never truncates the running executable, and fsyncs before rename.
func Replace(source, target string) error { return ReplaceVerified(source, target, "") }

func ReplaceVerified(source, target, expected string) error {
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("target is not a regular executable")
	}
	if info, err := os.Lstat(target); err != nil || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to replace a symlink; provide the real install path")
	}
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(target), ".portmap-update-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hash), src); err != nil {
		tmp.Close()
		return err
	}
	if expected != "" && hex.EncodeToString(hash.Sum(nil)) != expected {
		tmp.Close()
		return fmt.Errorf("candidate changed during staging; target unchanged")
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(target))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func Backup(target, dataDir string) (string, error) {
	root := filepath.Join(dataDir, "updates")
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(root, "backup-")
	if err != nil {
		return "", err
	}
	if err := os.Link(target, filepath.Join(dir, "portmap")); err != nil {
		src, err := os.Open(target)
		if err != nil {
			return "", err
		}
		defer src.Close()
		dst, err := os.OpenFile(filepath.Join(dir, "portmap"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
		if err != nil {
			return "", err
		}
		_, copyErr := io.Copy(dst, src)
		closeErr := dst.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	for _, name := range []string{"config.json", "state.json"} {
		data, err := os.ReadFile(filepath.Join(dataDir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func WriteData(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".portmap-data-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
