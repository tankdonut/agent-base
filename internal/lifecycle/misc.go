package lifecycle

import (
	"fmt"
	"os"
)

// PreCommitCheck runs `pre-commit run --all-files` in the project.
func PreCommitCheck(r Runner) error {
	if r == nil {
		return errNilRunner
	}
	if _, err := lookPath(r, "pre-commit"); err != nil {
		return fmt.Errorf("pre-commit not found in PATH — install it (e.g. `pipx install pre-commit`) and re-run")
	}
	return runArgv(r, nil, "pre-commit", "run", "--all-files")
}

// PreCommitHooks installs pre-commit's git hooks in the project.
func PreCommitHooks(r Runner) error {
	if r == nil {
		return errNilRunner
	}
	if _, err := lookPath(r, "pre-commit"); err != nil {
		return fmt.Errorf("pre-commit not found in PATH — install it (e.g. `pipx install pre-commit`) and re-run")
	}
	return runArgv(r, nil, "pre-commit", "install")
}

// readFileIfExists returns file content or "" when the file is absent;
// other read errors are returned.
func readFileIfExists(path string) (string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}
