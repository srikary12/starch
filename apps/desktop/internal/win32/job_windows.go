package win32

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ConfineToShellLifetime ties a child process's lifetime to this process's.
//
// This exists because the daemon's usual backstops are both absent on Windows,
// and the daemon is the process holding an API key in memory:
//
//   - The shell terminates it explicitly on quit — but Process.Signal on
//     Windows implements Kill and nothing else (os/exec_windows.go), so there
//     is no graceful stop to send, only a hard one.
//   - starchd watches for its parent going away — but Windows does not
//     reparent orphans, so the recorded parent id never changes and that check
//     can never fire. cmd/starchd/watchdog.go says as much in its comment.
//
// That would leave only the 30-second idle timeout to reclaim a daemon whose
// shell crashed: thirty seconds with someone's key in memory, against about
// two on Linux and none at all on macOS.
//
// A job object closes it completely, and more firmly than either backstop it
// replaces. The kernel kills every process in the job when the last handle to
// it closes, and our handle closes when this process exits — however it exits,
// including a kill from Task Manager, which nothing running inside the shell
// could have caught.
func ConfineToShellLifetime(pid int) error {
	job, err := shellJob()
	if err != nil {
		return err
	}

	// PROCESS_SET_QUOTA is what assignment needs; PROCESS_TERMINATE is what
	// the kernel needs to be able to carry the kill out later.
	handle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid),
	)
	if err != nil {
		return fmt.Errorf("opening process %d: %w", pid, err)
	}
	defer windows.CloseHandle(handle)

	if err := windows.AssignProcessToJobObject(job, handle); err != nil {
		return fmt.Errorf("confining process %d: %w", pid, err)
	}
	return nil
}

var (
	jobOnce sync.Once
	jobLive windows.Handle
	jobErr  error
)

// shellJob returns the one job object this process owns, creating it once.
//
// One per process rather than one per spawn, because the guarantee wanted is
// "these die with the shell" and that is exactly what one job whose handle
// lives as long as the process expresses. The handle is deliberately never
// closed: closing it is the kill signal.
func shellJob() (windows.Handle, error) {
	jobOnce.Do(func() {
		// A nil name keeps the job unnamed, and nil attributes keep the handle
		// out of any child's handle table — which matters, because a child
		// holding a handle would keep the job alive after we died and defeat
		// the whole arrangement.
		handle, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			jobErr = fmt.Errorf("creating a job object: %w", err)
			return
		}

		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
			BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
				LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
			},
		}
		_, err = windows.SetInformationJobObject(
			handle,
			windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)),
			uint32(unsafe.Sizeof(info)),
		)
		if err != nil {
			windows.CloseHandle(handle)
			jobErr = fmt.Errorf("setting the job object to kill on close: %w", err)
			return
		}
		jobLive = handle
	})
	return jobLive, jobErr
}
