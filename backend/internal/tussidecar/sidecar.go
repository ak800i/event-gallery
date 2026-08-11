// Package tussidecar parses tusd's `.info` sidecar files. Both the ingest
// reconciler and the incomplete-upload janitor read them, and both must apply
// the same bounds and validation, so the logic lives here once.
package tussidecar

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// maxSidecarBytes bounds how much untrusted JSON we will read from the shared
// upload volume.
const maxSidecarBytes = 64 * 1024

// ErrMalformed marks a sidecar that exists but cannot be trusted. Callers must
// treat it as "unknown", never as "safe to delete".
var ErrMalformed = errors.New("malformed tus sidecar")

type Info struct {
	ID              string
	Size            int64
	SizeIsDeferred  bool
	MetaData        map[string]string
	StoragePath     string
	StorageInfoPath string
}

type rawInfo struct {
	ID             string            `json:"ID"`
	Size           int64             `json:"Size"`
	SizeIsDeferred bool              `json:"SizeIsDeferred"`
	MetaData       map[string]string `json:"MetaData"`
	Storage        struct {
		Type     string `json:"Type"`
		Path     string `json:"Path"`
		InfoPath string `json:"InfoPath"`
	} `json:"Storage"`
}

// Parse reads and validates one sidecar. It never returns a partially trusted
// value: either every checked field is sane, or an error.
func Parse(infoPath string) (*Info, error) {
	f, err := os.Open(infoPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxSidecarBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read sidecar: %w", err)
	}
	if len(data) > maxSidecarBytes {
		return nil, fmt.Errorf("%w: larger than %d bytes", ErrMalformed, maxSidecarBytes)
	}

	var raw rawInfo
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if raw.ID == "" {
		return nil, fmt.Errorf("%w: empty upload id", ErrMalformed)
	}
	if raw.Size <= 0 {
		return nil, fmt.Errorf("%w: non-positive size", ErrMalformed)
	}
	if raw.SizeIsDeferred {
		return nil, fmt.Errorf("%w: deferred size", ErrMalformed)
	}
	if raw.Storage.Type != "filestore" {
		return nil, fmt.Errorf("%w: unsupported storage type %q", ErrMalformed, raw.Storage.Type)
	}
	if raw.Storage.Path == "" {
		return nil, fmt.Errorf("%w: empty storage path", ErrMalformed)
	}

	return &Info{
		ID:              raw.ID,
		Size:            raw.Size,
		SizeIsDeferred:  raw.SizeIsDeferred,
		MetaData:        raw.MetaData,
		StoragePath:     raw.Storage.Path,
		StorageInfoPath: raw.Storage.InfoPath,
	}, nil
}
