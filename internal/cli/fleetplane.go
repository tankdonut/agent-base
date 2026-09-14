// fleetplane.go hosts the plane lifecycle verbs (fleet plane
// up/down/status/logs — compose lifecycle against the rendered plane
// stack), `fleet render` (materialize every derived artifact without
// touching an engine), and `fleet key <agent>` (mint a per-agent
// LiteLLM virtual key through the running proxy and land it in the
// agent's .env — the key value is never printed).
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tankdonut/agent-base/internal/compose"
	"github.com/tankdonut/agent-base/internal/fleet"
	"github.com/tankdonut/agent-base/internal/process"
)

func newFleetPlaneCmd() *cobra.Command {
	plane := &cobra.Command{
		Use:   "plane",
		Short: "Shared-services plane lifecycle (render, up, down, status, logs)",
	}
	plane.AddCommand(newFleetPlaneUpCmd(), newFleetPlaneDownCmd(),
		newFleetPlaneStatusCmd(), newFleetPlaneLogsCmd())
	return plane
}

// runPlane resolves the manifest, materializes the plane stack, and
// runs fn with cwd pinned to the plane directory (relative compose
// argv) and the resolved engine. Teardown verbs pass requireEnv=false:
// a missing plane/.env must not block taking a stack down.
func runPlane(cmd *cobra.Command, requireEnv bool, fn func(engine string) error) error {
	m, err := loadFleet()
	if err != nil {
		return err
	}
	if !m.Plane.Enabled {
		return fmt.Errorf("plane is disabled in %s — enable it under `plane:` first", fleet.ManifestName)
	}
	if _, err := fleet.MaterializePlane(m); err != nil {
		if requireEnv {
			return err
		}
		fmt.Fprintln(cmd.ErrOrStderr(), "note: "+err.Error())
	}
	engine, err := process.ResolveEngine(m.Defaults.ComposeEngine, newRunner())
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := os.Chdir(filepath.Join(m.Root, fleet.PlaneDir)); err != nil {
		return err
	}
	defer func() { _ = os.Chdir(cwd) }()
	return fn(engine)
}

func newFleetPlaneUpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Render + converge the plane stack (postgres+litellm, observability)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlane(cmd, true, func(engine string) error {
				r := newRunner()
				out := cmdOut{cmd.OutOrStdout()}
				out.Printf("bringing up plane stack (engine: %s)\n", engine)
				return compose.Up(r, engine, ".")
			})
		},
	}
}

func newFleetPlaneDownCmd() *cobra.Command {
	var volumes bool
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Remove the plane stack (named volumes kept unless --volumes)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlane(cmd, false, func(engine string) error {
				down := composeArgv(engine, "down")
				if volumes {
					down = append(down, "--volumes")
				}
				return process.RunArgv(newRunner(), nil, down...)
			})
		},
	}
	cmd.Flags().BoolVar(&volumes, "volumes", false, "also delete the plane's named volumes (litellm DB, metrics, logs)")
	return cmd
}

func newFleetPlaneStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Plane stack state (compose ps)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlane(cmd, false, func(engine string) error {
				return compose.Ps(newRunner(), engine, ".")
			})
		},
	}
}

func newFleetPlaneLogsCmd() *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Plane stack logs (-f to follow)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlane(cmd, false, func(engine string) error {
				logArgs := []string{}
				if follow {
					logArgs = append(logArgs, "-f")
				}
				return compose.Logs(newRunner(), engine, ".", logArgs)
			})
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep the log stream open")
	return cmd
}

// composeArgv mirrors the compose package's argv vocabulary for the
// one plane call (down --volumes) the package does not wrap.
func composeArgv(engine string, verb ...string) []string {
	return append([]string{engine, "compose", "-f", "compose.yml"}, verb...)
}

func newFleetRenderCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "render",
		Short: "Materialize every derived artifact (agent envelopes + plane) — no engine",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := loadFleet()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, name := range m.AgentNames() {
				path, err := fleet.MaterializeAgentCompose(m, name)
				if err != nil {
					if strings.Contains(err.Error(), fleet.ErrAuthoredCompose.Error()) {
						fmt.Fprintf(out, "skip    %s (authored compose_file)\n", name)
						continue
					}
					return err
				}
				fmt.Fprintln(out, "wrote   "+path)
			}
			written, err := fleet.MaterializePlane(m)
			if err != nil {
				return err
			}
			for _, rel := range written {
				fmt.Fprintln(out, "wrote   "+rel)
			}
			fmt.Fprintln(out, "render complete — review with git status / fleet check")
			return nil
		},
	}
}

