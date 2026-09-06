package main

import (
	"context"
	"log/slog"
	"os"
	"time"
)

// parentPollInterval is how often the daemon checks whether it has been
// orphaned. Two seconds is far below the idle timeout, so in the common crash
// case this is what actually reclaims the process.
const parentPollInterval = 2 * time.Second

// withParentWatch returns a context that is cancelled when the spawning
// process goes away.
//
// On Unix an orphaned child is reparented, so a changed parent PID is the
// signal. This is a backstop, not the primary mechanism: the shell terminates
// the daemon explicitly on quit, and the idle timeout catches anything both of
// them miss. It is a no-op on platforms that do not reparent, which is fine —
// the idle timeout is the portable guarantee.
func withParentWatch(ctx context.Context, log *slog.Logger) context.Context {
	original := os.Getppid()
	// Already orphaned at startup means nobody owns us; let the idle timeout
	// handle it rather than exiting before the shell can connect.
	if original <= 1 {
		return ctx
	}

	ctx, cancel := context.WithCancel(ctx)
	go func() {
		defer cancel()
		t := time.NewTicker(parentPollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if os.Getppid() != original {
					log.Info("parent process exited, shutting down", "parent_pid", original)
					return
				}
			}
		}
	}()
	return ctx
}
