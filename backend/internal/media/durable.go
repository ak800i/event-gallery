package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// FsyncFile flushes one file's contents to stable storage.
func FsyncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", path, err)
	}
	return nil
}

// FsyncDir flushes a directory entry so a rename or unlink survives a crash.
func FsyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync dir %s: %w", path, err)
	}
	return nil
}

// Indirected so a test can assert that the copy and its directory entry are on
// stable storage before the rename publishes them. Without a seam the ordering
// is only protected by the comment below it.
var (
	prepareSyncFile = (*os.File).Sync
	prepareSyncDir  = FsyncDir
)

// PrepareRequest describes one copy into permanent storage. Size and SHA256
// are the already-computed identity of the source, which makes preparation
// idempotent across retries.
type PrepareRequest struct {
	SourcePath     string
	MediaID        string
	LeaseToken     string
	StoredFilename string
	Size           int64
	SHA256         string
}

// PrepareOriginal copies the source into permanent storage. It never removes
// the source: until the media row is committed, that source may be the only
// complete copy in existence.
//
// If a deterministic original is already present it is reused when its size
// and hash match, and is an error otherwise — a mismatch is never silently
// overwritten.
//
// Ordering is load-bearing — write, fsync file, fsync dir, rename, fsync dir —
// so that a crash can leave a stale temporary but never a truncated original.
func (p *Processor) PrepareOriginal(ctx context.Context, req PrepareRequest) error {
	if err := p.EnsureDirs(); err != nil {
		return err
	}
	finalPath := p.OriginalPath(req.StoredFilename)

	if _, err := os.Stat(finalPath); err == nil {
		if err := p.VerifyOriginal(req.StoredFilename, req.Size, req.SHA256); err != nil {
			return fmt.Errorf("existing original %s does not match this upload: %w", req.StoredFilename, err)
		}
		// Re-sync the directory: the previous attempt may have failed on
		// exactly this step, and skipping it would publish a rename that a
		// crash could still roll back.
		return prepareSyncDir(p.OriginalsDir())
	}

	src, err := os.Open(req.SourcePath)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer src.Close()

	// The lease token scopes the temporary, so a worker whose lease expired
	// cannot write into its successor's temporary.
	tmpPath := filepath.Join(p.OriginalsDir(), ".ingest-"+req.MediaID+"-"+req.LeaseToken+"-original.tmp")
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("create temporary: %w", err)
	}

	if err := copyWithContext(ctx, tmp, src); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := prepareSyncFile(tmp); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("fsync temporary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temporary: %w", err)
	}
	// Only now is the temporary a recoverable alternate copy.
	if err := prepareSyncDir(p.OriginalsDir()); err != nil {
		return err
	}
	// A worker whose attempt was cancelled must not publish an artifact after
	// its successor may already have cleaned up.
	if err := ctx.Err(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("rename original into place: %w", err)
	}
	return prepareSyncDir(p.OriginalsDir())
}

// SHA256FileContext hashes a file, honoring cancellation. The whole-file hash
// of a multi-gigabyte upload is long enough that a shutdown must be able to
// interrupt it.
func SHA256FileContext(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	sum := sha256.New()
	if err := copyWithContext(ctx, sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// VerifyOriginal proves a stored original is the exact content we expect.
// Absence, wrong size, and wrong hash are all errors: a missing file is never
// treated as permission to move on.
func (p *Processor) VerifyOriginal(storedFilename string, wantSize int64, wantSHA256 string) error {
	path := p.OriginalPath(storedFilename)
	stat, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat original: %w", err)
	}
	if !stat.Mode().IsRegular() {
		return errors.New("original is not a regular file")
	}
	if stat.Size() != wantSize {
		return fmt.Errorf("original size %d, want %d", stat.Size(), wantSize)
	}
	sum, err := SHA256File(path)
	if err != nil {
		return fmt.Errorf("hash original: %w", err)
	}
	if sum != wantSHA256 {
		return fmt.Errorf("original sha256 %s, want %s", sum, wantSHA256)
	}
	return nil
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) error {
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return fmt.Errorf("write copy: %w", writeErr)
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("read source: %w", readErr)
		}
	}
}
