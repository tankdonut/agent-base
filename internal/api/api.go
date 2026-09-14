// Package api is the agentctl control-plane server: a loopback-only,
// bearer-authenticated HTTP surface over the same fleet primitives the
// CLI verbs use. Long-running verbs (deploy) run as 202 jobs with a
// jobs table and an SSE event stream; approvals proxy the per-agent
// gateways over the pinned WS protocol. The package imports the port
// and foundations only — the platform registry is injected by the cli
// composition root, because a server process must never chdir, every
// execution is dir-scoped through the Runner.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tankdonut/agent-base/internal/fleet"
	"github.com/tankdonut/agent-base/internal/gatewayclient"
	"github.com/tankdonut/agent-base/internal/platform"
	"github.com/tankdonut/agent-base/internal/process"
)

// Deps are the injected capabilities: everything the handlers need
// that lives outside this package.
type Deps struct {
	// Manifest is the loaded fleet.
	Manifest *fleet.Manifest
	// Runner builds the process runner (normally process.Runner over
	// exec; a var seam in cli for tests).
	NewRunner func() process.Runner
	// PlatformFor resolves the constructed platform + deployment for
	// one registered agent (cli registry: config + Derive + adapter).
	PlatformFor func(agent string) (string, platform.Platform, platform.Deployment, error)
	// Version reports the CLI version for /healthz and job metadata.
	Version string
	// Token is the required bearer value.
	Token string
}

// Server is the serve instance.
type Server struct {
	deps Deps
	jobs *jobTable
	hub  *hub
	mux  *http.ServeMux
}

// New wires the routes. Loopback-only binding is enforced at Listen.
func New(deps Deps) *Server {
	s := &Server{deps: deps, jobs: newJobTable(), hub: newHub()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /api/v1/roster", s.auth(s.roster))
	mux.HandleFunc("GET /api/v1/status", s.auth(s.status))
	mux.HandleFunc("POST /api/v1/deploy", s.auth(s.deploy))
	mux.HandleFunc("GET /api/v1/jobs", s.auth(s.listJobs))
	mux.HandleFunc("GET /api/v1/jobs/{id}", s.auth(s.getJob))
	mux.HandleFunc("GET /api/v1/approvals", s.auth(s.listApprovals))
	mux.HandleFunc("POST /api/v1/approvals/resolve", s.auth(s.resolveApproval))
	mux.HandleFunc("GET /api/v1/events", s.auth(s.events))
	s.mux = mux
	return s
}

// ListenAndServe binds addr and serves until ctx is cancelled. A
// non-loopback host is a hard error: the API has no TLS story and its
// bearer is a local-automation credential, not a network one.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("bad serve address %q: %w", addr, err)
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return fmt.Errorf("refusing to bind %q — the serve API is loopback-only (remote access goes through a local reverse proxy)", addr)
	}
	srv := &http.Server{Addr: addr, Handler: s.mux, ReadHeaderTimeout: 10 * time.Second}
	go s.hub.run(ctx)
	go s.pollApprovals(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

// auth is the bearer middleware: constant-time token compare, 401
// JSON on any mismatch or absence.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.deps.Token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true", "version": s.deps.Version})
}

func (s *Server) roster(w http.ResponseWriter, _ *http.Request) {
	type rosterEntry struct {
		Agent       string `json:"agent"`
		Platform    string `json:"platform"`
		GatewayPort int    `json:"gateway_port"`
		Dir         string `json:"dir"`
	}
	rows := make([]rosterEntry, 0, len(s.deps.Manifest.Agents))
	for _, name := range s.deps.Manifest.AgentNames() {
		entry := s.deps.Manifest.Agents[name]
		rows = append(rows, rosterEntry{Agent: name, Platform: entry.Platform, GatewayPort: entry.GatewayPort, Dir: entry.Dir})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"plane":      s.deps.Manifest.Plane,
		"agents":     rows,
		"fleet_root": s.deps.Manifest.Root,
	})
}

// writeJSON marshals v as a JSON response; encoding failures become
// 500 (they indicate a programming error, not user input).
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		_ = err // header already sent; nothing sane left to do
	}
}

// failureWriter lets handlers report errors as JSON with a status.
type failureWriter struct {
	http.ResponseWriter
	status int
}

func (f *failureWriter) WriteHeader(code int) {
	f.status = code
	f.ResponseWriter.WriteHeader(code)
}

