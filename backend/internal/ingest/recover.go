package ingest

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"event-gallery/backend/internal/store"
	"event-gallery/backend/internal/tussidecar"
)

// unobservableReason marks a row closed out because its bytes were never
// visible. Unlike a cancellation it deletes nothing, so it stays reversible.
const unobservableReason = "unobservable"

// maxStaleUploadsPerPass bounds one reconciliation pass so a large backlog
// cannot hold the tick open. What is left is picked up by the next pass.
const maxStaleUploadsPerPass = 200

// startupRecovery makes interrupted work claimable and adopts any completed
// upload the previous process never queued, then opens the readiness gate.
// Readiness is withheld if the inventory could not be read at all: admitting
// uploads while pre-upgrade completions are still unknown would be worse than
// a few extra seconds of backpressure. The reconciler retries and opens the
// gate on the first pass that succeeds.
func (m *Manager) startupRecovery() {
	if _, err := m.store.RequeueStartup(m.lifetime, store.NowMicros()); err != nil {
		// A database failure here means nothing about the queue can be trusted
		// yet, so readiness stays closed and the reconciler retries.
		slog.Error("failed to requeue interrupted jobs; ingest stays not ready", "operation", "startup_recovery", "error", err)
		return
	}
	// The gate starts closed and pre-create refuses uploads while it is, so
	// something has to prove the media volume is mounted before we admit any.
	if err := m.health.Check(m.lifetime); err != nil {
		slog.Error("storage health check failed at startup", "operation", "startup_recovery", "error", err)
	}
	if err := m.reconcileOnce(); err != nil {
		slog.Error("startup inventory failed; ingest stays not ready", "operation", "startup_recovery", "error", err)
		return
	}
	if m.lifetime.Err() != nil {
		return // shutdown overtook recovery; admitting uploads now would be a lie
	}
	if m.ready.CompareAndSwap(false, true) {
		slog.Info("ingest ready", "operation", "startup_recovery")
	}
	m.Wake()
}

// reconcileOnce repairs what it can prove. It never deletes a file directly:
// the only outcomes are adoption, promotion, reopening a removal that could not
// finish, and leaving things exactly as they are. It reports whether the
// inventory pass actually completed, which is what gates readiness.
func (m *Manager) reconcileOnce() error {
	// The inventory is read before anything acts, for two reasons.
	// resolveRowsWithoutFiles draws its conclusions from the absence of files,
	// and on a volume that cannot be read at all that absence is not evidence:
	// every stale row would be closed out at once on the strength of it.
	// sweepCancelled reads no files, but it hands deletions to workers, and a
	// volume this pass could not read is not one to start removing things on.
	entries, err := os.ReadDir(m.opts.UploadDir)
	if err != nil {
		slog.Warn("cannot read upload directory", "operation", "reconcile", "error", err)
		return err
	}

	m.sweepCancelled(m.opts.ReconcileInterval)
	m.resolveRowsWithoutFiles()

	// Both sidecars and bare data files are inventoried. The old code ran
	// `defer cleanupTusInfoFile(...)` before its failure branch, so every
	// upload it failed to ingest left a complete data file with no sidecar
	// at all. Scanning only `.info` entries would strand exactly the files
	// this feature exists to rescue.
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		// A pass over a large volume is not instantaneous, and every store
		// call below would fail one by one against a cancelled lifetime. Bail
		// out instead, and report the pass as incomplete so readiness stays
		// shut.
		if err := m.lifetime.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			continue
		}
		uploadID := strings.TrimSuffix(entry.Name(), ".info")
		if !isSafeUploadID(uploadID) {
			continue // skips tusd lock files and our own dot-prefixed temporaries
		}
		if _, done := seen[uploadID]; done {
			continue
		}
		seen[uploadID] = struct{}{}

		if err := m.reconcileOne(uploadID); err != nil {
			slog.Warn("reconcile failed", "operation", "reconcile", "upload_id", uploadID, "error", err)
		}
	}
	return nil
}

