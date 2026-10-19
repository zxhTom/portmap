package manager

import (
	"fmt"
	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/forward"
	"time"
)

// expireMappings closes expired backends without discarding the user's running intent.
// Failed closes are retried on the next reconciliation, never reconnected.
func (m *Manager) expireMappings() error {
	m.mu.Lock()
	entries := make(map[mappingKey]*entry)
	for key, e := range m.entries {
		if e.Mapping.Expired(time.Now()) {
			entries[key] = e
		}
	}
	m.mu.Unlock()
	var first error
	for key, e := range entries {
		e.startMu.Lock()
		e.mu.RLock()
		b := e.Backend
		e.mu.RUnlock()
		if b != nil {
			if err := b.Close(); err != nil {
				if first == nil {
					first = fmt.Errorf("close expired %s: %w", key.String(), err)
				}
			} else {
				e.mu.Lock()
				e.Backend = nil
				e.mu.Unlock()
			}
		}
		e.startMu.Unlock()
	}
	return first
}

// SetExpiry renews from an absolute deadline; empty means unlimited. Renewal
// reactivates invalid mappings with running intent, but respects manual stops.
func (m *Manager) SetExpiry(group, name, expiresAt string) error {
	if expiresAt != "" {
		until, err := time.Parse(time.RFC3339Nano, expiresAt)
		if err != nil || !until.After(time.Now()) {
			return fmt.Errorf("expires_at must be a future RFC3339 timestamp with timezone")
		}
		expiresAt = until.Format(time.RFC3339Nano)
	}
	key := mappingKey{Group: group, Name: name}
	resume, err := m.saveExpiry(key, expiresAt)
	if err != nil {
		return err
	}
	if resume {
		m.mu.Lock()
		e := m.entries[key]
		m.mu.Unlock()
		if e == nil {
			return fmt.Errorf("mapping unavailable")
		}
		return m.startEntry(key, e, true)
	}
	return nil
}

func (m *Manager) saveExpiry(key mappingKey, expiresAt string) (bool, error) {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	m.mu.Lock()
	e := m.entries[key]
	shutting := m.shuttingDown
	m.mu.Unlock()
	if e == nil || shutting {
		return false, fmt.Errorf("mapping unavailable")
	}
	e.startMu.Lock()
	defer e.startMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.cfg
	next.Groups = append([]config.Group(nil), m.cfg.Groups...)
	for i := range next.Groups {
		next.Groups[i].Mappings = append([]config.Mapping(nil), next.Groups[i].Mappings...)
		if next.Groups[i].Name == key.Group {
			for j := range next.Groups[i].Mappings {
				if next.Groups[i].Mappings[j].Name == key.Name {
					next.Groups[i].Mappings[j].ExpiresAt = expiresAt
				}
			}
		}
	}
	if err := config.SaveConfig(m.dir, next); err != nil {
		return false, err
	}
	st := m.state.Mappings[key.String()]
	wasInvalid := e.Mapping.Expired(time.Now()) || st.Invalid
	m.cfg = next
	e.Mapping.ExpiresAt = expiresAt
	if wasInvalid {
		st.Invalid = false
		st.Failures = 0
		st.CheckedAt = ""
		st.CheckError = ""
		m.state.Mappings[key.String()] = st
		if err := config.SaveState(m.dir, m.state); err != nil {
			return false, err
		}
	}
	e.mu.RLock()
	b := e.Backend
	e.mu.RUnlock()
	return wasInvalid && st.Running && (b == nil || b.Status().State != forward.StateRunning), nil
}
