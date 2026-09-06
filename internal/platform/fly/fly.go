// Package fly is the fly.io Platform adapter: single machine + single
// NVMe volume, driven through the flyctl CLI (which must be on PATH and
// authenticated — `fly auth login`). fly deploy builds remotely from
// agent/Dockerfile, so no local container engine is involved. The
// repo-owned manifest is deploy/fly.toml, scaffolded from the embedded
// template and linted against the image contract on every deploy.
package fly

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/tankdonut/agent-base/internal/platform"
	"github.com/tankdonut/agent-base/internal/process"
	"github.com/tankdonut/agent-base/internal/project"
)

// ConfigName is the adapter's repo-owned manifest, relative to the
// project root.
const ConfigName = "deploy/fly.toml"

// dataMountPath is where the image keeps its warm state (the compose
// contract's agent-data volume mounts the same path).
const dataMountPath = "/home/node/.openclaw"

// gatewayPort is the image's gateway port; fly's http_service proxy
// targets it.
const gatewayPort = 18789

// maxDrainSec is fly's hard kill_timeout cap (verified against
// fly.io/docs/reference/configuration): the image's 600s default grace
// exceeds it, so the contract fields are clamped and checked.
const maxDrainSec = 300

// Config is the adapter's typed `.agentctl.yaml` namespace:
//
//	fly:
//	  app: my-agent   # optional verb override; deploy/fly.toml is authoritative
//	  region: sjc     # scaffold-time only
type Config struct {
	App    string
	Region string
}

// Adapter executes the fly platform. flyctl is resolved once at
// construction; the app name resolves per call (config override, else
// the manifest).
type Adapter struct {
	runner process.Runner
	cfg    Config
}

// New decodes the fly config namespace (fail-closed on unknown keys)
// and verifies flyctl is on PATH with an actionable install hint.
func New(r process.Runner, ns map[string]any) (platform.Platform, error) {
	cfg := Config{}
	for key, val := range ns {
		switch key {
		case "app", "region":
			s, ok := val.(string)
			if !ok {
				return nil, fmt.Errorf("fly.%s: want a string, got %T", key, val)
			}
			if key == "app" {
				cfg.App = s
			} else {
				cfg.Region = s
			}
		default:
			return nil, fmt.Errorf("unknown fly config key %q (known: app, region)", key)
		}
	}
	if _, err := process.LookPath(r, "fly"); err != nil {
		return nil, fmt.Errorf("fly CLI not found in PATH — install it (https://fly.io/docs/flyctl/) and run `fly auth login`")
	}
	return &Adapter{runner: r, cfg: cfg}, nil
}

// flyManifest is the decoded subset of deploy/fly.toml the contract
// lint cares about.
type flyManifest struct {
	App           string `toml:"app"`
	PrimaryRegion string `toml:"primary_region"`
	KillSignal    string `toml:"kill_signal"`
	KillTimeout   int    `toml:"kill_timeout"`
	Build         struct {
		Dockerfile string `toml:"dockerfile"`
	} `toml:"build"`
	Env    map[string]string `toml:"env"`
	Mounts *struct {
		Source      string `toml:"source"`
		Destination string `toml:"destination"`
	} `toml:"mounts"`
	HTTPService *struct {
		InternalPort int `toml:"internal_port"`
	} `toml:"http_service"`
}

// readManifest parses deploy/fly.toml under root, fail-closed.
func readManifest(root string) (*flyManifest, error) {
	data, err := os.ReadFile(filepath.Join(root, ConfigName))
	if err != nil {
		return nil, fmt.Errorf("deploy/fly.toml not found — scaffold it with `agentctl platform set fly --app <name> --region <code>`")
	}
	var m flyManifest
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing deploy/fly.toml: %w", err)
	}
	return &m, nil
}

// app resolves the fly app name: config override, else the manifest.
func (a *Adapter) app(root string) (string, error) {
	if a.cfg.App != "" {
		return a.cfg.App, nil
	}
	m, err := readManifest(root)
	if err != nil {
		return "", err
	}
	if m.App == "" {
		return "", fmt.Errorf("deploy/fly.toml: app is empty")
	}
	return m.App, nil
}

// Name identifies the adapter in errors and `platform ls`.
func (a *Adapter) Name() string { return "fly" }

// Capabilities: fly ssh console gives exec; machine stop/start exist;
// apps destroy always deletes the volume with the app.
func (a *Adapter) Capabilities() platform.Capabilities {
	return platform.Capabilities{Exec: true, StopStart: true, VolumePreservingDestroy: false}
}

// Check fail-closed lints deploy/fly.toml against the image contract:
// SIGTERM drain (fly defaults to SIGINT, which skips the drain), the
// 300s kill_timeout cap with a matching AGENT_SHUTDOWN_GRACE, the
// single volume at {data}, the agent Dockerfile, and the gateway port.
// Secrets must also be complete — fly deploys carry them, not env_file.
func (a *Adapter) Check(root string, d *platform.Deployment) error {
	m, err := readManifest(root)
	if err != nil {
		return err
	}
	switch {
	case m.App == "":
		return fmt.Errorf("deploy/fly.toml: app is empty")
	case m.Build.Dockerfile != "agent/Dockerfile":
		return fmt.Errorf("deploy/fly.toml: [build] dockerfile must be agent/Dockerfile — the project image is the deploy artifact")
	case m.KillSignal != "SIGTERM":
		return fmt.Errorf("deploy/fly.toml: kill_signal must be SIGTERM (is %q) — fly's default SIGINT skips the agent's graceful drain", m.KillSignal)
	case m.KillTimeout <= 0 || m.KillTimeout > maxDrainSec:
		return fmt.Errorf("deploy/fly.toml: kill_timeout must be 1..%d (is %d) — fly hard-caps the drain window", maxDrainSec, m.KillTimeout)
	}
	grace, err := strconv.Atoi(m.Env["AGENT_SHUTDOWN_GRACE"])
	if err != nil || grace <= 0 || grace > m.KillTimeout {
		return fmt.Errorf("deploy/fly.toml: [env] AGENT_SHUTDOWN_GRACE must be an integer between 1 and kill_timeout (%d)", m.KillTimeout)
	}
	if m.Mounts == nil {
		return fmt.Errorf("deploy/fly.toml: missing [mounts] — the warm {data} volume is contract")
	}
	if m.Mounts.Destination != dataMountPath {
		return fmt.Errorf("deploy/fly.toml: mounts destination must be %s (is %q)", dataMountPath, m.Mounts.Destination)
	}
	if m.HTTPService == nil || m.HTTPService.InternalPort != gatewayPort {
		return fmt.Errorf("deploy/fly.toml: [http_service] internal_port must be %d — the gateway port", gatewayPort)
	}
	if _, err := project.SecretsCheck(root); err != nil {
		return err
	}
	return nil
}

