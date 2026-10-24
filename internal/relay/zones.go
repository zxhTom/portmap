package relay

import (
	"encoding/json"
	"fmt"
	"github.com/zxhtom/portmap/internal/relayprotocol"
	"os"
	"path/filepath"
	"strings"
)

type zoneSettings struct {
	Zones   []string `json:"zones"`
	Primary string   `json:"primary"`
}

func (s *Server) acceptsZone(zone string) bool {
	for _, z := range s.zones {
		if z == zone {
			return true
		}
	}
	return false
}
func (s *Server) acceptsHost(host string) bool {
	for _, z := range s.zones {
		if strings.HasSuffix(host, "."+z) {
			return true
		}
	}
	return false
}
func (s *Server) loadZones() error {
	data, err := os.ReadFile(filepath.Join(s.dir, "zones.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var settings zoneSettings
	if err = json.Unmarshal(data, &settings); err != nil {
		return err
	}
	s.zones, s.zone, err = relayprotocol.NormalizeZones(settings.Zones, settings.Primary)
	return err
}
func (s *Server) configureZones(values []string, primary string) error {
	zones, primary, err := relayprotocol.NormalizeZones(values, primary)
	if err != nil {
		return err
	}
	for host := range s.bindings {
		retained := false
		for _, z := range zones {
			if strings.HasSuffix(host, "."+z) {
				retained = true
			}
		}
		if !retained {
			return fmt.Errorf("后缀仍被域名 %s 使用，不能移除", host)
		}
	}
	data, _ := json.MarshalIndent(zoneSettings{Zones: zones, Primary: primary}, "", "  ")
	file, err := os.CreateTemp(s.dir, ".zones-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), filepath.Join(s.dir, "zones.json")); err != nil {
		return err
	}
	s.zones, s.zone = zones, primary
	return nil
}
