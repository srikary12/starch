// Package brand is the single source of truth for the product name.
//
// The name appears in exactly two places in the Go tree: the Name constant
// below, and the module path in go.mod. Everything else derives from Name, so
// renaming the product is a one-line change here plus a module-path rewrite.
package brand

import "strings"

// Name is the product name. Change this and the go.mod module path to rename.
const Name = "Starch"

// Slug is the lowercase form used in identifiers and file names.
var Slug = strings.ToLower(Name)

// Daemon is the name of the daemon binary.
var Daemon = Slug + "d"

// SocketName is the file name of the Unix domain socket inside SupportDirName.
var SocketName = Daemon + ".sock"

// SupportDirName is the per-user application support directory name.
var SupportDirName = Name

// EnvPrefix prefixes every environment variable the daemon reads.
var EnvPrefix = strings.ToUpper(Slug) + "_"

// BundleID is the macOS bundle identifier of the native shell.
//
// Unlike the rest of this file, changing BundleID has a user-visible cost:
// macOS ties Accessibility and Keychain grants to it, so an existing install
// silently loses both and has to re-authorise. Treat it as frozen after the
// first public release.
var BundleID = "dev." + Slug + "." + Name
