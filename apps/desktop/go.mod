// One module for every Go desktop shell — Linux today, Windows next — mirroring
// apps/macos being its own Swift package.
//
// One rather than one per platform because most of a shell is not
// platform-specific: the daemon client, the supervisor, settings, the session
// handshake and the terminal interface are the same code everywhere, and Go's
// internal rule would block a sibling module from importing them, so the
// alternative is duplicating them and watching them drift. The OS-specific part
// lives in internal/x11 and internal/win32 behind build tags.
//
// The point of it being separate from the root module at all is that the root's
// dependency list stays empty: starchd is the thing users run with an API key
// in memory, and "it depends on nothing" is a claim worth being able to make
// without qualification.
//
// The replace directive is what lets the shell share brand names, path rules
// and the catalog types with the daemon instead of restating them and drifting.
module github.com/srikary12/starch/apps/desktop

go 1.23

require (
	github.com/godbus/dbus/v5 v5.2.2
	github.com/srikary12/starch v0.0.0
)

require golang.org/x/sys v0.27.0

replace github.com/srikary12/starch => ../..
