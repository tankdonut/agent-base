// Package compose is the reference Platform adapter: the project's
// docker/podman compose stack, driven through lifecycle's argv builders
// so adapter behavior and the dev surface share one compose vocabulary.
package dockercompose

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"

	"github.com/tankdonut/agent-base/internal/compose"
	"github.com/tankdonut/agent-base/internal/platform"
	"github.com/tankdonut/agent-base/internal/process"
	"github.com/tankdonut/agent-base/internal/project"
)

// Config is the adapter's typed `.agentctl.yaml` namespace:
//
//	compose:
//	  engine: auto      # auto | podman | docker
//	  gateway_port: 18789
type Config struct {
	EnginePref  string
	GatewayPort int // 0 = lifecycle/platform default
}

// DefaultGatewayPort mirrors the image's gateway port and the scaffold
// default.
const DefaultGatewayPort = 18789

// Adapter executes the compose platform. The engine binary is resolved
// once at construction so every verb fails early with the same
// install-hint error.
type Adapter struct {
	runner      process.Runner
	engine      string
	gatewayPort int
}

// New decodes the compose config namespace (fail-closed on unknown
// keys) and resolves the engine. ns may be nil — all defaults.
func New(r process.Runner, ns map[string]any) (platform.Platform, error) {
	cfg := Config{}
	for key, val := range ns {
		switch key {
		case "engine":
			s, ok := val.(string)
			if !ok {
				return nil, fmt.Errorf("compose.engine: want a string, got %T", val)
			}
			cfg.EnginePref = s
		case "gateway_port":
			n, ok := val.(int)
			if !ok {
				return nil, fmt.Errorf("compose.gateway_port: want an integer, got %T", val)
			}
			cfg.GatewayPort = n
		default:
			return nil, fmt.Errorf("unknown compose config key %q (known: engine, gateway_port)", key)
		}
	}
	engine, err := process.ResolveEngine(cfg.EnginePref, r)
	if err != nil {
		return nil, err
	}
	port := cfg.GatewayPort
	if port == 0 {
		port = DefaultGatewayPort
	}
	return &Adapter{runner: r, engine: engine, gatewayPort: port}, nil
}

// Name identifies the adapter in errors and `platform ls`.
func (a *Adapter) Name() string { return "compose" }

// Capabilities: compose has exec and stop/start; volumes are inherent.
func (a *Adapter) Capabilities() platform.Capabilities {
	return platform.Capabilities{Exec: true, StopStart: true}
}

// Check fail-closed lints the repo-owned manifest against the image
// contract: compose.yml must exist with the agent service and both
// named volumes ({data} at /home/node/.openclaw, /backups), and
// agent/.env must be present (compose mounts it via env_file).
func (a *Adapter) Check(root string, d *platform.Deployment) error {
	path := filepath.Join(root, "compose.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("compose.yml not found — run `agentctl init` (or restore it from git)")
	}
	var manifest map[string]any
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parsing compose.yml: %w", err)
	}
	services, _ := manifest["services"].(map[string]any)
	if _, ok := services["agent"]; !ok {
		return fmt.Errorf("compose.yml: missing the `agent` service")
	}
	volumes, _ := manifest["volumes"].(map[string]any)
	for _, name := range []string{"agent-data", "agent-backups"} {
		if _, ok := volumes[name]; !ok {
			return fmt.Errorf("compose.yml: missing named volume %q — the warm {data} and /backups mounts are contract", name)
		}
	}
	return compose.RequireEnvFile(root)
}

// Deploy converges: Check gates first, then build + up -d (--force for
// Force). DryRun stops after Check.
func (a *Adapter) Deploy(ctx context.Context, r process.Runner, root string, d *platform.Deployment, opts platform.DeployOptions, out platform.Output) error {
	if err := a.Check(root, d); err != nil {
		return err
	}
	if opts.DryRun {
		out.Printf("check passed — deploy %s dry run complete (platform: compose)\n", d.Project)
		return nil
	}
	project.WarnGatewayPortBusy(warnWriter{out}, root, a.gatewayPort)
	if opts.Force {
		if err := compose.Rebuild(r, a.engine, nil); err != nil {
			return err
		}
	} else {
		if err := compose.BuildImages(r, a.engine); err != nil {
			return err
		}
		if err := compose.Up(r, a.engine, root); err != nil {
			return err
		}
	}
	out.Printf("deployed %s (platform: compose, engine: %s) — `agentctl status` to verify\n", d.Project, a.engine)
	return nil
}

// Status prints the derived deployment header, then compose ps.
func (a *Adapter) Status(ctx context.Context, r process.Runner, root string, d *platform.Deployment, out platform.Output) error {
	out.Printf("platform: compose (engine: %s)\nproject: %s\nbase image: ghcr.io/tankdonut/agent-base:%s\nenv vars set: %d\n",
		a.engine, d.Project, d.BaseTag, len(d.EnvKeys))
	return compose.Ps(r, a.engine)
}

// Logs streams the agent service logs.
func (a *Adapter) Logs(ctx context.Context, r process.Runner, root string, d *platform.Deployment, follow bool, out platform.Output) error {
	args := []string{}
	if follow {
		args = append(args, "-f")
	}
	return compose.Logs(r, a.engine, append(args, "agent"))
}

// Mcp execs `openclaw mcp <args>` in the running agent container.
func (a *Adapter) Mcp(ctx context.Context, r process.Runner, root string, d *platform.Deployment, args []string, out platform.Output) error {
	return compose.Mcp(r, a.engine, args)
}

// Stop pauses the stack in place.
func (a *Adapter) Stop(ctx context.Context, r process.Runner, root string, d *platform.Deployment, out platform.Output) error {
	if err := compose.Stop(r, a.engine); err != nil {
		return err
	}
	out.Printf("stopped %s (volumes kept; `agentctl start` resumes)\n", d.Project)
	return nil
}

// Start resumes a stopped stack.
func (a *Adapter) Start(ctx context.Context, r process.Runner, root string, d *platform.Deployment, out platform.Output) error {
	if err := compose.Start(r, a.engine); err != nil {
		return err
	}
	out.Printf("started %s\n", d.Project)
	return nil
}

// Destroy tears the stack down; destroyData=false keeps the named
// volumes (the default — data safety beats availability).
func (a *Adapter) Destroy(ctx context.Context, r process.Runner, root string, d *platform.Deployment, destroyData bool, out platform.Output) error {
	if err := compose.Destroy(r, a.engine, destroyData); err != nil {
		return err
	}
	if destroyData {
		out.Printf("destroyed %s INCLUDING volumes agent-data and agent-backups\n", d.Project)
	} else {
		out.Printf("destroyed %s (volumes kept — `agentctl destroy --volumes` removes them too)\n", d.Project)
	}
	return nil
}

// warnWriter adapts Output to the io.Writer WarnGatewayPortBusy expects.
type warnWriter struct{ out platform.Output }

func (w warnWriter) Write(p []byte) (int, error) {
	w.out.Printf("%s", p)
	return len(p), nil
}
