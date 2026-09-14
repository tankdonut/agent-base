// drift.go runs the scaffold template-drift check: each contract-shaped
// file (compose.yml, the .env.example pair, litellm/*) re-rendered from
// the embedded scaffold with the project's recoverable values and
// byte-compared against the project tree.
package cli

import (
	"os"
	"path/filepath"

	"github.com/tankdonut/agent-base/internal/project"
	"github.com/tankdonut/agent-base/internal/scaffold"
)

// checkTemplateDrift adds one advisory check per contract file. Value
// recovery mirrors init's derivations: project name from the project
// root basename (root must be absolute — chdirProject guarantees it),
// agent name and the telegram channel from the spec, gateway port from
// .agentctl.yaml via the caller. Diffs and misses warn, never fail —
// operators legitimately customize contract files; drift before an
// upgrade deserves a look, not a gate. A non-default gateway port that
// was never recorded in .agentctl.yaml reads as drift, and the remedy
// names the config key.
func checkTemplateDrift(add addCheck, root string, info project.SpecInfo, gatewayPort int) {
	projectName := filepath.Base(root)
	data := scaffold.ContractData{
		ProjectName:    projectName,
		ComposeProject: scaffold.ComposeProject(projectName),
		AgentName:      info.AgentName,
		GatewayPort:    gatewayPort,
		Telegram:       info.HasTelegramChannel,
	}
	for _, rel := range scaffold.ContractFiles() {
		want, err := scaffold.RenderContractFile(rel, data)
		if err != nil {
			add("template/"+rel, StatusWarn, "scaffold render failed: %v", err)
			continue
		}
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		switch {
		case os.IsNotExist(err):
			add("template/"+rel, StatusWarn, "missing — restore it from a fresh `agentctl init --force` tree")
		case err != nil:
			add("template/"+rel, StatusWarn, "reading %s: %v", rel, err)
		case string(got) != string(want):
			add("template/"+rel, StatusWarn, "differs from the scaffolded shape — keep deliberate edits, but reconcile against a fresh init tree before an upgrade; a non-default gateway port belongs in fleet.yaml (agents.<name>.gateway_port)")
		default:
			add("template/"+rel, StatusOK, "matches the scaffolded shape")
		}
	}
}
