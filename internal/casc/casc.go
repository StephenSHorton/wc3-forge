// Package casc reads files from a local Warcraft III install's CASC storage.
//
// The Blizzard CASC format is non-trivial — multi-stage indirection from
// game-relative paths → content keys → encoding keys → archive offsets,
// plus the BLTE block-level encoding inside each archive. Rather than
// porting all of it to Go (a 2-3k LOC project), we dlopen Ladislav
// Zezula's battle-tested CascLib (same library HiveWE's scripts use for
// StormLib).
//
// We bind CascLib at runtime, not via cgo: no toolchain needed at build
// time, and the same high-level logic works on Windows (CascLib.dll) and
// macOS (libcasc.dylib). The library must sit next to the executable, or
// anywhere on the OS's dynamic loader search path. The project's
// scripts/casclib/ holds the vendored Windows binaries; the macOS .dylib is
// built on demand by scripts/build-casclib-macos.sh and is .gitignored.
//
// The FFI layer is platform-split (see casc_windows.go / casc_unix.go):
// macOS calls through purego, Windows calls through syscall.LazyProc.Call
// exactly as pre-PR main did. They are NOT unified — routing CascOpenFile /
// CascReadFile through purego's Windows SyscallN dispatch regressed every
// per-file open to a GetLastError==0 failure (pure-white, texture-less
// models). syscall.LazyProc.Call is //go:uintptrescapes, so it is
// memory-safe AND the path that demonstrably worked on Windows. The shared
// code below calls into platform raw-op helpers (rawOpenStorage, rawOpenFile,
// rawReadFile, rawFind*, …) rather than a common set of foreign func vars.
//
// Thread-safety: a single Storage's operations are serialised by a mutex.
// CascLib's storage handle is shared across reads internally; we keep
// concurrent callers single-file rather than reason about its internals.
package casc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Tiny indirection so locateLib stays readable without importing
// "os"+"path/filepath" in three places.
var (
	osExecutable = os.Executable
	osGetwd      = os.Getwd
	osStat       = os.Stat
	filepathDir  = filepath.Dir
)

// DLLPath, if non-empty, overrides the auto-locate. Set this before any
// Open call to point at a specific CascLib library (e.g. for tests).
// Despite the name, this is the path to the shared library on any OS —
// CascLib.dll on Windows, libcasc.dylib on macOS.
var DLLPath string

// CascLib is loaded once at process startup. bindCASCLib (platform-specific)
// loads the shared library at `path` and binds every CascLib entry point;
// subsequent FFI goes through the platform raw-op helpers, not a shared set
// of foreign func vars. libErr captures a load/bind failure for Open and
// ListByPrefix to surface.
var (
	libOnce sync.Once
	libErr  error
)

// CASC_FIND_DATA is a fixed-layout struct CascLib fills during enumeration
// (CascFindFirstFile/NextFile). findDataSize is its total size from
// CascLib.h; szFileName begins at findDataNameOff and is findDataNameLen
// (MAX_PATH) bytes wide. ListByPrefix reads the name straight out of that
// offset rather than mapping the whole struct.
const (
	findDataSize    = 0x1108
	findDataNameOff = 0x18
	findDataNameLen = 0x400
)

func loadLib() {
	path := DLLPath
	if path == "" {
		path = locateLib()
	}
	libErr = bindCASCLib(path)
}

// locateLib searches a few standard places for the CASC shared library.
// Production wc3-forge ships it alongside the executable; this helper also
// covers `go test` (where the test binary lives in a temp dir) by walking
// up to the repo's scripts/casclib/ for development convenience.
//
// libBaseName + the OS-specific filepath separator come from libname_*.go.
func locateLib() string {
	name := libFileName
	sep := string(filepath.Separator)
	candidates := []string{name} // current dir / OS loader search path

	if exe, err := osExecutable(); err == nil {
		candidates = append(candidates, filepathDir(exe)+sep+name)
	}
	if cwd, err := osGetwd(); err == nil {
		// scripts/casclib/<libFileName> relative to the project root.
		// Walk up a few levels because `go test` runs from the package dir.
		for i, p := 0, cwd; i < 5; i, p = i+1, filepathDir(p) {
			candidates = append(candidates, filepath.Join(p, "scripts", "casclib", name))
		}
	}

	for _, c := range candidates {
		if _, err := osStat(c); err == nil {
			return c
		}
	}
	// Fall through to the bare name and let the OS loader find it
	// (or fail with a clear "not found" at first call).
	return name
}

