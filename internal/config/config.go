package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type PortRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type Mapping struct {
	Name       string `json:"name"`
	LocalPort  int    `json:"local_port"`
	TargetHost string `json:"target_host"`
	TargetPort int    `json:"target_port"`
}

type Group struct {
	Name      string     `json:"name"`
	PortRange *PortRange `json:"port_range,omitempty"`
	Mappings  []Mapping  `json:"mappings"`
}

type RelayCredential struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

type RelayConfig struct {
	Enabled     bool               `json:"enabled"`
	ListenAddr  string             `json:"listen_addr"`
	Token       string             `json:"token,omitempty"`
	Users       []RelayCredential  `json:"users,omitempty"`
}

type Config struct {
	BindAddr  string      `json:"bind_addr"`
	PortRange PortRange   `json:"port_range"`
	Relay     RelayConfig `json:"relay,omitempty"`
	Groups    []Group     `json:"groups"`
}

type MappingState struct {
	Running   bool   `json:"running"`
	LocalPort int    `json:"local_port"`
	StartedAt string `json:"started_at,omitempty"`
}

type State struct {
	Mappings map[string]MappingState `json:"mappings"`
}

func DefaultConfig() Config {
	return Config{
		BindAddr: "0.0.0.0",
		PortRange: PortRange{
			Start: 20000,
			End:   30000,
		},
	}
}

func DefaultState() State {
	return State{
		Mappings: make(map[string]MappingState),
	}
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
	path := filepath.Join(dir, "config.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
	path := filepath.Join(dir, "state.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
