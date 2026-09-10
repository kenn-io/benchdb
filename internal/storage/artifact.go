package storage

// Artifact is an immutable diagnostic attachment stored with a result.
type Artifact struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	MediaType string `json:"media_type"`
	SHA256    string `json:"sha256"`
	Data      []byte `json:"data"`
}

// ArtifactMetadata identifies a download without loading its content.
type ArtifactMetadata struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	MediaType string `json:"media_type"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}
