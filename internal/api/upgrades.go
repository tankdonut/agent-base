// upgrades.go is the upgrade surface: per-agent previews (era
// crossings via the injected callback) and the apply flow — retag the
// Dockerfile, then converge through the normal deploy job machinery.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/tankdonut/agent-base/internal/project"
)

func (s *Server) upgrades(w http.ResponseWriter, r *http.Request) {
	if s.deps.UpgradesPreview == nil {
		s.fail(w, http.StatusNotImplemented, fmt.Errorf("upgrade previews are not wired in this build"))
		return
	}
	_, name, err := s.agentGate(r.URL.Query().Get("agent"))
	if err != nil {
		s.fail(w, http.StatusNotFound, err)
		return
	}
	target := r.URL.Query().Get("target")
	if target == "" {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("target=TAG is required (the image tag to upgrade to)"))
		return
	}
	preview, err := s.deps.UpgradesPreview(name, target)
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": name, "preview": preview})
}

func (s *Server) applyUpgrade(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Agent string `json:"agent"`
		Tag   string `json:"tag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("body: %w", err))
		return
	}
	if body.Tag == "" {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("body needs tag"))
		return
	}
	if !tagShapeOK(body.Tag) {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("tag %q is not a date tag (YYYY.MM.DD[.N])", body.Tag))
		return
	}
	entry, name, err := s.agentGate(body.Agent)
	if err != nil {
		s.fail(w, http.StatusNotFound, err)
		return
	}
	if err := rewriteAgentBaseTag(entry.Dir, body.Tag); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	job := s.jobs.new(jobSpec{kind: "upgrade", agents: []string{name}, tag: body.Tag})
	go s.runDeploy(job)
	s.hub.publish(event{Type: "job", JobID: job.ID, State: job.State})
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": job.ID})
}

// rewriteAgentBaseTag retags the agent Dockerfile through the
// project-package primitive (the same one the upgrade verb uses).
func rewriteAgentBaseTag(agentDir, tag string) error {
	return project.RewriteBaseTag(filepath.Join(agentDir, "Dockerfile"), tag)
}

// tagShapeOK gates the retag to date tags — no latest, no floating
// tags, no arbitrary strings reaching the Dockerfile.
func tagShapeOK(tag string) bool {
	if len(tag) < 10 || len(tag) > 13 {
		return false
	}
	digits := 0
	dots := 0
	for i, c := range tag {
		switch {
		case c >= '0' && c <= '9':
			digits++
		case c == '.':
			dots++
			if i == 0 || i == len(tag)-1 {
				return false
			}
		default:
			return false
		}
	}
	return dots >= 2 && digits >= 8
}
