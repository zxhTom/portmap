package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/zxhtom/portmap/internal/relayprotocol"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	TypeSSH       = "ssh"
	TypeDocker    = "docker"
	TypeMultipass = "multipass"

	DefaultDockerHost        = "unix:///var/run/docker.sock"
	DefaultDockerHelperImage = "ghcr.io/iammathew/docker-port-forward/sidecar:latest"
	DefaultBindHost          = "127.0.0.1"
)

type AuthConfig struct {
	Method     string `json:"method"`
	Password   string `json:"password,omitempty"`
	KeyPath    string `json:"key_path,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
}

type Remote struct {
	DomainZone string     `json:"domain_zone,omitempty"`
	Name       string     `json:"name"`
	Host       string     `json:"host"`
	Port       int        `json:"port"`
	User       string     `json:"user"`
	Auth       AuthConfig `json:"auth"`
	StrictHost bool       `json:"strict_host,omitempty"`
}

func (r Remote) Addr() string {
	port := r.Port
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(r.Host, strconv.Itoa(port))
}

type DockerConfig struct {
	Host        string `json:"host,omitempty"`
	HelperImage string `json:"helper_image,omitempty"`
}

type Mapping struct {
	DomainPrefixes []string `json:"domain_prefixes,omitempty"`
	UpstreamScheme string   `json:"upstream_scheme,omitempty"`
	DomainOwner    string   `json:"-"`
	ExpiresAt      string   `json:"expires_at,omitempty"`
	Description    string   `json:"description,omitempty"`
	Name           string   `json:"name"`
	Type           string   `json:"type,omitempty"`
	ListenPort     int      `json:"listen_port"`
	LocalHost      string   `json:"local_host,omitempty"`
	LocalPort      int      `json:"local_port,omitempty"`
	Remote         string   `json:"remote,omitempty"`
	Container      string   `json:"container,omitempty"`
	ContainerPort  int      `json:"container_port,omitempty"`
	BindHost       string   `json:"bind_host,omitempty"`
	Instance       string   `json:"instance,omitempty"`
	InstancePort   int      `json:"instance_port,omitempty"`
}

func (m Mapping) UsesRelay() bool {
	return m.EffectiveType() == TypeSSH || (m.EffectiveType() == TypeMultipass && m.Remote != "")
}

func (m Mapping) EffectiveType() string {
	if m.Type == "" {
		return TypeSSH
	}
	return strings.ToLower(m.Type)
}

func (m *Mapping) SetDefaults() {
	if len(m.DomainPrefixes) > 0 && m.UpstreamScheme == "" {
		m.UpstreamScheme = "http"
	}
	for i := range m.DomainPrefixes {
		m.DomainPrefixes[i] = strings.ToLower(strings.TrimSpace(m.DomainPrefixes[i]))
	}
	if m.EffectiveType() == TypeDocker || m.EffectiveType() == TypeMultipass {
		if m.BindHost == "" {
			m.BindHost = DefaultBindHost
		}
	} else if m.EffectiveType() == TypeSSH {
		if m.LocalHost == "" {
			m.LocalHost = "127.0.0.1"
		}
	}
}

func (m Mapping) Expired(now time.Time) bool {
	if m.ExpiresAt == "" {
		return false
	}
	until, err := time.Parse(time.RFC3339Nano, m.ExpiresAt)
	return err != nil || !now.Before(until)
}

func (m Mapping) Validate() error {
	if len(m.DomainPrefixes) > 0 {
		if !m.UsesRelay() {
			return fmt.Errorf("域名模式需要 SSH 中转站")
		}
		if _, err := relayprotocol.Normalize(m.DomainPrefixes); err != nil {
			return err
		}
		if m.UpstreamScheme != "http" && m.UpstreamScheme != "https" {
			return fmt.Errorf("域名模式的后端协议须为 http 或 https")
		}
	}
	if m.ExpiresAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, m.ExpiresAt); err != nil {
			return fmt.Errorf("expires_at must be RFC3339 with timezone: %w", err)
		}
	}
	if m.Name == "" {
		return fmt.Errorf("mapping name is required")
	}
	if m.ListenPort < 1 || m.ListenPort > 65535 {
		return fmt.Errorf("listen_port must be between 1 and 65535, got %d", m.ListenPort)
	}

	t := m.EffectiveType()
	switch t {
	case TypeSSH:
		if m.Remote == "" {
			return fmt.Errorf("remote is required for ssh mapping")
		}
		if m.LocalHost == "" {
			return fmt.Errorf("local_host is required for ssh mapping")
		}
		if m.LocalPort < 1 || m.LocalPort > 65535 {
			return fmt.Errorf("local_port must be between 1 and 65535 for ssh mapping, got %d", m.LocalPort)
		}
		return nil
	case TypeDocker:
		if m.Container == "" {
			return fmt.Errorf("container is required for docker mapping")
		}
		if m.ContainerPort < 1 || m.ContainerPort > 65535 {
			return fmt.Errorf("container_port must be between 1 and 65535 for docker mapping, got %d", m.ContainerPort)
		}
		bind := m.BindHost
		if bind == "" {
			bind = DefaultBindHost
		}
		ip := net.ParseIP(bind)
		if ip == nil {
			return fmt.Errorf("bind_host must be a valid IP address, got %q", m.BindHost)
		}
		return nil
	case TypeMultipass:
		if !ValidInstanceName(m.Instance) {
			return fmt.Errorf("instance must be a Multipass instance name (letters, digits and hyphens)")
		}
		if m.InstancePort < 1 || m.InstancePort > 65535 {
			return fmt.Errorf("instance_port must be between 1 and 65535")
		}
		if net.ParseIP(m.BindHost) == nil {
			return fmt.Errorf("bind_host must be a valid IP address")
		}
		return nil
	default:
		return fmt.Errorf("unknown mapping type %q (must be %q, %q or %q)", m.Type, TypeSSH, TypeDocker, TypeMultipass)
	}
}

type Group struct {
	Name     string    `json:"name"`
	Mappings []Mapping `json:"mappings"`
}

type Config struct {
	DomainZone string       `json:"domain_zone,omitempty"`
	Docker     DockerConfig `json:"docker,omitempty"`
	Remotes    []Remote     `json:"remotes"`
	Groups     []Group      `json:"groups"`
}

func DefaultConfig() Config {
	return Config{
		Docker: DockerConfig{
			Host:        DefaultDockerHost,
			HelperImage: DefaultDockerHelperImage,
		},
	}
}

type MappingState struct {
	Failures   int    `json:"consecutive_failures,omitempty"`
	Invalid    bool   `json:"invalid,omitempty"`
	CheckedAt  string `json:"checked_at,omitempty"`
	CheckError string `json:"check_error,omitempty"`
	Running    bool   `json:"running"`
	StartedAt  string `json:"started_at,omitempty"`
}

type State struct {
	Mappings map[string]MappingState `json:"mappings"`
}

func DefaultState() State {
	return State{Mappings: make(map[string]MappingState)}
}

func LoadConfig(dir string) (Config, error) {
	c := DefaultConfig()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return c, err
	}
	path := filepath.Join(dir, "config.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, SaveConfig(dir, c)
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	if c.Docker.Host == "" {
		c.Docker.Host = DefaultDockerHost
	}
	if c.Docker.HelperImage == "" {
		c.Docker.HelperImage = DefaultDockerHelperImage
	}
	return c, nil
}

func SaveConfig(dir string, c Config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "config.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "config.json"))
}

func LoadState(dir string) (State, error) {
	s := DefaultState()
	path := filepath.Join(dir, "state.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, err
	}
	if s.Mappings == nil {
		s.Mappings = make(map[string]MappingState)
	}
	return s, nil
}

func SaveState(dir string, s State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "state.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "state.json"))
}

type InstanceLock struct {
	file *os.File
	path string
}

func (l *InstanceLock) Release() error {
	if l.file != nil {
		_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
		err := l.file.Close()
		l.file = nil
		return err
	}
	return nil
}

func AcquireInstanceLock(dir string) (string, *InstanceLock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, fmt.Errorf("create data dir: %w", err)
	}
	lockPath := filepath.Join(dir, "daemon.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", nil, fmt.Errorf("open instance lock file: %w", err)
	}

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lockFile.Close()
		return "", nil, fmt.Errorf("acquire instance lock: another daemon is already running in %s: %w", dir, err)
	}

	instanceIDPath := filepath.Join(dir, "instance_id")
	idBytes, err := os.ReadFile(instanceIDPath)
	var instanceID string
	if err == nil {
		instanceID = strings.TrimSpace(string(idBytes))
	}
	if instanceID == "" {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			lockFile.Close()
			return "", nil, fmt.Errorf("generate instance id: %w", err)
		}
		instanceID = hex.EncodeToString(b)
		if err := os.WriteFile(instanceIDPath, []byte(instanceID+"\n"), 0o600); err != nil {
			lockFile.Close()
			return "", nil, fmt.Errorf("persist instance id: %w", err)
		}
	}

	return instanceID, &InstanceLock{file: lockFile, path: lockPath}, nil
}

// ValidInstanceName also prevents names from being interpreted as CLI options.
func ValidInstanceName(name string) bool {
	if name == "" || !((name[0] >= 'a' && name[0] <= 'z') || (name[0] >= 'A' && name[0] <= 'Z')) {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}
