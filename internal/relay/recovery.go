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
func recoveryPermitted(record TunnelRecord, now time.Time, manual bool) bool {
	if !manual {
		return recoveryEligible(record, now)
	}
	if record.Disabled {
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
	if !ok {
		return fmt.Errorf("隧道记录不存在")
	}
	if !recoveryPermitted(record, now, manual) {
		return fmt.Errorf("隧道被服务端禁用或已过期，请先启用或续期")
	}
	prior := i.recovery[owner]
	if prior != nil {
		pending := prior.Status == "pending" && (prior.Command.ExpiresAt.IsZero() || now.Before(prior.Command.ExpiresAt))
		if pending && (!manual || prior.Command.Action == "start") {
			return nil
		}
		if (!manual || prior.Command.Action == "start") && now.Before(prior.Next) {
			return fmt.Errorf("恢复请求冷却中")
		}
		if !manual && prior.Attempts >= 5 {
			return fmt.Errorf("自动恢复达到5次上限，请排查后手动唤醒")
		}
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	state := &recoveryState{Attempts: 1, Status: "pending", Message: "等待客户端接收恢复请求"}
	if prior != nil {
		state.Attempts = prior.Attempts + 1
	}
	state.Command = relayprotocol.RecoveryCommand{ID: hex.EncodeToString(id[:]), Owner: owner, ExpiresAt: now.Add(time.Minute)}
	state.Next = now.Add(time.Duration(1<<min(state.Attempts-1, 4)) * 30 * time.Second)
	if manual {
		state.Command.Action = "start"
		state.Command.ExpiresAt = time.Time{}
		state.Next = now.Add(30 * time.Second)
		state.Message = "手动唤醒请求已保存，等待客户端执行"
	}
	i.recovery[owner] = state
	if manual {
		if err := i.saveManualRecovery(); err != nil {
			if prior == nil {
				delete(i.recovery, owner)
			} else {
				i.recovery[owner] = prior
			}
			return err
		}
	}
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
	previous := make(map[string]*recoveryState, len(i.recovery))
	for owner, state := range i.recovery {
		copy := *state
		previous[owner] = &copy
	}
	for owner, record := range i.records {
		if record.ClientID != clientID {
			continue
		}
		if recoveryEligible(record, now) && (record.Status != "running" || record.ProbeError != "") {
			i.queueRecovery(owner, false)
		}
		state := i.recovery[owner]
		if state == nil || state.Status != "pending" {
			continue
		}
		manual := state.Command.Action == "start"
		if !recoveryPermitted(record, now, manual) {
			state.Status = "skipped"
			state.Message = "隧道策略已改变，未执行唤醒"
			continue
		}
		if manual {
			if now.Sub(record.LastSeen) > 45*time.Second || record.RecoveryProtocol < 2 && !recoveryEligible(record, now) {
				continue
			}
			if state.Command.ExpiresAt.IsZero() {
				state.Command.ExpiresAt = now.Add(time.Minute)
			}
		}
		if now.Before(state.Command.ExpiresAt) {
			command := state.Command
			if manual && record.RecoveryProtocol < 2 {
				command.Action = ""
			}
			commands = append(commands, command)
		} else {
			state.Status = "expired"
			state.Message = "客户端未在执行期限内回执，请重新唤醒"
		}
	}
	if err := i.saveManualRecovery(); err != nil {
		i.recovery = previous
		return nil
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
	return i.saveManualRecovery()
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
