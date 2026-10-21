package processgroup

import (
	"errors"
	"fmt"
)

// ErrExhausted means a command reached its configured process or memory ceiling.
var ErrExhausted = errors.New("command resource ceiling reached")

// ErrScopeUnconfirmed means a created command scope may still exist after cleanup.
var ErrScopeUnconfirmed = errors.New("command scope release unconfirmed")

// Exhaustion describes observed cgroup events and the ceilings that were hit.
type Exhaustion struct {
	MaxTasks           int
	MaxMemoryBytes     int64
	PIDEventsMax       uint64
	MemoryEventsMax    uint64
	MemoryOOM          uint64
	MemoryOOMKill      uint64
	MemoryOOMGroupKill uint64
	Stop               string
	Released           bool
	Scope              string
}

func (e *Exhaustion) Error() string {
	release := "unconfirmed"
	if e.Released {
		release = "confirmed"
	}
	message := fmt.Sprintf("%v: pids.max=%d pids.events:max=%d memory.max=%d memory.events:max=%d oom=%d oom_kill=%d oom_group_kill=%d stop=%s release=%s",
		ErrExhausted, e.MaxTasks, e.PIDEventsMax, e.MaxMemoryBytes, e.MemoryEventsMax, e.MemoryOOM, e.MemoryOOMKill, e.MemoryOOMGroupKill, e.Stop, release)
	if !e.Released && e.Scope != "" {
		message += " scope=" + e.Scope
	}
	return message
}

func (*Exhaustion) Unwrap() error { return ErrExhausted }
