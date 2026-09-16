package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// ReadSecretFromTerminal prompts on the console and reads a line without
// echoing it.
//
// Same reasoning as the Linux implementation: the key never travels as a
// command-line argument, because argv is readable by other processes and a
// shell writes it to history. Piped input works too, for a provisioning
// script, and there is no console to silence in that case.
func ReadSecretFromTerminal(prompt string) (string, error) {
	restore, err := silence(windows.Handle(os.Stdin.Fd()))
	if err == nil {
		// Only prompt when there is a person to prompt. A piped key should not
		// put stray text on stdout.
		fmt.Fprint(os.Stderr, prompt)
		defer func() {
			_ = restore()
			fmt.Fprintln(os.Stderr)
		}()
	}

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("reading the key: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// silence turns off console echo, returning a function that puts it back.
//
// GetConsoleMode failing is how piped input is detected: a redirected stdin is
// a file handle, not a console, and has no mode to read. ENABLE_LINE_INPUT is
// deliberately left alone, so backspace still works while typing — clearing
// echo alone is the whole change.
func silence(handle windows.Handle) (func() error, error) {
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return nil, errors.New("stdin is not a console")
	}

	if err := windows.SetConsoleMode(handle, mode&^windows.ENABLE_ECHO_INPUT); err != nil {
		return nil, fmt.Errorf("silencing the console: %w", err)
	}
	return func() error { return windows.SetConsoleMode(handle, mode) }, nil
}
