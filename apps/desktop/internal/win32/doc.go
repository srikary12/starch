// Package win32 is the Windows half of the shell.
//
// Everything here is a plain Win32 call reached through lazy DLL loading in
// golang.org/x/sys/windows, so the shell keeps CGO_ENABLED=0 and cross-compiles
// the way the daemon does. There is no C toolchain in this build, on any
// platform.
//
// Every file is built for Windows only. This one is not, so that the package
// still exists on macOS and Linux and `go build ./...` there does not trip over
// a directory with no buildable files.
package win32