// Storage is an open handle to a CASC install (Warcraft III, in our case).
type Storage struct {
	handle uintptr
	mu     sync.Mutex
	// reforged, when true, reorders the CASC mount-prefix search list so
	// `_hd.w3mod:` is checked BEFORE `war3.w3mod:`. This matters because the
	// same logical path (e.g. "units/human/footman/footman.mdx") can resolve
	// to either the SD or HD asset depending on which mount we hit first.
	// Default (false) prefers SD assets. Use SetReforged to flip.
	reforged bool
	// tileset is the current map's w3e tileset letter ('L', 'A', 'X', …).
	// When non-zero, ReadFile tries the per-tileset CASC mounts
	// (`_tilesets/<letter>.w3mod:`) that Reforged 2.0.3+ uses for terrain
	// art — including animated water frames — before the generic mounts.
	// Zero means "no map loaded"; prefixes match the legacy lists.
	tileset byte
}

// SetReforged sets the HD/SD preference. Concurrent-safe.
func (s *Storage) SetReforged(b bool) {
	s.mu.Lock()
	s.reforged = b
	s.mu.Unlock()
}

// Reforged returns the current HD/SD preference.
func (s *Storage) Reforged() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reforged
}

// SetTileset records the current map's tileset letter so ReadFile can try
// `_tilesets/<letter>.w3mod:` mounts. Pass 0 to clear (no map loaded).
// Concurrent-safe.
func (s *Storage) SetTileset(ts byte) {
	s.mu.Lock()
	s.tileset = ts
	s.mu.Unlock()
}

// Tileset returns the current map tileset letter, or 0 if none is set.
func (s *Storage) Tileset() byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tileset
}

// Open returns a Storage for the install at the given path. The path
// should be the install ROOT (the directory containing .build.info),
// e.g. "C:\\Program Files (x86)\\Warcraft III" or
// "/Applications/Warcraft III". A CascLib product suffix is appended
// automatically — `:w3` for the retail WC3 product. Without the suffix,
// multi-product installs can open a storage handle that returns 0 bytes
// for every file. CascLib's storage open is expensive (scans the archive
// index files) — call once at startup, reuse for the process lifetime,
// Close on shutdown.
func Open(installPath string) (*Storage, error) {
	libOnce.Do(loadLib)
	if libErr != nil {
		return nil, fmt.Errorf("load CASC library: %w", libErr)
	}

	// HiveWE passes the path WITH a trailing :w3 product specifier (e.g.
	// "C:\\Program Files (x86)\\Warcraft III\\:w3"). The separator before
	// :w3 is what their std::filesystem path-append produced; CascLib's
	// parser understands the :<product> tail either way. We use the
	// platform-native separator so the path looks natural to the user
	// in error messages.
	fullPath := installPath + string(filepath.Separator) + ":w3"

	// IMPORTANT: dwLocaleMask = 0 means "NO locales accessible"; storage
	// opens fine but every file read returns 0 bytes. CASC_LOCALE_ALL
	// (0xFFFFFFFF) opens every locale. HiveWE's casc.ixx uses the same.
	// rawOpenStorage does the platform-appropriate path encoding (UTF-16 on
	// Windows, UTF-8 on macOS) and keeps the buffer alive across the call.
	const cascLocaleAll uint32 = 0xFFFFFFFF
	handle, ok := rawOpenStorage(fullPath, cascLocaleAll)
	if !ok {
		// CascLib reports the cause through GetLastError (Windows) or
		// errno (POSIX). We surface whichever the platform-specific
		// lastError() returns plus the path so the caller can diagnose.
		return nil, fmt.Errorf("CascOpenStorage failed for %q (err=%d)", installPath, lastError())
	}
	return &Storage{handle: handle}, nil
}

// Close releases the storage. Idempotent.
func (s *Storage) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle == 0 {
		return nil
	}
	ok := rawCloseStorage(s.handle)
	s.handle = 0
	if !ok {
		return fmt.Errorf("CascCloseStorage failed (err=%d)", lastError())
	}
	return nil
}

