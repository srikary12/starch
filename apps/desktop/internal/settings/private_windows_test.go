package settings

import (
	"os"
	"testing"
)

// assertOwnerOnly cannot check a mode here, because there is not one to check.
//
// os.Stat on Windows synthesizes a mode out of FILE_ATTRIBUTE_READONLY: every
// file reads 0666 or 0444 regardless of who can open it. The 0600 passed to
// os.WriteFile is not ignored by our code — it is ignored by the platform, and
// os.Chmod there would return nil having changed nothing, which is why the
// daemon refuses to call it (internal/server/listener.go).
//
// What protects the file instead is the access list it inherits from
// %LocalAppData%, which is user-only on a stock profile. That is weaker than
// what the other two platforms state, and the difference is deliberate rather
// than overlooked: unlike the socket directory, this file holds no key, so it
// does not justify the explicit DACL that win32.EnsureOwnerOnlyDir sets. If it
// ever comes to hold one, this is the comment that should stop that landing
// quietly.
//
// So this asserts what is true and checkable — the file is there and is a
// regular file — rather than a mode that would pass without meaning anything.
func assertOwnerOnly(t *testing.T, path string) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("mode = %v, want a regular file", info.Mode())
	}
}
