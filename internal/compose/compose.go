package compose

import (
	"encoding/json"
	"fmt"
	"github.com/tankdonut/agent-base/internal/process"
	"os"
	"path/filepath"
)

// composeArgv builds the base compose invocation for the agent root:
// <engine> compose -f compose.yml [-f compose.dev.yml] <verb...>.
// Paths are relative, so callers must run with the agent directory as
// cwd. The rendered envelope sits AT the agent dir root deliberately:
// both docker compose (project-dir = first -f file's dir) and
// podman-compose (compose-file-dir-relative) resolve context,
// dockerfile, env_file, and mounts identically only when the file
// lives at the build-context root.
func composeArgv(engine string, verb ...string) []string {
	return append([]string{engine, "compose", "-f", "compose.yml"}, verb...)
}

// RequireEnvFile is the secrets gate for start commands: compose mounts
// .env via env_file, so a missing file fails deep inside the engine.
// Fail early with the fix instead. Projects shipping a LiteLLM sidecar
// (litellm/.env.example) must also have litellm/.env — the proxy's
// env_file — with the same early failure. Exported for platform
// adapters that gate their own deploy paths.
func RequireEnvFile(root string) error {
	if _, err := os.Stat(filepath.Join(root, ".env")); err != nil {
		return fmt.Errorf(".env not found — run `agentctl secrets init` first")
	}
	if _, err := os.Stat(filepath.Join(root, "litellm", ".env.example")); err == nil {
		if _, err := os.Stat(filepath.Join(root, "litellm", ".env")); err != nil {
			return fmt.Errorf("litellm/.env not found — run `agentctl secrets init` first")
		}
	}
	return nil
}

// Up gates on agent/.env, then starts the production stack detached.
func Up(r process.Runner, engine, root string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	if err := RequireEnvFile(root); err != nil {
		return err
	}
	return process.RunArgvIn(r, root, nil, composeArgv(engine, "up", "-d")...)
}

// Stop pauses the running containers in place (no removal); Start
// resumes them. The pair exists for capability-gated platform verbs.
func Stop(r process.Runner, engine, root string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	return process.RunArgvIn(r, root, nil, composeArgv(engine, "stop")...)
}

// Start resumes containers stopped with Stop.
func Start(r process.Runner, engine, root string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	return process.RunArgvIn(r, root, nil, composeArgv(engine, "start")...)
}

// Ps lists the stack's containers and their state.
func Ps(r process.Runner, engine, root string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	return process.RunArgvIn(r, root, nil, composeArgv(engine, "ps")...)
}

// PsJSON captures `compose ps --format json` — the running-config
// surface the drift check reads (published ports vs the manifest
// allocation). Relative argv: call with the agent directory as cwd.
func PsJSON(r process.Runner, engine, root string) ([]byte, error) {
	if r == nil {
		return nil, process.ErrNilRunner
	}
	argv := composeArgv(engine, "ps", "--format", "json")
	return r.RunOutputIn(root, nil, argv[0], argv[1:]...)
}

// Destroy removes the stack. volumes=false keeps the named volumes
// (warm {data} survives); volumes=true also deletes them — the
// explicit data-loss path.
func Destroy(r process.Runner, engine, root string, volumes bool) error {
	if r == nil {
		return process.ErrNilRunner
	}
	verb := []string{"down"}
	if volumes {
		verb = append(verb, "-v")
	}
	return process.RunArgvIn(r, root, nil, composeArgv(engine, verb...)...)
}

// Logs shows compose logs; args pass through untouched (e.g. -f agent).
func Logs(r process.Runner, engine, root string, args []string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	verb := append([]string{"logs"}, args...)
	return process.RunArgvIn(r, root, nil, composeArgv(engine, verb...)...)
}

// Mcp passes args to `openclaw mcp` inside the running agent container
// (login/logout/status/doctor/…). Stdio is inherited, so interactive
// flows — the OAuth login URL print and the --code paste-back — work
// verbatim against the pinned CLI.
func Mcp(r process.Runner, engine, root string, args []string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	verb := append([]string{"exec", "agent", "openclaw", "mcp"}, args...)
	return process.RunArgvIn(r, root, nil, composeArgv(engine, verb...)...)
}

// Backup drives the image's verified backup primitive inside the
// running agent container; the archive lands in the agent-backups
// volume (the /backups mount).
func Backup(r process.Runner, engine, root string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	return process.RunArgvIn(r, root, nil, composeArgv(engine,
		"exec", "agent", "openclaw", "backup", "create", "--verify", "--output", "/backups")...)
}

// Probe runs one read-only shell command inside the running agent
// container and returns its stdout. Agentctl-authored commands only —
// the platform port contract; -T because this is capture, not
// interaction.
func Probe(r process.Runner, engine, root, command string) (string, error) {
	if r == nil {
		return "", process.ErrNilRunner
	}
	argv := composeArgv(engine, "exec", "-T", "agent", "sh", "-c", command)
	out, err := r.RunOutputIn(root, nil, argv[0], argv[1:]...)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// BuildImages builds the project image(s).
func BuildImages(r process.Runner, engine, root string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	return process.RunArgvIn(r, root, nil, composeArgv(engine, "build")...)
}

// Rebuild rebuilds the named services' images and force-recreates them
// (all services when none given). The FROM-line change of an upgrade
// invalidates every dependent layer, so the default cache behavior is
// correct here — no --no-cache (podman-compose rejects its position).
func Rebuild(r process.Runner, engine, root string, services []string) error {
	if r == nil {
		return process.ErrNilRunner
	}
	build := append([]string{"build"}, services...)
	if err := process.RunArgvIn(r, root, nil, composeArgv(engine, build...)...); err != nil {
		return err
	}
	recreate := append([]string{"up", "-d", "--force-recreate"}, services...)
	return process.RunArgvIn(r, root, nil, composeArgv(engine, recreate...)...)
}

// ApproveOwnDevice resolves the agent's pending WS device pairing:
// `openclaw devices list --json` finds the newest request and
// `openclaw devices approve <requestId>` approves it — the operator
// already holds the stack, so the host-side exec IS the approval.
func ApproveOwnDevice(r process.Runner, engine, root string) error {
	argv := composeArgv(engine, "exec", "-T", "agent", "openclaw", "devices", "list", "--json")
	list, err := r.RunOutputIn(root, nil, argv[0], argv[1:]...)
	if err != nil {
		return fmt.Errorf("devices list: %w", err)
	}
	requestID := ""
	var parsed struct {
		Pending []struct {
			RequestID string `json:"requestId"`
		} `json:"pending"`
	}
	if err := json.Unmarshal(list, &parsed); err == nil && len(parsed.Pending) > 0 {
		requestID = parsed.Pending[0].RequestID
	}
	if requestID == "" {
		var arr []struct {
			RequestID string `json:"requestId"`
		}
		if err := json.Unmarshal(list, &arr); err == nil && len(arr) > 0 {
			requestID = arr[0].RequestID
		}
	}
	if requestID == "" {
		return fmt.Errorf("no pending device request (list: %.200s)", list)
	}
	approve := composeArgv(engine, "exec", "-T", "agent", "openclaw", "devices", "approve", requestID)
	if err := process.RunArgvIn(r, root, nil, approve...); err != nil {
		return fmt.Errorf("devices approve %s: %w", requestID, err)
	}
	return nil
}
