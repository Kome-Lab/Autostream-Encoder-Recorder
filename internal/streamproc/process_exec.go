package streamproc

import (
	"context"
	"errors"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

type Starter interface {
	Start(ctx context.Context, bin string, args []string) (RunningProcess, error)
}

type RunningProcess interface {
	PID() int
	Wait() error
	Terminate() error
	Kill() error
}

type ExecStarter struct{}

const maxFFmpegStderrBytes = 8 << 10

// boundedStderr keeps only the tail of FFmpeg stderr.  FFmpeg reports the
// useful failure near the end of its diagnostic output, while an unbounded
// pipe would allow a failed process to grow the Encoder's memory usage.
type boundedStderr struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
}

func (b *boundedStderr) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if len(p) >= maxFFmpegStderrBytes {
		b.data = append(b.data[:0], p[len(p)-maxFFmpegStderrBytes:]...)
		b.truncated = true
		return len(p), nil
	}
	b.data = append(b.data, p...)
	if len(b.data) > maxFFmpegStderrBytes {
		drop := len(b.data) - maxFFmpegStderrBytes
		b.data = append([]byte(nil), b.data[drop:]...)
		b.truncated = true
	}
	return len(p), nil
}

func (b *boundedStderr) String() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	text := strings.TrimSpace(string(b.data))
	if text == "" {
		return ""
	}
	if b.truncated {
		return "[truncated] " + text
	}
	return text
}

func (ExecStarter) Start(ctx context.Context, bin string, args []string) (RunningProcess, error) {
	if strings.TrimSpace(bin) == "" {
		return nil, errors.New("ffmpeg binary is required")
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	stderr := &boundedStderr{}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProcess{cmd: cmd, stdin: stdin, stderr: stderr}, nil
}

type execProcess struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stderr  *boundedStderr
	stdinMu sync.Mutex
}

type runtimeCommander interface {
	Command(target, command, argument string) error
}

func (p *execProcess) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *execProcess) Wait() error {
	return p.cmd.Wait()
}

func (p *execProcess) Stderr() string {
	if p == nil {
		return ""
	}
	return p.stderr.String()
}

func (p *execProcess) Command(target, command, argument string) error {
	if target != "volume@gain" || command != "volume" {
		return errors.New("unsupported ffmpeg runtime command")
	}
	value, err := strconv.ParseFloat(strings.TrimSuffix(argument, "dB"), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < -60 || value > 24 {
		return ErrInvalidRuntimeSettings
	}
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	if p.stdin == nil {
		return errors.New("ffmpeg runtime command input is unavailable")
	}
	// FFmpeg consumes the leading "c" as a single interactive key, then reads
	// the command from the remainder of that same input line. A newline directly
	// after "c" is therefore parsed as an empty command.
	// The command itself expects: target, time, command, argument.
	// A time of -1 applies the command immediately to the first matching
	// named filter instance.
	_, err = io.WriteString(p.stdin, "c"+target+" -1 "+command+" "+argument+"\n")
	return err
}

func (p *execProcess) Terminate() error {
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	if p.stdin == nil {
		return nil
	}
	_, err := io.WriteString(p.stdin, "q\n")
	closeErr := p.stdin.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (p *execProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}
