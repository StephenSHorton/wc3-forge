// Package mpqchain resolves STOCK Warcraft III assets from a Classic
// (pre-Reforged, MPQ-based) install's layered patch-archive chain.
//
// Reforged / modern WC3 ships stock assets via CASC (see internal/casc).
// Classic installs (1.27b-era, and the last MPQ patch generally) instead
// layer assets across a chain of MPQ archives where LATER patch archives
// override EARLIER base ones:
//
//	War3.mpq        (base — Reign of Chaos data)
//	War3x.mpq       (The Frozen Throne expansion data)
//	War3xLocal.mpq  (localized strings/textures)
//	War3Patch.mpq   (the cumulative patch — OVERRIDES everything above)
//
// The override rule is load-bearing: a file present in War3Patch.mpq must
// win over the same path in War3.mpq. We model this as an ordered slice of
// archives sorted HIGHEST-priority first, and ReadFile returns the first
// archive that has the file.
//
// This package is pure Go (no FFI) — it reuses internal/formats/mpq, the
// same reader used for map archives. Classic stock models/textures/SLKs use
// only ZLIB + PKWARE Implode, both supported by that reader; the
// unsupported compressions (Huffman/ADPCM/BZIP2/LZMA) are audio-only and
// the renderer never loads them. A per-archive decompress error is treated
// as "try the next archive / miss" rather than propagated, so one bad file
// can't 500 the whole asset handler.
//
// A Chain satisfies the same shape the asset handler uses for casc.Storage
// (ReadFile / ListByPrefix / SetReforged / Reforged / Close), so it slots
// in behind the existing resolution path without reordering the handler's
// "map source exhausted before stock" loops.
package mpqchain

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/StephenSHorton/wc3-forge/internal/formats/mpq"
)

// classicArchives lists the standard Classic WC3 patch chain in LOWEST-to-
// HIGHEST precedence order. Open reverses this so the in-memory slice is
// highest-first; ReadFile then returns the first hit.
//
// War3Patch.mpq is the cumulative patch and must override the base/expansion
// archives — it is therefore last here (highest precedence). Localized data
// (War3xLocal.mpq) sits between the expansion and the patch, matching how
// the engine layers them. Names are matched case-insensitively against the
// install root's directory listing, so "war3patch.mpq" or "War3Patch.MPQ"
// both resolve.
var classicArchives = []string{
	"War3.mpq",
	"War3x.mpq",
	"War3xLocal.mpq",
	"War3Patch.mpq",
}

// archiveReader is the subset of *mpq.Archive that Chain depends on. Keeping
// it as an interface (rather than a concrete *mpq.Archive slice) lets tests
// inject fakes to exercise paths a real on-disk archive can't easily produce
// — notably the "present but unreadable" read-error fall-through in ReadFile.
// *mpq.Archive satisfies it.
type archiveReader interface {
	Has(name string) bool
	Read(name string) ([]byte, error)
	List() []string
	Close() error
}

// Chain is an open, ordered set of Classic-install MPQ archives. archives is
// sorted HIGHEST-priority first (War3Patch.mpq before War3.mpq), so ReadFile
// can return the first archive that holds a given path.
//
// Safe for concurrent use after Open: ReadFile/Has/Read go through the
// archive's io.ReaderAt (itself concurrent-safe) and the read-only hash
// table, and ListByPrefix → mpq.Archive.List() guards its lazy listfile
// cache. The reforged flag is guarded by mu.
type Chain struct {
	archives []archiveReader
	// names parallels archives for diagnostic logging.
	names []string

	mu sync.Mutex
	// reforged mirrors casc.Storage's flag so the interface is satisfied.
	// Classic installs are SD-only, so this is effectively a no-op for
	// resolution — kept so callers don't special-case the chain.
	reforged bool
}

// IsClassicInstall reports whether installRoot looks like a Classic (MPQ-
// based) WC3 install: it lacks a CASC ".build.info" but contains at least
// one of the recognized stock MPQ archives (War3.mpq / War3Patch.mpq).
//
// The .build.info exclusion keeps a Reforged/modern install (which is CASC,
// and which wc3-forge already handles) from being misclassified even if it
// happens to retain a stray .mpq.
func IsClassicInstall(installRoot string) bool {
	if installRoot == "" {
		return false
	}
	if info, err := os.Stat(filepath.Join(installRoot, ".build.info")); err == nil && !info.IsDir() {
		return false
	}
	return hasArchive(installRoot, "War3.mpq") || hasArchive(installRoot, "War3Patch.mpq")
}

// hasArchive reports whether installRoot contains a file named like archive
// (case-insensitive). Returns false on any directory-read failure.
func hasArchive(installRoot, archive string) bool {
	return findArchive(installRoot, archive) != ""
}

