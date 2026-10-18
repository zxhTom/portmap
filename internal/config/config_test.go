package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zxhtom/portmap/internal/config"
)

func TestConfigCompatibility_OldJSON(t *testing.T) {
	tmpDir := t.TempDir()
	oldJSON := `{
  "remotes": [
    {
      "name": "vps1",
      "host": "1.2.3.4",
      "port": 22,
      "user": "root",
      "auth": {
        "method": "password",
        "password": "secret"
      }
    }
  ],
  "groups": [
    {
      "name": "web",
      "mappings": [
        {
          "name": "app",
          "listen_port": 8080,
          "local_host": "127.0.0.1",
          "local_port": 80,
          "remote": "vps1"
        }
      ]
    }
  ]
}`
	if err := os.WriteFile(filepath.Join(tmpDir, "config.json"), []byte(oldJSON), 0o600); err != nil {
		t.Fatalf("failed to write config.json: %v", err)
	}

	cfg, err := config.LoadConfig(tmpDir)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if len(cfg.Groups) != 1 || len(cfg.Groups[0].Mappings) != 1 {
		t.Fatalf("unexpected mappings count: %+v", cfg.Groups)
	}
	m := cfg.Groups[0].Mappings[0]
	// Type defaults to ssh if empty or unspecified
	if m.EffectiveType() != config.TypeSSH {
		t.Fatalf("expected effective type %q, got %q", config.TypeSSH, m.EffectiveType())
	}
	if m.Remote != "vps1" || m.ListenPort != 8080 || m.LocalPort != 80 {
		t.Fatalf("unexpected mapping values: %+v", m)
	}

	// Verify saving and reloading preserves structure
	if err := config.SaveConfig(tmpDir, cfg); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	cfg2, err := config.LoadConfig(tmpDir)
	if err != nil {
		t.Fatalf("LoadConfig reloaded failed: %v", err)
	}
	if cfg2.Groups[0].Mappings[0].EffectiveType() != config.TypeSSH {
		t.Fatalf("expected reloaded effective type %q, got %q", config.TypeSSH, cfg2.Groups[0].Mappings[0].EffectiveType())
	}
}

func TestConfig_DockerDefaultsAndValidation(t *testing.T) {
	tmpDir := t.TempDir()
	cfg, err := config.LoadConfig(tmpDir)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.Docker.Host != "unix:///var/run/docker.sock" {
		t.Fatalf("expected default docker host unix:///var/run/docker.sock, got %q", cfg.Docker.Host)
	}

	// Valid SSH mapping
	sshValid := config.Mapping{
		Name:       "ssh-ok",
		Type:       config.TypeSSH,
		Remote:     "vps1",
		ListenPort: 2222,
		LocalHost:  "127.0.0.1",
		LocalPort:  80,
	}
	if err := sshValid.Validate(); err != nil {
		t.Fatalf("expected sshValid to be valid, got: %v", err)
	}

	// Invalid SSH mappings
	sshMissingRemote := sshValid
	sshMissingRemote.Remote = ""
	if err := sshMissingRemote.Validate(); err == nil {
		t.Fatalf("expected error for missing remote in ssh mapping")
	}

	sshMissingLocalHost := sshValid
	sshMissingLocalHost.LocalHost = ""
	if err := sshMissingLocalHost.Validate(); err == nil {
		t.Fatalf("expected error for missing local_host in ssh mapping")
	}

	sshMissingLocalPort := sshValid
	sshMissingLocalPort.LocalPort = 0
	if err := sshMissingLocalPort.Validate(); err == nil {
		t.Fatalf("expected error for missing local_port in ssh mapping")
	}

	// Valid Docker mapping
	dockerValid := config.Mapping{
		Name:          "docker-ok",
		Type:          config.TypeDocker,
		Container:     "nginx-container",
		ContainerPort: 80,
		ListenPort:    8080,
	}
	dockerValid.SetDefaults()
	if dockerValid.BindHost != "127.0.0.1" {
		t.Fatalf("expected default bind_host 127.0.0.1, got %q", dockerValid.BindHost)
	}
	if err := dockerValid.Validate(); err != nil {
		t.Fatalf("expected dockerValid to be valid, got: %v", err)
	}

	// Invalid Docker mappings
	dockerMissingContainer := dockerValid
	dockerMissingContainer.Container = ""
	if err := dockerMissingContainer.Validate(); err == nil {
		t.Fatalf("expected error for missing container in docker mapping")
	}

	dockerMissingContainerPort := dockerValid
	dockerMissingContainerPort.ContainerPort = 0
	if err := dockerMissingContainerPort.Validate(); err == nil {
		t.Fatalf("expected error for missing container_port in docker mapping")
	}

	dockerBadBindHost := dockerValid
	dockerBadBindHost.BindHost = "localhost" // not a valid raw IP
	if err := dockerBadBindHost.Validate(); err == nil {
		t.Fatalf("expected error for non-IP bind_host")
	}

	dockerBadPort := dockerValid
	dockerBadPort.ListenPort = 70000
	if err := dockerBadPort.Validate(); err == nil {
		t.Fatalf("expected error for port > 65535")
	}

	// Unknown type
	unknownType := dockerValid
	unknownType.Type = "k8s"
	if err := unknownType.Validate(); err == nil {
		t.Fatalf("expected error for unknown type")
	}
}

func TestInstanceID_PersistenceAndLock(t *testing.T) {
	tmpDir := t.TempDir()
	id1, lock1, err := config.AcquireInstanceLock(tmpDir)
	if err != nil {
		t.Fatalf("AcquireInstanceLock failed: %v", err)
	}
	if id1 == "" {
		t.Fatalf("expected non-empty instance id")
	}
	defer lock1.Release()

	// Second acquire on same data directory should fail due to lock
	_, _, err = config.AcquireInstanceLock(tmpDir)
	if err == nil {
		t.Fatalf("expected second AcquireInstanceLock on same directory to fail")
	}

	// Release first lock
	if err := lock1.Release(); err != nil {
		t.Fatalf("Release failed: %v", err)
	}

	// Subsequent acquire should reuse the same persistent instance ID
	id2, lock2, err := config.AcquireInstanceLock(tmpDir)
	if err != nil {
		t.Fatalf("second AcquireInstanceLock after release failed: %v", err)
	}
	defer lock2.Release()
	if id1 != id2 {
		t.Fatalf("expected persistent instance ID to match (%q vs %q)", id1, id2)
	}
}
