package relay

import (
	"github.com/zxhtom/portmap/internal/relayprotocol"
	"testing"
)

func TestMultipleZonesPersistAndProtectRegisteredDomains(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, "first.example")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Apply(relayprotocol.Request{Operation: "configure_zones", Zone: "first.example", Zones: []string{"first.example", "second.example"}})
	if err != nil {
		t.Fatal(err)
	}
	req := requestFor("a", "app", 31001)
	req.Zone = "second.example"
	response, err := s.Apply(req)
	if err != nil || len(response.Domains) != 1 || response.Domains[0] != "app.second.example" {
		t.Fatalf("secondary route: %+v %v", response, err)
	}
	_, err = s.Apply(relayprotocol.Request{Operation: "configure_zones", Zone: "first.example", Zones: []string{"first.example"}})
	if err == nil {
		t.Fatal("removed occupied zone")
	}
	req.Zone = "unbound.example"
	if _, err = s.Apply(req); err == nil {
		t.Fatal("accepted unbound zone")
	}
	s.Close()
	s, err = New(dir, "first.example")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	info, _ := s.Apply(relayprotocol.Request{Operation: "info"})
	if len(info.Zones) != 2 {
		t.Fatalf("lost zones: %+v", info)
	}
}
