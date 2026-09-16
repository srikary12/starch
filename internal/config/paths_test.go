package config

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/srikary12/starch/internal/brand"
)

// Every case below drives the goos explicitly, so the Linux layout is checked
// on whatever machine runs the suite rather than only on the Linux CI job.

func fakeConfigDir(dir string) func() (string, error) {
	return func() (string, error) { return dir, nil }
}

func env(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

func TestRuntimeDir(t *testing.T) {
	const configRoot = "/home/u/.config"

	tests := []struct {
		name string
		goos string
		env  map[string]string
		want string
	}{
		{
			name: "linux uses the runtime dir the session manager made",
			goos: "linux",
			env:  map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"},
			want: "/run/user/1000/starch",
		},
		{
			name: "linux falls back to the config dir when it is unset",
			goos: "linux",
			env:  nil,
			want: configRoot + "/starch",
		},
		{
			// A relative value would put the socket wherever the daemon was
			// started from, which is worse than the fallback.
			name: "linux refuses a relative runtime dir",
			goos: "linux",
			env:  map[string]string{"XDG_RUNTIME_DIR": "run/user/1000"},
			want: configRoot + "/starch",
		},
		{
			// XDG_RUNTIME_DIR is set inside a macOS terminal by some tools,
			// and following it there would move the socket for no reason.
			name: "darwin ignores the runtime dir entirely",
			goos: "darwin",
			env:  map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"},
			want: configRoot + "/" + brand.SupportDirName,
		},
		{
			// %AppData% roams. A socket synced to a file server and restored at
			// next logon is a stale rendezvous the daemon has to clear.
			name: "windows puts the socket in the local app data dir",
			goos: "windows",
			env:  map[string]string{"LOCALAPPDATA": `C:\Users\me\AppData\Local`},
			want: `C:\Users\me\AppData\Local` + "/" + brand.SupportDirName,
		},
		{
			name: "windows accepts a UNC local app data dir",
			goos: "windows",
			env:  map[string]string{"LOCALAPPDATA": `\\fileserver\profiles\me\Local`},
			want: `\\fileserver\profiles\me\Local` + "/" + brand.SupportDirName,
		},
		{
			name: "windows falls back to the config dir when it is unset",
			goos: "windows",
			env:  nil,
			want: configRoot + "/" + brand.SupportDirName,
		},
		{
			// "C:foo" is relative to that drive's current directory, not a root.
			name: "windows refuses a drive-relative local app data dir",
			goos: "windows",
			env:  map[string]string{"LOCALAPPDATA": `C:Users\me`},
			want: configRoot + "/" + brand.SupportDirName,
		},
		{
			// LOCALAPPDATA is sometimes exported inside a Unix shell by tooling
			// that emulates Windows, and following it there would be wrong.
			name: "linux ignores the local app data dir",
			goos: "linux",
			env:  map[string]string{"LOCALAPPDATA": `C:\Users\me\AppData\Local`},
			want: configRoot + "/starch",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := runtimeDir(tc.goos, env(tc.env), fakeConfigDir(configRoot))
			if err != nil {
				t.Fatalf("runtimeDir: %v", err)
			}
			if got != filepath.FromSlash(tc.want) {
				t.Errorf("runtimeDir = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSupportDirNameIsLowercaseOnLinuxOnly(t *testing.T) {
	if got := supportDirName("linux"); got != brand.Slug {
		t.Errorf("linux support dir = %q, want %q", got, brand.Slug)
	}
	for _, goos := range []string{"darwin", "windows"} {
		if got := supportDirName(goos); got != brand.SupportDirName {
			t.Errorf("%s support dir = %q, want %q", goos, got, brand.SupportDirName)
		}
	}
}

// Presets are the user's own work and must not land in a directory the session
// manager wipes at logout. This is the half of the split that is easy to
// regress by pointing both helpers at RuntimeDir.
func TestPresetsStayOutOfTheRuntimeDir(t *testing.T) {
	const configRoot = "/home/u/.config"
	pairs := map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}

	support, err := supportDir("linux", fakeConfigDir(configRoot))
	if err != nil {
		t.Fatalf("supportDir: %v", err)
	}
	run, err := runtimeDir("linux", env(pairs), fakeConfigDir(configRoot))
	if err != nil {
		t.Fatalf("runtimeDir: %v", err)
	}
	if support == run {
		t.Fatalf("presets and the socket both resolved to %q", support)
	}
	if want := filepath.FromSlash(configRoot + "/starch"); support != want {
		t.Errorf("supportDir = %q, want %q", support, want)
	}
}

func TestConfigDirFailureIsReported(t *testing.T) {
	boom := errors.New("no home")
	failing := func() (string, error) { return "", boom }

	if _, err := supportDir("linux", failing); !errors.Is(err, boom) {
		t.Errorf("supportDir error = %v, want it to wrap %v", err, boom)
	}
	if _, err := runtimeDir("linux", env(nil), failing); !errors.Is(err, boom) {
		t.Errorf("runtimeDir error = %v, want it to wrap %v", err, boom)
	}
}

func TestDefaultSocketPathLivesInTheRuntimeDir(t *testing.T) {
	dir, err := RuntimeDir()
	if err != nil {
		t.Fatalf("RuntimeDir: %v", err)
	}
	path, err := DefaultSocketPath()
	if err != nil {
		t.Fatalf("DefaultSocketPath: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("socket dir = %q, want %q", filepath.Dir(path), dir)
	}
	if filepath.Base(path) != brand.SocketName {
		t.Errorf("socket name = %q, want %q", filepath.Base(path), brand.SocketName)
	}
}

// filepath.IsAbs answers for the host, so it would call every Windows path
// relative on a Mac and make that branch untestable here. This is the
// replacement, and it is worth its own table because getting it wrong means
// the socket quietly lands somewhere relative to the working directory.
func TestIsAbs(t *testing.T) {
	tests := []struct {
		goos, path string
		want       bool
	}{
		{"windows", `C:\Users\me`, true},
		{"windows", `c:/Users/me`, true},
		{"windows", `\\fileserver\share`, true},
		{"windows", `C:Users\me`, false}, // relative to that drive's cwd
		{"windows", `Users\me`, false},
		{"windows", `C:`, false},
		{"windows", ``, false},
		{"windows", `/Users/me`, false}, // rooted on the current drive, not absolute
		{"linux", "/run/user/1000", true},
		{"linux", "run/user/1000", false},
		{"linux", "", false},
		{"linux", `C:\Users\me`, false},
		{"darwin", "/Users/me", true},
	}

	for _, tc := range tests {
		if got := isAbs(tc.goos, tc.path); got != tc.want {
			t.Errorf("isAbs(%q, %q) = %v, want %v", tc.goos, tc.path, got, tc.want)
		}
	}
}
