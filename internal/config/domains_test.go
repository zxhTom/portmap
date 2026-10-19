package config

import (
	"github.com/zxhtom/portmap/internal/relayprotocol"
	"testing"
)

func TestDomainMappingDefaultsAndValidation(t *testing.T) {
	mp := Mapping{Name: "app", Remote: "relay", ListenPort: 31234, LocalPort: 80, DomainPrefixes: []string{" App ", "API"}}
	mp.SetDefaults()
	if err := mp.Validate(); err != nil {
		t.Fatal(err)
	}
	if mp.DomainPrefixes[0] != "app" || mp.UpstreamScheme != "http" {
		t.Fatal("defaults missing")
	}
	mp.Type = TypeDocker
	mp.Container = "test"
	mp.ContainerPort = 80
	if err := mp.Validate(); err == nil {
		t.Fatal("local docker accepted domain routing")
	}
	if _, err := relayprotocol.Normalize([]string{"one", "ONE"}); err == nil {
		t.Fatal("duplicate normalized domain accepted")
	}
}
