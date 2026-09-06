package fly

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
)

//go:embed fly.toml.tmpl
var manifestTmpl embed.FS

// defaultRegion is Fly's primary US East region (Ashburn, VA) — used
// when --region is omitted at scaffold time.
const defaultRegion = "iad"

// ScaffoldConfig renders deploy/fly.toml from the embedded template.
// Region falls back to defaultRegion; app is required. It refuses to
// overwrite an existing manifest — the file is repo-owned
// configuration, not a generated artifact to clobber.
func ScaffoldConfig(root, app, region string) error {
	if app == "" {
		return fmt.Errorf("fly scaffold needs --app <name> (region defaults to %s — override with --region, see https://fly.io/docs/reference/regions/)", defaultRegion)
	}
	if region == "" {
		region = defaultRegion
	}
	path := filepath.Join(root, ConfigName)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists — edit it, or remove it to re-scaffold", ConfigName)
	}
	tmpl, err := template.ParseFS(manifestTmpl, "fly.toml.tmpl")
	if err != nil {
		return fmt.Errorf("parsing embedded fly.toml template: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating deploy/: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("creating %s: %w", ConfigName, err)
	}
	defer f.Close()
	if err := tmpl.ExecuteTemplate(f, "fly.toml.tmpl", map[string]string{"App": app, "Region": region}); err != nil {
		return fmt.Errorf("rendering %s: %w", ConfigName, err)
	}
	return nil
}