// sweepCancelled moves cancelled uploads that have gone quiet into discard,
// where the ordinary worker path reclaims their bytes. idleFor is a parameter
// so tests can sweep immediately.
func (m *Manager) sweepCancelled(idleFor time.Duration) {
	now := store.NowMicros()
	swept, err := m.store.ClaimCancelledForDiscard(m.lifetime, now-idleFor.Microseconds(), now)
	if err != nil {
		slog.Warn("cancellation sweep failed", "operation", "reconcile", "error", err)
		return
	}
	if swept > 0 {
		m.Wake()
	}
}

// resolveRowsWithoutFiles closes out admitted uploads that never produced any
// bytes — a create whose client vanished, or a partial the retention janitor
// removed. Rows that reached the durability barrier are deliberately excluded:
// their absence may be a faulted mount, and they retry indefinitely instead.
//
// The closure is marked 'unobservable' rather than cancelled. That distinction
// is what makes it safe: it deletes nothing, and if the paths were merely
// hidden, reconcileOne re-adopts them when they come back.
func (m *Manager) resolveRowsWithoutFiles() {
	idleBefore := store.NowMicros() - m.opts.ReconcileInterval.Microseconds()
	ids, err := m.store.ListStaleUploading(m.lifetime, idleBefore, maxStaleUploadsPerPass)
	if err != nil {
		slog.Warn("could not list stale uploads", "operation", "reconcile", "error", err)
		return
	}
	for _, id := range ids {
		if m.UploadPathsExist(id) {
			continue
		}
		if err := m.store.MarkUnobservable(m.lifetime, id, store.NowMicros()); err != nil && !errors.Is(err, store.ErrNotClaimed) {
			slog.Warn("could not resolve upload with no bytes", "operation", "reconcile", "upload_id", id, "error", err)
		}
	}
}

func (m *Manager) reconcileOne(uploadID string) error {
	job, err := m.store.GetUploadJob(m.lifetime, uploadID)
	if err != nil {
		return err
	}

	// Both derived paths are observed before any decision below, because the
	// inventory keys on either name and no branch may act on a file this pass
	// has not itself seen. Lstat, like the incomplete-upload janitor: a
	// symlink is not something tusd wrote, and following one would take the
	// reconciler outside the volume it is responsible for.
	dataStat, dataErr := os.Lstat(m.DataPath(uploadID))
	sourcePresent := dataErr == nil && dataStat.Mode().IsRegular()
	infoStat, infoErr := os.Lstat(m.InfoPath(uploadID))
	sidecarPresent := infoErr == nil && infoStat.Mode().IsRegular()

	if job != nil {
		switch {
		case job.Status == store.JobUploading:
			// May still need the durability barrier; handled below.
		case job.Status == store.JobDiscarded && job.TerminalReason == unobservableReason:
			// The bytes were hidden, not gone — a faulted mount that has now
			// returned. Closing the row out deleted nothing, so drop it and
			// adopt the upload afresh rather than stranding a complete file.
			slog.Info("bytes returned for an unobservable upload; re-adopting",
				"operation", "reconcile", "upload_id", uploadID)
			if err := m.store.DeleteUploadJob(m.lifetime, uploadID); err != nil {
				return err
			}
			job = nil
		case job.Status == store.JobComplete || job.Status == store.JobDiscarded:
			if !sourcePresent && !sidecarPresent {
				return nil // nothing of this upload is here, so nothing came back
			}
			// A file this job verified as gone is on the volume again, so it
			// came back after the job was closed out. Finish the removal
			// rather than leaving it: bytes the guest cancelled must not
			// survive, a sidecar without its data file is one tusd can no
			// longer address and the incomplete-upload janitor skips, and once
			// the terminal row expires a rowless complete source would be
			// adopted and republished.
			target := store.JobCleanup
			if job.Status == store.JobDiscarded {
				target = store.JobDiscarding
			}
			slog.Warn("tus files reappeared for a finished upload; reopening to remove them",
				"operation", "reconcile", "upload_id", uploadID, "status", job.Status, "reason", job.TerminalReason)
			if err := m.store.ReopenTerminal(m.lifetime, uploadID, target, store.NowMicros()); err != nil {
				return err
			}
			m.Wake()
			return nil
		default:
			return nil // pending or later: the workers own it
		}
	}

	if !sourcePresent {
		return nil // nothing observable; absence is never actionable here
	}

	if job != nil {
		// Every upload currently being transferred has an 'uploading' row and
		// a partial file, so this must not fire for them: it would burn a
		// durability slot every tick and inject 503s into live uploads that
		// join the doomed operation.
		if dataStat.Size() != job.ExpectedSize {
			return nil
		}
		// Complete but never past the barrier: a crash inside the pre-finish
		// window, or a client that stopped retrying after a 503.
		return m.EnsureDurable(m.lifetime, uploadID)
	}

	info, identityMismatch := m.trustedSidecar(uploadID)
	switch {
	case info != nil:
		if dataStat.Size() != info.Size {
			return nil // still uploading; the incomplete-retention policy owns it
		}
		return m.adopt(uploadID, info.Size, info.MetaData)
	case identityMismatch:
		// A sidecar naming a different upload must not supply this file's
		// metadata: a checksum from the wrong upload would turn a good file
		// into a deterministic rejection and get it discarded.
		slog.Warn("tus sidecar does not describe its own upload; leaving both files untouched",
			"operation", "reconcile", "upload_id", uploadID)
		return nil
	default:
		// No sidecar, or one that cannot be parsed at all. tusd cannot resume
		// such an upload, so the file is residue, most often from the old
		// ingest path, which deleted the sidecar before its failure branch
		// returned. Adopt at the observed size; with no metadata there is no
		// declared checksum to mismatch.
		return m.adopt(uploadID, dataStat.Size(), nil)
	}
}

