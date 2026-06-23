package control

import (
	"os"
	"path"

	"github.com/example/autostream-encoder-recorder/internal/archive"
)

func ArchiveArtifacts(layout archive.Layout) []Artifact {
	candidates := []struct {
		kind string
		name string
		file string
	}{
		{kind: "archive", name: "final.mp4", file: layout.FinalMP4()},
		{kind: "caption", name: "captions.vtt", file: layout.FinalCaptions()},
		{kind: "transcript", name: "transcript.json", file: layout.FinalTranscript()},
		{kind: "metadata", name: "metadata.json", file: layout.FinalMetadata()},
		{kind: "logs", name: "logs.jsonl", file: layout.FinalLogs()},
	}
	artifacts := make([]Artifact, 0, len(candidates))
	for _, candidate := range candidates {
		info, err := os.Lstat(candidate.file)
		if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		artifacts = append(artifacts, Artifact{
			Kind:         candidate.kind,
			Name:         candidate.name,
			RelativePath: path.Join("final", layout.StreamID, candidate.name),
			SizeBytes:    info.Size(),
		})
	}
	return artifacts
}