// ReadFile fetches one file by its WC3-relative path (e.g.
// "units/human/footman/footman.mdx" or its backslash equivalent).
// We try CASC mount prefixes in order (see mountPrefixes): optional
// per-tileset `_tilesets/<letter>.w3mod:` mounts, then `war3.w3mod:`
// (the main stock-asset mount), then localized + HD variants. Returns
// (nil, false, nil) if the name isn't found in any mount.
//
// Why prefixes: WC3's CASC organizes assets via TVFS (TACT virtual
// file system). The same logical path can live under multiple .w3mod
// "mounts" (e.g. `war3.w3mod:` for shared SD, `en.w3mod:` for English
// localized strings/textures, `_hd.w3mod:` for the Reforged HD pack).
// CascOpenFile of a bare path returns a fake-success 0-byte handle
// instead of an error — so we explicitly try each prefix and accept
// the first non-zero response.
func (s *Storage) ReadFile(name string) ([]byte, bool, error) {
	// Normalize to backslash-style; CASC stores paths with `\`.
	bs := strings.ReplaceAll(name, "/", "\\")
	s.mu.Lock()
	prefixes := mountPrefixes(s.reforged, s.tileset)
	s.mu.Unlock()
	for _, prefix := range prefixes {
		full := prefix + bs
		data, ok, err := s.openOne(full)
		if err != nil {
			return nil, false, err
		}
		if ok && len(data) > 0 {
			return data, true, nil
		}
	}
	return nil, false, nil
}

// tilesetMountLetter lowercases a w3e tileset byte to the letter CASC uses
// in `_tilesets/<letter>.w3mod:` (listfile entries are lowercase). Returns
// empty when ts is unset or not an ASCII letter.
func tilesetMountLetter(ts byte) string {
	if ts >= 'A' && ts <= 'Z' {
		ts += 'a' - 'A'
	}
	if ts < 'a' || ts > 'z' {
		return ""
	}
	return string([]byte{ts})
}

// mountPrefixes is the ordered CASC TVFS prefix list for one ReadFile.
// tileset==0 reproduces sdCascPrefixes / hdCascPrefixes exactly so
// pre-map-load lookups and existing tests stay byte-identical.
//
// Reforged 2.0.3+ stores tileset-specific terrain art (ground textures,
// cliff textures, animated water frames) under
// `war3.w3mod:_hd.w3mod:_tilesets/<letter>.w3mod:` (HD) and
// `war3.w3mod:_tilesets/<letter>.w3mod:` (SD). HiveWE's hierarchy.ixx
// tries those mounts first; without them, water/terrain requests fall
// through to a generic (or missing) file and the viewport looks dry even
// though war3map.w3e still has the water flag set.
func mountPrefixes(reforged bool, tileset byte) []string {
	base := sdCascPrefixes
	if reforged {
		base = hdCascPrefixes
	}
	letter := tilesetMountLetter(tileset)
	if letter == "" {
		return base
	}
	hdTs := "war3.w3mod:_hd.w3mod:_tilesets/" + letter + ".w3mod:"
	sdTs := "war3.w3mod:_tilesets/" + letter + ".w3mod:"
	head := []string{sdTs, hdTs}
	if reforged {
		head = []string{hdTs, sdTs}
	}
	out := make([]string, 0, 2+len(base))
	out = append(out, head...)
	out = append(out, base...)
	return out
}

// sdCascPrefixes are the CASC mount paths we try in SD (Classic) mode.
// `war3.w3mod:` (the main shared SD-asset mount) is tried first; HD is
// available as a fallback for assets that only exist HD-side (e.g. some
// Reforged-only TeamColor textures).
var sdCascPrefixes = []string{
	"war3.w3mod:",
	"war3.w3mod:_hd.w3mod:",
	"war3.w3mod:_locales\\enus.w3mod:",
	"war3.w3mod:_deprecated.w3mod:",
}

// hdCascPrefixes are the CASC mount paths we try in HD (Reforged) mode.
// `_hd.w3mod:` first so HD models, materials and textures are preferred
// over their SD siblings when both exist under the same logical path.
var hdCascPrefixes = []string{
	"war3.w3mod:_hd.w3mod:",
	"war3.w3mod:",
	"war3.w3mod:_locales\\enus.w3mod:",
	"war3.w3mod:_deprecated.w3mod:",
}

