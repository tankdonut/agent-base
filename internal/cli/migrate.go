package cli

import (
	"context"

	"github.com/tankdonut/agent-base/internal/platform"
	"github.com/tankdonut/agent-base/internal/process"
	"github.com/tankdonut/agent-base/internal/project"
)

// addCheck is doctor's collect closure — passed to helpers so check
// order stays with the caller.
type addCheck func(name string, status CheckStatus, format string, a ...any)

// addTargetInstanceExplainers appends the migration explainers that need
// a reachable instance, under --target: the legacy-shape probes (warn
// only — each present shape names the one-time migration the next boot
// performs) and the LiteLLM migration preconditions for a non-litellm
// provider on a warm volume (the plain doctor advisory, expanded into a
// per-precondition checklist at upgrade-planning time).
func addTargetInstanceExplainers(add addCheck, plat platform.Platform, r process.Runner, root string, deploy *platform.Deployment, info project.SpecInfo, specOK, warm bool) {
	ctx := context.Background()
	if _, err := plat.Probe(ctx, r, root, deploy, "test -d "+dataDir+"/journal"); err == nil {
		add("migration/journal", StatusWarn, "legacy {data}/journal present — the wrapper's next boot moves it into workspace/journal and drops {data}/docs (docs/standard-agent.md#migrations)")
	}
	if _, err := plat.Probe(ctx, r, root, deploy, "test -f "+dataDir+"/workspace/journal/current-state.json"); err == nil {
		add("migration/state-file", StatusWarn, "legacy state file workspace/journal/current-state.json — the wrapper's next boot renames it to tent-state.json (docs/standard-agent.md#migrations)")
	}
	if warm && specOK && info.AuthChoice != "litellm-api-key" {
		add("litellm-migration/export", StatusWarn, "before migrating: agentctl backup, then copy the archive off the volume — persona content re-seeds from the image, agent state does not (docs/standard-agent.md#migrating-an-existing-agent-to-litellm)")
		add("litellm-migration/fresh-volume", StatusWarn, "auth flips never re-run setup on a warm volume — the blessed path is agentctl destroy --volumes, secrets init, secrets check, deploy (docs/standard-agent.md#migrating-an-existing-agent-to-litellm)")
		add("litellm-migration/shape", StatusWarn, "adopt the litellm shape — litellm/ tree, the compose sidecar + model-net, spec auth_choice litellm-api-key, litellm/<alias> model refs (docs/standard-agent.md#migrating-an-existing-agent-to-litellm)")
	}
}
