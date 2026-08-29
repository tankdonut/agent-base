package scaffold

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"text/template"

	"github.com/tankdonut/agent-base/internal/templates"
)

// templateData is the complete template surface: templates may use ONLY
// these fields. Keep in sync with the Config fields of the same names.
type templateData struct {
	ProjectName string
	AgentName   string
	BaseTag     string
	Model       string
	GatewayPort int
	Telegram    bool
}

// Run validates cfg, renders the embedded template tree into
// cfg.TargetDir, optionally runs git init, and returns the created paths
// relative to the target. A non-empty existing target is an error unless
// cfg.Force is set (then generated files are overwritten; unrelated
// files are left alone).
func Run(cfg Config) ([]string, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := checkTarget(cfg.TargetDir, cfg.Force); err != nil {
		return nil, err
	}
	data := templateData{
		ProjectName: cfg.ProjectName,
		AgentName:   cfg.AgentName,
		BaseTag:     cfg.BaseTag,
		Model:       cfg.Model,
		GatewayPort: cfg.GatewayPort,
		Telegram:    cfg.Telegram,
	}

	tmplFS := templates.FS()
	paths, err := templates.Paths()
	if err != nil {
		return nil, err
	}
	created := make([]string, 0, len(paths))
	for _, rel := range paths {
		if err := renderFile(tmplFS, cfg.TargetDir, rel, data); err != nil {
			return nil, fmt.Errorf("%w — partial scaffold left in %s: re-run with --force to overwrite generated files", err, cfg.TargetDir)
		}
		created = append(created, rel)
	}
	if cfg.GitInit {
		if err := gitInit(cfg.TargetDir); err != nil {
			return nil, err
		}
	}
	return created, nil
}

// checkTarget rejects a non-empty existing directory unless force is set.
func checkTarget(dir string, force bool) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	if !force {
		return fmt.Errorf("target %s is not empty — pass --force to overwrite existing files", dir)
	}
	return nil
}

// renderFile renders tmpl/<rel>.tmpl into <target>/<rel> with the
// mode from the templates table. Every non-empty file ends with a
// newline, keeping output deterministic.
func renderFile(fsys fs.FS, target, rel string, data templateData) error {
	src, err := fs.ReadFile(fsys, "tmpl/"+rel+".tmpl")
	if err != nil {
		return err
	}
	tmpl, err := template.New(rel).Parse(string(src))
	if err != nil {
		return fmt.Errorf("parse %s: %w", rel, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("render %s: %w", rel, err)
	}
	out := buf.Bytes()
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	dest := filepath.Join(target, filepath.FromSlash(rel))
	// --force must never write through a symlink planted at a manifest
	// path (WriteFile follows symlinks); refuse anything non-regular.
	if fi, err := os.Lstat(dest); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("%s exists and is not a regular file — remove it and re-run", dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(dest), err)
	}
	mode := templates.Mode(rel)
	if err := os.WriteFile(dest, out, mode); err != nil {
		return fmt.Errorf("writing %s: %w", dest, err)
	}
	// WriteFile applies mode only on create; chmod keeps modes
	// deterministic when --force overwrites existing files.
	if err := os.Chmod(dest, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", dest, err)
	}
	return nil
}

// gitInit runs `git init` inside dir.
func gitInit(dir string) error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("--git-init requires git in PATH")
	}
	cmd := exec.Command("git", "init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}
