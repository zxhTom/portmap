package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/manager"
	"github.com/zxhtom/portmap/internal/upgrade"
)

var updateBinary, installPath, expectedSHA, updateService string
var systemService bool
var updateTimeout time.Duration

func systemctl(args ...string) ([]byte, error) {
	if !systemService {
		args = append([]string{"--user"}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	if err != nil {
		return data, fmt.Errorf("systemctl failed: %w: %s", err, data)
	}
	return data, nil
}
func servicePID() (int, error) {
	data, err := systemctl("show", updateService, "--property=MainPID", "--value")
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("service is not running")
	}
	return pid, nil
}
func processDataDir(pid int) (string, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return "", err
	}
	env := map[string]string{}
	for _, v := range strings.Split(string(raw), "\x00") {
		parts := strings.SplitN(v, "=", 2)
		if len(parts) == 2 {
			env[parts[0]] = parts[1]
		}
	}
	path := env["PORTMAP_HOME"]
	if path == "" {
		if env["HOME"] == "" {
			return "", fmt.Errorf("cannot determine service data directory")
		}
		path = filepath.Join(env["HOME"], ".config", "portmap")
	}
	raw, err = os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "", err
	}
	args := strings.Split(string(raw), "\x00")
	for i, arg := range args {
		if strings.HasPrefix(arg, "--data-dir=") {
			path = strings.TrimPrefix(arg, "--data-dir=")
		}
		if arg == "--data-dir" && i+1 < len(args) {
			path = args[i+1]
		}
	}
	if !filepath.IsAbs(path) {
		cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
		if err != nil {
			return "", err
		}
		path = filepath.Join(cwd, path)
	}
	return filepath.Clean(path), nil
}
func readMappings() ([]manager.MappingInfo, error) {
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(apiURL("/api/mappings"))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API returned %d", resp.StatusCode)
	}
	var rows []manager.MappingInfo
	err = json.NewDecoder(resp.Body).Decode(&rows)
	return rows, err
}
func desired(dir string) (map[string]bool, error) {
	s, err := config.LoadState(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for k, v := range s.Mappings {
		out[k] = v.Running
	}
	return out, nil
}
func awaitUpgrade(ctx context.Context, oldPID int, original map[string]bool, rows []manager.MappingInfo, configSHA, dir string, requireHealth bool) error {
	var last error = fmt.Errorf("waiting for service startup")
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("service health check timed out: %w", last)
		case <-time.After(200 * time.Millisecond):
		}
		pid, err := servicePID()
		if err != nil || pid == oldPID {
			last = fmt.Errorf("waiting for restarted service")
			continue
		}
		if requireHealth {
			identity, err := apiIdentity()
			if err != nil || identity.PID != pid || identity.Dir != dir {
				last = fmt.Errorf("waiting for API to match restarted service")
				continue
			}
		}
		current, err := readMappings()
		if err != nil {
			last = err
			continue
		}
		digest, err := upgrade.Digest(filepath.Join(dir, "config.json"))
		if err != nil || digest != configSHA {
			return fmt.Errorf("configuration changed during update")
		}
		actual, err := desired(dir)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(actual, original) {
			return fmt.Errorf("mapping running intent changed during update")
		}
		indexed := map[string]manager.MappingInfo{}
		for _, mp := range current {
			indexed[mp.Group+"/"+mp.Name] = mp
		}
		if len(current) != len(rows) {
			return fmt.Errorf("mapping count changed during update")
		}
		healthy := true
		for _, mp := range rows {
			key := mp.Group + "/" + mp.Name
			now, ok := indexed[key]
			if !ok || now.Description != mp.Description || now.ExpiresAt != mp.ExpiresAt || !reflect.DeepEqual(now.Domains, mp.Domains) {
				return fmt.Errorf("mapping or description missing after update")
			}
			if mp.Status == "invalid" && now.Status != "invalid" {
				return fmt.Errorf("invalid mapping unexpectedly reactivated")
			}
			// An already-offline tunnel should not prevent updating the controller.
			if original[key] && mp.Status == "running" && now.Status != "running" && now.InvalidReason != "expired" {
				healthy = false
			}
			if !original[key] && now.Status == "running" {
				return fmt.Errorf("stopped mapping unexpectedly started")
			}
		}
		if healthy {
			return nil
		}
		last = fmt.Errorf("waiting for previously active tunnels to recover")
	}
}
func restoreData(backup, dir string) error {
	for _, name := range []string{"config.json", "state.json"} {
		data, err := os.ReadFile(filepath.Join(backup, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		path := filepath.Join(dir, name)
		// Retain the failed version as evidence instead of silently discarding it.
		if current, err := os.ReadFile(path); err == nil {
			if err := os.WriteFile(filepath.Join(backup, name+".failed"), current, 0600); err != nil {
				return err
			}
		}
		if err := upgrade.WriteData(path, data); err != nil {
			return err
		}
	}
	return nil
}

var upgradeCmd = &cobra.Command{Use: "upgrade", Short: "Update the executable with backups, tunnel recovery checks and automatic rollback", RunE: func(cmd *cobra.Command, args []string) error {
	if updateTimeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	source, err := filepath.Abs(updateBinary)
	if err != nil {
		return err
	}
	target := installPath
	if target == "" {
		target, err = os.Executable()
		if err != nil {
			return err
		}
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return err
	}
	dir, err := filepath.Abs(dataDirPath())
	if err != nil {
		return err
	}
	unlock, err := upgrade.Lock(dir)
	if err != nil {
		return err
	}
	defer unlock()
	if filepath.Base(updateService) != updateService || strings.Contains(updateService, "..") {
		return fmt.Errorf("invalid service name")
	}
	sourceSHA, err := upgrade.Digest(source)
	if err != nil {
		return err
	}
	if expectedSHA != "" && !strings.EqualFold(expectedSHA, sourceSHA) {
		return fmt.Errorf("SHA-256 mismatch; installed binary unchanged")
	}
	targetSHA, err := upgrade.Digest(target)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	raw, err := exec.CommandContext(ctx, source, "version").Output()
	cancel()
	if err != nil {
		return fmt.Errorf("candidate executable check failed: %w", err)
	}
	var candidate struct {
		ConfigVersion int  `json:"config_version"`
		DomainRouting bool `json:"domain_routing"`
	}
	if json.Unmarshal(raw, &candidate) != nil || candidate.ConfigVersion != 1 {
		return fmt.Errorf("candidate configuration version is incompatible")
	}
	if !candidate.DomainRouting {
		data, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil {
			return err
		}
		var cfg config.Config
		if err := json.Unmarshal(data, &cfg); err != nil {
			return err
		}
		for _, g := range cfg.Groups {
			for _, mp := range g.Mappings {
				if len(mp.DomainPrefixes) > 0 {
					return fmt.Errorf("candidate lacks domain routing; refusing an update that could expose domain-only tunnel ports")
				}
			}
		}
	}
	pid, err := servicePID()
	if err != nil {
		return err
	}
	actualExe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil || strings.TrimSuffix(actualExe, " (deleted)") != target {
		return fmt.Errorf("service executable does not match --install-path")
	}
	actualDir, err := processDataDir(pid)
	if err != nil {
		return err
	}
	if actualDir != dir {
		return fmt.Errorf("service data directory differs; use --data-dir %s", actualDir)
	}
	runningPath := fmt.Sprintf("/proc/%d/exe", pid)
	runningSHA, err := upgrade.Digest(runningPath)
	if err != nil {
		return err
	}
	if sourceSHA == targetSHA && sourceSHA == runningSHA {
		fmt.Fprintln(cmd.OutOrStdout(), "Already installed and running; no changes.")
		return nil
	}
	rows, err := readMappings()
	if err != nil {
		return err
	}
	wanted, err := desired(dir)
	if err != nil {
		return err
	}
	cfgSHA, err := upgrade.Digest(filepath.Join(dir, "config.json"))
	if err != nil {
		return err
	}
	oldHealth, healthErr := apiIdentity()
	if healthErr == nil && (oldHealth.PID != pid || oldHealth.Dir != dir) {
		return fmt.Errorf("API and service identity differ")
	}
	backupSource := target
	if runningSHA != targetSHA {
		backupSource = runningPath
	}
	backup, err := upgrade.Backup(backupSource, dir)
	if err != nil {
		return err
	}
	metadata, _ := json.MarshalIndent(map[string]interface{}{"target": target, "service": updateService, "system": systemService, "old_sha256": runningSHA, "new_sha256": sourceSHA, "created_at": time.Now().Format(time.RFC3339)}, "", "  ")
	if err := upgrade.WriteData(filepath.Join(backup, "metadata.json"), metadata); err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Backup:", backup)
	if err := upgrade.ReplaceVerified(source, target, sourceSHA); err != nil {
		return err
	}
	fail := func(reason error) error {
		// Stop the failed candidate before restoring its data (avoid late state writes).
		if _, err := systemctl("stop", updateService); err != nil {
			return fmt.Errorf("update failed: %v; cannot stop candidate for rollback: %w; backup: %s", reason, err, backup)
		}
		if err := upgrade.Replace(filepath.Join(backup, "portmap"), target); err != nil {
			return fmt.Errorf("update failed: %v; executable rollback failed: %w; backup: %s", reason, err, backup)
		}
		if err := restoreData(backup, dir); err != nil {
			return fmt.Errorf("update failed: %v; data rollback failed: %w; backup: %s", reason, err, backup)
		}
		_, _ = systemctl("reset-failed", updateService)
		if _, err := systemctl("start", updateService); err != nil {
			return fmt.Errorf("previous version restored but restart failed: %w; backup: %s", err, backup)
		}
		rollbackCtx, cancel := context.WithTimeout(context.Background(), updateTimeout)
		defer cancel()
		if err := awaitUpgrade(rollbackCtx, pid, wanted, rows, cfgSHA, dir, healthErr == nil); err != nil {
			return fmt.Errorf("previous version restored but recovery check failed: %w; backup: %s", err, backup)
		}
		return fmt.Errorf("update failed and was rolled back successfully: %w; backup: %s", reason, backup)
	}
	if _, err := systemctl("restart", updateService); err != nil {
		return fail(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), updateTimeout)
	defer cancel()
	if err := awaitUpgrade(ctx, pid, wanted, rows, cfgSHA, dir, true); err != nil {
		return fail(err)
	}
	installedSHA, err := upgrade.Digest(target)
	if err != nil || installedSHA != sourceSHA {
		return fail(fmt.Errorf("installed binary checksum mismatch"))
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Upgrade complete; configuration, descriptions, mappings and running intent preserved.")
	return nil
}}

func init() {
	upgradeCmd.Flags().StringVar(&updateBinary, "binary", "", "Path to the new executable")
	upgradeCmd.MarkFlagRequired("binary")
	upgradeCmd.Flags().StringVar(&installPath, "install-path", "", "Installed executable (defaults to this executable)")
	upgradeCmd.Flags().StringVar(&expectedSHA, "sha256", "", "Expected SHA-256 of the new executable")
	upgradeCmd.Flags().StringVar(&updateService, "service", "portmap.service", "systemd service name")
	upgradeCmd.Flags().BoolVar(&systemService, "system", false, "Use a system service instead of a user service")
	upgradeCmd.Flags().DurationVar(&updateTimeout, "timeout", 60*time.Second, "Time allowed for mapping recovery")
	rootCmd.AddCommand(upgradeCmd)
}

type apiHealth struct {
	PID int    `json:"pid"`
	Dir string `json:"data_dir"`
}

func apiIdentity() (apiHealth, error) {
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(apiURL("/api/health"))
	if err != nil {
		return apiHealth{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return apiHealth{}, fmt.Errorf("API identity unavailable")
	}
	var health apiHealth
	err = json.NewDecoder(resp.Body).Decode(&health)
	return health, err
}