// cascPrefixes is retained as the SD default for back-compat with callers
// (and the test suite) that referenced the old name. New code should use
// SetReforged + ReadFile instead of consulting this slice directly.
//
// Deprecated: use the Storage's reforged-aware ReadFile.
var cascPrefixes = sdCascPrefixes

// ListByPrefix returns every CASC entry whose lowercased name starts with
// prefix (forward-slash form). prefix MUST end with a "/" — e.g.
// "replaceabletextures/commandbuttons/". The returned names are lowercased +
// forward-slash. Backed by CascFindFirstFile/CascFindNextFile; expensive
// (~10-15ms for a full enumeration on a modern CASC install) so callers
// should cache the result.
//
// Returns an empty slice + nil error when no entries match; a real error
// only when CASC enumeration fails outright (storage closed, library load
// failure).
func (s *Storage) ListByPrefix(prefix string) ([]string, error) {
	libOnce.Do(loadLib)
	if libErr != nil {
		return nil, fmt.Errorf("load CASC library: %w", libErr)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle == 0 {
		return nil, fmt.Errorf("storage closed")
	}

	// rawFindFirstFile/NextFile do the platform FFI and fill `data` (a
	// CASC_FIND_DATA; see findData* consts). We read the szFileName field
	// back out of the fixed offset here.
	var data [findDataSize]byte
	hFind := rawFindFirstFile(s.handle, "*", &data)
	if hFind == 0 || hFind == ^uintptr(0) {
		// Empty storage or no listfile entries — treat as "no matches" rather
		// than an error. CascLib doesn't distinguish these.
		return nil, nil
	}
	defer rawFindClose(hFind)

	prefixLC := strings.ToLower(prefix)
	out := make([]string, 0, 256)
	// Hard cap to avoid runaway loops on a corrupt storage. Real CASC
	// installs have ~200k entries; we'll never legitimately need more.
	for n := 0; n < 500_000; n++ {
		name := readCStringFromBuf(data[findDataNameOff : findDataNameOff+findDataNameLen])
		// CASC stores paths with backslashes; normalize before the prefix
		// check.
		lname := strings.ToLower(strings.ReplaceAll(name, `\`, "/"))
		if strings.HasPrefix(lname, prefixLC) {
			out = append(out, lname)
		}
		if !rawFindNextFile(hFind, &data) {
			break
		}
	}
	return out, nil
}

// readCStringFromBuf returns the bytes up to (but excluding) the first NUL
// in b. Used by ListByPrefix to extract the szFileName field from a
// CASC_FIND_DATA struct.
func readCStringFromBuf(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// openOne does the raw open-read-close for a single fully-qualified
// CASC path. Caller assembles the path.
//
// The file name is `const void *` in CascLib's signature; for string
// lookups it's a null-terminated ASCII (or UTF-8) C-string regardless of OS
// — even on Windows where the storage path uses TCHAR, file names are
// narrow. rawOpenFile passes the UTF-8 bytes directly on both platforms.
func (s *Storage) openOne(name string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle == 0 {
		return nil, false, fmt.Errorf("storage closed")
	}

	fileHandle, ok := rawOpenFile(s.handle, name)
	if !ok {
		// ERROR_FILE_NOT_FOUND (2) and ERROR_PATH_NOT_FOUND (3) are the
		// common "not in CASC" cases on Windows; ENOENT (2) on POSIX
		// collapses to the same numeric value. Anything else is
		// unexpected and worth surfacing.
		errno := lastError()
		if errno == 2 || errno == 3 {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("CascOpenFile(%q) failed (err=%d)", name, errno)
	}
	defer rawCloseFile(fileHandle)

	size64, ok := rawGetFileSize(fileHandle)
	if !ok {
		return nil, false, fmt.Errorf("CascGetFileSize64 failed for %q (err=%d)", name, lastError())
	}
	if size64 == 0 {
		return []byte{}, true, nil
	}
	if size64 > 1<<32 {
		// We do single-shot reads with uint32 length parameter to
		// CascReadFile; >4GiB game assets aren't a thing, but guard
		// anyway so we don't truncate silently.
		return nil, false, fmt.Errorf("CASC file %q too large (%d bytes)", name, size64)
	}

	buf := make([]byte, uint32(size64))
	n, ok := rawReadFile(fileHandle, buf)
	if !ok {
		return nil, false, fmt.Errorf("CascReadFile(%q) failed (err=%d)", name, lastError())
	}
	return buf[:n], true, nil
}
