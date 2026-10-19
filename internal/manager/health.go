package manager

import (
	"github.com/zxhtom/portmap/internal/config"
	"log/slog"
	"time"
)

// recordCheck persists the circuit breaker across process restarts and upgrades.
func (m *Manager) recordCheck(key mappingKey, e *entry, checkErr error) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shuttingDown || m.entries[key] != e {
		return false
	}
	st := m.state.Mappings[key.String()]
	if !st.Running || st.Invalid {
		return false
	}
	st.CheckedAt = time.Now().Format(time.RFC3339Nano)
	if checkErr == nil {
		st.Failures = 0
		st.CheckError = ""
	} else {
		st.Failures++
		st.CheckError = checkErr.Error()
		st.Invalid = st.Failures > 10
	}
	m.state.Mappings[key.String()] = st
	if err := config.SaveState(m.dir, m.state); err != nil {
		slog.Error("persist tunnel health", "err", err)
	}
	if st.Invalid {
		slog.Warn("mapping invalidated after more than 10 consecutive failures; start manually to reset", "mapping", key.String())
	}
	return !st.Invalid
}
