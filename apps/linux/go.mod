// The Linux shell is its own module, mirroring apps/macos being its own Swift
// package. The point is that the root module's dependency list stays empty:
// starchd is the thing users run with an API key in memory, and "it depends on
// nothing" is a claim worth being able to make without qualification.
//
// The replace directive is what lets the shell share brand names, path rules
// and the catalog types with the daemon instead of restating them and drifting.
module github.com/srikary12/starch/apps/linux

go 1.23

require (
	github.com/godbus/dbus/v5 v5.2.2
	github.com/srikary12/starch v0.0.0
)

require golang.org/x/sys v0.27.0 // indirect

replace github.com/srikary12/starch => ../..