// litellmKeyURL and the mint payload keep `fleet key` thin: one POST,
// one .env line. The master key authenticates but is never printed;
// the minted key lands only in agents/<name>/.env.
type keyMintRequest struct {
	KeyAlias string            `json:"key_alias"`
	Metadata map[string]string `json:"metadata"`
}

type keyMintResponse struct {
	Key     string `json:"key"`
	TokenID string `json:"token_id"`
}

func newFleetKeyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "key <agent>",
		Short: "Mint a per-agent LiteLLM virtual key via the running plane proxy",
		Long: `Calls the plane's LiteLLM /key/generate with the master key
from plane/.env and writes the minted key into agents/<agent>/.env as
LITELLM_API_KEY (replacing any previous line). Neither key value is
ever printed. Requires the plane up (fleet plane up).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFleetKey(cmd, args[0])
		},
	}
}

func runFleetKey(cmd *cobra.Command, name string) error {
	m, err := loadFleet()
	if err != nil {
		return err
	}
	if m.Plane.LiteLLM != fleet.LiteLLMShared || !m.Plane.Enabled {
		return fmt.Errorf("plane.litellm must be %q to mint virtual keys", fleet.LiteLLMShared)
	}
	entry, ok := m.Agents[name]
	if !ok {
		return fmt.Errorf("agent %q is not registered in %s", name, fleet.ManifestName)
	}
	envMap, err := readPlaneEnv(m)
	if err != nil {
		return err
	}
	master := envMap["LITELLM_MASTER_KEY"]
	if master == "" || strings.Contains(master, "GENERATE_ME") {
		return fmt.Errorf("plane/.env carries no real LITELLM_MASTER_KEY — fill it first (fleet plane up needs it anyway)")
	}
	port := envMap["PLANE_LITELLM_PORT"]
	if port == "" {
		port = "4000"
	}
	body, err := json.Marshal(keyMintRequest{
		KeyAlias: name,
		Metadata: map[string]string{"managed_by": "agentctl", "fleet": m.Plane.Name},
	})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("http://127.0.0.1:%s/key/generate", port)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+master)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("plane proxy unreachable at %s — run `fleet plane up` first: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("key generation failed (HTTP %d) — is the plane's postgres healthy? `fleet plane status`", resp.StatusCode)
	}
	var minted keyMintResponse
	if err := json.Unmarshal(data, &minted); err != nil || minted.Key == "" {
		return fmt.Errorf("proxy response carried no key (HTTP %d)", resp.StatusCode)
	}
	if err := writeAgentLitellmKey(entry.Dir, minted.Key); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "key minted for %s (alias %q", name, name)
	if minted.TokenID != "" {
		fmt.Fprintf(out, ", token %s", minted.TokenID)
	}
	fmt.Fprintf(out, ") → %s LITELLM_API_KEY (value not shown)\n", filepath.Join(fleet.AgentsDir, name, ".env"))
	return nil
}

// readPlaneEnv parses plane/.env (KEY=VALUE lines, comments skipped)
// for the two values the CLI needs; the map never leaves this file's
// call sites and is never printed.
func readPlaneEnv(m *fleet.Manifest) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(m.Root, fleet.PlaneDir, ".env"))
	if err != nil {
		return nil, fmt.Errorf("plane/.env missing — copy plane/.env.example and fill it: %w", err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			env[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return env, nil
}

// writeAgentLitellmKey sets LITELLM_API_KEY in the agent's .env —
// replacing an existing line in place (order preserved) or appending
// one. The file stays 0o600 when it exists; new files match the
// secrets convention.
func writeAgentLitellmKey(agentDir, key string) error {
	path := filepath.Join(agentDir, ".env")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	lines := strings.Split(string(data), "\n")
	wrote := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "LITELLM_API_KEY=") {
			lines[i] = "LITELLM_API_KEY=" + key
			wrote = true
			break
		}
	}
	if !wrote {
		trimmed := strings.TrimRight(string(data), "\n")
		if trimmed != "" {
			lines = append(strings.Split(trimmed, "\n"), "LITELLM_API_KEY="+key)
		} else {
			lines = []string{"LITELLM_API_KEY=" + key}
		}
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), mode)
}
