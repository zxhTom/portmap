package relay

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type savedManualRecovery struct {
	Record TunnelRecord  `json:"record"`
	State  recoveryState `json:"state"`
}

func (i *Inventory) loadManualRecovery() error {
	data, err := os.ReadFile(filepath.Join(i.dir, "manual-recovery-requests.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var requests map[string]savedManualRecovery
	if err = json.Unmarshal(data, &requests); err != nil {
		return err
	}
	for owner, request := range requests {
		record := request.Record
		state := request.State
		identity := sha256.Sum256([]byte(record.ClientID + "\x00" + record.Group + "\x00" + record.Name))
		if !validHexID(owner, 32) || !validHexID(record.ClientID, 8) || owner != hex.EncodeToString(identity[:]) || record.Owner != owner || state.Command.Owner != owner || state.Command.Action != "start" || !validHexID(state.Command.ID, 16) || state.Status != "pending" {
			return fmt.Errorf("invalid saved manual wake request")
		}
		record.LastSeen = time.Time{}
		record.Online = false
		record.Disabled = i.disabled[owner]
		if !state.Command.ExpiresAt.IsZero() && !time.Now().Before(state.Command.ExpiresAt) {
			state.Status = "expired"
			state.Message = "上次指令未收到执行回执，请重新唤醒"
		}
		i.records[owner] = record
		i.recovery[owner] = &state
	}
	return nil
}
func (i *Inventory) saveManualRecovery() error {
	pending := map[string]savedManualRecovery{}
	for owner, state := range i.recovery {
		if state.Command.Action == "start" && state.Status == "pending" {
			if record, ok := i.records[owner]; ok {
				pending[owner] = savedManualRecovery{Record: record, State: *state}
			}
		}
	}
	path := filepath.Join(i.dir, "manual-recovery-requests.json")
	data, err := json.MarshalIndent(pending, "", "  ")
	if err != nil {
		return err
	}
	old, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return readErr
	}
	if bytes.Equal(old, data) || len(pending) == 0 && os.IsNotExist(readErr) {
		return nil
	}
	file, err := os.CreateTemp(i.dir, ".manual-recovery-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
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
	return os.Rename(file.Name(), path)
}
