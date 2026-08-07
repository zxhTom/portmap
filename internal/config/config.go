package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type AuthConfig struct {
	Method     string `json:"method"`
	Password   string `json:"password,omitempty"`
	KeyPath    string `json:"key_path,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
}

type Remote struct {
	Name       string     `json:"name"`
	Host       string     `json:"host"`
	Port       int        `json:"port"`
	User       string     `json:"user"`
	Auth       AuthConfig `json:"auth"`
	StrictHost bool       `json:"strict_host,omitempty"`
}

func (r Remote) Addr() string {
	if r.Port == 0 {
		return r.Host + ":22"
	}
	return fmt.Sprintf("%s:%d", r.Host, r.Port)
}

type Mapping struct {
	Name       string `json:"name"`
	ListenPort int    `json:"listen_port"`
	LocalHost  string `json:"local_host"`
	LocalPort  int    `json:"local_port"`
	Remote     string `json:"remote"`
}

type Group struct {
	Name     string    `json:"name"`
	Mappings []Mapping `json:"mappings"`
}

type Config struct {
	Remotes []Remote `json:"remotes"`
	Groups  []Group  `json:"groups"`
}

func DefaultConfig() Config {
	return Config{}
}

type MappingState struct {
	Running   bool   `json:"running"`
	StartedAt string `json:"started_at,omitempty"`
}

type State struct {
	Mappings map[string]MappingState `json:"mappings"`
}

func DefaultState() State {
	return State{Mappings: make(map[string]MappingState)}
}

func LoadConfig(dir string) (Config, error) {
	var c Config
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
