package forge

// Atomic + all-or-nothing folder saves, backup-on-save, external-change
// detection. This file holds the save-durability machinery that Save() in
// session.go drives:
//
//   - A batch commit for folder-backed maps: encode every dirty file first,
//     stage each into a sibling temp file (fsync'd), and only once EVERY temp
//     is written do we rename them into place. A failure before the rename
//     phase leaves every original file byte-for-byte untouched, so a mid-save
//     crash / disk-full / encode error can never leave an internally
//     inconsistent map (new units + old terrain).
//   - Backup-on-save: before a file's bytes are replaced, its prior contents
//     are copied to a sibling <name>.bak. Mirrors the codegen / save_script
//     .bak behavior (ScriptBackupSuffix) so a prior version stays recoverable.
//   - External-change detection: each source file's mtime+size is recorded at
//     Open. Before a save commits, the files about to be written are re-stat'd;
//     if any changed on disk since Open the save is refused (the editor is
//     explicitly multi-instance + agent-driven, so a silent last-writer-wins
//     clobber is exactly what we want to catch). A force override mirrors the
//     existing option-passing (SaveTriggerScriptGuarded's overwrite bool).
//
// MPQ-backed sources already get all-or-nothing for free: writes buffer in
// memory and flush() repacks the whole archive in one atomic temp+rename (see
// mpq_write_source.go). The batch-commit logic below is folder-specific; the
// staleness baseline covers both (the .w3x file's own stamp for MPQ).

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// BackupSuffix is appended to a file's name when Save backs up its prior
// contents before overwriting it. Reuses the same suffix the trigger
// save-guard uses (ScriptBackupSuffix == ".bak") so a map directory only ever
// grows one backup convention.
const BackupSuffix = ScriptBackupSuffix

// ErrSourceChangedOnDisk is returned by Save when a file it is about to write
// has changed on disk (mtime or size) since the map was opened — another
// wc3-forge instance, an agent, or a human edited it underneath us. Refusing
// by default prevents a silent last-writer-wins clobber. Callers that genuinely
// want to overwrite pass SaveOptions{Force: true} (the GUI surfaces this as a
// "saved elsewhere — overwrite?" prompt). errors.Is checks rely on this
// sentinel.
var ErrSourceChangedOnDisk = errors.New("map file changed on disk since it was opened (another editor or agent may have saved it); pass force to overwrite")

// SaveOptions tweaks Save's behavior. The zero value is the safe default:
// refuse to overwrite files that changed on disk since Open. Mirrors the
// overwrite-bool option-passing already used by SaveTriggerScriptGuarded.
type SaveOptions struct {
	// Force overrides external-change detection: when true, Save proceeds even
	// if a target file changed on disk since Open. The prior on-disk bytes are
	// still backed up to <name>.bak first, so a forced overwrite stays
	// recoverable.
	Force bool
}

// fileStamp is a cheap identity for a file on disk: its modification time and
// size. Two stamps comparing unequal means the file changed underneath us.
// Recorded at Open (recordSourceBaseline) and re-checked at Save.
type fileStamp struct {
	modTime time.Time
	size    int64
}

func (a fileStamp) equal(b fileStamp) bool {
	return a.size == b.size && a.modTime.Equal(b.modTime)
}

// (pendingWrite — the encoded-file-staged-for-commit type — is declared at
// package scope in session.go, where Save builds the slice; commitFolderWrites
// + checkStaleness below only read its .name/.data.)

// statFolderFile returns the on-disk stamp for name under the folder root.
// ok=false (nil error) means the file is absent — a not-yet-created file (e.g.
// war3mapMisc.txt the user just added) has no baseline and is never "stale".
func statFolderFile(root, name string) (fileStamp, bool, error) {
	fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fileStamp{}, false, nil
		}
		return fileStamp{}, false, err
	}
	return fileStamp{modTime: fi.ModTime(), size: fi.Size()}, true, nil
}

