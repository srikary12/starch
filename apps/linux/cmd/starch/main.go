// Command starch is the Linux shell: hot key, text capture, overlay and
// replacement, with starchd doing everything that is not OS-specific.
//
// It is the counterpart of the macOS app in apps/macos, and it speaks the same
// wire contract in api/README.md. Nothing here is a second daemon — the one in
// cmd/starchd is spawned as a child and owns every provider call.
package main

import (
	"fmt"
	"os"

	"github.com/srikary12/starch/internal/brand"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Printf("%s %s\n", brand.Slug, version)
		return
	}
	fmt.Fprintf(os.Stderr, "%s: nothing to run yet\n", brand.Slug)
	os.Exit(1)
}
