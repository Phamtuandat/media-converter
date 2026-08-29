package processor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

var ErrOutputLimitExceeded = errors.New("converter output limit exceeded")

type ToolRunner struct{ Path string }

const maxToolOutputBytes = 1 << 20

type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	written := len(p)
	if b.buf.Len() < b.max {
		remaining := b.max - b.buf.Len()
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buf.Write(p)
	}
	return written, nil
}

func (b *cappedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *cappedBuffer) String() string { return b.buf.String() }

func (r ToolRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	return r.run(ctx, "", 0, args...)
}

func (r ToolRunner) RunWithOutputLimit(ctx context.Context, outputPath string, limit int64, args ...string) ([]byte, error) {
	return r.run(ctx, outputPath, limit, args...)
}

func (r ToolRunner) run(ctx context.Context, outputPath string, limit int64, args ...string) ([]byte, error) {
	cmd := exec.Command(r.Path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr cappedBuffer
	stdout.max, stderr.max = maxToolOutputBytes, maxToolOutputBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	result := make(chan error, 1)
	go func() { result <- cmd.Wait() }()
	var ticker *time.Ticker
	var limitExceeded chan struct{}
	monitorDone := make(chan struct{})
	var monitorWG sync.WaitGroup
	if outputPath != "" && limit > 0 {
		ticker = time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		limitExceeded = make(chan struct{}, 1)
		monitorWG.Add(1)
		go func() {
			defer monitorWG.Done()
			for {
				select {
				case <-monitorDone:
					return
				case <-ticker.C:
				}
				info, err := os.Stat(outputPath)
				if err == nil && info.Size() > limit {
					select {
					case limitExceeded <- struct{}{}:
					default:
					}
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					return
				}
			}
		}()
	}
	stopMonitor := func() {
		if ticker != nil {
			close(monitorDone)
			ticker.Stop()
			monitorWG.Wait()
		}
	}
	select {
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-result
		stopMonitor()
		return nil, ctx.Err()
	case err := <-result:
		stopMonitor()
		if limitExceeded != nil {
			select {
			case <-limitExceeded:
				return nil, ErrOutputLimitExceeded
			default:
			}
		}
		if outputPath != "" && limit > 0 {
			if info, statErr := os.Stat(outputPath); statErr == nil && info.Size() > limit {
				return nil, ErrOutputLimitExceeded
			}
		}
		if err == nil {
			return stdout.Bytes(), nil
		}
		message := strings.TrimSpace(stderr.String())
		if len(message) > 1000 {
			message = message[:1000]
		}
		return nil, fmt.Errorf("%w: %s", err, message)
	}
}

func WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, timeout)
}