// recordSourceBaseline snapshots the on-disk identity of the files Save might
// later overwrite, so external-change detection has something to compare
// against. Called from Open after the state swap. Best-effort: a stat error on
// any one file just omits it from the baseline (it'll be treated as "no
// baseline" → never stale), since the baseline is a clobber-guard, not a load
// gate.
//
// For folder sources we stamp each standard war3map.* file present on disk.
// For MPQ sources we stamp the single .w3x at the source path under its own
// path key — a whole-archive repack replaces that one file, so its stamp is the
// right granularity. The result is stored on s.srcBaseline under the write lock.
func (s *Session) recordSourceBaseline(src fileSource, abs string) {
	baseline := make(map[string]fileStamp)
	switch v := src.(type) {
	case folderSource:
		for _, n := range baselineFileNames {
			st, ok, err := statFolderFile(v.root, n)
			if err != nil || !ok {
				continue
			}
			baseline[n] = st
		}
	case *mpqSource:
		if v != nil && v.path != "" {
			if fi, err := os.Stat(v.path); err == nil {
				baseline[mpqArchiveKey] = fileStamp{modTime: fi.ModTime(), size: fi.Size()}
			}
		}
	}
	s.mu.Lock()
	s.srcBaseline = baseline
	s.mu.Unlock()
}

// mpqArchiveKey is the baseline-map key under which an MPQ-backed source stamps
// its single .w3x file (folder sources key by war3map.* name instead).
const mpqArchiveKey = "\x00mpq-archive"

// baselineFileNames is the set of files recordSourceBaseline stamps for folder
// sources — the standard war3map.* set Save can write back. Mirrors
// collectMapFilePresence's list plus the script files Save's sky / hand-rolled
// paths touch. A file not in this set simply has no baseline (never "stale"),
// which is acceptable: the multi-instance clobber case we care about is the
// standard map files two editors both save.
var baselineFileNames = []string{
	"war3map.w3i", "war3map.w3e", "war3map.shd", "war3map.wpm",
	"war3mapUnits.doo", "war3map.doo", "war3map.wts", "war3map.imp",
	"war3map.w3d", "war3map.w3b", "war3map.w3u", "war3map.w3t",
	"war3map.w3a", "war3map.w3h", "war3map.w3q",
	"war3map.w3r", "war3map.w3c", "war3mapMisc.txt",
	"war3map.wtg", "war3map.wct", "war3map.j", "war3map.lua",
}

// checkStaleness verifies none of the files about to be written changed on disk
// since Open. Returns ErrSourceChangedOnDisk (wrapped with the offending name)
// on the first mismatch. A file with no recorded baseline (created after Open,
// or absent at Open) is skipped — there's nothing to clobber. Force callers
// skip the whole check.
//
// Only folder sources are checked here per-file; MPQ sources are checked
// whole-archive by the caller against mpqArchiveKey. s.mu must NOT be held —
// this snapshots the baseline under its own RLock.
func (s *Session) checkStaleness(src fileSource, writes []pendingWrite) error {
	fs, ok := src.(folderSource)
	if !ok {
		return nil
	}
	// Snapshot the baseline stamps we need by VALUE under the lock — copy the
	// fileStamp entries into a fresh local map, NOT the map reference. A bare
	// `baseline := s.srcBaseline` would alias the live map, and the per-file
	// loop below (which deliberately runs UNLOCKED so the os.Stat I/O doesn't
	// hold the session lock) would then read that map concurrently with the
	// baseline writers that mutate it in place under s.mu
	// (refreshBaselineEntryLocked / refreshBaselineAfterCommit). A simultaneous
	// Go map read+write is a fatal "concurrent map read and map write" panic —
	// reachable here because the GUI and the MCP bridge dispatch onto the same
	// Session singleton from independent goroutines. fileStamp is a plain value
	// (time.Time + int64), so the copy fully detaches us from the shared map.
	s.mu.RLock()
	if s.srcBaseline == nil {
		// No baseline recorded (e.g. a session constructed directly in a test
		// without going through Open). Nothing to compare; allow the save.
		s.mu.RUnlock()
		return nil
	}
	want := make(map[string]fileStamp, len(writes))
	for _, w := range writes {
		if st, recorded := s.srcBaseline[w.name]; recorded {
			want[w.name] = st
		}
	}
	s.mu.RUnlock()
	for _, w := range writes {
		recorded, ok := want[w.name]
		if !ok {
			continue
		}
		cur, exists, err := statFolderFile(fs.root, w.name)
		if err != nil {
			return fmt.Errorf("stat %s for change-detection: %w", w.name, err)
		}
		if !exists {
			// The file we recorded at Open is gone — treat as an external
			// change (another tool deleted it) rather than silently recreating.
			return fmt.Errorf("%w (%s)", ErrSourceChangedOnDisk, w.name)
		}
		if !cur.equal(recorded) {
			return fmt.Errorf("%w (%s)", ErrSourceChangedOnDisk, w.name)
		}
	}
	return nil
}

