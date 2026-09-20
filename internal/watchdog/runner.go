package watchdog

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner executes external commands (nmcli, systemctl).
//
// Everything that touches the system goes through this interface so the
// recovery ladder can be tested without a NetworkManager or a Wi-Fi card.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout string, err error)
}

// ExecRunner runs real commands.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s",
			name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// RecordingRunner records commands and returns scripted output. Test-only, but
// it lives here so it can be shared across the package's tests.
type RecordingRunner struct {
	Calls   []string
	Outputs map[string]string
	Errors  map[string]error
	// FailOn makes any call whose key contains this substring fail.
	FailOn string
}

func NewRecordingRunner() *RecordingRunner {
	return &RecordingRunner{Outputs: map[string]string{}, Errors: map[string]error{}}
}

func (r *RecordingRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	key := name + " " + strings.Join(args, " ")
	r.Calls = append(r.Calls, key)
	if r.FailOn != "" && strings.Contains(key, r.FailOn) {
		return "", fmt.Errorf("injected failure: %s", key)
	}
	if err, ok := r.Errors[key]; ok {
		return r.Outputs[key], err
	}
	return r.Outputs[key], nil
}

// CallLog joins recorded calls for assertions.
func (r *RecordingRunner) CallLog() string { return strings.Join(r.Calls, "\n") }