// findArchive returns the absolute path of the file in installRoot whose name
// case-insensitively equals archive, or "" if none exists. Classic installs
// on case-sensitive filesystems (Wine prefixes on Linux, macOS) can store the
// archive as "war3.mpq" while Windows uses "War3.mpq"; matching
// case-insensitively keeps both working.
func findArchive(installRoot, archive string) string {
	entries, err := os.ReadDir(installRoot)
	if err != nil {
		return ""
	}
	want := strings.ToLower(archive)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.ToLower(e.Name()) == want {
			return filepath.Join(installRoot, e.Name())
		}
	}
	return ""
}

// Open mounts the Classic MPQ patch chain at installRoot. Archives are opened
// in highest-precedence-first order (War3Patch.mpq before War3.mpq). Missing
// archives are skipped silently; Open errors only if NONE of the known
// archives exist (i.e. installRoot is not a Classic install).
//
// CascLib's storage open is expensive; the same is true here (each archive
// parses its hash + block tables). Call once at startup, reuse for the
// process lifetime, Close on shutdown.
func Open(installRoot string) (*Chain, error) {
	c := &Chain{}
	// Walk LOWEST-to-HIGHEST and prepend, leaving archives highest-first.
	for _, name := range classicArchives {
		p := findArchive(installRoot, name)
		if p == "" {
			continue
		}
		a, err := mpq.Open(p)
		if err != nil {
			// A corrupt or unreadable archive shouldn't abort the whole
			// chain — log and continue so the remaining archives still
			// serve assets.
			log.Printf("mpqchain: skipping %q: %v", p, err)
			continue
		}
		// Prepend so War3Patch.mpq (opened last) ends up first.
		c.archives = append([]archiveReader{a}, c.archives...)
		c.names = append([]string{name}, c.names...)
	}
	if len(c.archives) == 0 {
		return nil, fmt.Errorf("mpqchain: no Classic WC3 MPQ archives found in %q", installRoot)
	}
	return c, nil
}

// ReadFile fetches one stock file by its WC3-relative path (e.g.
// "units/human/footman/footman.mdx", forward- or backslash-separated). It
// walks archives in priority order (patch first) and returns the first hit,
// preserving the patch-overrides-base rule. Returns (nil, false, nil) on a
// total miss — matching casc.Storage's contract so the asset handler's
// "map exhausted first, then stock" ordering is preserved byte-for-byte.
//
// A per-archive read error (e.g. a sector compressed with an unsupported
// codec) is logged and treated as "not in this archive": resolution
// continues to lower-priority archives rather than failing the request.
func (c *Chain) ReadFile(name string) ([]byte, bool, error) {
	for i, a := range c.archives {
		if !a.Has(name) {
			continue
		}
		data, err := a.Read(name)
		if err != nil {
			log.Printf("mpqchain: %s present in %s but unreadable: %v", name, c.names[i], err)
			continue
		}
		return data, true, nil
	}
	return nil, false, nil
}

// ListByPrefix returns the union of every archive's listed entries whose
// lowercased, forward-slash name starts with prefix. Names are de-duped
// across archives and returned lowercased + forward-slash, mirroring
// casc.Storage.ListByPrefix so command-button / type-index enumeration
// behaves identically regardless of which stock source is mounted.
//
// Note: MPQ archives only enumerate the names present in their internal
// (listfile) plus the standard WC3 file set (see mpq.Archive.List). Classic
// archives carry a full (listfile), so prefix enumeration of e.g.
// "replaceabletextures/commandbuttons/" works; an archive lacking a listfile
// simply contributes nothing here, exactly as a CASC install without a
// listfile would.
func (c *Chain) ListByPrefix(prefix string) ([]string, error) {
	prefixLC := strings.ToLower(prefix)
	seen := make(map[string]struct{})
	var out []string
	for _, a := range c.archives {
		for _, n := range a.List() {
			lname := strings.ToLower(strings.ReplaceAll(n, `\`, "/"))
			if !strings.HasPrefix(lname, prefixLC) {
				continue
			}
			if _, dup := seen[lname]; dup {
				continue
			}
			seen[lname] = struct{}{}
			out = append(out, lname)
		}
	}
	sort.Strings(out)
	return out, nil
}

// SetReforged records the HD/SD preference. Classic installs are SD-only so
// this does not change resolution; it exists to satisfy the stock-source
// interface and keep callers uniform. Concurrent-safe.
func (c *Chain) SetReforged(b bool) {
	c.mu.Lock()
	c.reforged = b
	c.mu.Unlock()
}

// Reforged returns the recorded HD/SD preference.
func (c *Chain) Reforged() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reforged
}

// SetTileset is a no-op on Classic MPQ installs (no per-tileset mounts).
// It exists to satisfy the stock-source interface.
func (c *Chain) SetTileset(byte) {}

// Close releases every mounted archive. Idempotent; safe to call more than
// once (it nils the archive slice, so a second call is a no-op).
func (c *Chain) Close() error {
	var firstErr error
	for _, a := range c.archives {
		if err := a.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	c.archives = nil
	c.names = nil
	return firstErr
}
