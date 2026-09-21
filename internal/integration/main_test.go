//go:build integration

package integration

import (
	"os"
	"testing"
)

// TestMain owns the live fixture's teardown: the stack must outlive
// every test that shares it, so the down runs after the whole suite,
// never inside a fixture boot (which deleted the stack the moment it
// came up — the original Tier B health failure).
func TestMain(m *testing.M) {
	code := m.Run()
	teardownLive()
	os.Exit(code)
}
