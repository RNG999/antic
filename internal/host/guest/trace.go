package guest

import (
	"encoding/json"
	"os"

	"github.com/RNG999/antic/internal/tea/operation"
)

// traceEnv names an NDJSON file the shell appends its saga events to:
// taking and releasing the guest's lock, planning (with the plan's number
// of commit steps), and each executed effect's stage and outcome.
// internal/spec/refine checks these traces against formal/saga/Saga.tla.
const traceEnv = "ANTIC_SAGA_TRACE"

// traceEvent is one line of a saga trace.
type traceEvent struct {
	PID     int             `json:"pid"`
	Guest   string          `json:"guest"`
	Event   string          `json:"event"` // acquire, plan, step, release
	Commits int             `json:"commits,omitempty"`
	Stage   operation.Stage `json:"stage,omitempty"`
	OK      bool            `json:"ok,omitempty"`
}

func (g *Guest) trace(ev traceEvent) {
	path := os.Getenv(traceEnv)
	if path == "" {
		return
	}
	ev.PID, ev.Guest = os.Getpid(), g.Name()
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n')) // one write: lines of concurrent processes do not mix
}
