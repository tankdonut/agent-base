// methods.go is the typed surface: the closed method allowlist and
// the status/approval calls built on it. Adding a method here is a
// deliberate security decision, not a convenience — the client never
// becomes a general RPC surface.
package gatewayclient

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// allowedMethods is the closed set this client may call: read-only
// status probes plus the approval resolution pair. Reserved admin
// namespaces (config.*, exec.approvals.* policy, wizard.*, update.*)
// are unreachable by construction.
var allowedMethods = map[string]bool{
	"agent":                   true,
	"sessions.list":           true,
	"cron.list":               true,
	"exec.approval.list":      true,
	"exec.approval.resolve":   true,
	"plugin.approval.list":    true,
	"plugin.approval.resolve": true,
}

func methodAllowed(method string) bool { return allowedMethods[method] }

// Decision values accepted by *.approval.resolve.
const (
	DecisionApprove = "approve"
	DecisionDeny    = "deny"
)

// Approval is the normalized pending-approval row (exec and plugin
// families share the shape; extraneous fields are ignored).
type Approval struct {
	ID      string `json:"id"`
	Kind    string `json:"kind,omitempty"`
	Summary string `json:"summary,omitempty"`
	Command string `json:"command,omitempty"`
	Agent   string `json:"agentId,omitempty"`
	Session string `json:"sessionKey,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// ListApprovals returns pending approvals (exec family first, then
// plugin), each tagged with its family so resolve routes correctly.
func (c *Client) ListApprovals(ctx context.Context) ([]Approval, error) {
	var out []Approval
	exec, err := c.listFamily(ctx, "exec")
	if err != nil {
		return nil, err
	}
	out = append(out, exec...)
	plugin, err := c.listFamily(ctx, "plugin")
	if err != nil {
		return nil, err
	}
	out = append(out, plugin...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// rawApprovalRow mirrors the wire shape of a pending approval entry:
// `{approvalKind?, id, request:{command,...}, createdAtMs, ...}` — the
// human-readable payload nests under request (schema:
// listVisiblePendingApprovalRequests in the gateway server).
type rawApprovalRow struct {
	ID           string `json:"id"`
	ApprovalKind string `json:"approvalKind"`
	Request      struct {
		Command     string `json:"command"`
		Summary     string `json:"summary"`
		AgentID     string `json:"agentId"`
		SessionKey  string `json:"sessionKey"`
		Description string `json:"description"`
	} `json:"request"`
	// Lenient fallbacks for flattened shapes.
	Command string `json:"command"`
	Summary string `json:"summary"`
	Agent   string `json:"agentId"`
	Session string `json:"sessionKey"`
	Reason  string `json:"reason"`
}

func (r rawApprovalRow) toApproval(family string) Approval {
	a := Approval{
		ID:      r.ID,
		Kind:    family,
		Command: r.Request.Command,
		Summary: r.Request.Summary,
		Agent:   r.Request.AgentID,
		Session: r.Request.SessionKey,
		Reason:  r.Reason,
	}
	if a.Command == "" {
		a.Command = r.Command
	}
	if a.Summary == "" {
		a.Summary = r.Summary
		if a.Summary == "" {
			a.Summary = r.Request.Description
		}
	}
	if a.Agent == "" {
		a.Agent = r.Agent
	}
	if a.Session == "" {
		a.Session = r.Session
	}
	return a
}

func (c *Client) listFamily(ctx context.Context, family string) ([]Approval, error) {
	var raw json.RawMessage
	if err := c.Call(ctx, family+".approval.list", map[string]any{}, &raw); err != nil {
		return nil, fmt.Errorf("%s.approval.list: %w", family, err)
	}
	var rows []rawApprovalRow
	// The family endpoints wrap the rows differently across builds
	// (approvals/items/bare array) — accept all three, tag the family.
	var wrapped struct {
		Approvals []rawApprovalRow `json:"approvals"`
		Items     []rawApprovalRow `json:"items"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil {
		rows = append(wrapped.Approvals, wrapped.Items...)
	}
	if len(rows) == 0 {
		if err := json.Unmarshal(raw, &rows); err != nil {
			rows = nil
		}
	}
	out := make([]Approval, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toApproval(family))
	}
	return out, nil
}

// ResolveApproval approves or denies one pending approval by ID and
// family. decision must be exactly DecisionApprove or DecisionDeny.
func (c *Client) ResolveApproval(ctx context.Context, family, id, decision string) error {
	if decision != DecisionApprove && decision != DecisionDeny {
		return fmt.Errorf("decision must be %q or %q", DecisionApprove, DecisionDeny)
	}
	method := family + ".approval.resolve"
	if !methodAllowed(method) {
		return fmt.Errorf("unknown approval family %q", family)
	}
	return c.Call(ctx, method, map[string]any{"id": id, "decision": decision}, nil)
}
