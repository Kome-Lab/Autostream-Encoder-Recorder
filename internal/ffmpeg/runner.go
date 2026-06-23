package ffmpeg

import (
	"context"
	"errors"
	"os/exec"
)

type Runner interface {
	Run(ctx context.Context, bin string, args []string) error
}

type CommandRunner struct{}

func (CommandRunner) Run(ctx context.Context, bin string, args []string) error {
	if bin == "" {
		return errors.New("ffmpeg binary is required")
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	return cmd.Run()
}

type DryRunRunner struct {
	Commands []Command
}

type Command struct {
	Bin  string   `json:"bin"`
	Args []string `json:"args"`
}

func (r *DryRunRunner) Run(ctx context.Context, bin string, args []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	copied := append([]string(nil), args...)
	r.Commands = append(r.Commands, Command{Bin: bin, Args: copied})
	return nil
}