// checkArchiveStaleness verifies the MPQ-backed map's single .w3x file hasn't
// changed on disk since Open. Returns ErrSourceChangedOnDisk if it has. A
// missing baseline (no stamp recorded) or a now-absent archive is treated as
// "nothing to clobber" (allow) — the repack will simply create it. s.mu must
// NOT be held.
func (s *Session) checkArchiveStaleness(mp *mpqSource) error {
	if mp == nil || mp.path == "" {
		return nil
	}
	// Capture the single archive stamp by VALUE under the lock (see the longer
	// note in checkStaleness): aliasing s.srcBaseline and reading it after
	// RUnlock would race the in-place baseline writers (restampArchiveBaseline)
	// and fatally panic with "concurrent map read and map write".
	s.mu.RLock()
	want, recorded := fileStamp{}, false
	if s.srcBaseline != nil {
		want, recorded = s.srcBaseline[mpqArchiveKey]
	}
	s.mu.RUnlock()
	if !recorded {
		return nil
	}
	fi, err := os.Stat(mp.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat archive for change-detection: %w", err)
	}
	cur := fileStamp{modTime: fi.ModTime(), size: fi.Size()}
	if !cur.equal(want) {
		return fmt.Errorf("%w (%s)", ErrSourceChangedOnDisk, filepath.Base(mp.path))
	}
	return nil
}

// restampArchiveBaseline re-records the MPQ archive's stamp after a successful
// repack, so a subsequent Save doesn't flag this session's own write as an
// external change. Best-effort. s.mu must NOT be held.
func (s *Session) restampArchiveBaseline(mp *mpqSource) {
	if mp == nil || mp.path == "" {
		return
	}
	fi, err := os.Stat(mp.path)
	if err != nil {
		return
	}
	s.mu.Lock()
	if s.srcBaseline == nil {
		s.srcBaseline = make(map[string]fileStamp)
	}
	s.srcBaseline[mpqArchiveKey] = fileStamp{modTime: fi.ModTime(), size: fi.Size()}
	s.mu.Unlock()
}

// refreshBaselineAfterCommit updates the recorded stamps for the files Save just
// wrote, so a subsequent Save in the same session doesn't falsely flag its own
// previous write as an external change. Called after a successful folder commit.
// Best-effort (a stat error just leaves the old stamp, which would conservatively
// refuse the next save — acceptable, and unlikely).
func (s *Session) refreshBaselineAfterCommit(root string, writes []pendingWrite) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srcBaseline == nil {
		s.srcBaseline = make(map[string]fileStamp)
	}
	for _, w := range writes {
		s.refreshBaselineEntryLocked(root, w.name)
	}
}

