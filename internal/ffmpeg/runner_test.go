package ffmpeg

import (
	"context"
	"testing"
)

func TestDryRunRunnerRecordsCommand(t *testing.T) {
	runner := &DryRunRunner{}
	if err := runner.Run(context.Background(), "ffmpeg", []string{"-version"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.Commands) != 1 || runner.Commands[0].Bin != "ffmpeg" || runner.Commands[0].Args[0] != "-version" {
		t.Fatalf("unexpected commands: %#v", runner.Commands)
	}
}
