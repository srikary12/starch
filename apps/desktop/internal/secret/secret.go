// Package secret stores the API key in the operating system's secret store.
//
// This is the counterpart of the macOS Keychain, and the same rule applies
// everywhere: it is the only place a key is ever stored. It never goes into the
// settings file, never onto disk in the Go layer, and never into a log. The
// daemon receives it over the local socket at POST /v1/session and holds it in
// memory for the life of that process.
//
// Two implementations, one per platform, behind the same small surface — Open,
// Set, Get, Has, Delete, Close. The freedesktop Secret Service on Linux
// (secret_dbus.go) and Credential Manager on Windows (secret_windows.go). They
// differ more than the surface suggests, and where they do the difference is
// documented next to the method rather than smoothed over.
package secret

// Account is the key under which one provider's API key is stored.
//
// Per provider rather than one shared entry, so configuring a second provider
// does not throw away the first one's key — switching back should not mean
// finding the key again.
func Account(providerID string) string { return "api-key." + providerID }
