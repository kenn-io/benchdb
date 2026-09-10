package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"

	"go.kenn.io/benchdb/internal/storage"
)

const maxArtifactBytes = 16 << 20
const maxResultArtifactBytes = 32 << 20

var artifactName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,95}$`)

// ArtifactInput attaches diagnostics from a separate profiling run. Artifacts
// are not measurements and never participate in series fingerprints or gates.
type ArtifactInput struct {
	Name      string `json:"name" maxLength:"96"`
	Kind      string `json:"kind" enum:"cpu-profile,memory-profile,diagnostics"`
	MediaType string `json:"media_type" enum:"application/vnd.google.pprof,application/json"`
	Data      []byte `json:"data" doc:"Base64-encoded artifact content."`
}

func prepareArtifacts(inputs []ArtifactInput) ([]storage.Artifact, error) {
	if len(inputs) > 16 {
		return nil, &ValidationError{Message: "at most 16 artifacts are allowed"}
	}
	out := make([]storage.Artifact, 0, len(inputs))
	names := make(map[string]bool, len(inputs))
	total := 0
	for _, a := range inputs {
		if !artifactName.MatchString(a.Name) || names[a.Name] {
			return nil, &ValidationError{Message: "artifact names must be unique lowercase filenames"}
		}
		names[a.Name] = true
		switch a.Kind {
		case "cpu-profile", "memory-profile":
			if a.MediaType != "application/vnd.google.pprof" {
				return nil, &ValidationError{Message: "profiles require the pprof media type"}
			}
		case "diagnostics":
			if a.MediaType != "application/json" {
				return nil, &ValidationError{Message: "diagnostics require the JSON media type"}
			}
		default:
			return nil, &ValidationError{Message: "unknown artifact kind"}
		}
		total += len(a.Data)
		if len(a.Data) == 0 || len(a.Data) > maxArtifactBytes || total > maxResultArtifactBytes {
			return nil, &ValidationError{Message: "artifacts must be nonempty, at most 16 MiB each and 32 MiB per result"}
		}
		sum := sha256.Sum256(a.Data)
		out = append(out, storage.Artifact{Name: a.Name, Kind: a.Kind, MediaType: a.MediaType, Data: a.Data, SHA256: hex.EncodeToString(sum[:])})
	}
	return out, nil
}

// Artifact returns one diagnostic attachment for download.
func (r *Reader) Artifact(ctx context.Context, resultID, name string) (storage.Artifact, error) {
	artifact, err := r.store.GetResultArtifact(ctx, resultID, name)
	if errors.Is(err, storage.ErrNotFound) {
		return storage.Artifact{}, ErrNotFound
	}
	return artifact, err
}
