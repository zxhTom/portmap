package relayprotocol

import (
	"fmt"
	"strings"
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
	ClientID    string          `json:"client_id,omitempty"`
	ClientName  string          `json:"client_name,omitempty"`
	Mappings    []MappingReport `json:"mappings,omitempty"`
	LeaseID     string          `json:"lease_id,omitempty"`
	Operation   string          `json:"operation"`
	Owner       string          `json:"owner"`
	Prefixes    []string        `json:"prefixes,omitempty"`
	Port        int             `json:"port,omitempty"`
	Scheme      string          `json:"scheme,omitempty"`
	Fingerprint string          `json:"fingerprint,omitempty"`
	Zone        string          `json:"zone,omitempty"`
}
type Response struct {
	Zone           string   `json:"zone,omitempty"`
	Domains        []string `json:"domains,omitempty"`
	Error          string   `json:"error,omitempty"`
	Disabled       bool     `json:"disabled,omitempty"`
	DisabledOwners []string `json:"disabled_owners,omitempty"`
}

// MappingReport is a client-reported view of one remote mapping. It contains
// no SSH credentials and is scoped to the VPS receiving the snapshot.
type MappingReport struct {
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
