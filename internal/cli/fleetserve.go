// fleetserve.go hosts `fleet serve` (the loopback API server) and
// `fleet serve-init` (the systemd user unit). The serve path must
// never chdir the process — platform construction here is dir-scoped
// from the manifest, and execution flows through the Runner's
// RunIn/RunOutputIn.
package cli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tankdonut/agent-base/internal/api"
	"github.com/tankdonut/agent-base/internal/fleet"
	"github.com/tankdonut/agent-base/internal/platform"
	"github.com/tankdonut/agent-base/internal/project"
)

// platformForAgent resolves the constructed platform + deployment for
// one registered agent without touching the process cwd: the config
// comes from the manifest entry (+ env overrides), the deployment
// derives from the agent dir, and the registry builds the adapter.
func platformForAgent(m *fleet.Manifest, name string) (string, platform.Platform, platform.Deployment, error) {
	entry := m.Agents[name]
	cfg := Config{Platform: "compose", Namespaces: map[string]map[string]any{}}
	if entry.Platform != "" {
		cfg.Platform = entry.Platform
	}
	if e := m.Defaults.ComposeEngine; e != "" {
		cfg.Namespaces["compose"] = map[string]any{"engine": e}
	}
	if err := applyConfigEnv(&cfg); err != nil {
		return "", nil, platform.Deployment{}, err
	}
	p, err := forPlatform(cfg.Platform, newRunner(), cfg.Namespaces)
	if err != nil {
		return "", nil, platform.Deployment{}, err
	}
	d, err := platform.Derive(entry.Dir)
	if err != nil {
		return "", nil, platform.Deployment{}, err
	}
	return entry.Dir, p, d, nil
}

// serveTokenPath pins the bearer token outside the repo (XDG state;
// a repo-side token file would expand .gitignore and risk commits).
// The fleet hash namespaces multiple fleets on one host.
func serveTokenPath(fleetRoot string) (string, error) {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		state = filepath.Join(home, ".local", "state")
	}
	hash := fleetRoot
	if sum := sha256.Sum256([]byte(fleetRoot)); len(sum) >= 6 {
		hash = hex.EncodeToString(sum[:6])
	}
	dir := filepath.Join(state, "agentctl", hash)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "serve-token"), nil
}

// ensureServeToken reads the stored bearer or mints one.
func ensureServeToken(fleetRoot string) (string, error) {
	path, err := serveTokenPath(fleetRoot)
	if err != nil {
		return "", err
	}
	if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) != "" {
		return strings.TrimSpace(string(data)), nil
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

func newFleetServeCmd() *cobra.Command {
	var port int
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the loopback fleet API (bearer auth, 202 deploy jobs, SSE events)",
		Long: `Serves the fleet control-plane API on 127.0.0.1. The bearer token
lives under the XDG state directory (~/.local/state/agentctl/<fleet>/serve-token)
and is minted on first run — print it with the path this command reports.
Remote access goes through a local reverse proxy with TLS, never a
wider bind.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := loadFleet()
			if err != nil {
				return err
			}
			token, err := ensureServeToken(m.Root)
			if err != nil {
				return err
			}
			path, _ := serveTokenPath(m.Root)
			out := cmd.OutOrStdout()
			deps := api.Deps{
				Manifest:  m,
				NewRunner: newRunner,
				PlatformFor: func(agent string) (string, platform.Platform, platform.Deployment, error) {
					return platformForAgent(m, agent)
				},
				Version:         Version,
				Token:           token,
				UpgradesPreview: func(agent, target string) (any, error) { return upgradesPreview(m, agent, target) },
			}
			srv := api.New(deps)
			fmt.Fprintf(out, "serving fleet %s on http://127.0.0.1:%d\n", m.Root, port)
			fmt.Fprintf(out, "bearer token: %s\n", path)
			return srv.ListenAndServe(cmd.Context(), fmt.Sprintf("127.0.0.1:%d", port))
		},
	}
	cmd.Flags().IntVar(&port, "port", 8787, "loopback port to bind")
	return cmd
}

func newFleetServeInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve-init",
		Short: "Write a systemd user unit for `fleet serve` in this fleet",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := loadFleet()
			if err != nil {
				return err
			}
			exe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("resolving the agentctl binary: %w", err)
			}
			out := cmd.OutOrStdout()
			unit := fmt.Sprintf(`# agentctl fleet API for %s
[Unit]
Description=agentctl fleet serve (%s)
After=network-online.target

[Service]
ExecStart=%s fleet serve
WorkingDirectory=%s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, m.Root, filepath.Base(m.Root), exe, m.Root)
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			unitDir := filepath.Join(home, ".config", "systemd", "user")
			if err := os.MkdirAll(unitDir, 0o755); err != nil {
				return err
			}
			unitPath := filepath.Join(unitDir, "agentctl-serve.service")
			if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(out, "wrote   %s\n", unitPath)
			fmt.Fprintf(out, "next:   systemctl --user daemon-reload && systemctl --user enable --now agentctl-serve\n")
			fmt.Fprintf(out, "token:  %s\n", mustServeTokenPath(m.Root))
			return nil
		},
	}
}

func mustServeTokenPath(fleetRoot string) string {
	path, err := serveTokenPath(fleetRoot)
	if err != nil {
		return "<token path unavailable>"
	}
	return path
}

// upgradesPreview is the dir-scoped per-agent upgrade preview the API
// serves: the current Dockerfile pin, the target, downgrade detection,
// and the era crossings a boot on the target would cross (the same
// era table doctor --target walks).
func upgradesPreview(m *fleet.Manifest, agent, target string) (any, error) {
	entry, ok := m.Agents[agent]
	if !ok {
		return nil, fmt.Errorf("agent %q is not registered in %s", agent, fleet.ManifestName)
	}
	d, err := platform.Derive(entry.Dir)
	if err != nil {
		return nil, err
	}
	info, err := project.ReadSpec(filepath.Join(entry.Dir, "spec.json"))
	if err != nil {
		return nil, fmt.Errorf("reading spec: %w", err)
	}
	preview := struct {
		Agent      string   `json:"agent"`
		CurrentTag string   `json:"current_tag"`
		Target     string   `json:"target"`
		Downgrade  bool     `json:"downgrade"`
		Crossings  []eraRow `json:"crossings"`
	}{
		Agent:      agent,
		CurrentTag: d.BaseTag,
		Target:     target,
		Crossings:  []eraRow{},
	}
	if !tagAfter(target, d.BaseTag) && target != d.BaseTag {
		preview.Downgrade = true
		return preview, nil
	}
	for _, e := range eraCrossings(d.BaseTag, target, &info) {
		preview.Crossings = append(preview.Crossings, eraRow{
			ID: e.ID, Release: e.Release, Severity: string(e.Severity), Summary: e.Summary, Action: e.Action,
		})
	}
	return preview, nil
}

// eraRow is the JSON projection of one Era.
type eraRow struct {
	ID       string `json:"id"`
	Release  string `json:"release"`
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
	Action   string `json:"action"`
}
