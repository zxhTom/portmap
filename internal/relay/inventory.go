package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zxhtom/portmap/internal/relayprotocol"
)

// Inventory is the server's observed view. Clients own mapping definitions;
// only the disabled policy is authoritative and persisted on the VPS.
type Inventory struct {
	mu       sync.RWMutex
	dir      string
	records  map[string]TunnelRecord
	disabled map[string]bool
	recovery map[string]*recoveryState
}

type TunnelRecord struct {
	RecoveryProtocol int       `json:"recovery_protocol,omitempty"`
	ClientOnline     bool      `json:"client_online"`
	ProbeError       string    `json:"probe_error,omitempty"`
	ProbedAt         time.Time `json:"probed_at"`
	RecoveryStatus   string    `json:"recovery_status,omitempty"`
	RecoveryMessage  string    `json:"recovery_message,omitempty"`
	RecoveryAttempts int       `json:"recovery_attempts"`
	relayprotocol.MappingReport
	ClientID   string    `json:"client_id"`
	ClientName string    `json:"client_name"`
	LastSeen   time.Time `json:"last_seen"`
	Online     bool      `json:"online"`
	Disabled   bool      `json:"disabled"`
}

func validHexID(v string, bytes int) bool {
	if len(v) != bytes*2 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil && v == strings.ToLower(v)
}

func NewInventory(dir string) (*Inventory, error) {
	i := &Inventory{dir: dir, records: map[string]TunnelRecord{}, disabled: map[string]bool{}, recovery: map[string]*recoveryState{}}
	data, err := os.ReadFile(filepath.Join(dir, "disabled-tunnels.json"))
	if err == nil {
		err = json.Unmarshal(data, &i.disabled)
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for owner := range i.disabled {
		if !validHexID(owner, 32) {
			return nil, fmt.Errorf("invalid disabled tunnel identity")
		}
	}
	if err := i.loadManualRecovery(); err != nil {
		return nil, err
	}
	return i, nil
}

func (i *Inventory) saveDisabled(next map[string]bool) error {
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(i.dir, ".disabled-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(i.dir, "disabled-tunnels.json"))
}

func (i *Inventory) SetDisabled(owner string, disabled bool) error {
	if !validHexID(owner, 32) {
		return fmt.Errorf("invalid tunnel identity")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, exists := i.records[owner]; !exists {
		return fmt.Errorf("tunnel not found")
	}
	next := make(map[string]bool, len(i.disabled)+1)
	for k, v := range i.disabled {
		next[k] = v
	}
	if disabled {
		next[owner] = true
	} else {
		delete(next, owner)
	}
	if err := i.saveDisabled(next); err != nil {
		return err
	}
	i.disabled = next
	record := i.records[owner]
	record.Disabled = disabled
	if disabled {
		record.Online = false
	}
	i.records[owner] = record
	return nil
}

func (i *Inventory) Check(owner string) (bool, error) {
	if !validHexID(owner, 32) {
		return false, fmt.Errorf("invalid tunnel identity")
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.disabled[owner], nil
}

func (i *Inventory) Snapshot(clientID, clientName string, mappings []relayprotocol.MappingReport, protocols ...int) ([]string, error) {
	if !validHexID(clientID, 8) || len(clientName) > 255 || len(mappings) > 1000 {
		return nil, fmt.Errorf("invalid snapshot")
	}
	protocol := 0
	if len(protocols) > 0 {
		protocol = protocols[0]
	}
	next := make(map[string]TunnelRecord, len(mappings))
	now := time.Now()
	for _, mp := range mappings {
		if !validHexID(mp.Owner, 32) || mp.Name == "" || mp.Group == "" || len(mp.Name) > 100 || len(mp.Group) > 100 || mp.ListenPort < 0 || mp.ListenPort > 65535 || len(mp.Target) > 255 || len(mp.Description) > 1000 || len(mp.Domains) > 100 {
			return nil, fmt.Errorf("invalid mapping report")
		}
		owner := sha256.Sum256([]byte(clientID + "\x00" + mp.Group + "\x00" + mp.Name))
		if mp.Owner != hex.EncodeToString(owner[:]) {
			return nil, fmt.Errorf("mapping identity does not match client")
		}
		if _, exists := next[mp.Owner]; exists {
			return nil, fmt.Errorf("duplicate mapping identity")
		}
		next[mp.Owner] = TunnelRecord{RecoveryProtocol: protocol, MappingReport: mp, ClientID: clientID, ClientName: clientName, LastSeen: now}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	for owner := range next {
		if prior, exists := i.records[owner]; exists && prior.ClientID != clientID {
			return nil, fmt.Errorf("mapping identity belongs to another client")
		}
	}
	for owner, record := range i.records {
		if candidate, ok := next[owner]; ok {
			candidate.ProbeError = record.ProbeError
			candidate.ProbedAt = record.ProbedAt
			next[owner] = candidate
		}
		if record.ClientID == clientID {
			if _, kept := next[owner]; !kept {
				delete(i.recovery, owner)
			}
			delete(i.records, owner)
		}
	}
	disabled := make([]string, 0)
	for owner, record := range next {
		if prior, ok := i.records[owner]; ok {
			record.ProbeError = prior.ProbeError
			record.ProbedAt = prior.ProbedAt
		}
		record.Disabled = i.disabled[owner]
		record.Online = record.Status == "running" && !record.Disabled
		i.records[owner] = record
		if state := i.recovery[owner]; state != nil && state.Status == "pending" && !recoveryPermitted(record, now, state.Command.Action == "start") {
			state.Status = "skipped"
			state.Message = "当前启停意图或策略不允许恢复"
			state.Next = time.Time{}
		}
		if record.Disabled {
			disabled = append(disabled, owner)
		}
	}
	sort.Strings(disabled)
	if err := i.saveManualRecovery(); err != nil {
		return nil, err
	}
	return disabled, nil
}

func (i *Inventory) List() []TunnelRecord {
	i.mu.RLock()
	defer i.mu.RUnlock()
	result := make([]TunnelRecord, 0, len(i.records))
	for _, record := range i.records {
		record.ClientOnline = time.Since(record.LastSeen) < 45*time.Second
		record.Online = record.Online && record.ClientOnline && !record.Disabled && record.ProbeError == ""
		if state := i.recovery[record.Owner]; state != nil {
			record.RecoveryStatus = state.Status
			record.RecoveryMessage = state.Message
			record.RecoveryAttempts = state.Attempts
			if state.Status == "pending" && state.Command.Action == "start" {
				if !record.ClientOnline {
					record.RecoveryStatus = "waiting_client"
					record.RecoveryMessage = "请求已保存，等待客户端控制通道连接"
				} else if record.RecoveryProtocol < 2 && !recoveryEligible(record, time.Now()) {
					record.RecoveryStatus = "waiting_upgrade"
					record.RecoveryMessage = "请求已保存，需升级客户端以支持手动唤醒"
				}
			}
		}
		result = append(result, record)
	}
	sort.Slice(result, func(a, b int) bool {
		if result[a].ClientName != result[b].ClientName {
			return result[a].ClientName < result[b].ClientName
		}
		if result[a].Group != result[b].Group {
			return result[a].Group < result[b].Group
		}
		return result[a].Name < result[b].Name
	})
	return result
}
