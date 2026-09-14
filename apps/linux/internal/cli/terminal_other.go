//go:build !linux

package cli

import "errors"

// ReadSecretFromTerminal exists on other platforms only so that the package
// builds and its tests run there. The shell itself is Linux-only; the tests
// supply their own reader, so nothing exercises this.
func ReadSecretFromTerminal(prompt string) (string, error) {
	return "", errors.New("reading a key from the terminal is implemented for Linux only")
}
