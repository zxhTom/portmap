package manager

import (
	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/relayprotocol"
	"time"
)

func (m *Manager) processRecovery(remote config.Remote, items []relayprotocol.MappingReport, command relayprotocol.RecoveryCommand) {
	if command.Action != "" && command.Action != "start" {
		return
	}
	manual := command.Action == "start"
	if command.ID == "" || len(command.ID) > 100 || !time.Now().Before(command.ExpiresAt) || command.ExpiresAt.After(time.Now().Add(2*time.Minute)) {
		return
	}
	var key mappingKey
	matched := false
	for _, item := range items {
		if item.Owner == command.Owner {
			key = mappingKey{Group: item.Group, Name: item.Name}
			matched = true
			break
		}
	}
	if !matched {
		return
	}
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	if !time.Now().Before(command.ExpiresAt) {
		return
	}
	identity := relayIdentity(remote)
	m.mu.Lock()
	if prior, seen := m.recoveryResults[identity][command.Owner]; seen && prior.ID == command.ID {
		m.mu.Unlock()
		return
	}
	e := m.entries[key]
	st := m.state.Mappings[key.String()]
	eligible := e != nil && !m.shuttingDown && (manual || st.Running && !st.Invalid) && !e.Mapping.Expired(time.Now()) && !m.serverDisabled[command.Owner] && relayIdentity(remoteFor(m.cfg, e.Mapping.Remote)) == identity
	m.mu.Unlock()
	result := relayprotocol.RecoveryResult{ID: command.ID, Owner: command.Owner, Status: "skipped", Message: "本地启停意图、有效期或禁用策略不允许恢复"}
	if eligible {
		e.startMu.Lock()
		if !time.Now().Before(command.ExpiresAt) {
			e.startMu.Unlock()
			return
		}
		e.mu.RLock()
		backend := e.Backend
		e.mu.RUnlock()
		var err error
		if backend != nil {
			err = backend.Close()
		}
		if err == nil {
			e.mu.Lock()
			e.Backend = nil
			e.mu.Unlock()
		}
		e.startMu.Unlock()
		if err == nil {
			err = m.startEntry(key, e, !manual)
		}
		if err != nil {
			result.Status = "failed"
			result.Message = "客户端重建失败，请查看客户端隧道错误"
		} else {
			result.Status = "restarted"
			result.Message = "客户端已执行重建，等待VPS入口探测"
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.recoveryResults[identity] == nil {
		m.recoveryResults[identity] = map[string]relayprotocol.RecoveryResult{}
	}
	m.recoveryResults[identity][command.Owner] = result
}