// trustedSidecar returns the sidecar only when it demonstrably describes this
// upload. The second result flags the one case where the data file must be
// left alone: a parseable sidecar naming a different upload. A missing or
// unreadable sidecar is reported as absent, because tusd cannot resume such an
// upload and stranding the bytes would be worse than adopting them.
func (m *Manager) trustedSidecar(uploadID string) (info *tussidecar.Info, identityMismatch bool) {
	infoPath := m.InfoPath(uploadID)
	parsed, err := tussidecar.Parse(infoPath)
	if err != nil {
		return nil, false
	}
	if parsed.ID != uploadID ||
		filepath.Clean(parsed.StoragePath) != filepath.Clean(m.DataPath(uploadID)) ||
		(parsed.StorageInfoPath != "" && filepath.Clean(parsed.StorageInfoPath) != filepath.Clean(infoPath)) {
		return nil, true
	}
	return parsed, false
}

func (m *Manager) adopt(uploadID string, size int64, metadata map[string]string) error {
	job := &store.UploadJob{
		UploadID:         uploadID,
		MediaID:          uuid.NewString(),
		OriginalFilename: sanitizeAdoptedFilename(metadata["filename"]),
		ExpectedSize:     size,
		DeclaredSHA256:   strings.ToLower(strings.TrimSpace(metadata["sha256"])),
		GuestName:        strings.TrimSpace(metadata["guestName"]),
	}
	if err := m.store.CreateUploadingJob(m.lifetime, job); err != nil {
		return err
	}
	// Promote through the same barrier the live path uses, so an adopted
	// upload is fsynced and size-checked exactly like any other.
	if err := m.EnsureDurable(m.lifetime, uploadID); err != nil {
		return err
	}
	slog.Info("adopted completed upload", "operation", "reconcile", "upload_id", uploadID, "media_id", job.MediaID)
	return nil
}

func sanitizeAdoptedFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "recovered-upload"
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

func isSafeUploadID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		isAlnum := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !isAlnum && r != '-' && r != '_' {
			return false
		}
	}
	return true
}
