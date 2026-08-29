// Package templates embeds the agentctl scaffold file tree and exposes
// its shape: one "<output-path>.tmpl" file per generated file.
package templates

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed all:tmpl
var embedded embed.FS

// FS returns the embedded template tree. Paths are slash-separated and
// rooted at "tmpl".
func FS() fs.FS { return embedded }

// modes is the file-mode table for generated paths. Directories are
// always 0755; make.sh is the only executable file.
var modes = map[string]fs.FileMode{
	"make.sh": 0o755,
}

// defaultMode is the mode for every generated file not in the table.
const defaultMode = fs.FileMode(0o644)

// Mode returns the output file mode for a generated path.
func Mode(path string) fs.FileMode {
	if m, ok := modes[path]; ok {
		return m
	}
	return defaultMode
}

// Paths returns every generated output path (the tmpl tree minus the
// ".tmpl" suffix), sorted. It fails loudly on stray files so a template
// added without the suffix cannot silently ship.
func Paths() ([]string, error) {
	var out []string
	err := fs.WalkDir(embedded, "tmpl", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(p, "tmpl/")
		if !strings.HasSuffix(rel, ".tmpl") {
			return fmt.Errorf("templates: stray non-template file: %s", rel)
		}
		out = append(out, strings.TrimSuffix(rel, ".tmpl"))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}
