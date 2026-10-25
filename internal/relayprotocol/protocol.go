package relayprotocol

import (
	"fmt"
	"strings"
	"time"
)

const DefaultZone = "port.toms.chat"

func ValidateZone(zone string) (string, error) {
	zone = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(zone), "."))
	if len(zone) > 253 || !strings.Contains(zone, ".") {
		return "", fmt.Errorf("域名后缀必须是有效的多级域名")
	}
	for _, label := range strings.Split(zone, ".") {
		if _, err := Normalize([]string{label}); err != nil {
			return "", err
		}
	}
	return zone, nil
}

type Request struct {
	RecoveryProtocol int              `json:"recovery_protocol,omitempty"`
	RecoveryResults  []RecoveryResult `json:"recovery_results,omitempty"`
	Zones            []string         `json:"zones,omitempty"`
	ClientID         string           `json:"client_id,omitempty"`
	ClientName       string           `json:"client_name,omitempty"`
	Mappings         []MappingReport  `json:"mappings,omitempty"`
	LeaseID          string           `json:"lease_id,omitempty"`
	Operation        string           `json:"operation"`
	Owner            string           `json:"owner"`
	Prefixes         []string         `json:"prefixes,omitempty"`
	Port             int              `json:"port,omitempty"`
	Scheme           string           `json:"scheme,omitempty"`
	Fingerprint      string           `json:"fingerprint,omitempty"`
	Zone             string           `json:"zone,omitempty"`
}
type Response struct {
	RecoveryCommands []RecoveryCommand `json:"recovery_commands,omitempty"`
	Zones            []string          `json:"zones,omitempty"`
	Zone             string            `json:"zone,omitempty"`
	Domains          []string          `json:"domains,omitempty"`
	Error            string            `json:"error,omitempty"`
	Disabled         bool              `json:"disabled,omitempty"`
	DisabledOwners   []string          `json:"disabled_owners,omitempty"`
}

// MappingReport is a client-reported view of one remote mapping. It contains
// no SSH credentials and is scoped to the VPS receiving the snapshot.
type MappingReport struct {
	Health         string   `json:"health,omitempty"`
	Invalid        bool     `json:"invalid,omitempty"`
	ExpiresAt      string   `json:"expires_at,omitempty"`
	Owner          string   `json:"owner"`
	Group          string   `json:"group"`
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	ListenHost     string   `json:"listen_host"`
	ListenPort     int      `json:"listen_port"`
	Target         string   `json:"target"`
	Domains        []string `json:"domains,omitempty"`
	Description    string   `json:"description,omitempty"`
	DesiredRunning bool     `json:"desired_running"`
	Status         string   `json:"status"`
}

func Normalize(prefixes []string) ([]string, error) {
	out := make([]string, 0, len(prefixes))
	seen := map[string]bool{}
	for _, p := range prefixes {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			return nil, fmt.Errorf("域名前缀不能为空")
		}
		if len(p) > 63 || p[0] == '-' || p[len(p)-1] == '-' {
			return nil, fmt.Errorf("无效的域名前缀 %q", p)
		}
		for _, c := range p {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return nil, fmt.Errorf("域名前缀只允许字母、数字和内部横线：%q", p)
			}
		}
		if seen[p] {
			return nil, fmt.Errorf("域名前缀重复：%s", p)
		}
		seen[p] = true
		out = append(out, p)
	}
	return out, nil
}

func NormalizeZones(zones []string, primary string) ([]string, string, error) {
	primary, err := ValidateZone(primary)
	if err != nil {
		return nil, "", err
	}
	if len(zones) == 0 {
		zones = []string{primary}
	}
	out := make([]string, 0, len(zones))
	seen := map[string]bool{}
	for _, value := range zones {
		zone, err := ValidateZone(value)
		if err != nil {
			return nil, "", err
		}
		if seen[zone] {
			return nil, "", fmt.Errorf("域名后缀重复：%s", zone)
		}
		seen[zone] = true
		out = append(out, zone)
	}
	if !seen[primary] {
		return nil, "", fmt.Errorf("主后缀必须属于此 VPS 的后缀列表")
	}
	return out, primary, nil
}

type RecoveryCommand struct {
	Action    string    `json:"action,omitempty"`
	ID        string    `json:"id"`
	Owner     string    `json:"owner"`
	ExpiresAt time.Time `json:"expires_at"`
}
type RecoveryResult struct {
	ID      string `json:"id"`
	Owner   string `json:"owner"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}
