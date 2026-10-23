package manager

import (
	"testing"

	"github.com/zxhtom/portmap/internal/config"
)

func TestDomainZonePrecedenceTableDriven(t *testing.T) {
	tests := []struct {
		name         string
		globalZone   string
		remoteZone   string
		expectedZone string
	}{
		{
			name:         "explicit remote zone takes precedence over non-empty global zone",
			globalZone:   "global.example",
			remoteZone:   "custom.example",
			expectedZone: "custom.example",
		},
		{
			name:         "empty remote zone inherits non-empty global zone",
			globalZone:   "global.example",
			remoteZone:   "",
			expectedZone: "global.example",
		},
		{
			name:         "both remote and global zones empty remains empty",
			globalZone:   "",
			remoteZone:   "",
			expectedZone: "",
		},
		{
			name:         "explicit remote zone preserved when global zone empty",
			globalZone:   "",
			remoteZone:   "custom.example",
			expectedZone: "custom.example",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{
				DomainZone: tc.globalZone,
				Remotes: []config.Remote{
					{
						Name:       "test-remote",
						DomainZone: tc.remoteZone,
					},
				},
			}

			// 1. 测试 RemoteByName 入口
			m := &Manager{cfg: cfg}
			r, err := m.RemoteByName("test-remote")
			if err != nil {
				t.Fatalf("RemoteByName failed: %v", err)
			}
			if r.DomainZone != tc.expectedZone {
				t.Errorf("RemoteByName expected zone %q, got %q", tc.expectedZone, r.DomainZone)
			}

			// 2. 测试 remoteFor 入口
			rf := remoteFor(cfg, "test-remote")
			if rf.DomainZone != tc.expectedZone {
				t.Errorf("remoteFor expected zone %q, got %q", tc.expectedZone, rf.DomainZone)
			}
		})
	}
}