// refreshBaselineEntry re-records (or, if the file is now absent, drops) the
// change-detection baseline stamp for a single source-relative name a DIRECT
// writer just put on disk — i.e. a path that bypasses Save's batch commit:
// Convert-to-Lua (war3map.lua / war3map.j), ImportModel (war3map.imp's byte
// payloads), and SaveTriggerScript. Without this, the editor's OWN direct write
// leaves a stale baseline and the NEXT Save() re-stats the file, sees a changed
// mtime/size, and spuriously refuses with ErrSourceChangedOnDisk.
//
// Only folder sources have per-name baselines; MPQ sources buffer direct writes
// until flush() (which only Save drives, and Save restamps the archive), so this
// is a no-op for them. Best-effort: a stat error just leaves the old stamp,
// which conservatively refuses the next save (acceptable, and unlikely).
//
// s.mu must NOT be held — this acquires the write lock itself. Use
// refreshBaselineEntryLocked from a caller that already holds it.
func (s *Session) refreshBaselineEntry(src fileSource, name string) {
	fs, ok := src.(folderSource)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshBaselineEntryLocked(fs.root, name)
}

// refreshBaselineEntryLocked is the lock-free core of refreshBaselineEntry /
// refreshBaselineAfterCommit. s.mu MUST already be held for writing. Stamps the
// file at root/name into s.srcBaseline; if the file is now absent (a Convert-to-
// Lua delete dropped war3map.j) the stale baseline entry is removed so a later
// checkStaleness doesn't read a recorded-but-gone file as an external delete.
func (s *Session) refreshBaselineEntryLocked(root, name string) {
	if s.srcBaseline == nil {
		s.srcBaseline = make(map[string]fileStamp)
	}
	st, ok, err := statFolderFile(root, name)
	if err != nil {
		return
	}
	if ok {
		s.srcBaseline[name] = st
	} else {
		delete(s.srcBaseline, name)
	}
}

