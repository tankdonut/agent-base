// Package scaffold implements `agentctl init`: rendering the embedded
// template tree into a fresh downstream agent repository.
package scaffold

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
)

// Init flag defaults.
const (
	DefaultBaseTag     = "2026.08.28"
	DefaultModel       = "zai/glm-5.2"
	DefaultGatewayPort = 18789
)

// baseTagRe pins agent-base date tags: YYYY.MM.DD with an optional
// same-day run suffix (.N).
var baseTagRe = regexp.MustCompile(`^\d{4}\.\d{2}\.\d{2}(\.\d+)?$`)

// Config is the complete `agentctl init` configuration. The first six
// fields form the template surface; the rest controls scaffolding
// behavior and is never visible to templates.
type Config struct {
	ProjectName string // target dir basename
	AgentName   string
	BaseTag     string
	Model       string
	GatewayPort int
	Telegram    bool

	TargetDir string // absolute path to the scaffold destination
	GitInit   bool   // run `git init` in the target after scaffolding
	Force     bool   // overwrite an existing non-empty target directory
}

// ParseFlags parses the args of the init subcommand (excluding "init"
// itself). Flag errors and wrong positional counts are usage errors.
func ParseFlags(args []string) (Config, error) {
	var cfg Config
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.AgentName, "agent-name", "", "agent name (default: title-cased dir basename)")
	fs.StringVar(&cfg.BaseTag, "base-tag", DefaultBaseTag, "agent-base image tag")
	fs.StringVar(&cfg.Model, "model", DefaultModel, "fallback and automations model")
	fs.IntVar(&cfg.GatewayPort, "gateway-port", DefaultGatewayPort, "host gateway port")
	fs.BoolVar(&cfg.Telegram, "telegram", true, "wire the telegram channel")
	fs.BoolVar(&cfg.GitInit, "git-init", false, "run git init in the target after scaffolding")
	fs.BoolVar(&cfg.Force, "force", false, "overwrite an existing non-empty target directory")
	// flag.Parse stops at the first positional, but the CLI contract is
	// `init <dir> [flags]`: keep parsing after each positional so flags
	// work on either side of the directory.
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	var positional []string
	for fs.NArg() > 0 {
		positional = append(positional, fs.Arg(0))
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return cfg, err
		}
	}
	if len(positional) != 1 {
		return cfg, fmt.Errorf("init takes exactly one target directory, got %d", len(positional))
	}
	dir := positional[0]
	abs, err := filepath.Abs(dir)
	if err != nil {
		return cfg, fmt.Errorf("resolving target dir: %w", err)
	}
	cfg.TargetDir = abs
	cfg.ProjectName = filepath.Base(abs)
	if cfg.AgentName == "" {
		cfg.AgentName = DefaultAgentName(cfg.ProjectName)
	}
	return cfg, nil
}

// DefaultAgentName title-cases a project directory name: "my-agent" and
// "my_agent" both become "My Agent".
func DefaultAgentName(project string) string {
	words := strings.FieldsFunc(project, func(r rune) bool {
		return r == '-' || r == '_' || r == ' ' || r == '.'
	})
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

// Validate enforces the config contract: names and model non-empty,
// BaseTag a YYYY.MM.DD[.N] date tag, GatewayPort a valid port.
func (c Config) Validate() error {
	if c.TargetDir == "" {
		return fmt.Errorf("target directory is empty")
	}
	if c.ProjectName == "" || c.ProjectName == "." || c.ProjectName == "/" {
		return fmt.Errorf("cannot derive a project name from the target directory")
	}
	if c.AgentName == "" {
		return fmt.Errorf("--agent-name is empty")
	}
	if !baseTagRe.MatchString(c.BaseTag) {
		return fmt.Errorf("--base-tag %q must match YYYY.MM.DD[.N]", c.BaseTag)
	}
	if c.Model == "" {
		return fmt.Errorf("--model is empty")
	}
	if c.GatewayPort < 1 || c.GatewayPort > 65535 {
		return fmt.Errorf("--gateway-port %d out of range 1-65535", c.GatewayPort)
	}
	return nil
}
