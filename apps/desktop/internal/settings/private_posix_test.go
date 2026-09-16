//go:build !windows

package settings

import (
	"os"
	"testing"
)

// assertOwnerOnly checks that the settings file is readable only by the user
// who owns it.
//
// The file holds no API key — that lives in the OS secret store on all three
// platforms — but it does record the endpoint and model the user chose, which
// is nobody else's business on a shared machine.
func assertOwnerOnly(t *testing.T, path string) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 0600", perm)
	}
}