// Deploy converges: Check gates, then fly deploy (remote build from
// agent/Dockerfile — no local engine). DryRun stops after Check.
func (a *Adapter) Deploy(ctx context.Context, r process.Runner, root string, d *platform.Deployment, opts platform.DeployOptions, out platform.Output) error {
	if err := a.Check(root, d); err != nil {
		return err
	}
	if opts.DryRun {
		out.Printf("check passed — deploy %s dry run complete (platform: fly)\n", d.Project)
		return nil
	}
	if err := process.RunArgv(r, nil, "fly", "deploy", "-c", ConfigName, "--remote-only"); err != nil {
		return err
	}
	app, _ := a.app(root)
	out.Printf("deployed %s (platform: fly, app: %s) — secrets flow via `fly secrets import -a %s < agent/.env`\n", d.Project, app, app)
	return nil
}

// Status prints the derived header, then fly status.
func (a *Adapter) Status(ctx context.Context, r process.Runner, root string, d *platform.Deployment, out platform.Output) error {
	app, err := a.app(root)
	if err != nil {
		return err
	}
	out.Printf("platform: fly (app: %s)\nproject: %s\nbase image: ghcr.io/tankdonut/agent-base:%s\nenv vars set: %d\n",
		app, d.Project, d.BaseTag, len(d.EnvKeys))
	return process.RunArgv(r, nil, "fly", "status", "-a", app)
}

// Logs streams the app logs; fly logs streams by default, so the
// follow flag is advisory.
func (a *Adapter) Logs(ctx context.Context, r process.Runner, root string, d *platform.Deployment, follow bool, out platform.Output) error {
	app, err := a.app(root)
	if err != nil {
		return err
	}
	return process.RunArgv(r, nil, "fly", "logs", "-a", app)
}

// Mcp runs `openclaw mcp <args>` in the running machine via
// fly ssh console. Args join with spaces — keep them shell-simple
// (codes and names, not free text).
func (a *Adapter) Mcp(ctx context.Context, r process.Runner, root string, d *platform.Deployment, args []string, out platform.Output) error {
	app, err := a.app(root)
	if err != nil {
		return err
	}
	command := strings.Join(append([]string{"openclaw", "mcp"}, args...), " ")
	return process.RunArgv(r, nil, "fly", "ssh", "console", "-a", app, "-C", command)
}

// machineID resolves the app's single machine via fly machine list.
func (a *Adapter) machineID(r process.Runner, app string) (string, error) {
	if r == nil {
		return "", process.ErrNilRunner
	}
	out, err := r.RunOutput(nil, "fly", "machine", "list", "-a", app, "-j")
	if err != nil {
		return "", fmt.Errorf("fly machine list: %w", err)
	}
	var machines []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &machines); err != nil {
		return "", fmt.Errorf("parsing `fly machine list -j` output: %w", err)
	}
	if len(machines) == 0 {
		return "", fmt.Errorf("app %s has no machines — run `agentctl deploy` first", app)
	}
	return machines[0].ID, nil
}

// Stop pauses the single machine in place.
func (a *Adapter) Stop(ctx context.Context, r process.Runner, root string, d *platform.Deployment, out platform.Output) error {
	app, err := a.app(root)
	if err != nil {
		return err
	}
	id, err := a.machineID(r, app)
	if err != nil {
		return err
	}
	if err := process.RunArgv(r, nil, "fly", "machine", "stop", id, "-a", app); err != nil {
		return err
	}
	out.Printf("stopped machine %s (data volume kept; `agentctl start` resumes)\n", id)
	return nil
}

// Start resumes the stopped machine.
func (a *Adapter) Start(ctx context.Context, r process.Runner, root string, d *platform.Deployment, out platform.Output) error {
	app, err := a.app(root)
	if err != nil {
		return err
	}
	id, err := a.machineID(r, app)
	if err != nil {
		return err
	}
	if err := process.RunArgv(r, nil, "fly", "machine", "start", id, "-a", app); err != nil {
		return err
	}
	out.Printf("started machine %s\n", id)
	return nil
}

// Destroy deletes the app (the volume dies with it — that is why the
// destroy verb demands --volumes for this platform).
func (a *Adapter) Destroy(ctx context.Context, r process.Runner, root string, d *platform.Deployment, destroyData bool, out platform.Output) error {
	app, err := a.app(root)
	if err != nil {
		return err
	}
	if err := process.RunArgv(r, nil, "fly", "apps", "destroy", app, "-y"); err != nil {
		return err
	}
	out.Printf("destroyed fly app %s including its data volume\n", app)
	return nil
}
