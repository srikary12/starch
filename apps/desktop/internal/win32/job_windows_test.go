package win32

import (
	"os"
	"os/exec"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TestJobHelperSleeps is the child process the test below confines. It does
// nothing unless re-executed with the marker set, so a normal run skips it.
func TestJobHelperSleeps(t *testing.T) {
	if os.Getenv("STARCH_JOB_HELPER") == "" {
		t.Skip("helper process, run only by TestConfineToShellLifetime")
	}
	time.Sleep(30 * time.Second)
}

// What cannot be checked here is the part that matters most — that the child
// dies when this process does — because proving it means killing the test
// runner. What can be checked is the arrangement that produces it: the child is
// in our job, and our job is set to kill on close. Together those are the
// guarantee; the manual test covers watching it actually happen.
func TestConfineToShellLifetime(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestJobHelperSleeps")
	cmd.Env = append(os.Environ(), "STARCH_JOB_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the helper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	if err := ConfineToShellLifetime(cmd.Process.Pid); err != nil {
		t.Fatalf("ConfineToShellLifetime: %v", err)
	}

	job, err := shellJob()
	if err != nil {
		t.Fatalf("shellJob: %v", err)
	}

	var list jobProcessIDList
	err = windows.QueryInformationJobObject(
		job,
		windows.JobObjectBasicProcessIdList,
		uintptr(unsafe.Pointer(&list)),
		uint32(unsafe.Sizeof(list)),
		nil,
	)
	if err != nil {
		t.Fatalf("QueryInformationJobObject: %v", err)
	}

	want := uintptr(cmd.Process.Pid)
	for i := range list.NumberOfProcessIdsInList {
		if list.ProcessIdList[i] == want {
			return
		}
	}
	t.Fatalf("the helper (%d) is not in the job; a crashed shell would strand the daemon holding a key", want)
}

// JOBOBJECT_BASIC_PROCESS_ID_LIST. Not in x/sys/windows, and only the test
// needs it — the shell never reads a job back, it only assigns to one. Room for
// far more entries than a job holding one daemon will ever have.
type jobProcessIDList struct {
	NumberOfAssignedProcesses uint32
	NumberOfProcessIdsInList  uint32
	ProcessIdList             [64]uintptr
}

// One job for the process, not one per spawn. The guarantee wanted is "these
// die with the shell", and that is what a single job whose handle lives as long
// as the process expresses.
func TestShellJobIsCreatedOnce(t *testing.T) {
	first, err := shellJob()
	if err != nil {
		t.Fatalf("shellJob: %v", err)
	}
	second, err := shellJob()
	if err != nil {
		t.Fatalf("shellJob, second call: %v", err)
	}
	if first != second {
		t.Errorf("got two job handles, %v and %v", first, second)
	}
}
