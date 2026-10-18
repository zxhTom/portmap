package forward

import "sync"

type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateFailed   State = "failed"
)

type Status struct {
	State State  `json:"state"`
	Error string `json:"error,omitempty"`
}

type Backend interface {
	Start() error
	Close() error
	Status() Status
}

type BaseBackend struct {
	mu     sync.RWMutex
	status Status
}

func NewBaseBackend(initial State) *BaseBackend {
	return &BaseBackend{
		status: Status{State: initial},
	}
}

func (b *BaseBackend) SetStatus(st Status) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.status = st
}

func (b *BaseBackend) Status() Status {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.status
}
