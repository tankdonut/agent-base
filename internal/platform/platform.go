// Package platform defines agentctl's deployment port: a platform-
// neutral Deployment IR derived from a project repo at invoke time, and
// the Platform interface each adapter implements (compose first; fly,
// render, k8s later). Like internal/lifecycle, this package is
// cobra-free; adapters receive a process.Runner for process execution
// and print through Output so tests capture without buffers.
package platform

import (
	"context"
	"errors"

	"github.com/tankdonut/agent-base/internal/process"
)

// Output is what adapters print through. Commands inject their stdout;
// tests inject a recorder.
type Output interface {
	Printf(format string, a ...any)
}

// Capabilities declares which verbs an adapter can honor. Verbs are
// platform-agnostic; adapters that cannot support one say so through
// these flags (and backstop with ErrUnsupported) so commands degrade
// with an actionable message instead of a broken run.
type Capabilities struct {
	// Exec: `agentctl mcp` can reach a running instance (compose exec,
	// fly ssh console, ecs execute-command).
	Exec bool
	// StopStart: stop and start exist without destroying the containers
	// (compose stop/start, machine stop). Platforms without it go
	// straight to destroy.
	StopStart bool
}

// DeployOptions parameterizes Deploy.
type DeployOptions struct {
	// DryRun stops after Check — the same fail-closed lint, no side
	// effects.
	DryRun bool
	// Force recreates running containers even when the image did not
	// change (compose --force-recreate).
	Force bool
}

// Errors adapters return for unmet capabilities; commands translate
// them into fixes.
var (
	ErrNoExec      = errors.New("platform has no exec capability")
	ErrNoStopStart = errors.New("platform has no stop/start capability")
)

// Platform is the deployment port. One instance represents the pinned
// platform of a project (from .agentctl.yaml), constructed through the
// registry (For) with its config namespace already resolved. All
// methods are idempotent converges — the platform is the state store;
// agentctl never keeps deployment state.
type Platform interface {
	Name() string
	Capabilities() Capabilities

	// Check fail-closed lints the repo-owned manifest(s) against the
	// agent-base image contract (volumes at the right paths, single
	// instance, required files present). Unknown keys and missing
	// pieces abort with the fix named.
	Check(root string, d *Deployment) error

	// Deploy converges the stack onto the platform: build (and push,
	// where remote), provision/update, and wait for health.
	Deploy(ctx context.Context, r process.Runner, root string, d *Deployment, opts DeployOptions, out Output) error

	// Status prints where the instance stands (platform state, health,
	// image tag).
	Status(ctx context.Context, r process.Runner, root string, d *Deployment, out Output) error

	// Logs streams instance logs; follow keeps the stream open.
	Logs(ctx context.Context, r process.Runner, root string, d *Deployment, follow bool, out Output) error

	// Mcp runs `openclaw mcp <args>` against the running instance. The
	// payload is part of the agent contract; exec is the mechanism,
	// gated by Capabilities.Exec.
	Mcp(ctx context.Context, r process.Runner, root string, d *Deployment, args []string, out Output) error

	// Stop pauses the instance without destroying it (capability-gated).
	Stop(ctx context.Context, r process.Runner, root string, d *Deployment, out Output) error

	// Start resumes a stopped instance (capability-gated).
	Start(ctx context.Context, r process.Runner, root string, d *Deployment, out Output) error

	// Destroy tears the instance down. destroyData=false keeps the
	// persistent volumes — data safety beats availability; nuking the
	// warm volume is the caller's explicit choice.
	Destroy(ctx context.Context, r process.Runner, root string, d *Deployment, destroyData bool, out Output) error
}
