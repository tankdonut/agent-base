package lifecycle

import "fmt"

// ResolveEngine maps the configured engine preference to a concrete
// binary name. "auto" (or empty) prefers podman and falls back to
// docker; an explicit engine must be on PATH; anything else is a
// configuration error.
func ResolveEngine(pref string, r Runner) (string, error) {
	if r == nil {
		return "", errNilRunner
	}
	switch pref {
	case "", "auto":
		if _, err := lookPath(r, "podman"); err == nil {
			return "podman", nil
		}
		if _, err := lookPath(r, "docker"); err == nil {
			return "docker", nil
		}
		return "", fmt.Errorf("no container engine found — install podman or docker")
	case "podman", "docker":
		if _, err := lookPath(r, pref); err != nil {
			return "", fmt.Errorf("engine %q not found in PATH", pref)
		}
		return pref, nil
	default:
		return "", fmt.Errorf("invalid engine %q — want auto, podman, or docker", pref)
	}
}
