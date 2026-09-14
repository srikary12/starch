package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// ReadSecretFromTerminal prompts on the terminal and reads a line without
// echoing it.
//
// The key never travels as a command-line argument: argv is readable by every
// process on the machine through ps, and an interactive shell writes it to
// history as well. Reading it here is the only route that avoids both.
//
// Piped input is accepted too, so `starch config --key < keyfile` works in a
// provisioning script. In that case there is no terminal to silence and none
// is needed.
func ReadSecretFromTerminal(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())

	restore, err := silence(fd)
	if err == nil {
		// Only prompt when there is a person to prompt. A piped key should
		// not put stray text on stdout.
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

// silence turns off terminal echo, returning a function that puts it back. It
// fails when stdin is not a terminal, which is how piped input is detected.
func silence(fd int) (func() error, error) {
	original, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, errors.New("stdin is not a terminal")
	}

	quiet := *original
	quiet.Lflag &^= unix.ECHO
	// Keep ECHONL so the newline the user types still moves the cursor down.
	quiet.Lflag |= unix.ECHONL
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &quiet); err != nil {
		return nil, fmt.Errorf("silencing the terminal: %w", err)
	}

	return func() error { return unix.IoctlSetTermios(fd, unix.TCSETS, original) }, nil
}
