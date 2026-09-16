package terminal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

var ErrOutputLimit = errors.New("native process output exceeded its byte limit")

// DirectOptions is a bounded, finite-stdin invocation. No field is shell code.
type DirectOptions struct {
	Executable     string
	Args           []string
	WorkDir        string
	Stdin          []byte
	Env            []string
	Timeout        time.Duration
	MaxOutputBytes int
}

type boundedWriter struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
	cancel   context.CancelFunc
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if len(p) > w.limit-w.buffer.Len() {
		w.overflow = true
		w.cancel()
		return 0, ErrOutputLimit
	}
	return w.buffer.Write(p)
}

// ExecDirect reuses process-group handling but never passes data through a shell.
func ExecDirect(ctx context.Context, opts DirectOptions) (Result, error) {
	if opts.Executable == "" || opts.MaxOutputBytes < 1 || opts.Timeout <= 0 {
		return Result{}, fmt.Errorf("bounded direct process options are required")
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	cmd := exec.Command(opts.Executable, opts.Args...)
	cmd.Dir, cmd.Env = opts.WorkDir, opts.Env
	cmd.Stdin = bytes.NewReader(opts.Stdin)
	stdout := &boundedWriter{limit: opts.MaxOutputBytes, cancel: cancel}
	stderr := &boundedWriter{limit: opts.MaxOutputBytes, cancel: cancel}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	started := time.Now()
	group, err := startManagedProcess(cmd)
	if err != nil {
		return Result{ExitCode: -1, DurationMs: time.Since(started).Milliseconds()}, err
	}
	waiting := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = group.kill()
		case <-waiting:
		}
	}()
	err = cmd.Wait()
	close(waiting)
	// A finite native operation must not leave an orphan doing work after its
	// structured response. This also closes the parent-exits cancellation gap.
	_ = group.kill()
	groupErr := group.wait()
	result := Result{Stdout: stdout.buffer.String(), Stderr: stderr.buffer.String(), DurationMs: time.Since(started).Milliseconds()}
	if stdout.overflow || stderr.overflow {
		result.ExitCode = -1
		return result, ErrOutputLimit
	}
	if ctx.Err() != nil {
		result.ExitCode = -1
		return result, ctx.Err()
	}
	if groupErr != nil {
		result.ExitCode = -1
		return result, groupErr
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			result.ExitCode = exit.ExitCode()
			return result, nil
		}
		result.ExitCode = -1
		return result, err
	}
	return result, nil
}

// FilterEnvironment forwards only explicitly named variables, never all server secrets.
func FilterEnvironment(environ []string, names []string) []string {
	allowed := map[string]bool{}
	for _, name := range names {
		if name != "" && !strings.ContainsAny(name, "=\x00") {
			allowed[name] = true
		}
	}
	result := []string{}
	for _, entry := range environ {
		name, _, ok := strings.Cut(entry, "=")
		if ok && allowed[name] {
			result = append(result, entry)
		}
	}
	return result
}
