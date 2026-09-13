// manifest.go exposes the contract-file drift surface: the scaffold
// files whose scaffolded shape the deployment contract depends on
// (compose.yml, the .env.example pair, litellm/*), their
// embedded-template fingerprints, and a render path shared with Run so
// drift checks byte-compare against exactly what init writes.
package scaffold

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
)

// contractFiles lists the drift-scope output paths: files a downstream
// project should keep scaffold-shaped because agentctl and the base
// image rely on their structure (volumes and networks, env
// indirection, the litellm sidecar). spec.json and AGENTS.md are
// deliberately absent — operator-owned surfaces.
var contractFiles = []string{
	"agent/.env.example",
	"compose.yml",
	"litellm/.env.example",
	"litellm/config.yaml",
}

// ContractFiles returns the drift-scope output paths, sorted. A copy:
// callers cannot widen the scope through the return value.
func ContractFiles() []string {
	out := make([]string, len(contractFiles))
	copy(out, contractFiles)
	return out
}

// ContractData is the template surface the contract files render with:
// a subset of Config's template fields (BaseTag and Model reach only
// operator-owned files and stay out).
type ContractData struct {
	ProjectName    string
	ComposeProject string
	AgentName      string
	GatewayPort    int
	Telegram       bool
}

// Manifest returns sha256 fingerprints of the raw template sources for
// the contract files, keyed by output path. Derived from the embedded
// FS at call time — a template edit without the manifest following is
// impossible by construction.
func Manifest() (map[string]string, error) {
	out := make(map[string]string, len(contractFiles))
	for _, rel := range contractFiles {
		src, err := fs.ReadFile(FS(), "tmpl/"+rel+".tmpl")
		if err != nil {
			return nil, fmt.Errorf("reading template %s: %w", rel, err)
		}
		sum := sha256.Sum256(src)
		out[rel] = hex.EncodeToString(sum[:])
	}
	return out, nil
}

// RenderContractFile renders the contract template for output path rel
// with data via the same parse/execute/trailing-newline path as Run.
// Non-contract paths fail closed: the drift scope cannot creep by
// accident from a caller.
func RenderContractFile(rel string, data ContractData) ([]byte, error) {
	i := sort.SearchStrings(contractFiles, rel)
	if i >= len(contractFiles) || contractFiles[i] != rel {
		return nil, fmt.Errorf("%q is not a contract file", rel)
	}
	return renderBytes(FS(), rel, data)
}
