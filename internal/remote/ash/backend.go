// Package ash transports Elephant's JSON protocol using ASH's one-shot execv
// CLI. The request travels on stdin (ash execv HOST --stdin -- elephant
// receive), so no shell runs on either side and a request is bounded by ASH's
// input limit rather than a command line. No remote database is accessed.
package ash

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

const (
	// MaxInputSize is the most stdin ASH forwards to a remote command.
	MaxInputSize = 64 << 10
	// MaxResponseSize bounds remote output below ASH's own stdout limit.
	MaxResponseSize = 4 << 20
)

// Config selects an ASH host and optional local config and remote executable.
// ElephantPath is a literal executable name or path, never shell code.
type Config struct {
	Host         string `json:"host"`
	ConfigPath   string `json:"config_path,omitempty"`
	ElephantPath string `json:"elephant_path,omitempty"`
}

// ValidateConfig checks execution configuration without connecting to the host.
func ValidateConfig(c Config) error {
	if strings.TrimSpace(c.Host) == "" || len(c.Host) > 256 {
		return errors.New("ASH host must contain 1 to 256 bytes")
	}
	for _, value := range []string{c.Host, c.ConfigPath, c.ElephantPath} {
		if strings.ContainsRune(value, '\x00') || len(value) > 4096 {
			return errors.New("ASH configuration values must be NUL-free and at most 4096 bytes")
		}
	}
	return nil
}

// Executor is the local process boundary. Implementations must honor ctx and
// propagate stdout/stderr writer errors. It never receives a local shell command.
type Executor interface {
	Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error
}

// Backend executes a single request. Its zero value uses the ash binary on PATH.
// Retries belong to the caller, which must reuse the same message ID and payload.
type Backend struct {
	Executor Executor
	Binary   string
}

// Exchange returns the raw JSON emitted by remote Elephant. Protocol semantics
// (including a structured rejection) are interpreted by the application layer.
func (b Backend) Exchange(ctx context.Context, c Config, payload []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ValidateConfig(c); err != nil {
		return nil, err
	}
	if !json.Valid(payload) {
		return nil, errors.New("ASH request must be valid JSON")
	}
	if len(payload) > MaxInputSize {
		return nil, fmt.Errorf("ASH request is %d bytes; ASH forwards at most %d bytes of input", len(payload), MaxInputSize)
	}
	path := c.ElephantPath
	if path == "" {
		path = "elephant"
	}
	args := make([]string, 0, 8)
	if c.ConfigPath != "" {
		args = append(args, "--config", c.ConfigPath)
	}
	args = append(args, "execv", c.Host, "--stdin", "--", path, "receive")
	binary := b.Binary
	if binary == "" {
		binary = "ash"
	}
	runner := b.Executor
	if runner == nil {
		runner = processExecutor{}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	stdout := limitedBuffer{limit: MaxResponseSize}
	stderr := limitedBuffer{limit: 16 << 10}
	err := runner.Run(ctx, binary, args, bytes.NewReader(payload), &stdout, &stderr)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if stdout.exceeded || stderr.exceeded {
		return nil, errors.New("ASH response exceeded output limit")
	}
	if err != nil {
		return nil, fmt.Errorf("ASH execution failed: %w: %s", err, strings.TrimSpace(stderr.buffer.String()))
	}
	if !json.Valid(stdout.buffer.Bytes()) {
		return nil, errors.New("ASH returned invalid JSON from Elephant")
	}
	return stdout.buffer.Bytes(), nil
}

type processExecutor struct{}

func (processExecutor) Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.WaitDelay = time.Second
	return cmd.Run()
}

type limitedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		b.exceeded = true
		return 0, errors.New("output limit exceeded")
	}
	return b.buffer.Write(p)
}
