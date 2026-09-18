//go:build !windows

package main

import (
	"context"
	"errors"
)

// runDesktop is not built here yet.
//
// The Linux half is the portal work — GlobalShortcuts for the shortcut, and one
// RemoteDesktop session for both the clipboard and the synthetic keystrokes.
// Until that lands, `starch rewrite` is the whole loop without the desktop, and
// saying so is better than offering a command that does nothing.
func runDesktop(context.Context) error {
	return errors.New("the desktop shell is not built for this platform yet; " +
		"`starch rewrite` is the whole loop without it")
}
