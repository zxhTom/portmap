package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/zxhtom/portmap/internal/relayprotocol"
	"net"
	"strconv"
	"sync"
	"time"
)

type recoveryState struct {
	Command         relayprotocol.RecoveryCommand
	Attempts        int
	Next            time.Time
	Status, Message string
}

func recoveryEligible(record TunnelRecord, now time.Time) bool {
	if !record.DesiredRunning || record.Disabled || record.Invalid || now.Sub(record.LastSeen) > 45*time.Second {
		return false
	}
	if record.ExpiresAt != "" {
		until, err := time.Parse(time.RFC3339Nano, record.ExpiresAt)
		if err != nil || !now.Before(until) {
			return false
		}
	}
	return true
}
func (i *Inventory) queueRecovery(owner string, manual bool) error {
	record, ok := i.records[owner]
	now := time.Now()
	if !ok || !recoveryEligible(record, now) {
		return fmt.Errorf("隧道未启用、已过期、已作废、被禁用或客户端未连接")
	}
	state := i.recovery[owner]
	if state != nil {
		if state.Status == "pending" && now.Before(state.Command.ExpiresAt) {
			return nil
		}
		if now.Before(state.Next) {
			return fmt.Errorf("恢复请求冷却中")
		}
		if !manual && state.Attempts >= 5 {
			return fmt.Errorf("自动恢复达到5次上限，请排查后手动恢复")
		}
	} else {
		state = &recoveryState{}
		i.recovery[owner] = state
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	state.Attempts++
	state.Command = relayprotocol.RecoveryCommand{ID: hex.EncodeToString(id[:]), Owner: owner, ExpiresAt: now.Add(time.Minute)}
	delay := time.Duration(1<<min(state.Attempts-1, 4)) * 30 * time.Second
	state.Next = now.Add(delay)
	state.Status = "pending"
	state.Message = "等待客户端接收恢复请求"
	return nil
}
func (i *Inventory) RequestRecovery(owner string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.queueRecovery(owner, true)
}
func (i *Inventory) RecoveryCommands(clientID string) []relayprotocol.RecoveryCommand {
	i.mu.Lock()
	defer i.mu.Unlock()
	now := time.Now()
	commands := []relayprotocol.RecoveryCommand{}
	for owner, record := range i.records {
		if record.ClientID != clientID || !recoveryEligible(record, now) {
			continue
		}
		if record.Status != "running" || record.ProbeError != "" {
			i.queueRecovery(owner, false)
		}
		if state := i.recovery[owner]; state != nil && state.Status == "pending" {
			if now.Before(state.Command.ExpiresAt) {
				commands = append(commands, state.Command)
			} else {
				state.Status = "expired"
				state.Message = "客户端未在有效期内执行"
			}
		}
	}
	return commands
}
func (i *Inventory) Acknowledge(clientID string, results []relayprotocol.RecoveryResult) error {
	if len(results) > 1000 {
		return fmt.Errorf("too many recovery results")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, result := range results {
		record, ok := i.records[result.Owner]
		state := i.recovery[result.Owner]
		if !ok || record.ClientID != clientID || state == nil || state.Command.ID != result.ID || state.Status != "pending" {
			continue
		}
		if len(result.Message) > 500 {
			return fmt.Errorf("recovery result too long")
		}
		switch result.Status {
		case "restarted", "failed", "skipped":
		default:
			return fmt.Errorf("invalid recovery result")
		}
		state.Status = result.Status
		state.Message = result.Message
	}
	return nil
}
func (i *Inventory) Probe(ctx context.Context) {
	records := i.List()
	jobs := make(chan TunnelRecord)
	var workers sync.WaitGroup
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for n := 0; n < 8; n++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for record := range jobs {
				conn, err := (&net.Dialer{Timeout: 500 * time.Millisecond}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(record.ListenPort)))
				if conn != nil {
					conn.Close()
				}
				i.mu.Lock()
				current, ok := i.records[record.Owner]
				if ok && current.ListenPort == record.ListenPort && recoveryEligible(current, time.Now()) {
					current.ProbedAt = time.Now()
					current.ProbeError = ""
					if err != nil {
						current.ProbeError = "VPS入口端口不可达"
						current.Online = false
					}
					i.records[record.Owner] = current
					if err != nil {
						i.queueRecovery(record.Owner, false)
					} else if state := i.recovery[record.Owner]; state != nil && state.Status == "restarted" && current.Status == "running" {
						state.Attempts = 0
						state.Next = time.Time{}
						state.Status = "healthy"
						state.Message = "客户端已重建，VPS入口探测通过"
					}
				}
				i.mu.Unlock()
			}
		}()
	}
loop:
	for _, record := range records {
		if !recoveryEligible(record, time.Now()) {
			continue
		}
		select {
		case jobs <- record:
		case <-ctx.Done():
			break loop
		}
	}
	close(jobs)
	workers.Wait()
}
func (i *Inventory) Monitor(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			i.Probe(ctx)
		}
	}
}
