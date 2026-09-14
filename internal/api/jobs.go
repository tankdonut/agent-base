// jobs.go is the 202 job table: async verb execution with a bounded
// log ring, monotonic IDs, and the state transitions the SSE hub
// fans out. Job logs capture command OUTPUT only — the serve token
// and agent env values never enter a job's log.
package api

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Job lifecycle states.
const (
	StateQueued  = "queued"
	StateRunning = "running"
	StateDone    = "done"
	StateFailed  = "failed"
)

// logRing keeps the last N lines of a job's output.
const logRingLines = 400

// Job is one async verb execution.
type Job struct {
	ID    string
	Kind  string
	State string
	Err   error

	mu      sync.Mutex
	spec    jobSpec
	ctx     context.Context
	log     []string
	created time.Time
}

// set is the internal state transition (callers publish the event).
func (j *Job) set(state string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.State = state
}

// printf appends one formatted line to the ring, dropping the oldest.
func (j *Job) printf(format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.log = append(j.log, fmt.Sprintf(format, args...))
	if len(j.log) > logRingLines {
		j.log = j.log[len(j.log)-logRingLines:]
	}
}

// write implements platform.Output — adapter progress lands in the ring.
func (j *Job) Write(p []byte) (int, error) {
	j.printf("%s", p)
	return len(p), nil
}

// Printf is the platform.Output formatted variant.
func (j *Job) Printf(format string, args ...any) { j.printf(format, args...) }

// jobView is the JSON projection (err flattened, log as lines).
type jobView struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	State   string   `json:"state"`
	Agents  []string `json:"agents"`
	Error   string   `json:"error,omitempty"`
	Log     []string `json:"log"`
	Created string   `json:"created_at"`
}

func (j *Job) view() jobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := jobView{
		ID:      j.ID,
		Kind:    j.Kind,
		State:   j.State,
		Agents:  j.spec.agents,
		Log:     append([]string{}, j.log...),
		Created: j.created.UTC().Format(time.RFC3339),
	}
	if j.Err != nil {
		v.Error = j.Err.Error()
	}
	if v.Log == nil {
		v.Log = []string{}
	}
	return v
}

// jobTable is the process-lifetime job registry (jobs die with the
// server; persistence is a later concern).
type jobTable struct {
	mu   sync.Mutex
	seq  int
	jobs map[string]*Job
}

func newJobTable() *jobTable { return &jobTable{jobs: map[string]*Job{}} }

func (t *jobTable) new(spec jobSpec) *Job {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	id := fmt.Sprintf("job-%04d", t.seq)
	j := &Job{
		ID:      id,
		Kind:    spec.kind,
		State:   StateQueued,
		spec:    spec,
		ctx:     context.Background(),
		log:     []string{},
		created: time.Now(),
	}
	t.jobs[id] = j
	return j
}

func (t *jobTable) get(id string) (*Job, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	j, ok := t.jobs[id]
	return j, ok
}

func (t *jobTable) snapshot() []jobView {
	t.mu.Lock()
	defer t.mu.Unlock()
	ids := make([]string, 0, len(t.jobs))
	for id := range t.jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]jobView, 0, len(ids))
	for _, id := range ids {
		out = append(out, t.jobs[id].view())
	}
	return out
}
