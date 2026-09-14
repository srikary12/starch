package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
		check   func(t *testing.T, c Config)
	}{
		{
			name: "token only, everything else defaulted",
			env:  map[string]string{EnvToken: "abc"},
			check: func(t *testing.T, c Config) {
				if c.Token != "abc" {
					t.Errorf("Token = %q", c.Token)
				}
				if c.IdleTimeout != DefaultIdleTimeout {
					t.Errorf("IdleTimeout = %s, want %s", c.IdleTimeout, DefaultIdleTimeout)
				}
				if c.Debug {
					t.Error("Debug = true, want false by default")
				}
				want, err := DefaultSocketPath()
				if err != nil {
					t.Fatalf("DefaultSocketPath: %v", err)
				}
				if c.SocketPath != want {
					t.Errorf("SocketPath = %q, want %q", c.SocketPath, want)
				}
			},
		},
		{
			name:    "missing token is fatal",
			env:     map[string]string{},
			wantErr: EnvToken,
		},
		{
			name: "socket override wins",
			env:  map[string]string{EnvToken: "abc", EnvSocket: "/tmp/custom.sock"},
			check: func(t *testing.T, c Config) {
				if c.SocketPath != "/tmp/custom.sock" {
					t.Errorf("SocketPath = %q", c.SocketPath)
				}
			},
		},
		{
			name: "idle timeout parsed",
			env:  map[string]string{EnvToken: "abc", EnvIdleTimeout: "90s"},
			check: func(t *testing.T, c Config) {
				if c.IdleTimeout != 90*time.Second {
					t.Errorf("IdleTimeout = %s, want 90s", c.IdleTimeout)
				}
			},
		},
		{
			name: "zero idle timeout disables idle exit",
			env:  map[string]string{EnvToken: "abc", EnvIdleTimeout: "0"},
			check: func(t *testing.T, c Config) {
				if c.IdleTimeout != 0 {
					t.Errorf("IdleTimeout = %s, want 0", c.IdleTimeout)
				}
			},
		},
		{
			name:    "unparseable idle timeout is fatal",
			env:     map[string]string{EnvToken: "abc", EnvIdleTimeout: "soon"},
			wantErr: EnvIdleTimeout,
		},
		{
			name:    "negative idle timeout is fatal",
			env:     map[string]string{EnvToken: "abc", EnvIdleTimeout: "-5s"},
			wantErr: "negative",
		},
		{
			name: "debug accepts bare words as well as bools",
			env:  map[string]string{EnvToken: "abc", EnvDebug: "yes"},
			check: func(t *testing.T, c Config) {
				if !c.Debug {
					t.Error("Debug = false, want true")
				}
			},
		},
		{
			name: "debug=0 is off",
			env:  map[string]string{EnvToken: "abc", EnvDebug: "0"},
			check: func(t *testing.T, c Config) {
				if c.Debug {
					t.Error("Debug = true, want false")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Clear every variable so the developer's own environment cannot
			// leak into the result.
			for _, k := range []string{EnvToken, EnvSocket, EnvIdleTimeout, EnvDebug} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			got, err := Load()
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Load succeeded, want an error mentioning %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tc.check(t, got)
		})
	}
}

func TestMissingTokenIsErrNoToken(t *testing.T) {
	for _, k := range []string{EnvToken, EnvSocket, EnvIdleTimeout, EnvDebug} {
		t.Setenv(k, "")
	}
	_, err := Load()
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("Load error = %v, want ErrNoToken", err)
	}
}