func (s *Server) fail(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// agentGate resolves one agent for single-agent endpoints: the
// implicit fleet-of-one default, else an explicit name.
func (s *Server) agentGate(name string) (fleet.AgentEntry, string, error) {
	if name == "" && len(s.deps.Manifest.Agents) == 1 {
		name = s.deps.Manifest.AgentNames()[0]
	}
	entry, ok := s.deps.Manifest.Agents[name]
	if !ok {
		return fleet.AgentEntry{}, "", fmt.Errorf("agent %q is not registered in the fleet (pass ?agent=)", name)
	}
	return entry, name, nil
}

// concurrency caps heavy compose jobs; deploy is the only 202 verb.
var jobSlots = make(chan struct{}, 2)

type jobSpec struct {
	kind   string
	agents []string
	force  bool
	dryRun bool
}

func (s *Server) deploy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Agent  string `json:"agent"`
		All    bool   `json:"all"`
		Force  bool   `json:"force"`
		DryRun bool   `json:"dry_run"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("body: %w", err))
		return
	}
	var names []string
	switch {
	case body.Agent != "" && body.All:
		s.fail(w, http.StatusBadRequest, fmt.Errorf("agent and all are mutually exclusive"))
		return
	case body.Agent != "":
		_, name, err := s.agentGate(body.Agent)
		if err != nil {
			s.fail(w, http.StatusNotFound, err)
			return
		}
		names = []string{name}
	case body.All:
		names = s.deps.Manifest.AgentNames()
	case len(s.deps.Manifest.Agents) == 1:
		names = s.deps.Manifest.AgentNames()
	default:
		s.fail(w, http.StatusBadRequest, fmt.Errorf("%d agents registered — the deploy body needs agent or all (batches are never implicit)", len(s.deps.Manifest.Agents)))
		return
	}

	job := s.jobs.new(jobSpec{kind: "deploy", agents: names, force: body.Force, dryRun: body.DryRun})
	go s.runDeploy(job)
	s.hub.publish(event{Type: "job", JobID: job.ID, State: job.State})
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": job.ID})
}

// runDeploy converges the job's agents sequentially (compose stacks
// are heavy; the slot semaphore bounds parallelism across jobs).
func (s *Server) runDeploy(job *Job) {
	job.set(StateRunning)
	s.hub.publish(event{Type: "job", JobID: job.ID, State: job.State})
	jobSlots <- struct{}{}
	defer func() { <-jobSlots }()
	var failures []string
	for _, name := range job.spec.agents {
		root, p, d, err := s.deps.PlatformFor(name)
		if err == nil {
			if _, err = fleet.MaterializeAgentCompose(s.deps.Manifest, name); err != nil {
				root, p, d = "", nil, platform.Deployment{}
			}
		}
		if err == nil {
			err = p.Deploy(job.ctx, s.deps.NewRunner(), root, &d, platform.DeployOptions{Force: job.spec.force, DryRun: job.spec.dryRun}, job)
		}
		if err != nil {
			failures = append(failures, name+": "+err.Error())
			job.printf("FAIL %s: %v\n", name, err)
		}
	}
	job.mu.Lock()
	if len(failures) > 0 {
		job.State = StateFailed
		job.Err = fmt.Errorf("%d of %d agent(s) failed", len(failures), len(job.spec.agents))
	} else {
		job.State = StateDone
	}
	job.mu.Unlock()
	s.hub.publish(event{Type: "job", JobID: job.ID, State: job.State})
}

// listJobs is the jobs table snapshot.
func (s *Server) listJobs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"jobs": s.jobs.snapshot()})
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.jobs.get(r.PathValue("id"))
	if !ok {
		s.fail(w, http.StatusNotFound, fmt.Errorf("no such job"))
		return
	}
	writeJSON(w, http.StatusOK, job.view())
}

// connectGateway is the WS dial seam (tests swap it).
var connectGateway = gatewayclient.Connect

// pollApprovals watches every agent's pending-approval count and
// publishes deltas on the hub. Poll cadence is deliberately slow —
// approvals are human-scale; gateways that fail the probe are silent.
func (s *Server) pollApprovals(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	last := map[string]int{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, name := range s.deps.Manifest.AgentNames() {
				entry := s.deps.Manifest.Agents[name]
				token := gatewayToken(entry.Dir)
				if token == "" {
					continue
				}
				probeCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
				count, err := func() (int, error) {
					c, err := connectGateway(probeCtx, fmt.Sprintf("ws://127.0.0.1:%d", entry.GatewayPort), token, gatewayclient.Options{})
					if err != nil {
						return 0, err
					}
					defer c.Close()
					approvals, err := c.ListApprovals(probeCtx)
					if err != nil {
						return 0, err
					}
					return len(approvals), nil
				}()
				cancel()
				if err == nil && last[name] != count {
					last[name] = count
					s.hub.publish(event{Type: "approvals", Agent: name, Detail: fmt.Sprintf("%d", count)})
				}
			}
		}
	}
}

// status is the live gateway summary per agent (same probe the CLI
// --live path uses, inlined to keep gateway probing dependency-light).
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	rows := make([]map[string]any, 0, len(s.deps.Manifest.Agents))
	for _, name := range s.deps.Manifest.AgentNames() {
		entry := s.deps.Manifest.Agents[name]
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		row := map[string]any{"agent": name, "gateway": "unreachable"}
		if token := gatewayToken(entry.Dir); token != "" {
			if c, err := connectGateway(ctx, fmt.Sprintf("ws://127.0.0.1:%d", entry.GatewayPort), token, gatewayclient.Options{}); err == nil {
				approvals, _ := c.ListApprovals(ctx)
				row["gateway"] = c.ServerVersion()
				row["pending_approvals"] = len(approvals)
				c.Close()
			}
		} else {
			row["gateway"] = "no token"
		}
		cancel()
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": rows})
}

// gatewayToken reads OPENCLAW_GATEWAY_TOKEN from the agent's .env —
// the same convention the CLI surface uses. The value stays in
// process memory; handlers never echo it.
func gatewayToken(agentDir string) string {
	data, err := os.ReadFile(filepath.Join(agentDir, ".env"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok && key == "OPENCLAW_GATEWAY_TOKEN" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) {
	entry, name, err := s.agentGate(r.URL.Query().Get("agent"))
	if err != nil {
		s.fail(w, http.StatusNotFound, err)
		return
	}
	token := gatewayToken(entry.Dir)
	if token == "" {
		s.fail(w, http.StatusConflict, fmt.Errorf("agents/%s/.env carries no gateway token", name))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	c, err := connectGateway(ctx, fmt.Sprintf("ws://127.0.0.1:%d", entry.GatewayPort), token, gatewayclient.Options{})
	if err != nil {
		s.fail(w, http.StatusBadGateway, fmt.Errorf("gateway unreachable for %s: %w", name, err))
		return
	}
	defer c.Close()
	approvals, err := c.ListApprovals(ctx)
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	if approvals == nil {
		approvals = []gatewayclient.Approval{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": name, "approvals": approvals})
}

func (s *Server) resolveApproval(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Agent    string `json:"agent"`
		ID       string `json:"id"`
		Decision string `json:"decision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("body: %w", err))
		return
	}
	if body.ID == "" || (body.Decision != "approve" && body.Decision != "deny") {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("body needs id and decision (approve|deny)"))
		return
	}
	entry, name, err := s.agentGate(body.Agent)
	if err != nil {
		s.fail(w, http.StatusNotFound, err)
		return
	}
	token := gatewayToken(entry.Dir)
	if token == "" {
		s.fail(w, http.StatusConflict, fmt.Errorf("agents/%s/.env carries no gateway token", name))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	c, err := connectGateway(ctx, fmt.Sprintf("ws://127.0.0.1:%d", entry.GatewayPort), token, gatewayclient.Options{})
	if err != nil {
		s.fail(w, http.StatusBadGateway, fmt.Errorf("gateway unreachable for %s: %w", name, err))
		return
	}
	defer c.Close()
	family := "exec"
	if strings.HasPrefix(body.ID, "pl_") {
		family = "plugin"
	}
	if err := c.ResolveApproval(ctx, family, body.ID, body.Decision); err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"result": body.Decision, "id": body.ID})
}

// event is the SSE payload union.
type event struct {
	Type   string `json:"type"`
	JobID  string `json:"job_id,omitempty"`
	State  string `json:"state,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// hub fans events out to SSE subscribers.
type hub struct {
	mu   sync.Mutex
	subs []chan event
}

func newHub() *hub { return &hub{} }

func (h *hub) run(ctx context.Context) {
	<-ctx.Done()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs {
		close(ch)
	}
	h.subs = nil
}

func (h *hub) publish(evt event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs {
		select {
		case ch <- evt:
		default: // a slow subscriber drops the event; snapshots recover
		}
	}
}

func (h *hub) subscribe() <-chan event {
	ch := make(chan event, 16)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subs = append(h.subs, ch)
	return ch
}

// events streams the hub as text/event-stream.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.fail(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	// Headers go out immediately: the client learns the stream is
	// live before anything publishes to it.
	flusher.Flush()
	ch := s.hub.subscribe()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			body, _ := json.Marshal(evt)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.Type, body)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
