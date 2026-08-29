package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindProjectRootWalksUp(t *testing.T) {
	root := writeProject(t, map[string]string{"agent/spec.json": "{}"})
	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, start := range []string{nested, filepath.Join(root, "a"), root} {
		got, err := FindProjectRoot(start)
		if err != nil {
			t.Fatalf("FindProjectRoot(%s): %v", start, err)
		}
		if got != root {
			t.Errorf("FindProjectRoot(%s) = %s, want %s", start, got, root)
		}
	}
}

func TestFindProjectRootNoMarker(t *testing.T) {
	empty := t.TempDir()
	_, err := FindProjectRoot(filepath.Join(empty, "sub"))
	if err == nil {
		t.Fatal("expected an error when no agent/spec.json exists upward")
	}
	if !strings.Contains(err.Error(), ProjectMarker) {
		t.Errorf("error does not name the missing marker: %v", err)
	}
}