// commitFolderWrites performs the all-or-nothing batch commit for a folder
// source. Two phases:
//
//  1. STAGE: every pending write goes to a sibling temp file (in the same
//     directory as its target, so the later rename stays on one filesystem),
//     each temp fsync'd. If any staging step fails, every temp written so far
//     is removed and the function returns — NO target file has been touched, so
//     all originals remain byte-identical.
//  2. COMMIT: for each staged file, back up the prior on-disk contents to
//     <name>.bak (if the target exists), then rename the temp over the target.
//
// The vulnerable window shrinks to the rename loop itself (renames are the
// fastest, least-failure-prone filesystem op); an encode error, a disk-full, or
// a crash during STAGE leaves the map wholly intact. A failure mid-COMMIT (rare:
// a rename failing on Windows because the target is locked) is surfaced, but by
// then the all-or-nothing encode guarantee has already held — the files that
// did commit are each individually consistent (atomic rename), and the .bak of
// each holds its prior bytes.
//
// Backups (.bak) and temp files (.tmp) are themselves skipped as write targets
// — Save never enqueues them.
func commitFolderWrites(fs folderSource, writes []pendingWrite) (err error) {
	type staged struct {
		tmpPath string // sibling temp holding the new bytes
		dstPath string // final destination
		name    string // source-relative name (for backup + errors)
		data    []byte // the new bytes (alias of the pending write; for the idempotent-retry check)
	}
	stagedAll := make([]staged, 0, len(writes))

	// On any error before/within COMMIT, clean up the temp files we created.
	// After a successful return the temps have been renamed away, so the
	// Remove loop is a harmless no-op.
	defer func() {
		if err != nil {
			for _, st := range stagedAll {
				_ = os.Remove(st.tmpPath)
			}
		}
	}()

	// --- Phase 1: STAGE ---------------------------------------------------
	for _, w := range writes {
		dst, derr := fs.resolve(w.name)
		if derr != nil {
			return derr
		}
		if dir := filepath.Dir(dst); dir != "" {
			if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
				return fmt.Errorf("commit %q: mkdir: %w", w.name, mkErr)
			}
		}
		tmp, terr := os.CreateTemp(filepath.Dir(dst), ".forgesave-*.tmp")
		if terr != nil {
			return fmt.Errorf("commit %q: create temp: %w", w.name, terr)
		}
		tmpName := tmp.Name()
		if _, werr := tmp.Write(w.data); werr != nil {
			tmp.Close()
			os.Remove(tmpName)
			return fmt.Errorf("commit %q: write temp: %w", w.name, werr)
		}
		if serr := tmp.Sync(); serr != nil {
			tmp.Close()
			os.Remove(tmpName)
			return fmt.Errorf("commit %q: sync temp: %w", w.name, serr)
		}
		if cerr := tmp.Close(); cerr != nil {
			os.Remove(tmpName)
			return fmt.Errorf("commit %q: close temp: %w", w.name, cerr)
		}
		stagedAll = append(stagedAll, staged{tmpPath: tmpName, dstPath: dst, name: w.name, data: w.data})
	}

	// --- Phase 2: COMMIT --------------------------------------------------
	// All bytes are durably on disk in temp files now. Back up + rename each
	// into place. Renames are atomic and the least-likely op to fail.
	for _, st := range stagedAll {
		// Backup-on-save: copy the prior bytes to <name>.bak before the rename
		// replaces them. Skip when there's no existing target (a new file has
		// nothing to back up). A backup failure is fatal — better to refuse than
		// to overwrite without a recoverable prior version.
		prior, perr := os.ReadFile(st.dstPath)
		switch {
		case perr == nil:
			// IDEMPOTENT-RETRY GUARD: if the target already holds exactly the bytes
			// we're staging, this file committed on a PRIOR attempt (a forced retry
			// after a rare mid-COMMIT rename failure) or is simply unchanged. Backing
			// it up now would copy the POST-save bytes over the genuine pre-save
			// <name>.bak, destroying the only recoverable original — and the rename
			// would be a no-op. Skip both; drop our now-redundant temp.
			if bytes.Equal(prior, st.data) {
				_ = os.Remove(st.tmpPath)
				continue
			}
			if berr := os.WriteFile(st.dstPath+BackupSuffix, prior, 0o644); berr != nil {
				return fmt.Errorf("commit %q: write backup: %w", st.name, berr)
			}
		case errors.Is(perr, os.ErrNotExist):
			// Brand-new file — nothing to back up.
		default:
			return fmt.Errorf("commit %q: read prior for backup: %w", st.name, perr)
		}
		if rerr := os.Rename(st.tmpPath, st.dstPath); rerr != nil {
			return fmt.Errorf("commit %q: atomic replace: %w", st.name, rerr)
		}
	}
	return nil
}

// resolve maps a source-relative name to an absolute on-disk path under the
// folder root, applying the same path-traversal defense folderSource.write used
// before the batch-commit refactor. Shared by write, read, delete, and the
// batch-commit path so WC3 backslash names (`war3mapImported\foo.blp`) resolve
// to a real subdirectory on POSIX, not a filename containing a backslash.
//
// We normalize Windows-style backslashes to forward slashes before cleaning so
// the same code rejects `C:\Windows\…` and `subdir\..\..\evil` on both macOS
// and Windows. path.Clean (POSIX, '/'-only) doesn't treat '\' as a separator,
// so without this normalization a backslash-traversal string survives Clean
// untouched and the prefix / ".." checks all miss it. The Windows drive-letter
// form (`X:`) is rejected explicitly because it's "absolute" on Windows but not
// according to POSIX filepath.IsAbs.
func (f folderSource) resolve(name string) (string, error) {
	clean := path.Clean(strings.ReplaceAll(name, `\`, "/"))
	switch {
	case strings.HasPrefix(clean, "/"),
		strings.HasPrefix(clean, ".."),
		strings.Contains(clean, "/.."),
		len(clean) >= 2 && clean[1] == ':':
		return "", fmt.Errorf("write %q: unsafe path", name)
	}
	return filepath.Join(f.root, filepath.FromSlash(clean)), nil
}
