// Package state decides whether an output file is stale.
//
// Each output has a sidecar "<output>.status" file holding the command
// line that produced it. An output is up to date only when it exists,
// its status file matches the command we would run now, and it is no
// older than its input.
package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Reasons returned by Needs. An empty reason means up to date.
const (
	NoStatus       = "status file is missing"
	CommandChanged = "command line doesn't match status file"
	OutputMissing  = "output file is missing"
	InputNewer     = "input file is newer than output file"
)

func statusFile(output string) string { return output + ".status" }

// Needs reports why output must be rebuilt from input using cmdline,
// or "" if it is up to date. It returns an error only when the input
// can't be examined, since nothing can be built from it then.
func Needs(input, output, cmdline string) (string, error) {
	status, err := os.ReadFile(statusFile(output))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return NoStatus, nil
	case err != nil:
		return "", fmt.Errorf("reading status for %q: %w", output, err)
	case string(status) != cmdline:
		return CommandChanged, nil
	}

	out, err := os.Stat(output)
	if err != nil {
		return OutputMissing, nil
	}
	in, err := os.Stat(input)
	if err != nil {
		return "", fmt.Errorf("examining input: %w", err)
	}
	if in.ModTime().After(out.ModTime()) {
		return InputNewer, nil
	}
	return "", nil
}

// Record marks output as built by cmdline.
func Record(output, cmdline string) error {
	if err := os.WriteFile(statusFile(output), []byte(cmdline), 0o644); err != nil {
		return fmt.Errorf("recording status for %q: %w", output, err)
	}
	return nil
}

// Clear forgets how output was built, so an interrupted rebuild is
// never mistaken for a finished one.
func Clear(output string) error {
	err := os.Remove(statusFile(output))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clearing status for %q: %w", output, err)
	}
	return nil
}
