package project

import "os"

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
