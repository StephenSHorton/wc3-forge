// Package forge is the editor core: the currently-loaded map (Session) and
// the MCP handlers that read/write it. The Session is a singleton protected
// by an RWMutex; bridge handlers run on per-connection goroutines and may
// touch the session concurrently.
package forge

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/StephenSHorton/wc3-forge/internal/formats/doodadsdoo"
	"github.com/StephenSHorton/wc3-forge/internal/formats/imp"
	"github.com/StephenSHorton/wc3-forge/internal/formats/miscdata"
	"github.com/StephenSHorton/wc3-forge/internal/formats/mpq"
	"github.com/StephenSHorton/wc3-forge/internal/formats/shd"
	"github.com/StephenSHorton/wc3-forge/internal/formats/unitsdoo"
	"github.com/StephenSHorton/wc3-forge/internal/formats/w3c"
	"github.com/StephenSHorton/wc3-forge/internal/formats/w3e"
	"github.com/StephenSHorton/wc3-forge/internal/formats/w3i"
	"github.com/StephenSHorton/wc3-forge/internal/formats/w3objmod"
	"github.com/StephenSHorton/wc3-forge/internal/formats/w3r"
	"github.com/StephenSHorton/wc3-forge/internal/formats/wct"
	"github.com/StephenSHorton/wc3-forge/internal/formats/wpm"
	"github.com/StephenSHorton/wc3-forge/internal/formats/wtg"
	"github.com/StephenSHorton/wc3-forge/internal/formats/wts"
)

// fileSource abstracts "where does a file's bytes come from" so the same
// Open code path covers both folder-based extracted maps and MPQ-backed
// .w3x / .w3m / .mpq files.
type fileSource interface {
	// read returns the file's bytes. ok=false + nil error means "the file
	// isn't present" (the caller decides whether that's fatal). A non-nil
	// error means a real I/O / format problem.
	read(name string) (data []byte, ok bool, err error)
	// write replaces (or creates) the named file's bytes in this source.
	// Folder sources write straight to disk; MPQ sources BUFFER the write
	// in memory and commit it during flush() (a single atomic repack).
	write(name string, data []byte) error
	// delete removes the named file from the source. Returns nil if the
	// file is already absent (idempotent — used by Convert-to-Lua to drop
	// war3map.j after the .lua replacement has been written). MPQ sources
	// buffer the tombstone and apply it during flush().
	delete(name string) error
	// flush commits any buffered changes durably. Folder sources write
	// eagerly so their flush is a no-op; MPQ sources repack the whole
	// archive and atomically replace the .w3x on disk. Save calls flush
	// exactly once after the per-file write loop.
	flush() error
	// close releases any open handles. Safe to call once at end of Open.
	close() error
}

// ErrMPQRepackFailed is the sentinel wrapping the unusual fallback path where
// an MPQ-backed source cannot repack the archive for a reason the pure-Go MPQ
// writer (see internal/formats/mpq's write.go) can't recover from — e.g. no
// path, no open archive, or a BuildLossless failure. The common path WRITES
// the archive successfully; this only fires on a genuine repack failure, and
// the wrapped message describes the specific cause. external errors.Is checks
// rely on this sentinel.
var ErrMPQRepackFailed = errors.New("MPQ archive repack failed")

type folderSource struct{ root string }

func (f folderSource) read(name string) ([]byte, bool, error) {
	b, err := os.ReadFile(filepath.Join(f.root, name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return b, true, nil
}

// write replaces (or creates) the named file under f.root. Path traversal is
// defended against via filepath.Clean — name comes from Session's own code
// today but plumb safely in case future callers route untrusted strings here.
//
// We normalize Windows-style backslashes to forward slashes before cleaning
// so the same code rejects `C:\Windows\…` and `subdir\..\..\evil` on both
// macOS and Windows. filepath.Clean on POSIX doesn't treat `\` as a
// separator (only `/`), so without this normalization a backslash-traversal
// string survives Clean untouched and the IsAbs / `..` checks all miss it.
// The Windows drive-letter form (`X:`) is rejected explicitly because it's
// "absolute" on Windows but not according to POSIX filepath.IsAbs.
func (f folderSource) write(name string, data []byte) error {
	dst, err := f.resolve(name) // path-traversal defense (see atomic_save.go)
	if err != nil {
		return err
	}
	// Create any intermediate directories so writes into subdirectories work.
	// Model imports write under war3mapImported\, which won't exist in a
	// freshly-extracted folder map; without this the write fails with "cannot
	// find the path". No-op when the parent already exists.
	if dir := filepath.Dir(dst); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("write %q: mkdir: %w", name, err)
		}
	}
	// Atomic temp+fsync+rename — this is the user's PRIMARY copy, so a truncating
	// os.WriteFile could leave war3map.w3e / Units.doo half-written + unparseable
	// on a crash/power-loss/disk-full mid-write. Protects the DIRECT writers
	// (ImportModel, Convert-to-Lua, SaveTriggerScript, sky); Save's batch commit
	// adds all-or-nothing + backup on top for the dirty-file set.
	return mpq.WriteFileAtomic(dst, data)
}

// delete removes the named file under f.root. Returns nil if the file is
// absent — callers (Convert-to-Lua) treat the operation as idempotent.
// Same path-traversal defense as write.
func (f folderSource) delete(name string) error {
	clean := filepath.Clean(name)
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") || strings.Contains(clean, string(filepath.Separator)+"..") {
		return fmt.Errorf("delete %q: unsafe path", name)
	}
	err := os.Remove(filepath.Join(f.root, clean))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// flush is a no-op for folder sources — write/delete already hit disk.
func (f folderSource) flush() error { return nil }

func (f folderSource) close() error { return nil }

// mpqSource is an MPQ-backed file source. Reads come straight from the open
// archive; writes/deletes are BUFFERED in memory (pending/deleted) and applied
// in one atomic repack when flush() runs (see mpq_write_source.go). path is the
// on-disk .w3x location flush() repacks over.
//
// Pointer receiver: the pending/deleted maps + archive handle are mutated by
// write/delete/flush, so the source must be a single shared instance, not a
// value copy. mu guards ALL of those fields because ReadFile reads the source
// concurrently (on bridge goroutines) with a Save-driven flush — see read().
type mpqSource struct {
	mu      sync.Mutex
	a       *mpq.Archive
	path    string
	pending map[string][]byte // name -> new bytes (overrides the archive)
	deleted map[string]bool   // name -> tombstone
}

func newMPQSource(a *mpq.Archive, path string) *mpqSource {
	return &mpqSource{
		a:       a,
		path:    path,
		pending: make(map[string][]byte),
		deleted: make(map[string]bool),
	}
}

func (m *mpqSource) read(name string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Pending writes shadow the archive; tombstones shadow both.
	key := mpqNameKey(name)
	if b, ok := m.pending[key]; ok {
		return append([]byte(nil), b...), true, nil
	}
	if m.deleted[key] {
		return nil, false, nil
	}
	// m.a can be nil only in the degenerate no-archive construction (tests) or
	// transiently if a repack reopen failed; treat as "not present" so callers
	// degrade gracefully rather than panicking.
	if m.a == nil || !m.a.Has(name) {
		return nil, false, nil
	}
	b, err := m.a.Read(name)
	if err != nil {
		// mapIO diagnostics: an archive read that failed (decompress / corrupt
		// block). Previously surfaced only to the immediate caller; the counter
		// makes a flaky source observable in diagnostics.get.
		mapIODiag.mpqReadErrors.Add(1)
		return nil, false, err
	}
	return b, true, nil
}

// write buffers the new bytes; the archive on disk is untouched until flush().
// Buffering (rather than rebuilding the whole MPQ per file) keeps Save's
// per-file write loop cheap and lets one repack commit every change atomically.
func (m *mpqSource) write(name string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := mpqNameKey(name)
	m.pending[key] = append([]byte(nil), data...)
	delete(m.deleted, key)
	return nil
}

// delete buffers a tombstone; applied at flush(). Idempotent.
func (m *mpqSource) delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := mpqNameKey(name)
	m.deleted[key] = true
	delete(m.pending, key)
	return nil
}

func (m *mpqSource) close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.a == nil {
		return nil
	}
	return m.a.Close()
}

// Session holds the currently-loaded map. Phase 1 only supports folder-based
// maps (an extracted .w3x). MPQ-backed opening is deferred to a follow-up.
//
// The Session also owns Selection — the editor's first-class selection state.
// Tools (brushes, UI panels, MCP handlers) READ selection; they never own it.
// Mutations are funneled through SetSelection so listeners get notified.
type Session struct {
	mu      sync.RWMutex
	loaded  bool
	path    string
	source  fileSource // kept open for ReadFile (mdx-m3-viewer pathSolver asks for arbitrary files inside the map MPQ)
	rawMap  []byte     // the raw .w3x bytes if opened from an archive; nil for folder-based opens
	info    *w3i.Info
	units   *unitsdoo.File
	doodads *doodadsdoo.File
	terrain *w3e.File
	// Per-map object-modification tables. The renderer merges these on top
	// of the stock SLK type indices so customs ("D006") and stock-row edits
	// both resolve to a usable MDX. nil if the map doesn't include them.
	//
	// Phase 2b adds abilityMods/buffMods/upgradeMods so the Object Editor's
	// kind-agnostic surface (SetObjectField/AddCustomObject/...) can route
	// through KindConfig.GetMods/SetMods for all seven kinds.
	doodadMods       *w3objmod.File // war3map.w3d
	destructibleMods *w3objmod.File // war3map.w3b
	unitMods         *w3objmod.File // war3map.w3u
	itemMods         *w3objmod.File // war3map.w3t
	abilityMods      *w3objmod.File // war3map.w3a (opt=true)
	buffMods         *w3objmod.File // war3map.w3h
	upgradeMods      *w3objmod.File // war3map.w3q (opt=true)
	// objectSkinMods holds the Reforged "skin" companion tables
	// (war3mapSkin.w3u/.w3t/.w3a/.w3b/.w3d/.w3h/.w3q), keyed by KindConfig.Kind
	// ("units", "items", ...). Reforged's World Editor splits per-map object
	// overrides across two files per kind: war3map.w3* carries gameplay/logic
	// edits, war3mapSkin.w3* carries the art/skin overrides (Name unam, Model
	// File umdl, Icon uico, Tooltip utip). We load these read-only and merge
	// them UNDER the primary shadow (primary wins; skin fills fields the primary
	// doesn't set) so custom units surface their real name + model instead of
	// the base type's. Never edited in place, so Save preserves them verbatim
	// via the lossless copy-through — there is no dirty flag for them.
	objectSkinMods map[string]*w3objmod.File
	shadowMap      *shd.File   // war3map.shd
	pathingMap     *wpm.File   // war3map.wpm
	strings        wts.Strings // war3map.wts, for TRIGSTR_<n> resolution
	// infoTokens maps a Map Info Description field key ("name"/"author"/
	// "description"/"suggestedPlayers") to its ORIGINAL "TRIGSTR_<n>" token,
	// captured at Open before ResolveStrings resolved the field to its display
	// value. Lets a Map Info edit update the referenced wts entry (rather than
	// orphaning it) and lets Save re-inject the token into w3i. nil/empty for a
	// non-localized map. See info_trigstr.go.
	infoTokens map[string]string
	gameplay   *miscdata.File // war3mapMisc.txt — per-map gameplay-constants overrides

	// regions / cameras — OPTIONAL. Phase 2b2 added Parse-only support so the
	// Trigger Editor's gg_rct_*/gg_cam_* entity-name resolver + the region /
	// camera entity pickers have a real source instead of falling back to the
	// raw identifier. Encode is deferred (no Region Editor in scope yet).
	regions *w3r.File // war3map.w3r
	cameras *w3c.File // war3map.w3c

	// imp is the lazily-parsed war3map.imp import table (the registry the World
	// Editor + engine consult for custom files). nil until the first ImportModel
	// call parses the existing table off the source (or starts a fresh one if
	// the map ships none). Cached on the Session so repeated imports in one
	// session don't re-read + re-parse the file every time.
	imp *imp.File

	// Trigger Editor data — Phase 2a (read-write). war3map.wtg is the GUI
	// trigger tree (categories + variables + triggers + ECAs); war3map.wct
	// holds the per-trigger custom-script blobs and the global JASS header.
	// Open binds .wct entries onto their owning Trigger.CustomText via
	// triggers.go's bindCustomTexts. For hand-rolled-script maps (no .wtg,
	// only war3map.lua/.j), the loader synthesizes a synthetic "Map Header"
	// script trigger holding the raw script text.
	//
	// Either can be nil — both are optional. The Trigger Editor handles nil
	// gracefully (empty tree).
	triggers    *wtg.Triggers
	triggersWct *wct.File

	// triggerNextID is the per-session counter the mutators allocate from
	// when creating a new trigger / category / variable. Initialized at Open
	// to max(every existing id) + 1 so synthesized ids never collide with
	// the parsed ones.
	triggerNextID int32

	// triggerIsHandRolled tracks whether the loaded map's triggers were
	// synthesized by loadTriggersForOpen (no wtg, only a war3map.j/.lua).
	// Save uses this to bypass the wtg/wct write path and instead echo the
	// synthetic Map Header script entry's CustomText back into the script
	// file. mapHeaderScriptName carries the chosen filename
	// (war3map.lua | war3map.j).
	triggerIsHandRolled  bool
	mapHeaderScriptName  string
	mapHeaderScriptDirty bool

	selection    SelectionState
	listeners    []func(SelectionState)
	mapListeners []func(bool) // fired after Open/Close; bool = loaded

	// Dirty tracking — per-file granularity. Save iterates these and writes
	// only the dirty files back through the source's write path. Open + Close
	// reset them. The boolean dirty-changed bus mirrors the map/selection
	// notification pattern (lock-free copy → invoke listeners).
	//
	// The PUBLIC dirty-changed event is a single boolean ("any file dirty");
	// per-file flags are the internal granularity that lets Save write only
	// the files that actually changed. Mutator fires dirty=true on the
	// 0→1 transition of (dirtyUnits || dirtyDoodads || dirtyInfo), Save
	// fires dirty=false on the 1→0 transition.
	dirtyUnits    bool
	dirtyDoodads  bool
	dirtyInfo     bool
	dirtyTerrain  bool
	dirtyGameplay bool
	// dirtyStrings tracks pending edits to war3map.wts (the trigger-strings
	// table). Set when a Map Info edit touches a TRIGSTR-backed field on a
	// localized map (the edit updates the referenced wts entry instead of
	// inlining a literal into w3i — see info_trigstr.go). Save writes
	// war3map.wts when set.
	dirtyStrings bool
	// dirtyXMods tracks pending edits to a per-map war3map.w3* shadow (the
	// Object Editor's add-custom / delete-custom / set-field surface). Kept
	// separate from dirtyUnits/dirtyDoodads (which track placed-instance .doo
	// files) so Save only touches the file that actually changed.
	//
	// One flag per kind — added in Phase 2b for the six new kinds. IsDirty
	// + Save iterate every flag; KindConfig.GetDirty/SetDirty wraps the
	// per-kind read/write into a closure so the generic mutator path doesn't
	// need to switch on Kind.
	dirtyUnitMods         bool
	dirtyItemMods         bool
	dirtyAbilityMods      bool
	dirtyBuffMods         bool
	dirtyDestructibleMods bool
	dirtyDoodadMods       bool
	dirtyUpgradeMods      bool
	// dirtyTriggers tracks pending edits to the loaded map's wtg + wct
	// (structural adds/deletes/renames, field-flag toggles, script-text
	// edits). Save flushes both files atomically when set. Hand-rolled-script
	// maps additionally bear mapHeaderScriptDirty for the script-source
	// write path.
	dirtyTriggers bool
	// dirtyImports tracks pending edits to the war3map.imp import table from
	// ImportModel (model + texture registration). The imported byte files
	// themselves go through s.source.write directly (and so are already
	// buffered/durable per the source); this flag only gates the war3map.imp
	// re-encode in Save and the public dirty event. Reset on Open/Close/Save
	// like every other dirty flag.
	dirtyImports bool
	// dirtyRegions tracks pending edits to the loaded map's war3map.w3r regions
	// table (the Region Editor's create/move/resize/delete/rename surface). Save
	// re-encodes the whole table via w3r.Encode when set. Reset on
	// Open/Close/Save like every other dirty flag. Independent of every other
	// file on disk + the dirty bus.
	dirtyRegions   bool
	dirtyListeners []func(bool)

	// Sky-model override: set by the Map Info → Sky picker (or any caller
	// that wants to change the SetSkyModel call). nil = no pending change
	// (use whatever's in war3map.j/.lua already). Non-nil = the desired
	// argument string for the next SetSkyModel call; empty value is a valid
	// "explicitly disable sky" intent and is distinct from nil.
	//
	// Lives outside the dirty<X> family because it doesn't map to a single
	// encoded file — Save handles the script-rewrite path separately.
	pendingSkyModel *string

	// Entity-change bus — fired from any mutator (MoveUnit today; SetRotation,
	// SetType, etc. in the future). Subscribers refresh stale views of the
	// changed entity (Properties panel re-fetches, scene re-positions the
	// model). Mirrors the OnDirtyChanged pattern: slice of listeners, copied
	// under the read lock and invoked outside it so listeners may call back
	// into Session safely. Distinct from OnDirtyChanged because dirty fires
	// only on the first edit per save-cycle (boolean transition), whereas
	// every mutation needs to repaint UI even when the session was already
	// dirty.
	entityListeners []func(EntityChange)

	// Bridge-call observability bus — every MCP handler dispatch fires a
	// BridgeCallEvent through here, so subscribers (the Wails app forwards
	// to the in-page Agent Console) can stream a live log of agent activity.
	// Passive: listeners must not call back into bridge handlers from this
	// callback or they'll recurse. Held under the same lock+copy pattern as
	// the other listener slices.
	bridgeCallListeners []func(BridgeCallEvent)

	// UI-command bus — used by MCP handlers that need to drive JS-side state
	// (View menu visibility, terrain/doodad mode toggle, camera positioning).
	// The App layer subscribes and forwards each command string to the same
	// `wc3-forge:test-command` Wails event the existing test-driver hook
	// already handles. Keeps the bridge layer App-free (no Wails imports in
	// forge.*) while still letting bridge handlers reach UI state.
	uiCommandListeners []func(string)

	// Agent label — free-form short string set by a connected MCP client to
	// describe what that agent is doing in this wc3-forge window. The App
	// layer reads it when building the OS window title so users running
	// multiple wc3-forge instances in parallel can tell them apart at a
	// glance (taskbar + alt-tab list) without having to memorize PIDs.
	// Persists across map opens — the label describes the agent, not the map.
	agentLabel          string
	agentLabelListeners []func(string)

	// Diagnostics cache. The frontend pushes a small JSON snapshot of its live
	// render/camera/GL state here a few times a second (App.ReportDiagnostics);
	// the diagnostics.get MCP handler reads it back so an agent can inspect the
	// running viewport's actual numbers without a screenshot. Own mutex so the
	// frequent writes never contend with the main session lock.
	diagMu   sync.Mutex
	diagJSON string
	diagAtNS int64 // unix nanos of last report; 0 = never reported

	// Undo/redo machinery (history.go). history stores applied commands
	// oldest-first; redoStack holds commands that have been undone and are
	// ready to be re-applied. groupDepth + pendingGroup support transactional
	// commits (gizmo drag wraps N per-entity mutations into one undo step).
	// historyListeners subscribe to stack-mutation events for UI repaints.
	history          []Command
	redoStack        []Command
	groupDepth       int
	pendingGroup     *groupCmd
	historyListeners []func()

	// viewMode is the session's record of the editor pick mode
	// ("terrain" | "doodad"), updated by view.set_mode and read back by
	// view.get_mode. It tracks the last view.set_mode REQUEST, not the live
	// frontend toggle (App.svelte's terrainPickModeOn is authoritative for the
	// renderer). Empty means "never set"; Session.ViewMode reports the
	// frontend's initial "doodad" default in that case. See
	// session_polish_cmd.go for the accessor + the read-back rationale.
	viewMode string

	// doodadVisibility is the session's record of per-category doodad
	// visibility, updated by view.set_doodad_category_visible and read back as
	// that handler's return value. Like viewMode it shadows the last MCP
	// REQUEST, not the live frontend state (App.svelte's doodadVisibility is
	// authoritative for the renderer). A category absent from the map defaults
	// to visible, so GetDoodadCategoryVisible returns true for unknown keys.
	doodadVisibility map[string]bool

	// srcBaseline is the external-change-detection baseline: the on-disk
	// identity (mtime+size) of each source file as of Open (folder sources key
	// by war3map.* name; MPQ sources stamp the single .w3x under mpqArchiveKey).
	// Save re-stats the files it's about to write and refuses with
	// ErrSourceChangedOnDisk if any drifted — another wc3-forge instance, an
	// agent, or a human saved underneath us. Written by recordSourceBaseline at
	// Open, refreshed after each commit, cleared on Close. Guarded by s.mu. See
	// atomic_save.go.
	srcBaseline map[string]fileStamp
}

// EntityChange is the payload for OnEntityChanged. Kind/ID identify which
// entity changed; Field names the conceptual property that moved. Convention:
// one event per changed channel — "position" | "rotation" | "scale" | "transform".
// A single mutator that changes only one channel fires one event with that
// channel's Field tag; the other fields are zero-valued and ignored by
// subscribers that don't care. A future "transform" field may carry all three
// simultaneously if a mutator needs to atomically change position+rotation
// (e.g. orbit around centroid fires two separate events today — one MoveUnit
// + one RotateUnit — which is fine and keeps the event shape self-documenting).
//
// Forward-compatible by design: new mutators add new Field strings; existing
// subscribers branching on Field safely ignore unknown tags.
type EntityChange struct {
	Kind     string     `json:"kind"`     // "unit" | "doodad" | "info" | ...
	ID       uint32     `json:"id"`       // creation_number (per-kind in WC3)
	Field    string     `json:"field"`    // "position" | "rotation" | "scale" | "transform"
	Position [3]float32 `json:"position"` // valid when Field == "position" or "transform"
	Rotation float32    `json:"rotation"` // valid when Field == "rotation" or "transform" (radians, Z-axis only)
	Scale    [3]float32 `json:"scale"`    // valid when Field == "scale" or "transform"
}

// SelectionState is the editor's current selection. Items are entity IDs in
// a kind-agnostic shape — kind+id pairs that resolve through the document.
type SelectionState struct {
	Items   []SelectionItem `json:"items"`
	Primary int             `json:"primary"` // index into Items, or -1 if empty
}

type SelectionItem struct {
	Kind string `json:"kind"` // "unit" | "item" | "doodad" | "region" | "trigger" | ...
	ID   uint32 `json:"id"`   // creation_number for unit/item, opaque per kind
}

// Current is the process-wide singleton session.
var Current = &Session{}

// Open replaces the loaded map with the one at path. path may be:
//   - a directory containing extracted map files (war3map.w3i, etc.), or
//   - an .w3x / .w3m / .mpq archive (HM3W shunt auto-detected).
//
// war3map.w3i is required; everything else is best-effort.
//
// A malformed / truncated / protected map can drive one of the format parsers
// to panic (e.g. an index-out-of-range on a bogus count). On the GUI / --open /
// new-window load path there is no bridge-style recover() wrapping this call,
// so an unrecovered panic vanishes the window. We recover any panic here and
// convert it to a normal error ("map appears corrupt or unsupported: …"),
// mirroring the mesh3d import parsers (parseOBJBuf et al.). Ordinary returned
// errors are left untouched — only panics are converted.
func (s *Session) Open(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("stat %q: %w", abs, err)
	}

	var src fileSource
	var rawMapBytes []byte
	sourceKind := "folder"
	if fi.IsDir() {
		src = folderSource{root: abs}
	} else {
		sourceKind = "mpq"
		// Read the whole .w3x into memory once. mdx-m3-viewer's
		// War3MapViewer.loadMap wants the raw bytes, and we also want
		// the archive open for per-file asset reads via pathSolver.
		rawMapBytes, err = os.ReadFile(abs)
		if err != nil {
			return fmt.Errorf("read %q: %w", abs, err)
		}
		archive, err := mpq.Open(abs)
		if err != nil {
			return fmt.Errorf("open MPQ %q: %w", abs, err)
		}
		src = newMPQSource(archive, abs)
	}
	return s.openWithSource(abs, src, rawMapBytes, sourceKind)
}

// openWithSource is the parse-and-swap body of Open, split out so the panic
// guard wraps every untrusted-byte parser AND so tests can drive it with a
// synthetic fileSource (see TestOpen_RecoversParserPanic). abs is the resolved
// path used for diagnostics + messages; src is the already-constructed source
// (folder or MPQ); rawMapBytes is the raw .w3x bytes for MPQ opens (nil for
// folders); sourceKind is "folder" | "mpq" for the manifest.
func (s *Session) openWithSource(abs string, src fileSource, rawMapBytes []byte, sourceKind string) (err error) {
	// mapIO diagnostics: time the whole Open + collect a per-open manifest of
	// the war3map.* files present and their sizes. recordOpenManifest captures
	// the snapshot after a successful state swap; an error return before that
	// bumps openFails so a failed open isn't silent. See mapio_diag.go.
	openStart := time.Now()
	manifest := &mapOpenManifest{Path: abs, SourceKind: sourceKind}
	opened := false // flipped true after the successful state swap below
	defer func() {
		// Any return before the swap (opened==false) is a failed open — count
		// it so a map that won't load isn't silent in diagnostics.get.
		if !opened {
			mapIODiag.openFails.Add(1)
		}
	}()
	// Panic guard. A parser blowing up on a corrupt/unsupported map must not
	// crash the process — turn it into a returned error the caller (GUI dialog,
	// --open startup, OpenMapInNewWindow child) can surface to the user. Runs
	// before the openFails defer (LIFO) and sets the named return, leaving
	// opened==false so the failed-open counter still ticks. Only overrides err
	// when recover() actually caught a panic. Every untrusted-byte parser below
	// runs before the state swap takes s.mu, so a recovered panic can't leave
	// the session lock held. Mirrors the mesh3d import parsers (parseOBJBuf).
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("map appears corrupt or unsupported: %v", r)
		}
	}()

	// war3map.w3i — REQUIRED.
	w3iBytes, ok, err := src.read("war3map.w3i")
	if err != nil {
		return fmt.Errorf("read war3map.w3i: %w", err)
	}
	if !ok {
		return fmt.Errorf("%q has no war3map.w3i", abs)
	}
	info, err := w3i.Parse(w3iBytes)
	if err != nil {
		return fmt.Errorf("parse war3map.w3i: %w", err)
	}

	// war3map.wts — OPTIONAL trigger strings. Resolve TRIGSTR_<n> on Info.
	wtsStrings, err := readOpt(src, "war3map.wts", wts.Parse)
	if err != nil {
		return err
	}
	// Capture which Description fields are TRIGSTR-backed BEFORE resolving them
	// to display values, so a later Map Info edit can update the referenced wts
	// entry (and Save can re-inject the token) instead of orphaning it.
	infoTokens := captureInfoTokens(info)
	if wtsStrings != nil {
		info.ResolveStrings(wtsStrings.Display)
	}

	// war3mapUnits.doo — OPTIONAL placed units/items.
	units, err := readOpt(src, "war3mapUnits.doo", unitsdoo.Parse)
	if err != nil {
		return err
	}
	if units == nil {
		units = &unitsdoo.File{}
	}

	// war3map.doo — OPTIONAL placed doodads/destructibles.
	doodads, err := readOpt(src, "war3map.doo", doodadsdoo.Parse)
	if err != nil {
		return err
	}
	if doodads == nil {
		doodads = &doodadsdoo.File{}
	}

	// war3map.w3e — OPTIONAL terrain. nil downstream means "can't render".
	terrain, err := readOpt(src, "war3map.w3e", w3e.Parse)
	if err != nil {
		return err
	}

	// war3map.shd — OPTIONAL static shadow map. Dimensions are derived
	// from the terrain we just parsed; depends on `terrain` being non-nil.
	var shadowMap *shd.File
	if terrain != nil {
		shdBytes, ok, err := src.read("war3map.shd")
		if err != nil {
			return fmt.Errorf("read war3map.shd: %w", err)
		}
		if ok {
			sm, perr := shd.Parse(shdBytes, int(terrain.Width), int(terrain.Height))
			if perr != nil {
				// Recoverable: shd.Parse returns a usable zero-fill File along
				// with the warning. Previously this error was swallowed silently;
				// surface it via mapIO diagnostics (count + last-error) and a
				// [WARN] log so a recovered/failed shadow map is observable.
				log.Printf("[WARN] Open: war3map.shd parse failed, recovering with zero-fill: %v", perr)
				recordShadowMapFail(perr.Error())
				if sm != nil {
					shadowMap = sm
				}
			} else {
				shadowMap = sm
			}
		}
	}

	// war3map.wpm — OPTIONAL static pathing map. Independent of terrain
	// dimensions (file declares its own width/height) so we don't gate on
	// `terrain != nil`. Pathing exists for terrainless maps in principle,
	// though every real map ships terrain.
	pathingMap, err := readOpt(src, "war3map.wpm", wpm.Parse)
	if err != nil {
		return err
	}

	// war3map.w3{d,b,u,t,a,h,q} — OPTIONAL object-modification tables. Custom
	// type IDs ("D006") + stock-row edits ("ATtr scale = 1.5") live here.
	// The renderer's type indices + Object Editor apply these on top of the
	// stock SLK. w3d (doodads) and w3a (abilities) and w3q (upgrades) use
	// the opt=true wire form (per-mod level/variation/dataPointer slots);
	// the rest are opt=false.
	parseOpt := func(b []byte) (*w3objmod.File, error) { return w3objmod.Parse(b, true, nil) }
	parseFlat := func(b []byte) (*w3objmod.File, error) { return w3objmod.Parse(b, false, nil) }
	doodadMods, err := readOpt(src, "war3map.w3d", parseOpt)
	if err != nil {
		return err
	}
	destructibleMods, err := readOpt(src, "war3map.w3b", parseFlat)
	if err != nil {
		return err
	}
	unitMods, err := readOpt(src, "war3map.w3u", parseFlat)
	if err != nil {
		return err
	}
	itemMods, err := readOpt(src, "war3map.w3t", parseFlat)
	if err != nil {
		return err
	}
	abilityMods, err := readOpt(src, "war3map.w3a", parseOpt)
	if err != nil {
		return err
	}
	buffMods, err := readOpt(src, "war3map.w3h", parseFlat)
	if err != nil {
		return err
	}
	upgradeMods, err := readOpt(src, "war3map.w3q", parseOpt)
	if err != nil {
		return err
	}

	// war3mapSkin.w3{d,b,u,t,a,h,q} — OPTIONAL Reforged "skin" companion tables.
	// Reforged's World Editor writes the art/skin overrides (Name unam, Model
	// File umdl, Icon uico, Tooltip utip) into these siblings of the plain
	// war3map.w3* tables. Loaded read-only, keyed by kind, and merged UNDER the
	// primary shadow at read time (see MergedObjects / mergeUnitIndex). Same
	// per-kind extension + opt flag as the primary table, derived from each
	// registered KindConfig so this stays in lockstep as kinds are added.
	skinMods := map[string]*w3objmod.File{}
	for _, kc := range kindConfigs {
		skinName := strings.Replace(kc.ShadowFile, "war3map.", "war3mapSkin.", 1)
		parse := parseFlat
		if kc.ShadowOpt {
			parse = parseOpt
		}
		f, err := readOpt(src, skinName, parse)
		if err != nil {
			return err
		}
		if f != nil {
			skinMods[kc.Kind] = f
		}
	}

	// war3mapMisc.txt — OPTIONAL per-map gameplay-constants overrides. Maps
	// without this file inherit the stock MiscData.txt values; nothing to
	// load. The editor exposes whatever overrides are present + lets the
	// user add new ones.
	gameplay, err := readOpt(src, "war3mapMisc.txt", miscdata.Parse)
	if err != nil {
		return err
	}
	if gameplay == nil {
		gameplay = &miscdata.File{}
	}

	// war3map.w3r — OPTIONAL regions table. Phase 2b2 needs this for
	// gg_rct_ → region-name resolution and the Trigger Editor's region picker.
	// Parse-only; a malformed file logs + returns nil rather than failing
	// open (region pickers degrade to "no regions" + the resolver falls back
	// to the raw identifier).
	regionsFile, err := readOpt(src, "war3map.w3r", w3r.Parse)
	if err != nil {
		log.Printf("Open: war3map.w3r: %v (proceeding without regions)", err)
		regionsFile = nil
	}

	// war3map.w3c — OPTIONAL game cameras. Same rationale as w3r above.
	camerasFile, err := readOpt(src, "war3map.w3c", w3c.Parse)
	if err != nil {
		log.Printf("Open: war3map.w3c: %v (proceeding without cameras)", err)
		camerasFile = nil
	}

	// war3map.wtg + war3map.wct — OPTIONAL. The trigger loader handles both
	// the "neither present" + "hand-rolled-script only" cases internally;
	// errors are logged but never fail the open (a malformed .wtg shouldn't
	// block access to the rest of the map). We pass info.Lua so the
	// hand-rolled-script synth picks the right script file when needed.
	triggers, triggersWct, isHandRolled, scriptName := loadTriggersForOpenV2(src, info != nil && info.Lua)

	// mapIO diagnostics: classify what trigger files actually resolved at Open
	// (none / wtg-only / wtg+wct / error) so a map whose triggers silently
	// failed to parse is visible. "error" means the .wtg/.wct were present but
	// produced no usable tree (parse failure logged in loadTriggersForOpenV2);
	// a hand-rolled-script synth counts as a wtg-equivalent ("wtg-only").
	triggerLoadStatus := classifyTriggerLoadStatus(src, triggers, triggersWct)

	// mapIO diagnostics: probe presence + size of the standard war3map.* files
	// for the per-open manifest. Cheap re-read against the already-open source
	// (folder: a stat+read; mpq: a buffered archive read). Done before the swap
	// so a slow probe doesn't hold the session lock.
	manifest.Files = collectMapFilePresence(src)

	// Atomically swap state; close any previously-held source before stomping it.
	s.mu.Lock()
	prevSource := s.source
	s.loaded = true
	s.path = abs
	s.source = src
	s.rawMap = rawMapBytes
	s.info = info
	s.units = units
	s.doodads = doodads
	s.terrain = terrain
	s.doodadMods = doodadMods
	s.destructibleMods = destructibleMods
	s.unitMods = unitMods
	s.itemMods = itemMods
	s.abilityMods = abilityMods
	s.buffMods = buffMods
	s.upgradeMods = upgradeMods
	s.objectSkinMods = skinMods
	s.shadowMap = shadowMap
	s.pathingMap = pathingMap
	s.regions = regionsFile
	s.cameras = camerasFile
	// war3map.imp is parsed lazily on first ImportModel — drop any cached table
	// from a previously-loaded map so the next import re-reads this map's.
	s.imp = nil
	s.strings = wtsStrings
	s.infoTokens = infoTokens
	s.gameplay = gameplay
	s.triggers = triggers
	s.triggersWct = triggersWct
	s.triggerIsHandRolled = isHandRolled
	s.mapHeaderScriptName = scriptName
	s.mapHeaderScriptDirty = false
	s.triggerNextID = computeNextTriggerID(triggers)
	s.selection = SelectionState{Items: nil, Primary: -1}
	wasDirty := s.anyDirtyLocked()
	s.dirtyUnits = false
	s.dirtyDoodads = false
	s.dirtyInfo = false
	s.dirtyStrings = false
	s.dirtyGameplay = false
	s.dirtyTerrain = false
	s.dirtyUnitMods = false
	s.dirtyItemMods = false
	s.dirtyAbilityMods = false
	s.dirtyBuffMods = false
	s.dirtyDestructibleMods = false
	s.dirtyDoodadMods = false
	s.dirtyUpgradeMods = false
	s.dirtyTriggers = false
	s.dirtyImports = false
	s.dirtyRegions = false
	// Reset history — previous map's undo stack must not leak across opens
	// (creation_numbers would dangle and Revert would error). Mutating the
	// slices directly under the existing write-lock; ClearHistory's own lock
	// would deadlock here.
	hadHistory := len(s.history) > 0 || len(s.redoStack) > 0
	s.history = nil
	s.redoStack = nil
	s.groupDepth = 0
	s.pendingGroup = nil
	s.mu.Unlock()
	if prevSource != nil {
		_ = prevSource.close()
	}
	// mapIO diagnostics: the swap succeeded — record the manifest + trigger
	// status and mark this Open as successful so the deferred openFails guard
	// is a no-op. recordOpenManifest copies the pointer pull-only; nothing here
	// mutates manifest after this point.
	manifest.OpenMs = time.Since(openStart).Milliseconds()
	recordOpenManifest(manifest, triggerLoadStatus)
	opened = true
	// Snapshot the on-disk identity (mtime+size) of every source file we might
	// later write back, so Save's external-change detection has a baseline to
	// compare against. Does its own short lock + a handful of stats; runs
	// unlocked here so the I/O doesn't hold the session lock. See atomic_save.go.
	s.recordSourceBaseline(src, abs)
	s.notifySelection()
	s.notifyMapChanged(true)
	if wasDirty {
		s.notifyDirty(false)
	}
	if hadHistory {
		s.notifyHistoryChanged()
	}
	return nil
}

// collectMapFilePresence probes the standard war3map.* set against an open
// source and returns each present file's name + uncompressed size, for the
// mapIO per-open manifest. Absent files are omitted. read errors are skipped
// (the manifest is best-effort observability, not a load gate).
func collectMapFilePresence(src fileSource) []mapFilePresence {
	names := []string{
		"war3map.w3i", "war3map.w3e", "war3map.shd", "war3map.wpm",
		"war3mapUnits.doo", "war3map.doo", "war3map.wts", "war3map.imp",
		"war3map.w3d", "war3map.w3b", "war3map.w3u", "war3map.w3t",
		"war3map.w3a", "war3map.w3h", "war3map.w3q",
		"war3map.w3r", "war3map.w3c", "war3mapMisc.txt",
		"war3map.wtg", "war3map.wct", "war3map.j", "war3map.lua",
	}
	out := make([]mapFilePresence, 0, len(names))
	for _, n := range names {
		b, ok, err := src.read(n)
		if err != nil || !ok {
			continue
		}
		out = append(out, mapFilePresence{Name: n, Bytes: len(b)})
	}
	return out
}

// classifyTriggerLoadStatus derives the triggerLoadStatus enum for the mapIO
// manifest from the loaded trigger artefacts. Distinguishes the four cases an
// agent cares about when "the Trigger Editor is empty": the map genuinely has
// no triggers (none), a GUI tree loaded with/without its script blobs
// (wtg-only / wtg+wct), or the .wtg/.wct were present but failed to parse into
// a tree (error). A hand-rolled-script synth presents as a non-nil tree with no
// wct, so it reports "wtg-only".
func classifyTriggerLoadStatus(src fileSource, triggers *wtg.Triggers, wctFile *wct.File) string {
	if triggers != nil {
		if wctFile != nil {
			return "wtg+wct"
		}
		return "wtg-only"
	}
	// No usable tree. If trigger files were present on disk, the loader failed
	// to parse them — flag as "error" so the silent parse-failure path (logged
	// in loadTriggersForOpenV2) is visible. Otherwise the map simply has none.
	if _, hasWTG, _ := src.read("war3map.wtg"); hasWTG {
		return "error"
	}
	if _, hasWCT, _ := src.read("war3map.wct"); hasWCT {
		return "error"
	}
	return "none"
}

// readOpt fetches one optional file via src and runs its parser. Returns
// (nil, nil) if the file is absent. Wraps both I/O and parse errors so the
// caller gets a clear "while loading <name>" trail.
func readOpt[T any](src fileSource, name string, parse func([]byte) (T, error)) (T, error) {
	var zero T
	b, ok, err := src.read(name)
	if err != nil {
		return zero, fmt.Errorf("read %s: %w", name, err)
	}
	if !ok {
		return zero, nil
	}
	v, err := parse(b)
	if err != nil {
		return zero, fmt.Errorf("parse %s: %w", name, err)
	}
	return v, nil
}

// Close discards the loaded map. Safe to call when nothing is loaded.
func (s *Session) Close() {
	s.mu.Lock()
	prevSource := s.source
	s.loaded = false
	s.path = ""
	s.source = nil
	s.rawMap = nil
	s.info = nil
	s.units = nil
	s.doodads = nil
	s.terrain = nil
	s.doodadMods = nil
	s.destructibleMods = nil
	s.unitMods = nil
	s.itemMods = nil
	s.abilityMods = nil
	s.buffMods = nil
	s.upgradeMods = nil
	s.objectSkinMods = nil
	s.shadowMap = nil
	s.pathingMap = nil
	s.regions = nil
	s.cameras = nil
	s.imp = nil
	s.strings = nil
	s.infoTokens = nil
	s.srcBaseline = nil // drop the change-detection baseline; the next Open records a fresh one
	s.dirtyStrings = false
	s.doodadVisibility = nil // don't leak one map's category-visibility into the next
	s.gameplay = nil
	s.triggers = nil
	s.triggersWct = nil
	s.triggerIsHandRolled = false
	s.mapHeaderScriptName = ""
	s.mapHeaderScriptDirty = false
	s.triggerNextID = 0
	s.selection = SelectionState{Items: nil, Primary: -1}
	wasDirty := s.anyDirtyLocked()
	s.dirtyUnits = false
	s.dirtyDoodads = false
	s.dirtyInfo = false
	s.dirtyTerrain = false
	s.dirtyGameplay = false
	s.dirtyUnitMods = false
	s.dirtyItemMods = false
	s.dirtyAbilityMods = false
	s.dirtyBuffMods = false
	s.dirtyDestructibleMods = false
	s.dirtyDoodadMods = false
	s.dirtyUpgradeMods = false
	s.dirtyTriggers = false
	s.dirtyImports = false
	s.dirtyRegions = false
	hadHistory := len(s.history) > 0 || len(s.redoStack) > 0
	s.history = nil
	s.redoStack = nil
	s.groupDepth = 0
	s.pendingGroup = nil
	s.mu.Unlock()
	if prevSource != nil {
		_ = prevSource.close()
	}
	s.notifySelection()
	s.notifyMapChanged(false)
	if wasDirty {
		s.notifyDirty(false)
	}
	if hadHistory {
		s.notifyHistoryChanged()
	}
}

// Strings returns the parsed war3map.wts trigger-strings table, or nil if
// the loaded map doesn't ship one. Used to resolve TRIGSTR_<n> references
// in per-map object modifications.
func (s *Session) Strings() wts.Strings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.strings
}

// RawMapBytes returns the raw .w3x file bytes if the current map was opened
// from an archive (suitable for War3MapViewer.loadMap). Returns nil for
// folder-based opens or when no map is loaded.
func (s *Session) RawMapBytes() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rawMap
}

// ReadFile fetches one named file from the currently-loaded map's source
// (the open MPQ or the folder). Returns (nil, false, nil) if the file
// isn't present. Used by the pathSolver bridge so mdx-m3-viewer can pull
// custom-imported assets out of the map archive.
func (s *Session) ReadFile(name string) ([]byte, bool, error) {
	s.mu.RLock()
	src := s.source
	s.mu.RUnlock()
	if src == nil {
		return nil, false, nil
	}
	return src.read(name)
}

// OnMapChanged subscribes to load/unload notifications. Called after the
// Session lock is released, so listeners may safely call back into Session.
// Fires with loaded=true after Open succeeds, loaded=false after Close.
func (s *Session) OnMapChanged(fn func(loaded bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mapListeners = append(s.mapListeners, fn)
}

func (s *Session) notifyMapChanged(loaded bool) {
	s.mu.RLock()
	listeners := make([]func(bool), len(s.mapListeners))
	copy(listeners, s.mapListeners)
	s.mu.RUnlock()
	for _, fn := range listeners {
		fn(loaded)
	}
}

// IsLoaded reports whether a map is currently open.
func (s *Session) IsLoaded() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loaded
}

// Path returns the absolute path of the loaded map (or "" if none).
func (s *Session) Path() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.path
}

// Info returns the parsed war3map.w3i, or nil if no map is loaded.
// The returned pointer is shared — callers must not mutate.
func (s *Session) Info() *w3i.Info {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.info
}

// Units returns the parsed war3mapUnits.doo, or nil if no map is loaded.
func (s *Session) Units() *unitsdoo.File {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.units
}

// Doodads returns the parsed war3map.doo, or nil if no map is loaded.
func (s *Session) Doodads() *doodadsdoo.File {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.doodads
}

// Terrain returns the parsed war3map.w3e, or nil if no map is loaded or the
// map has no terrain file. Phase 3 viewport needs this.
func (s *Session) Terrain() *w3e.File {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.terrain
}

// DoodadMods returns the parsed war3map.w3d (per-map doodad modifications +
// new derived types), or nil if absent.
func (s *Session) DoodadMods() *w3objmod.File {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.doodadMods
}

// DestructibleMods returns the parsed war3map.w3b, or nil if absent.
func (s *Session) DestructibleMods() *w3objmod.File {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.destructibleMods
}

// UnitMods returns the parsed war3map.w3u (per-map unit modifications +
// new derived types), or nil if absent.
func (s *Session) UnitMods() *w3objmod.File {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.unitMods
}

// ItemMods returns the parsed war3map.w3t, or nil if absent.
func (s *Session) ItemMods() *w3objmod.File {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.itemMods
}

// SkinMods returns the parsed war3mapSkin.w3* companion table for the given
// KindConfig.Kind ("units", "items", ...), or nil if the map has none. These
// are the Reforged art/skin overrides (Name, Model File, Icon) that the
// renderer + Object Editor layer UNDER the primary shadow.
func (s *Session) SkinMods(kind string) *w3objmod.File {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.objectSkinMods[kind]
}

// UnitSkinMods returns war3mapSkin.w3u (unit art/skin overrides), or nil.
func (s *Session) UnitSkinMods() *w3objmod.File { return s.SkinMods("units") }

// ItemSkinMods returns war3mapSkin.w3t (item art/skin overrides), or nil.
func (s *Session) ItemSkinMods() *w3objmod.File { return s.SkinMods("items") }

// AbilityMods returns the parsed war3map.w3a (per-map ability modifications +
// new derived abilities), or nil if absent. Phase 2b accessor.
func (s *Session) AbilityMods() *w3objmod.File {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.abilityMods
}

// BuffMods returns the parsed war3map.w3h (per-map buff modifications + new
// derived buffs), or nil if absent. Phase 2b accessor.
func (s *Session) BuffMods() *w3objmod.File {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.buffMods
}

// UpgradeMods returns the parsed war3map.w3q (per-map upgrade modifications +
// new derived upgrades), or nil if absent. Phase 2b accessor.
func (s *Session) UpgradeMods() *w3objmod.File {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.upgradeMods
}

// ShadowMap returns the parsed war3map.shd, or nil if absent.
func (s *Session) ShadowMap() *shd.File {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.shadowMap
}

// PathingMap returns the parsed war3map.wpm, or nil if absent.
func (s *Session) PathingMap() *wpm.File {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pathingMap
}

// Regions returns the parsed war3map.w3r, or nil if absent. Phase 2b2 accessor;
// the Trigger Editor's region picker + the gg_rct_ resolver consume this.
func (s *Session) Regions() *w3r.File {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.regions
}

// Cameras returns the parsed war3map.w3c, or nil if absent. Same Phase 2b2
// rationale as Regions.
func (s *Session) Cameras() *w3c.File {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cameras
}

// Selection returns the current selection. Safe to call before a map is loaded
// (returns an empty selection).
func (s *Session) Selection() SelectionState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Return a copy so callers can't mutate Items in place.
	items := make([]SelectionItem, len(s.selection.Items))
	copy(items, s.selection.Items)
	return SelectionState{Items: items, Primary: s.selection.Primary}
}

// SetSelection replaces the selection. If items is nil/empty, selection clears.
// Primary defaults to len(items)-1 (the most recently added) if out of range.
// Fires SelectionListeners after release of the write lock.
func (s *Session) SetSelection(items []SelectionItem, primary int) {
	s.mu.Lock()
	if len(items) == 0 {
		s.selection = SelectionState{Items: nil, Primary: -1}
	} else {
		if primary < 0 || primary >= len(items) {
			primary = len(items) - 1
		}
		copied := make([]SelectionItem, len(items))
		copy(copied, items)
		s.selection = SelectionState{Items: copied, Primary: primary}
	}
	s.mu.Unlock()
	s.notifySelection()
}

// OnSelectionChanged subscribes a listener. Listeners are called synchronously
// from SetSelection after the lock is released, so they may call back into
// other Session methods safely. There is no unsubscribe — Session is a process
// singleton and listeners live for the process.
func (s *Session) OnSelectionChanged(fn func(SelectionState)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listeners = append(s.listeners, fn)
}

func (s *Session) notifySelection() {
	s.mu.RLock()
	state := s.selection
	listeners := make([]func(SelectionState), len(s.listeners))
	copy(listeners, s.listeners)
	s.mu.RUnlock()
	for _, fn := range listeners {
		fn(state)
	}
}

// MoveUnit relocates the unit with the given creation_number to the supplied
// game coordinates. Fires the dirty-changed event when this is the first
// pending edit. Returns an error if no entity with that creation_number is
// loaded.
//
// Game-coords contract: x/y/z are already in WC3 game coordinates (centered
// at 0,0), matching the wire format used everywhere in this package. The
// unitsdoo parser stores Position verbatim — no conversion needed here.
func (s *Session) MoveUnit(creationNumber uint32, x, y, z float32) error {
	s.mu.Lock()
	if s.units == nil {
		s.mu.Unlock()
		return fmt.Errorf("no map loaded")
	}
	found := -1
	for i := range s.units.Entities {
		if s.units.Entities[i].CreationNumber == creationNumber {
			found = i
			break
		}
	}
	if found < 0 {
		s.mu.Unlock()
		return fmt.Errorf("no unit with creation_number %d", creationNumber)
	}
	// No-op when the new position matches the current one. The Properties
	// panel commits on blur/Enter even when the user only inspected the
	// input (or pressed Escape, which blurs without a real change), so
	// without this guard the Save pill flips to amber on every focus-out
	// for no actual edit. Bit-exact float compare is fine: parseFloat of
	// the same stringified value round-trips, and the JS side always
	// formats current truth into the input before the user touches it.
	cur := s.units.Entities[found].Position
	if cur[0] == x && cur[1] == y && cur[2] == z {
		s.mu.Unlock()
		return nil
	}
	oldPos := cur
	newPos := [3]float32{x, y, z}
	wasDirty := s.dirtyUnits
	s.units.Entities[found].Position = newPos
	s.dirtyUnits = true
	s.recordCommand(&moveUnitCmd{cn: creationNumber, oldPos: oldPos, newPos: newPos})
	historyChanged := s.groupDepth == 0
	s.mu.Unlock()
	if !wasDirty {
		s.notifyDirty(true)
	}
	// Notify entity-change subscribers AFTER dirty so the Save pill flips
	// before Properties + scene repaint — keeps the visible-state ordering
	// consistent with the older UI-driven path (which fired dirty via the
	// MoveUnit call, then refreshed the scene via the explicit JS-side
	// scene.updateUnitPosition immediately after).
	s.notifyEntityChanged(EntityChange{
		Kind:     "unit",
		ID:       creationNumber,
		Field:    "position",
		Position: newPos,
	})
	if historyChanged {
		s.notifyHistoryChanged()
	}
	return nil
}

// MoveDoodad relocates the doodad with the given creation_number to the
// supplied game coordinates. Fires the dirty-changed event when this is the
// first pending edit (across all per-file dirty flags). Returns an error if
// no doodad with that creation_number is loaded.
//
// Game-coords contract: x/y/z are already in WC3 game coordinates (centered
// at 0,0). doodadsdoo stores Position verbatim — no conversion needed here.
// Mirrors MoveUnit's shape; the only differences are:
//   - we look up creation_number in s.doodads.Doodads (per-kind id namespace —
//     a unit and a doodad can share the same numeric creation_number)
//   - the dirty flag flipped is dirtyDoodads (so Save writes war3map.doo, not
//     war3mapUnits.doo)
//   - the EntityChange kind is "doodad" so scene/Properties subscribers can
//     branch correctly
func (s *Session) MoveDoodad(creationNumber uint32, x, y, z float32) error {
	s.mu.Lock()
	if s.doodads == nil {
		s.mu.Unlock()
		return fmt.Errorf("no map loaded")
	}
	found := -1
	for i := range s.doodads.Doodads {
		if s.doodads.Doodads[i].CreationNumber == creationNumber {
			found = i
			break
		}
	}
	if found < 0 {
		s.mu.Unlock()
		return fmt.Errorf("no doodad with creation_number %d", creationNumber)
	}
	// No-op short-circuit: same rationale as MoveUnit — Properties commits on
	// blur even when nothing changed, and we don't want every focus-out to
	// flip the Save pill to amber.
	cur := s.doodads.Doodads[found].Position
	if cur[0] == x && cur[1] == y && cur[2] == z {
		s.mu.Unlock()
		return nil
	}
	oldPos := cur
	newPos := [3]float32{x, y, z}
	// 0→1 transition of the combined dirty flag (any per-file flag). If the
	// session was already dirty for another reason (e.g. dirtyUnits), this
	// edit doesn't re-fire the public dirty event.
	wasDirty := s.dirtyUnits || s.dirtyDoodads || s.dirtyInfo || s.dirtyGameplay
	s.doodads.Doodads[found].Position = newPos
	s.dirtyDoodads = true
	s.recordCommand(&moveDoodadCmd{cn: creationNumber, oldPos: oldPos, newPos: newPos})
	historyChanged := s.groupDepth == 0
	s.mu.Unlock()
	if !wasDirty {
		s.notifyDirty(true)
	}
	s.notifyEntityChanged(EntityChange{
		Kind:     "doodad",
		ID:       creationNumber,
		Field:    "position",
		Position: newPos,
	})
	if historyChanged {
		s.notifyHistoryChanged()
	}
	return nil
}

// RotateUnit sets the facing angle (radians, Z-axis only) of the unit with the
// given creation_number. Mirrors MoveUnit's shape: no-op short-circuit when
// unchanged, dirty-flip on first edit, entity-changed event after unlock.
//
// WC3 units store a single float32 Rotation (radians around Z). X/Y rotation
// has no on-disk storage — this mutator only accepts Z-axis values.
func (s *Session) RotateUnit(creationNumber uint32, rotation float32) error {
	s.mu.Lock()
	if s.units == nil {
		s.mu.Unlock()
		return fmt.Errorf("no map loaded")
	}
	found := -1
	for i := range s.units.Entities {
		if s.units.Entities[i].CreationNumber == creationNumber {
			found = i
			break
		}
	}
	if found < 0 {
		s.mu.Unlock()
		return fmt.Errorf("no unit with creation_number %d", creationNumber)
	}
	if s.units.Entities[found].Rotation == rotation {
		s.mu.Unlock()
		return nil
	}
	oldRot := s.units.Entities[found].Rotation
	wasDirty := s.dirtyUnits
	s.units.Entities[found].Rotation = rotation
	s.dirtyUnits = true
	pos := s.units.Entities[found].Position
	s.recordCommand(&rotateUnitCmd{cn: creationNumber, oldRot: oldRot, newRot: rotation})
	historyChanged := s.groupDepth == 0
	s.mu.Unlock()
	if !wasDirty {
		s.notifyDirty(true)
	}
	s.notifyEntityChanged(EntityChange{
		Kind:     "unit",
		ID:       creationNumber,
		Field:    "rotation",
		Position: pos,
		Rotation: rotation,
	})
	if historyChanged {
		s.notifyHistoryChanged()
	}
	return nil
}

// RotateDoodad sets the facing angle (radians, Z-axis only) of the doodad with
// the given creation_number. Mirrors MoveDoodad's shape.
//
// NOTE: Some doodad types carry a fixed_rot flag in the SLK type index, which
// restricts valid rotations to a small set of angles (e.g. quarter-turns for
// bridges). The backend stores whatever value it's given — snapping to
// acceptable angles per fixed_rot is the responsibility of the gizmo's commit
// path (Phase C). This comment is the contract notice for Phase C agents.
func (s *Session) RotateDoodad(creationNumber uint32, rotation float32) error {
	s.mu.Lock()
	if s.doodads == nil {
		s.mu.Unlock()
		return fmt.Errorf("no map loaded")
	}
	found := -1
	for i := range s.doodads.Doodads {
		if s.doodads.Doodads[i].CreationNumber == creationNumber {
			found = i
			break
		}
	}
	if found < 0 {
		s.mu.Unlock()
		return fmt.Errorf("no doodad with creation_number %d", creationNumber)
	}
	if s.doodads.Doodads[found].Rotation == rotation {
		s.mu.Unlock()
		return nil
	}
	oldRot := s.doodads.Doodads[found].Rotation
	wasDirty := s.dirtyUnits || s.dirtyDoodads || s.dirtyInfo || s.dirtyGameplay
	s.doodads.Doodads[found].Rotation = rotation
	s.dirtyDoodads = true
	pos := s.doodads.Doodads[found].Position
	s.recordCommand(&rotateDoodadCmd{cn: creationNumber, oldRot: oldRot, newRot: rotation})
	historyChanged := s.groupDepth == 0
	s.mu.Unlock()
	if !wasDirty {
		s.notifyDirty(true)
	}
	s.notifyEntityChanged(EntityChange{
		Kind:     "doodad",
		ID:       creationNumber,
		Field:    "rotation",
		Position: pos,
		Rotation: rotation,
	})
	if historyChanged {
		s.notifyHistoryChanged()
	}
	return nil
}

// ScaleUnit sets the per-axis scale of the unit with the given creation_number.
// Mirrors MoveUnit's shape.
//
// CRITICAL: This mutator clears the entity's scaleRaw preservation field so
// the next Encode emits the new Scale value rather than the original on-disk
// bits. Without this, unitsdoo.Encode would preserve the OLD bytes (scaleRaw
// takes precedence over Scale*128 when non-zero), silently discarding the edit.
// See feedback_unitsdoo_scale_raw.md for the gotcha background.
//
// Doodads do NOT have this issue (their Scale is stored raw, no /128 divide at
// Parse). Only units need scaleRaw invalidation.
func (s *Session) ScaleUnit(creationNumber uint32, sx, sy, sz float32) error {
	s.mu.Lock()
	if s.units == nil {
		s.mu.Unlock()
		return fmt.Errorf("no map loaded")
	}
	found := -1
	for i := range s.units.Entities {
		if s.units.Entities[i].CreationNumber == creationNumber {
			found = i
			break
		}
	}
	if found < 0 {
		s.mu.Unlock()
		return fmt.Errorf("no unit with creation_number %d", creationNumber)
	}
	cur := s.units.Entities[found].Scale
	if cur[0] == sx && cur[1] == sy && cur[2] == sz {
		s.mu.Unlock()
		return nil
	}
	oldScale := cur
	newScale := [3]float32{sx, sy, sz}
	// Capture the pre-mutation scaleRaw so Revert can restore byte-faithful
	// round-trip (slocs store raw=1.0, real units raw=128.0; the original
	// bits matter for save-after-undo equality).
	oldScaleRaw := unitsdoo.ScaleRaw(&s.units.Entities[found])
	wasDirty := s.dirtyUnits
	s.units.Entities[found].Scale = newScale
	// Invalidate scaleRaw so Encode derives from the new Scale value.
	// scaleRaw is unexported; we call the package-level helper to set it.
	unitsdoo.ClearScaleRaw(&s.units.Entities[found])
	s.dirtyUnits = true
	pos := s.units.Entities[found].Position
	s.recordCommand(&scaleUnitCmd{
		cn:          creationNumber,
		oldScale:    oldScale,
		newScale:    newScale,
		oldScaleRaw: oldScaleRaw,
	})
	historyChanged := s.groupDepth == 0
	s.mu.Unlock()
	if !wasDirty {
		s.notifyDirty(true)
	}
	s.notifyEntityChanged(EntityChange{
		Kind:     "unit",
		ID:       creationNumber,
		Field:    "scale",
		Scale:    newScale,
		Position: pos,
	})
	if historyChanged {
		s.notifyHistoryChanged()
	}
	return nil
}

// ScaleDoodad sets the per-axis scale of the doodad with the given
// creation_number. Mirrors MoveDoodad's shape.
//
// Doodad Scale is stored RAW on disk (no /128 divide at Parse — the opposite
// convention from unitsdoo). Encode writes Scale verbatim, so no scaleRaw
// invalidation is needed here. The mutator simply sets the public Scale field.
func (s *Session) ScaleDoodad(creationNumber uint32, sx, sy, sz float32) error {
	s.mu.Lock()
	if s.doodads == nil {
		s.mu.Unlock()
		return fmt.Errorf("no map loaded")
	}
	found := -1
	for i := range s.doodads.Doodads {
		if s.doodads.Doodads[i].CreationNumber == creationNumber {
			found = i
			break
		}
	}
	if found < 0 {
		s.mu.Unlock()
		return fmt.Errorf("no doodad with creation_number %d", creationNumber)
	}
	cur := s.doodads.Doodads[found].Scale
	if cur[0] == sx && cur[1] == sy && cur[2] == sz {
		s.mu.Unlock()
		return nil
	}
	oldScale := cur
	newScale := [3]float32{sx, sy, sz}
	wasDirty := s.dirtyUnits || s.dirtyDoodads || s.dirtyInfo || s.dirtyGameplay
	s.doodads.Doodads[found].Scale = newScale
	s.dirtyDoodads = true
	pos := s.doodads.Doodads[found].Position
	s.recordCommand(&scaleDoodadCmd{cn: creationNumber, oldScale: oldScale, newScale: newScale})
	historyChanged := s.groupDepth == 0
	s.mu.Unlock()
	if !wasDirty {
		s.notifyDirty(true)
	}
	s.notifyEntityChanged(EntityChange{
		Kind:     "doodad",
		ID:       creationNumber,
		Field:    "scale",
		Scale:    newScale,
		Position: pos,
	})
	if historyChanged {
		s.notifyHistoryChanged()
	}
	return nil
}

// MutateInfo applies fn to the in-memory war3map.w3i Info under the session
// write lock, flips dirtyInfo, and fires the standard entity-changed event
// so subscribers (Properties panel, header pill, the new Map Info Editor
// dialog) refresh. Mirrors the MoveUnit/MoveDoodad pattern but at the file
// level — the Info struct is a single document, so there's no creation_number
// to disambiguate; the EntityChange payload carries Kind="info", ID=0,
// Field="info".
//
// Returns an error if no map is loaded. The fn callback should be small and
// non-blocking — it runs under the write lock, which serializes every other
// reader/writer in the session.
//
// MUST be the only path that mutates Info — bypassing MutateInfo leaves the
// session in a state where IsDirty() is wrong, the Save button stays
// disabled, and the changes are silently dropped on next Open. Wails app
// methods + bridge handlers + future undo/redo all funnel through here.
//
// No no-op short-circuit: unlike MoveUnit, MutateInfo can't cheaply compare
// "before" and "after" because Info is a deep struct with slices of slices.
// Callers that want no-op detection should diff their inputs themselves
// before calling MutateInfo.
func (s *Session) MutateInfo(fn func(*w3i.Info)) error {
	if fn == nil {
		return fmt.Errorf("MutateInfo: nil fn")
	}
	s.mu.Lock()
	if s.info == nil {
		s.mu.Unlock()
		return fmt.Errorf("no map loaded")
	}
	fn(s.info)
	wasDirty := s.dirtyUnits || s.dirtyDoodads || s.dirtyInfo || s.dirtyGameplay
	s.dirtyInfo = true
	s.mu.Unlock()
	if !wasDirty {
		s.notifyDirty(true)
	}
	// Map Info changes don't fit the position-only EntityChange shape today,
	// but firing the event lets the same UI subscribers (Properties panel,
	// future Map Info Editor) repaint without inventing a new event channel.
	// Kind="info" / Field="info" keeps the discriminator unambiguous; consumers
	// branch on Kind before reading Position.
	s.notifyEntityChanged(EntityChange{
		Kind:  "info",
		ID:    0,
		Field: "info",
	})
	return nil
}

// SetSkyModel queues a sky-model change for the next Save. path is the raw
// argument to insert into SetSkyModel("…") — typically a normalized
// `environment/sky/…/….mdx` form; the rewriter handles Lua/JASS escaping.
// Empty string is a valid intent (SetSkyModel("") — disables the sky at
// runtime); pass nil to clear a pending change without committing.
//
// Records the change in the undo history, so Ctrl+Z reverts to the previous
// override (or to "no override at all" if there wasn't one). Sequential
// picks each create one history entry — the user can step back through them
// individually. Marks the session dirty so the existing dirty-changed bus +
// Save-enabling UI cues pick it up. The script-rewrite happens during Save
// (see the pendingSky branch there).
//
// No-op when the new value matches the current pending value (by content,
// not pointer identity — comparing pointer addresses would record a no-op
// command every time the picker re-emits the same selection on focus).
func (s *Session) SetSkyModel(path *string) error {
	s.mu.Lock()
	if !s.loaded {
		s.mu.Unlock()
		return fmt.Errorf("no map loaded")
	}
	// Idempotence check — same content as current pending? Bail without
	// adding a history entry. We compare values, not pointer identity, so
	// frontends that wrap-and-unwrap don't pollute the undo stack.
	if skyPtrEq(s.pendingSkyModel, path) {
		s.mu.Unlock()
		return nil
	}
	oldPath := clonePtr(s.pendingSkyModel)
	newPath := clonePtr(path)
	wasDirty := s.dirtyUnits || s.dirtyDoodads || s.dirtyInfo || s.dirtyTerrain || s.dirtyGameplay || s.pendingSkyModel != nil
	s.pendingSkyModel = newPath
	s.recordCommand(&setSkyModelCmd{oldPath: oldPath, newPath: newPath})
	nowDirty := s.pendingSkyModel != nil
	historyChanged := s.groupDepth == 0
	s.mu.Unlock()
	if !wasDirty && nowDirty {
		s.notifyDirty(true)
	}
	// Mirror the MutateInfo notification path so the sky picker doesn't have
	// to learn a private event channel — it can subscribe to the same
	// EntityChange stream as everything else and refresh on any "info" event.
	s.notifyEntityChanged(EntityChange{Kind: "info", ID: 0, Field: "sky_model"})
	if historyChanged {
		s.notifyHistoryChanged()
	}
	return nil
}

// skyPtrEq returns true when two *string sky-override pointers point to the
// same logical state. nil and nil are equal; non-nil values compare by
// content. (nil vs &"" is NOT equal — "" is an explicit "disable sky" intent
// distinct from "no override".)
func skyPtrEq(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// PendingSkyModel returns the queued sky-model override or nil if none is
// pending. Renderers should prefer this over scanning the script directly so
// the editor reflects unsaved picker changes immediately.
func (s *Session) PendingSkyModel() *string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.pendingSkyModel == nil {
		return nil
	}
	v := *s.pendingSkyModel
	return &v
}

// SwapTilesetRequest re-tiles the loaded map. NewLetter is the single-byte
// tileset code written into both war3map.w3i and war3map.w3e (HiveWE keeps
// these in sync; Blizzard's editor refuses to load maps where they disagree).
// NewGroundTilesets / NewCliffTilesets are the new palette FourCCs in the
// order callers want them stored on disk. GroundFromTo / CliffFromTo are
// per-tilepoint remap tables — index by the OLD palette slot, value is the
// NEW palette slot to assign. Lengths must match the OLD palette lengths.
//
// Per the HiveWE tile-setter dialog, callers are expected to have already
// resolved every old tile to a concrete new tile (no "auto-pick" magic at
// this layer — that's policy that belongs in the UI).
type SwapTilesetRequest struct {
	NewLetter         byte     // tileset code letter ('L', 'A', …)
	NewGroundTilesets []string // length 1..maxGround (16 for v11, 64 for v12+)
	NewCliffTilesets  []string // length 0..16
	GroundFromTo      []int    // len == len(old ground palette); value is index into NewGroundTilesets
	CliffFromTo       []int    // len == len(old cliff palette);  value is index into NewCliffTilesets
}

// SwapTileset retiles the loaded map: replaces the ground/cliff palettes,
// remaps every tilepoint's GroundTexture / CliffTexture via the from→to
// tables, and updates the tileset letter in both .w3e and .w3i. Sets the
// terrain + info dirty flags so Save will persist both files.
//
// Mirrors HiveWE's Terrain::change_tileset (src/base/terrain.ixx) but stays
// at the file-format level — the UI owns palette-choice policy (what new
// tileset, which old tiles to carry over, what to substitute when a tile
// isn't carried over) and hands SwapTileset a fully-resolved remap.
//
// Errors out of band, before mutating anything, so a failed call leaves the
// in-memory map unchanged.
func (s *Session) SwapTileset(req SwapTilesetRequest) error {
	s.mu.Lock()
	if s.terrain == nil || s.info == nil {
		s.mu.Unlock()
		return fmt.Errorf("SwapTileset: no map loaded")
	}
	terrain := s.terrain
	maxGround := 16
	if terrain.Version >= 12 {
		maxGround = 64
	}
	oldGround := len(terrain.GroundTilesets)
	oldCliff := len(terrain.CliffTilesets)
	if len(req.NewGroundTilesets) == 0 || len(req.NewGroundTilesets) > maxGround {
		s.mu.Unlock()
		return fmt.Errorf("SwapTileset: ground palette size %d out of range [1, %d]", len(req.NewGroundTilesets), maxGround)
	}
	if len(req.NewCliffTilesets) > 16 {
		s.mu.Unlock()
		return fmt.Errorf("SwapTileset: cliff palette size %d exceeds cap 16", len(req.NewCliffTilesets))
	}
	for i, id := range req.NewGroundTilesets {
		if len(id) != 4 {
			s.mu.Unlock()
			return fmt.Errorf("SwapTileset: NewGroundTilesets[%d] = %q, want 4-char FourCC", i, id)
		}
	}
	for i, id := range req.NewCliffTilesets {
		if len(id) != 4 {
			s.mu.Unlock()
			return fmt.Errorf("SwapTileset: NewCliffTilesets[%d] = %q, want 4-char FourCC", i, id)
		}
	}
	if len(req.GroundFromTo) != oldGround {
		s.mu.Unlock()
		return fmt.Errorf("SwapTileset: GroundFromTo len %d, want %d (old palette size)", len(req.GroundFromTo), oldGround)
	}
	for oldIdx, newIdx := range req.GroundFromTo {
		if newIdx < 0 || newIdx >= len(req.NewGroundTilesets) {
			s.mu.Unlock()
			return fmt.Errorf("SwapTileset: GroundFromTo[%d] = %d, out of range [0, %d)", oldIdx, newIdx, len(req.NewGroundTilesets))
		}
	}
	if len(req.CliffFromTo) != oldCliff {
		s.mu.Unlock()
		return fmt.Errorf("SwapTileset: CliffFromTo len %d, want %d (old palette size)", len(req.CliffFromTo), oldCliff)
	}
	if oldCliff > 0 && len(req.NewCliffTilesets) == 0 {
		s.mu.Unlock()
		return fmt.Errorf("SwapTileset: cannot remove cliff palette while map references cliffs")
	}
	for oldIdx, newIdx := range req.CliffFromTo {
		if newIdx < 0 || newIdx >= len(req.NewCliffTilesets) {
			s.mu.Unlock()
			return fmt.Errorf("SwapTileset: CliffFromTo[%d] = %d, out of range [0, %d)", oldIdx, newIdx, len(req.NewCliffTilesets))
		}
	}

	// All validation passed — snapshot the BEFORE state for undo, build the
	// new per-tile arrays, then apply via the same helper Apply/Revert call.
	// We snapshot both directions in full because the remap can be lossy
	// (multiple old slots → one new slot), so inverting the from_to table
	// wouldn't restore the original GroundTexture/CliffTexture values.
	oldStateGround := make([]uint8, len(terrain.Tiles))
	oldStateCliff := make([]uint8, len(terrain.Tiles))
	newStateGround := make([]uint8, len(terrain.Tiles))
	newStateCliff := make([]uint8, len(terrain.Tiles))
	for i := range terrain.Tiles {
		tp := terrain.Tiles[i]
		oldStateGround[i] = tp.GroundTexture
		oldStateCliff[i] = tp.CliffTexture
		newG := tp.GroundTexture
		if int(newG) < len(req.GroundFromTo) {
			newG = uint8(req.GroundFromTo[newG])
		}
		newStateGround[i] = newG
		// CliffTexture is 4 bits (0..15). The value 15 is the WC3 "no cliff"
		// sentinel that often appears even on non-cliff vertices, so we only
		// remap indices that point inside the OLD palette and leave higher
		// values untouched. New palette will still validate on Encode.
		newC := tp.CliffTexture
		if int(newC) < len(req.CliffFromTo) {
			newC = uint8(req.CliffFromTo[newC])
		}
		newStateCliff[i] = newC
	}

	cmd := &swapTilesetCmd{
		oldLetter: terrain.Tileset,
		oldGround: append([]string(nil), terrain.GroundTilesets...),
		oldCliff:  append([]string(nil), terrain.CliffTilesets...),
		oldTileG:  oldStateGround,
		oldTileC:  oldStateCliff,
		newLetter: req.NewLetter,
		newGround: append([]string(nil), req.NewGroundTilesets...),
		newCliff:  append([]string(nil), req.NewCliffTilesets...),
		newTileG:  newStateGround,
		newTileC:  newStateCliff,
	}

	applyTilesetSnapshot(s, cmd.newLetter, cmd.newGround, cmd.newCliff, cmd.newTileG, cmd.newTileC)

	wasDirty := s.dirtyUnits || s.dirtyDoodads || s.dirtyInfo || s.dirtyTerrain || s.dirtyGameplay
	s.dirtyTerrain = true
	s.dirtyInfo = true
	s.recordCommand(cmd)
	s.mu.Unlock()
	if !wasDirty {
		s.notifyDirty(true)
	}
	s.notifyEntityChanged(EntityChange{Kind: "terrain", ID: 0, Field: "tileset"})
	s.notifyHistoryChanged()
	return nil
}

// SetObjectField writes `value` to the named field on the object with FourCC
// `id` for the kind described by cfg. The field can be a FourCC (e.g. "unam")
// OR a column-name (e.g. "name") — setObjectField normalizes via the kind's
// metadata. Stock rows land in the OriginalEdits table; customs land on
// their own Overrides map. The edit is recorded as one undo step via
// setObjectFieldCmd.
//
// Idempotence: a SetObjectField call with the same value as the current
// override is a no-op (no dirty flip, no history entry, no event). Without
// this short-circuit the Properties panel's commit-on-blur path would
// pollute the undo stack with non-edits.
//
// Returns an error if id isn't known (neither stock nor custom for this
// kind) or the field isn't in the kind's metadata. Does not flip dirty /
// record history when the call errored out.
func (s *Session) SetObjectField(cfg *KindConfig, id, field, value string) error {
	// LOCK-ORDERING HAZARD — warm the per-kind base cache BEFORE taking s.mu.
	// loadObjectBase's first-ever call for a kind runs readBaseAsset, whose
	// production reader (main.readBaseAsset → Current.ReadFile) takes
	// Current.mu.RLock(). If that first load happened while this goroutine
	// already held s.mu.Lock() (write), the RLock would block forever —
	// Go's sync.RWMutex is non-reentrant, so a write-holder can't re-acquire
	// a read lock on the same mutex. Warming here (no lock held) makes the
	// in-lock loadObjectBase calls below idempotent cache hits (once.Do is
	// already satisfied), so no readBaseAsset / RLock runs under s.mu. Do NOT
	// move base loading inside the lock or move this warm-up below it.
	loadObjectBase(cfg)
	s.mu.Lock()
	if !s.loaded {
		s.mu.Unlock()
		return fmt.Errorf("no map loaded")
	}
	mods := cfg.GetMods(s)
	_, meta, _ := loadObjectBase(cfg)
	fourCC := fieldKeyForMods(meta, field)
	if fourCC == "" {
		s.mu.Unlock()
		return fmt.Errorf("unknown field %q (not in %s)", field, cfg.MetaDataFile)
	}
	var prev string
	var had bool
	if mods != nil {
		if ci := findCustomIndex(mods, id); ci >= 0 {
			prev, had = mods.Customs[ci].Overrides[fourCC]
		} else if ei := findOriginalEditIndex(mods, id); ei >= 0 {
			prev, had = mods.OriginalEdits[ei].Overrides[fourCC]
		}
	}
	if had && prev == value {
		s.mu.Unlock()
		return nil // no-op
	}
	// Validate id is a real object BEFORE mutating. setObjectField does the
	// same check, but it allocates a fresh shadow as a side-effect — we don't
	// want a missing-id call to leave an empty File hanging around.
	if mods == nil || findCustomIndex(mods, id) < 0 {
		base, _, _ := loadObjectBase(cfg)
		if base == nil || base.Rows[id] == nil {
			s.mu.Unlock()
			return fmt.Errorf("no %s object with id %q", cfg.Kind, id)
		}
	}
	if _, _, err := setObjectField(s, cfg, id, field, value); err != nil {
		s.mu.Unlock()
		return err
	}
	wasDirty := s.anyDirtyLocked()
	cfg.SetDirty(s, true)
	s.recordCommand(&setObjectFieldCmd{
		kind: cfg.Kind, id: id, column: field, oldVal: prev, newVal: value, hadOverride: had,
	})
	historyChanged := s.groupDepth == 0
	s.mu.Unlock()
	if !wasDirty {
		s.notifyDirty(true)
	}
	s.notifyEntityChanged(EntityChange{Kind: cfg.Kind + "_mod", ID: 0, Field: field})
	if historyChanged {
		s.notifyHistoryChanged()
	}
	return nil
}

// AddCustomObject appends a new custom row of the given kind inheriting from
// baseID. If newID is empty, an allocator picks the next free FourCC
// starting from the first character of baseID (e.g. "hpea" → "h001"); the
// chosen ID is returned.
//
// Errors if newID collides with an existing custom or shadows a stock row.
// Recorded in history as one undo step.
func (s *Session) AddCustomObject(cfg *KindConfig, newID, baseID string) (string, error) {
	// LOCK-ORDERING HAZARD — warm the base cache before s.mu (see the comment
	// on SetObjectField). addCustomObject + allocateCustomID both call
	// loadObjectBase while we hold the write lock; the first-ever load would
	// otherwise deadlock on Current.mu.RLock inside readBaseAsset.
	loadObjectBase(cfg)
	s.mu.Lock()
	if !s.loaded {
		s.mu.Unlock()
		return "", fmt.Errorf("no map loaded")
	}
	if newID == "" {
		newID = allocateCustomID(s, cfg, baseID)
		if newID == "" {
			s.mu.Unlock()
			return "", fmt.Errorf("no free custom id available for base %q", baseID)
		}
	}
	if err := addCustomObject(s, cfg, newID, baseID); err != nil {
		s.mu.Unlock()
		return "", err
	}
	wasDirty := s.anyDirtyLocked()
	cfg.SetDirty(s, true)
	s.recordCommand(&addCustomObjectCmd{kind: cfg.Kind, newID: newID, baseID: baseID})
	historyChanged := s.groupDepth == 0
	s.mu.Unlock()
	if !wasDirty {
		s.notifyDirty(true)
	}
	s.notifyEntityChanged(EntityChange{Kind: cfg.Kind + "_mod", ID: 0, Field: "customs"})
	if historyChanged {
		s.notifyHistoryChanged()
	}
	return newID, nil
}

// DeleteCustomObject removes a custom row of the given kind from the shadow.
// Errors if id isn't a custom (stock rows can't be deleted — they live in
// the base SLK). Recorded in history as one undo step; Revert re-appends
// the snapshot.
func (s *Session) DeleteCustomObject(cfg *KindConfig, id string) error {
	// LOCK-ORDERING HAZARD — warm the base cache before s.mu (see the comment
	// on SetObjectField). DeleteCustomObject's in-lock path doesn't load base
	// today, but warming up-front is idempotent + keeps every object-mutator's
	// lock-ordering uniform so a future edit that adds a base lookup can't
	// silently reintroduce the deadlock.
	loadObjectBase(cfg)
	s.mu.Lock()
	if !s.loaded {
		s.mu.Unlock()
		return fmt.Errorf("no map loaded")
	}
	mods := cfg.GetMods(s)
	if mods == nil || findCustomIndex(mods, id) < 0 {
		s.mu.Unlock()
		return fmt.Errorf("no custom %s with id %q", cfg.Kind, id)
	}
	saved, ok := removeCustomObject(s, cfg, id)
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("no custom %s with id %q", cfg.Kind, id)
	}
	wasDirty := s.anyDirtyLocked()
	cfg.SetDirty(s, true)
	s.recordCommand(&deleteCustomObjectCmd{kind: cfg.Kind, saved: saved})
	historyChanged := s.groupDepth == 0
	s.mu.Unlock()
	if !wasDirty {
		s.notifyDirty(true)
	}
	s.notifyEntityChanged(EntityChange{Kind: cfg.Kind + "_mod", ID: 0, Field: "customs"})
	if historyChanged {
		s.notifyHistoryChanged()
	}
	return nil
}

// ImportTexture is one texture file to import alongside a model. Name is the
// full in-archive path the file is written under (e.g.
// "war3mapImported\foo.blp") — the caller is responsible for synthesizing a
// collision-free name; ImportModel writes + registers it verbatim.
type ImportTexture struct {
	Name string
	Data []byte
}

// ImportModelReq is the request for Session.ImportModel: the converted MDX
// bytes (written under ModelName, an in-archive path like
// "war3mapImported\foo.mdx") plus any baked texture files.
type ImportModelReq struct {
	ModelName string
	MdxData   []byte
	Textures  []ImportTexture
}

// ImportModelResult reports the in-archive paths the import landed at, so the
// caller can preview the model and/or assign ModelPath to an object's "file"
// field.
type ImportModelResult struct {
	ModelPath    string
	TexturePaths []string
}

// ImportModel writes a converted MDX (and its baked textures) into the loaded
// map's archive under war3mapImported\, registers every written path in
// war3map.imp, and marks the session dirty so Save flushes the updated import
// table. The byte files go straight through s.source.write — buffered for MPQ
// sources (committed at flush()), durable immediately for folder sources — so
// ReadFile returns them for in-editor preview before Save.
//
// NOT undoable: like the World Editor's Import Manager, an imported asset is
// not a per-file undo step. (For folder-backed maps the bytes hit disk
// immediately; rolling them back would mean deleting files off the user's
// disk, which the editor deliberately doesn't do. The model-path *assignment*
// to an object — a separate SetObjectField call — IS undoable on its own.)
// Hence no recordCommand here.
func (s *Session) ImportModel(req ImportModelReq) (ImportModelResult, error) {
	if req.ModelName == "" {
		return ImportModelResult{}, fmt.Errorf("ImportModel: empty model name")
	}
	if len(req.MdxData) == 0 {
		return ImportModelResult{}, fmt.Errorf("ImportModel: empty model data")
	}

	s.mu.Lock()
	if !s.loaded {
		s.mu.Unlock()
		return ImportModelResult{}, fmt.Errorf("no map loaded")
	}
	src := s.source
	if src == nil {
		s.mu.Unlock()
		return ImportModelResult{}, fmt.Errorf("no source for writing")
	}

	// Lazily parse the existing war3map.imp once per session; start a fresh
	// v1 table when the map ships none. Cached on s.imp so repeated imports
	// don't re-read.
	if err := s.ensureImpLocked(src); err != nil {
		s.mu.Unlock()
		return ImportModelResult{}, err
	}

	// Write the MDX, then each texture. Register every written path in the
	// import table (Add is idempotent + normalizes slashes + uses StandardFlag).
	// No change-detection baseline refresh here: these bytes land under
	// war3mapImported\ (not in baselineFileNames, so never "stale"), and the
	// war3map.imp registry itself is re-encoded by the next Save (dirtyImports),
	// whose batch commit refreshes its baseline. See atomic_save.go.
	if err := src.write(req.ModelName, req.MdxData); err != nil {
		s.mu.Unlock()
		return ImportModelResult{}, fmt.Errorf("write %s: %w", req.ModelName, err)
	}
	s.imp.Add(req.ModelName)

	texPaths := make([]string, 0, len(req.Textures))
	for _, t := range req.Textures {
		if t.Name == "" {
			s.mu.Unlock()
			return ImportModelResult{}, fmt.Errorf("ImportModel: texture with empty name")
		}
		if err := src.write(t.Name, t.Data); err != nil {
			s.mu.Unlock()
			return ImportModelResult{}, fmt.Errorf("write %s: %w", t.Name, err)
		}
		s.imp.Add(t.Name)
		texPaths = append(texPaths, t.Name)
	}

	wasDirty := s.anyDirtyLocked()
	s.dirtyImports = true
	nowDirty := s.anyDirtyLocked()
	s.mu.Unlock()

	if !wasDirty && nowDirty {
		s.notifyDirty(true)
	}
	// One entity-changed event so subscribers (asset preview, import manager
	// panel) refresh. Kind "import" is a new tag; existing subscribers that
	// branch on Kind ignore it harmlessly.
	s.notifyEntityChanged(EntityChange{Kind: "import", ID: 0, Field: "model"})

	return ImportModelResult{ModelPath: req.ModelName, TexturePaths: texPaths}, nil
}

// ConvertObject converts one object to a different kind. Today only the
// doodad ↔ destructable pair is supported (they share roughly the same shape:
// a placed environment object with a model + pathing footprint), so the
// "convert" operation is essentially:
//
//  1. Build a new custom in dstKind, copying the source's MergedObject overrides
//     for any field whose FourCC ALSO exists in the destination kind's metadata.
//     Fields that have no analogue in the destination are silently dropped.
//  2. If the source was itself a custom row, DELETE it from the source kind
//     (so the user doesn't end up with two near-duplicates). If the source was
//     STOCK, leave it alone — stock objects can't be deleted and continue to
//     exist, with a console log noting the situation.
//  3. The whole thing is recorded as one convertObjectCmd so Ctrl+Z restores
//     everything: source custom (if it existed) comes back, destination custom
//     is removed.
//
// Returns the new destination custom's ID. The caller (UI) typically switches
// to dstKind + selects the new id after the call.
//
// Unsupported kind pairs return a clear error rather than half-converting —
// kind shapes vary enough (units have race/inventory, abilities have levels)
// that a generic blind copy across arbitrary pairs would produce broken
// objects. Extending to a new pair is intentionally explicit.
func (s *Session) ConvertObject(srcKind, srcID, dstKind string) (string, error) {
	if srcKind == dstKind {
		return "", fmt.Errorf("source and destination kinds are identical (%q)", srcKind)
	}
	// Whitelist: doodad↔destructable only for now. Add more pairs explicitly
	// when each pair's field-mapping semantics have been thought through.
	pair := srcKind + "→" + dstKind
	switch pair {
	case "doodads→destructables", "destructables→doodads":
		// ok
	default:
		return "", fmt.Errorf("conversion %s is not supported (only doodads↔destructables)", pair)
	}

	srcCfg, err := resolveKind(srcKind)
	if err != nil {
		return "", err
	}
	dstCfg, err := resolveKind(dstKind)
	if err != nil {
		return "", err
	}

	// Snapshot source object's merged view OUTSIDE the write lock — MergedObjects
	// takes its own RLock. We need overrides + base id + (for the delete branch)
	// whether the source was a custom.
	merged, _, err := MergedObjects(srcCfg)
	if err != nil {
		return "", fmt.Errorf("load src %s: %w", srcKind, err)
	}
	if _, ok := merged[srcID]; !ok {
		return "", fmt.Errorf("no %s object with id %q", srcKind, srcID)
	}
	// Source base id — for a stock row this IS srcID; for a custom it's the
	// row's BaseID. We use the SOURCE's base id to find an analogous base in
	// the destination kind — doodads and destructables don't share FourCCs so
	// there's no automatic mapping. Today the simplest sensible default is:
	// use the FIRST stock row of the destination kind as the new custom's base.
	// The user can edit it afterwards via the right-pane "base" pseudo-field
	// (or just edit field-by-field).
	dstBase, _, err := loadObjectBase(dstCfg)
	if err != nil {
		return "", fmt.Errorf("load dst %s: %w", dstKind, err)
	}
	if dstBase == nil || len(dstBase.Rows) == 0 {
		return "", fmt.Errorf("destination kind %q has no stock rows to base on", dstKind)
	}
	// Pick a deterministic base — sort the stock ids and grab the first. Avoids
	// flakiness in tests + makes round-trip behavior predictable.
	var dstBaseID string
	{
		ids := make([]string, 0, len(dstBase.Rows))
		for id := range dstBase.Rows {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		dstBaseID = ids[0]
	}

	// Identify which source overrides translate. For a CUSTOM source, the
	// overrides live on the custom's row directly; for a STOCK source they're
	// in OriginalEdits. MergedObjects already merged both onto src.Fields but we
	// want the raw on-disk overrides for translation (not the merged base+ovr).
	//
	// Take a read lock + copy the maps so the subsequent mutation under the undo
	// group can't race the snapshot read.
	var srcOverrides w3objmod.Overrides
	var srcCustomSnapshot *w3objmod.CustomObject
	s.mu.RLock()
	if srcMods := srcCfg.GetMods(s); srcMods != nil {
		if ci := findCustomIndex(srcMods, srcID); ci >= 0 {
			c := srcMods.Customs[ci]
			srcCustomSnapshot = &c
			srcOverrides = w3objmod.Overrides{}
			for k, v := range c.Overrides {
				srcOverrides[k] = v
			}
		} else if ei := findOriginalEditIndex(srcMods, srcID); ei >= 0 {
			srcOverrides = w3objmod.Overrides{}
			for k, v := range srcMods.OriginalEdits[ei].Overrides {
				srcOverrides[k] = v
			}
		}
	}
	s.mu.RUnlock()

	// Translate overrides — only FourCCs the destination metadata knows about.
	_, dstMeta, _ := loadObjectBase(dstCfg)
	carriedOverrides := w3objmod.Overrides{}
	dropped := 0
	for fourCC, val := range srcOverrides {
		if dstMeta != nil {
			if _, has := dstMeta.ByID[fourCC]; has {
				carriedOverrides[fourCC] = val
				continue
			}
		}
		dropped++
	}

	// Lock + apply: add destination custom + populate overrides + (optionally)
	// delete source custom. All under one group so a single Ctrl+Z reverts.
	s.BeginUndoGroup("Convert " + srcKind + " → " + dstKind)
	dstID, addErr := s.AddCustomObject(dstCfg, "", dstBaseID)
	if addErr != nil {
		_ = s.EndUndoGroup()
		return "", fmt.Errorf("create destination custom: %w", addErr)
	}
	// Populate carried overrides — each SetObjectField records its own
	// command under the group; Revert unwinds them in reverse order.
	for fourCC, val := range carriedOverrides {
		if err := s.SetObjectField(dstCfg, dstID, fourCC, val); err != nil {
			// Field FourCC came from dstMeta.ByID; the mutator's "unknown field"
			// path should not fire. Log and continue so we don't half-convert.
			log.Printf("ConvertObject: skip field %s: %v", fourCC, err)
		}
	}
	if srcCustomSnapshot != nil {
		if err := s.DeleteCustomObject(srcCfg, srcID); err != nil {
			log.Printf("ConvertObject: failed to delete source custom %s/%s: %v", srcKind, srcID, err)
		}
	} else {
		log.Printf("ConvertObject: source %s/%s is stock — leaving it in place (created %s/%s)", srcKind, srcID, dstKind, dstID)
	}
	if err := s.EndUndoGroup(); err != nil {
		return "", fmt.Errorf("close convert group: %w", err)
	}
	if dropped > 0 {
		log.Printf("ConvertObject: %s/%s → %s/%s: dropped %d override(s) with no analogue in destination", srcKind, srcID, dstKind, dstID, dropped)
	}
	return dstID, nil
}

// anyDirtyLocked reports whether the session holds any pending edit to any
// kind's per-map shadow + the other per-file dirty flags. Caller MUST hold
// s.mu (read or write). Used by the object-edit mutators to detect the 0→1
// transition that fires the public dirty event.
//
// Phase 2b: includes every per-kind shadow dirty flag. Add new dirty flags
// here AND to IsDirty() AND to Save()'s save-loop AND to Open/Close's reset
// block; otherwise the Save pill leaks state across map opens.
func (s *Session) anyDirtyLocked() bool {
	return s.dirtyUnits || s.dirtyDoodads || s.dirtyInfo || s.dirtyTerrain ||
		s.dirtyGameplay || s.dirtyStrings || s.dirtyUnitMods || s.dirtyItemMods ||
		s.dirtyAbilityMods || s.dirtyBuffMods || s.dirtyDestructibleMods ||
		s.dirtyDoodadMods || s.dirtyUpgradeMods ||
		s.dirtyTriggers || s.mapHeaderScriptDirty || s.dirtyImports ||
		s.dirtyRegions
}

// ---------------------------------------------------------------------------
// Phase-1b compat shims — preserve the previous method names so external
// callers (Wails app.go, future MCP additions) keep compiling. New code
// should target the kind-agnostic SetObjectField / AddCustomObject /
// DeleteCustomObject directly with the right config.
// ---------------------------------------------------------------------------

// SetUnitField is the Phase-1b alias of SetObjectField bound to UnitsConfig().
func (s *Session) SetUnitField(id, field, value string) error {
	return s.SetObjectField(UnitsConfig(), id, field, value)
}

// AddCustomUnit is the Phase-1b alias of AddCustomObject bound to UnitsConfig().
func (s *Session) AddCustomUnit(newID, baseID string) (string, error) {
	return s.AddCustomObject(UnitsConfig(), newID, baseID)
}

// DeleteCustomUnit is the Phase-1b alias of DeleteCustomObject bound to
// UnitsConfig().
func (s *Session) DeleteCustomUnit(id string) error {
	return s.DeleteCustomObject(UnitsConfig(), id)
}

// Gameplay returns the parsed war3mapMisc.txt (per-map gameplay constants),
// or nil if no map is loaded. The returned pointer is shared — callers must
// not mutate; use MutateGameplay for changes so dirty-tracking fires.
func (s *Session) Gameplay() *miscdata.File {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gameplay
}

// MutateGameplay applies fn to the in-memory war3mapMisc.txt file under the
// session write lock, flips dirtyGameplay, and fires an entity-changed
// event so subscribers (the Gameplay Constants Editor) repaint. Mirrors
// MutateInfo's shape — single-document mutation, no creation_number.
//
// MUST be the only path that mutates Gameplay so dirty-tracking stays
// honest. The editor calls this through the GameplayConstantsApply Wails
// binding.
func (s *Session) MutateGameplay(fn func(*miscdata.File)) error {
	if fn == nil {
		return fmt.Errorf("MutateGameplay: nil fn")
	}
	s.mu.Lock()
	if !s.loaded {
		s.mu.Unlock()
		return fmt.Errorf("no map loaded")
	}
	if s.gameplay == nil {
		s.gameplay = &miscdata.File{}
	}
	fn(s.gameplay)
	wasDirty := s.dirtyUnits || s.dirtyDoodads || s.dirtyInfo || s.dirtyTerrain || s.dirtyGameplay
	s.dirtyGameplay = true
	s.mu.Unlock()
	if !wasDirty {
		s.notifyDirty(true)
	}
	s.notifyEntityChanged(EntityChange{Kind: "gameplay", ID: 0, Field: "gameplay"})
	return nil
}

// applyTilesetSnapshot is the shared mutation helper used by SwapTileset's
// initial apply path and by swapTilesetCmd.Apply/Revert for undo/redo.
// Caller MUST hold s.mu. No notifications / dirty flips happen here —
// caller is responsible for those, post-unlock.
func applyTilesetSnapshot(s *Session, letter byte, ground, cliff []string, tileG, tileC []uint8) {
	s.terrain.Tileset = letter
	s.terrain.GroundTilesets = append([]string(nil), ground...)
	s.terrain.CliffTilesets = append([]string(nil), cliff...)
	for i := range s.terrain.Tiles {
		s.terrain.Tiles[i].GroundTexture = tileG[i]
		s.terrain.Tiles[i].CliffTexture = tileC[i]
	}
	s.info.Tileset = letter
}

// IsDirty reports whether the session holds unsaved edits to any in-memory
// map file. The flag is the OR of every per-file dirty flag — the UI cares
// only about "anything to save" granularity. Save itself reads each per-file
// flag and writes only what's dirty.
func (s *Session) IsDirty() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.anyDirtyLocked() || s.pendingSkyModel != nil
}

// pendingWrite is one fully-encoded file awaiting commit. setDirty flips this
// file's dirty flag (false = mark saved, true = re-dirty on a write failure);
// onSaved (optional) publishes a rebuilt object back to the session (the
// gameplay [Misc] fallback, the wct table). Both run under s.mu. Declared at
// package scope so the save-durability helpers in atomic_save.go
// (commitFolderWrites, checkStaleness, refreshBaselineAfterCommit) can take a
// []pendingWrite; SaveWith builds the slice and is the only producer.
type pendingWrite struct {
	name     string
	data     []byte
	setDirty func(s *Session, dirty bool)
	onSaved  func(s *Session)
}

// Save flushes every dirty in-memory file back through the source's write
// path. On success the dirty flag clears and a dirty=false event fires. It is
// the default-options entry point; SaveWith carries the force override.
func (s *Session) Save() error {
	return s.SaveWith(SaveOptions{})
}

// SaveWith flushes every dirty in-memory file back through the source's write
// path, honoring opts. On success the dirty flag clears and a dirty=false event
// fires.
//
// Save-durability (see atomic_save.go):
//   - Folder-backed maps commit all-or-nothing: every dirty file is encoded,
//     staged into an fsync'd sibling temp, and only once EVERY temp is written
//     are they renamed into place (prior bytes copied to <name>.bak first). A
//     crash/disk-full/encode error before the rename phase leaves every original
//     byte-for-byte intact — never a torn map (new units + old terrain).
//   - MPQ-backed maps already get all-or-nothing for free: writes buffer in
//     memory and flush() repacks the whole .w3x in one atomic temp+rename.
//   - External-change detection: unless opts.Force, the files about to be
//     written are re-stat'd against the Open-time baseline; if any drifted on
//     disk (another wc3-forge instance, an agent, or a human saved underneath
//     us) the save is REFUSED with ErrSourceChangedOnDisk and every dirty flag
//     is restored, rather than silently clobbering. Force overwrites anyway
//     (prior bytes still backed up to <name>.bak).
//
// A clean (non-dirty) .w3x save still takes the MPQ repack path — it does not
// falsely report success without writing.
func (s *Session) SaveWith(opts SaveOptions) (err error) {
	// mapIO diagnostics: trace every Save — its sequence id, which dirty files
	// were written, total bytes, duration, and outcome. saveTrace.Written +
	// .Bytes accumulate at each write site below; the deferred recorder snaps
	// the final state (ok/error) regardless of which return fired. See
	// mapio_diag.go. saveSeq is monotone so an agent can tell two saves apart.
	saveStart := time.Now()
	mapIODiag.saves.Add(1)
	saveTrace := &mapSaveTrace{Seq: mapIODiag.saveSeq.Add(1)}
	defer func() {
		saveTrace.SaveMs = time.Since(saveStart).Milliseconds()
		saveTrace.OK = err == nil
		if err != nil {
			saveTrace.Error = err.Error()
		}
		recordSaveTrace(saveTrace)
	}()

	// Prefetch each object kind's FieldMap BEFORE taking the encode read lock.
	// loadObjectBase self-RLocks s.mu, so calling it INSIDE the RLock below
	// risks a recursive read-lock deadlock (Go's RWMutex can deadlock on a
	// re-entrant RLock when a writer is queued between the two acquisitions).
	// The metadata is cached after first load, so this is cheap.
	objFieldMaps := make(map[*KindConfig]w3objmod.FieldMap, len(kindConfigs))
	for _, cfg := range kindConfigs {
		if _, meta, _ := loadObjectBase(cfg); meta != nil {
			objFieldMaps[cfg] = meta.FieldMap()
		}
	}

	var pending []pendingWrite
	var encErr error
	// addWrite queues an encoded file (no-op once an earlier encode errored, so
	// first error wins). Called under the write lock during the ENCODE PHASE.
	addWrite := func(name string, data []byte, err error, setDirty func(*Session, bool), onSaved func(*Session)) {
		if encErr != nil {
			return
		}
		if err != nil {
			encErr = fmt.Errorf("encode %s: %w", name, err)
			return
		}
		pending = append(pending, pendingWrite{name: name, data: data, setDirty: setDirty, onSaved: onSaved})
	}

	// noteWrite records one successfully-written file into the mapIO save trace.
	// Called immediately after each src.write succeeds so the trace lists only
	// files that actually committed to the source buffer.
	noteWrite := func(name string, n int) {
		saveTrace.Written = append(saveTrace.Written, name)
		saveTrace.Bytes += int64(n)
	}

	// ENCODE PHASE — under the WRITE lock. Every Encode reads shared session
	// state (s.units, s.doodads, …) and we clear each file's dirty flag, all
	// under one lock acquisition, so concurrent MUTATORS (which take the write
	// lock) can't (a) tear a half-encoded file mid-Save, nor (b) sneak in
	// between "we encoded it" and "we cleared its flag" and have their flag
	// wrongly cleared (which would drop their edit from the NEXT save — a data-
	// loss race the prior clear-after-write structure had). A mutation that
	// lands AFTER this lock releases correctly re-dirties the flag and is NOT in
	// our encoded bytes, so the next Save persists it. NO slow I/O happens here
	// — only in-memory serialization into `pending`; the writes + the MPQ repack
	// run UNLOCKED below on the now-immutable byte buffers.
	s.mu.Lock()
	if !s.loaded {
		s.mu.Unlock()
		return fmt.Errorf("no map loaded")
	}
	if !s.anyDirtyLocked() && s.pendingSkyModel == nil {
		// Nothing dirty. For folder-backed maps that genuinely means "no
		// work" (every file already on disk). For MPQ-backed maps a "Save"
		// still rewrites the archive at the source path so the user gets a
		// real, packed .w3x rather than a no-op that misleadingly reports
		// success — this is the SAME repack path a dirty save would take.
		src := s.source
		s.mu.Unlock()
		if mp, ok := src.(*mpqSource); ok {
			if err := mp.forceRepackAll(); err != nil {
				return err
			}
			// Keep the change-detection baseline current with our own repack so a
			// later dirty Save doesn't flag this clean repack as an external write.
			// (No staleness check on the clean path: nothing is dirty, so there's
			// nothing to lose — this is the explicit "give me a packed .w3x"
			// affordance.)
			s.restampArchiveBaseline(mp)
		}
		return nil
	}
	src := s.source
	info := s.info
	isLua := info != nil && info.Lua
	pendingSky := s.pendingSkyModel

	if s.dirtyUnits {
		data, err := unitsdoo.Encode(s.units)
		addWrite("war3mapUnits.doo", data, err, func(s *Session, d bool) { s.dirtyUnits = d }, nil)
	}
	if s.dirtyDoodads {
		data, err := doodadsdoo.Encode(s.doodads)
		addWrite("war3map.doo", data, err, func(s *Session, d bool) { s.dirtyDoodads = d }, nil)
	}
	if s.dirtyInfo {
		// Re-inject any captured TRIGSTR tokens so a localized map's Description
		// fields encode as their original tokens (keeping the war3map.wts
		// reference) rather than the resolved literal. reinjectInfoTokens returns
		// s.info unchanged for a non-localized map (no copy).
		data, err := w3i.Encode(reinjectInfoTokens(s.info, s.infoTokens))
		addWrite("war3map.w3i", data, err, func(s *Session, d bool) { s.dirtyInfo = d }, nil)
	}
	if s.dirtyStrings && len(s.strings) > 0 {
		// war3map.wts re-encode — a Map Info edit on a localized map updated a
		// TRIGSTR entry. Independent of every other file; canonical-format encode.
		data, err := wts.Encode(s.strings)
		addWrite("war3map.wts", data, err, func(s *Session, d bool) { s.dirtyStrings = d }, nil)
	}
	if s.dirtyTerrain {
		data, err := w3e.Encode(s.terrain)
		addWrite("war3map.w3e", data, err, func(s *Session, d bool) { s.dirtyTerrain = d }, nil)
	}
	if s.dirtyGameplay {
		// Encode a normalized COPY: an empty gameplay file still writes a
		// [Misc] block (so an explicit "delete every override" persists as an
		// empty section rather than the file vanishing). We must NOT mutate the
		// shared s.gameplay under the read lock, so synthesize a local instead.
		gp := s.gameplay
		if gp == nil || len(gp.Sections) == 0 {
			gp = &miscdata.File{Sections: []*miscdata.Section{{Name: "Misc"}}}
		}
		data, err := miscdata.Encode(gp)
		addWrite("war3mapMisc.txt", data, err,
			func(s *Session, d bool) { s.dirtyGameplay = d },
			func(s *Session) {
				// Keep the synthesized [Misc] in memory (mirrors legacy behavior)
				// so a later read sees a non-empty file. Only when s.gameplay was
				// nil/empty — never clobber a concurrently-populated gameplay.
				if s.gameplay == nil || len(s.gameplay.Sections) == 0 {
					s.gameplay = gp
				}
			})
	}
	// Per-kind object-shadow encodes (war3map.w3u/.w3t/.w3a/.w3b/.w3d/.w3h/.w3q).
	// GetDirty/GetMods read session fields (the caller holds the lock — they
	// don't lock themselves), the FieldMap was prefetched above, and
	// w3objmod.Encode is pure, so all three are safe under the read lock.
	for _, cfg := range kindConfigs {
		if encErr != nil {
			break
		}
		if !cfg.GetDirty(s) {
			continue
		}
		mods := cfg.GetMods(s)
		if mods == nil {
			mods = &w3objmod.File{Version: 3}
		}
		c := cfg
		data, err := w3objmod.Encode(mods, c.ShadowOpt, objFieldMaps[c])
		addWrite(c.ShadowFile, data, err, func(s *Session, d bool) { c.SetDirty(s, d) }, nil)
	}
	// Trigger Editor encodes. Hand-rolled-script maps write the Map Header text
	// straight to war3map.lua/.j; wtg-backed maps encode both wtg + wct (wtg
	// first since wct's blob order depends on it).
	if encErr == nil && s.triggerIsHandRolled {
		if s.mapHeaderScriptDirty {
			scriptName := s.mapHeaderScriptName
			if scriptName == "" {
				scriptName = "war3map.lua"
				if !isLua {
					scriptName = "war3map.j"
				}
			}
			// The synthetic Map Header script trigger's CustomText is the
			// authoritative source (index 0, invariant held by mutators).
			if s.triggers == nil || len(s.triggers.Triggers) == 0 {
				encErr = fmt.Errorf("hand-rolled-script map has no Map Header trigger to save")
			} else {
				content := []byte(s.triggers.Triggers[0].CustomText)
				addWrite(scriptName, content, nil, func(s *Session, d bool) { s.mapHeaderScriptDirty = d }, nil)
			}
		}
	} else if encErr == nil && s.dirtyTriggers {
		if s.triggers == nil {
			encErr = fmt.Errorf("dirtyTriggers set but no trigger tree loaded")
		} else {
			triggers := s.triggers
			td := TriggerDataSnapshot()
			var argc map[string]int
			if td != nil {
				argc = td.ArgumentCounts
			}
			wtgData, werr := wtg.Encode(triggers, argc)
			addWrite("war3map.wtg", wtgData, werr, func(s *Session, d bool) { s.dirtyTriggers = d }, nil)
			// wct is required for any map with custom-script triggers or a
			// global JASS header. Encode from a LOCAL copy so we don't mutate
			// the shared s.triggersWct under the lock; onSaved publishes the
			// rebuilt wct back to the session. (lw escapes to the heap because
			// the closure outlives this scope — Go's escape analysis handles it.)
			base := s.triggersWct
			if base == nil {
				base = &wct.File{Version: 0x80000004, SubVersion: 1}
			}
			localWct := *base
			localWct.CustomTexts = collectOrderedCustomTexts(triggers, base.IsPre131)
			lw := &localWct
			wctData, werr2 := wct.Encode(lw, triggers)
			addWrite("war3map.wct", wctData, werr2,
				func(s *Session, d bool) { s.dirtyTriggers = d },
				func(s *Session) { s.triggersWct = lw })
		}
	}
	// war3map.imp re-encode — flush the import table (the imported bytes were
	// already written by ImportModel). Independent of every other file.
	if s.dirtyImports {
		f := s.imp
		if f == nil {
			// Defensive: ImportModel always allocates s.imp before flipping the
			// dirty flag, but never crash if a future path trips this.
			f = &imp.File{Version: 1}
		}
		addWrite("war3map.imp", f.Encode(), nil, func(s *Session, d bool) { s.dirtyImports = d }, nil)
	}
	// war3map.w3r re-encode — the Region Editor surface. Independent of every
	// other file; w3r.Encode is byte-faithful for an unmodified table.
	if s.dirtyRegions {
		f := s.regions
		if f == nil {
			// Defensive: region mutators always allocate s.regions first; a
			// version-5 empty table is a valid (and tiny) on-disk shape.
			f = &w3r.File{Version: 5}
		}
		data, err := w3r.Encode(f)
		addWrite("war3map.w3r", data, err, func(s *Session, d bool) { s.dirtyRegions = d }, nil)
	}
	// Mark every queued file SAVED + publish rebuilt objects, still under the
	// write lock — a concurrent mutator can't run between encode and this clear,
	// so it can't have its flag wrongly cleared. (On a later write FAILURE the
	// unwritten files are re-dirtied below.)
	if encErr == nil {
		for _, pw := range pending {
			pw.setDirty(s, false)
			if pw.onSaved != nil {
				pw.onSaved(s)
			}
		}
	}
	s.mu.Unlock()
	// END ENCODE PHASE.

	if encErr != nil {
		return encErr
	}
	if src == nil {
		return fmt.Errorf("no source for writing")
	}

	// reDirtyAll restores every queued file's dirty flag (cleared under the
	// encode lock above) when the WHOLE commit is aborted — a staleness refusal,
	// or the all-or-nothing folder commit failing. The next Save then re-encodes
	// + retries them. Per-file partial failures use the narrower pending[i:]
	// re-dirty in the MPQ loop below instead.
	reDirtyAll := func() {
		s.mu.Lock()
		for _, pw := range pending {
			pw.setDirty(s, true)
		}
		s.mu.Unlock()
	}

	// EXTERNAL-CHANGE CHECK (unless Force). Refuse to clobber files another
	// editor/agent/human changed on disk since Open; restore dirty flags so the
	// edits aren't lost and the caller can retry (or force). Folder sources are
	// checked per-file against the names we're about to write; MPQ sources are
	// checked whole-archive (the repack replaces the single .w3x).
	if !opts.Force {
		var staleErr error
		if mp, ok := src.(*mpqSource); ok {
			staleErr = s.checkArchiveStaleness(mp)
		} else {
			staleErr = s.checkStaleness(src, pending)
		}
		if staleErr != nil {
			reDirtyAll()
			return staleErr
		}
	}

	// COMMIT PHASE — unlocked I/O on the immutable encoded buffers. Dirty flags
	// were already cleared under the encode lock above.
	if fs, ok := src.(folderSource); ok {
		// Folder: all-or-nothing batch (stage fsync'd temps, then back up + atomic
		// rename each into place — see commitFolderWrites). A failure before the
		// rename phase leaves every original untouched; re-dirty everything so the
		// next Save retries. The flags are restored as a unit because the batch is
		// atomic — there's no meaningful per-file partial state to preserve.
		if cerr := commitFolderWrites(fs, pending); cerr != nil {
			reDirtyAll()
			return cerr
		}
		for _, pw := range pending {
			noteWrite(pw.name, len(pw.data))
		}
		// Re-stamp the baseline for the files we just wrote so the NEXT Save in
		// this session doesn't read our own write back as an external change.
		s.refreshBaselineAfterCommit(fs.root, pending)
	} else {
		// MPQ (or other buffered source): per-file write into the in-memory
		// buffer, then the single atomic repack at flush() below. On the first
		// write error, re-dirty the failed file + every file after it (none of
		// those were written) so the next Save retries them; earlier files stayed
		// buffered + clean. (An ENCODE error short-circuited before any write.)
		for i := range pending {
			pw := pending[i]
			if werr := src.write(pw.name, pw.data); werr != nil {
				s.mu.Lock()
				for _, rest := range pending[i:] {
					rest.setDirty(s, true)
				}
				s.mu.Unlock()
				return fmt.Errorf("write %s: %w", pw.name, werr)
			}
			noteWrite(pw.name, len(pw.data))
		}
	}
	if pendingSky != nil {
		scriptName := "war3map.j"
		if isLua {
			scriptName = "war3map.lua"
		}
		orig, ok, err := src.read(scriptName)
		if err != nil {
			return fmt.Errorf("read %s: %w", scriptName, err)
		}
		if !ok || len(orig) == 0 {
			// Most real maps ship a script; if the file is missing entirely
			// we don't try to synthesize one here — that's a much bigger ask
			// (we'd need a stub `function main`/`function config` skeleton).
			// Surface the missing-file as a friendly error so the picker
			// can toast it. The pendingSky stays in place so a future Save
			// after the user adds a script can commit.
			return fmt.Errorf("write SetSkyModel: %s not present in map", scriptName)
		}
		updated, err := rewriteSetSkyModel(orig, *pendingSky, isLua)
		if err != nil {
			return fmt.Errorf("rewrite SetSkyModel in %s: %w", scriptName, err)
		}
		if err := src.write(scriptName, updated); err != nil {
			return fmt.Errorf("write %s: %w", scriptName, err)
		}
		noteWrite(scriptName, len(updated))
		// The sky rewrite is a read-modify-write that bypasses the batch commit
		// (it reads the CURRENT script and rewrites only the SetSkyModel line, so
		// it inherently preserves any external edits — no staleness check needed),
		// but it does leave the script's on-disk stamp stale. Refresh the folder
		// baseline so the NEXT Save doesn't read our own sky write as external.
		s.refreshBaselineEntry(src, scriptName)
		s.mu.Lock()
		s.pendingSkyModel = nil
		s.mu.Unlock()
	}

	// Commit any buffered changes durably. For folder sources this is a no-op
	// (write/delete already hit disk). For MPQ sources this is the single
	// atomic repack that rewrites the .w3x at the source path. If it fails the
	// per-file dirty flags have already cleared above, but the buffered bytes
	// stay pending in the source so a later Save retries the repack; surface
	// the error so the UI doesn't falsely report success.
	if err := src.flush(); err != nil {
		return err
	}
	// MPQ: the repack just rewrote the .w3x at the source path, so its Open-time
	// stamp is now stale against our own write. Re-stamp so the NEXT Save doesn't
	// read our repack back as an external change. (Folder sources already
	// refreshed per-file via refreshBaselineAfterCommit above.)
	if mp, ok := src.(*mpqSource); ok {
		s.restampArchiveBaseline(mp)
	}

	// Fire the public dirty=false event only when everything cleared. (If a
	// later per-file write fails we'll have returned above with the partial
	// clear in place; the next Save call clears the rest.)
	s.notifyDirty(false)
	return nil
}

// OnDirtyChanged subscribes to dirty-state-change notifications. Fired
// AFTER the lock is released, so listeners may safely call back into Session.
// Bool argument is the new dirty value (true = pending edits, false = clean).
//
// No-op when the dirty state doesn't actually change (e.g. a second MoveUnit
// when the session is already dirty does not re-fire).
func (s *Session) OnDirtyChanged(fn func(dirty bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirtyListeners = append(s.dirtyListeners, fn)
}

func (s *Session) notifyDirty(dirty bool) {
	s.mu.RLock()
	listeners := make([]func(bool), len(s.dirtyListeners))
	copy(listeners, s.dirtyListeners)
	s.mu.RUnlock()
	for _, fn := range listeners {
		fn(dirty)
	}
}

// OnEntityChanged subscribes to entity-mutation notifications. Fires AFTER the
// session lock is released, so listeners may safely call back into Session.
// Payload carries the (kind, id) of the changed entity, the conceptual field
// that moved, and (for position-field changes) the new coordinates — so
// scene-side subscribers don't need a round-trip fetch to repaint.
//
// Today only Field=="position" is emitted (from MoveUnit). Future mutators
// (rotation, type swap) emit their own Field tags and either populate fields
// added here, or expose their own per-event payload type.
func (s *Session) OnEntityChanged(fn func(EntityChange)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entityListeners = append(s.entityListeners, fn)
}

func (s *Session) notifyEntityChanged(c EntityChange) {
	s.mu.RLock()
	listeners := make([]func(EntityChange), len(s.entityListeners))
	copy(listeners, s.entityListeners)
	s.mu.RUnlock()
	for _, fn := range listeners {
		fn(c)
	}
}

// BridgeCallEvent is the payload for OnBridgeCall — one event per dispatched
// MCP bridge handler. Powers the in-page Agent Console (BridgeConsole.svelte)
// so the user can see, live, what every connected agent is doing.
//
// Field shape kept intentionally flat + JSON-friendly so it rides cleanly
// across the Wails event bus.
//
// ParamsSummary / Result are pre-truncated (~120 chars) — the console is a
// streaming live view, not a full audit log. Heavy payloads (terrain DTOs,
// full unit lists) shouldn't dominate the row width.
type BridgeCallEvent struct {
	Timestamp      time.Time `json:"timestamp"`
	Method         string    `json:"method"`
	ParamsSummary  string    `json:"params_summary"` // "" when the handler took no params
	Result         string    `json:"result"`         // "ok" when the handler returned a void-ish payload
	Error          string    `json:"error"`          // "" on success
	DurationMicros int64     `json:"duration_micros"`
}

// OnBridgeCall subscribes a listener to MCP-handler-dispatch events. Fires
// AFTER the handler completes (success or error), so the listener sees the
// final outcome. Lock+copy pattern matches the other listener buses; listeners
// must not call back into bridge handlers from this callback.
func (s *Session) OnBridgeCall(fn func(BridgeCallEvent)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bridgeCallListeners = append(s.bridgeCallListeners, fn)
}

// notifyBridgeCall fires every BridgeCall listener with the given event.
// Exported (capitalized) so the bridge dispatch wrapper in handlers.go can
// invoke it from the bridge.Handler shim. Lock+copy then invoke outside the
// lock — same shape as notifySelection/notifyDirty/notifyEntityChanged.
func (s *Session) notifyBridgeCall(c BridgeCallEvent) {
	s.mu.RLock()
	listeners := make([]func(BridgeCallEvent), len(s.bridgeCallListeners))
	copy(listeners, s.bridgeCallListeners)
	s.mu.RUnlock()
	for _, fn := range listeners {
		fn(c)
	}
}

// NotifyBridgeCall is the exported wrapper used by the bridge-dispatch shim in
// handlers.go (which lives in the same package but uses this name for
// readability + grep-ability when scanning for "what fires the agent console").
func (s *Session) NotifyBridgeCall(c BridgeCallEvent) {
	s.notifyBridgeCall(c)
}

// OnUICommand subscribes to UI-command events from MCP handlers. Used by App
// to forward `view.*` / `camera.*` MCP calls into the JS-side test-command
// bus. Lock+copy pattern matches the other listener buses.
func (s *Session) OnUICommand(fn func(string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uiCommandListeners = append(s.uiCommandListeners, fn)
}

// EmitUICommand fires a UI-command string at every subscriber. MCP handlers
// invoke this for surfaces (View menu visibility, terrain/doodad mode, camera
// positioning) whose authoritative state lives in JS.
func (s *Session) EmitUICommand(cmd string) {
	s.mu.RLock()
	listeners := make([]func(string), len(s.uiCommandListeners))
	copy(listeners, s.uiCommandListeners)
	s.mu.RUnlock()
	for _, fn := range listeners {
		fn(cmd)
	}
}

// SetDiagnostics stores the latest frontend diagnostics snapshot (raw JSON)
// and stamps the receive time. Called ~5Hz from App.ReportDiagnostics.
func (s *Session) SetDiagnostics(snapshot string) {
	s.diagMu.Lock()
	s.diagJSON = snapshot
	s.diagAtNS = time.Now().UnixNano()
	s.diagMu.Unlock()
}

// Diagnostics returns the last reported snapshot and how stale it is. ok is
// false when the frontend has never reported (no map open / pre-first-frame).
func (s *Session) Diagnostics() (snapshot string, ageMs int64, ok bool) {
	s.diagMu.Lock()
	defer s.diagMu.Unlock()
	if s.diagAtNS == 0 {
		return "", 0, false
	}
	return s.diagJSON, (time.Now().UnixNano() - s.diagAtNS) / 1e6, true
}

// AgentLabel returns the free-form label most recently set by an MCP client.
// Empty when no agent has labeled this window.
func (s *Session) AgentLabel() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.agentLabel
}

// SetAgentLabel replaces the agent label and fires the change bus. No-op
// (and no notification) when the value matches the current label, so
// repeated SetAgentLabel("foo") calls don't churn the window title.
func (s *Session) SetAgentLabel(label string) {
	s.mu.Lock()
	if s.agentLabel == label {
		s.mu.Unlock()
		return
	}
	s.agentLabel = label
	s.mu.Unlock()
	s.notifyAgentLabel(label)
}

// OnAgentLabelChanged subscribes to agent-label changes. Fires AFTER the
// session lock is released, so listeners may call back into Session safely.
// The App layer uses this to rebuild the OS window title when an agent
// re-labels its instance.
func (s *Session) OnAgentLabelChanged(fn func(string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agentLabelListeners = append(s.agentLabelListeners, fn)
}

func (s *Session) notifyAgentLabel(label string) {
	s.mu.RLock()
	listeners := make([]func(string), len(s.agentLabelListeners))
	copy(listeners, s.agentLabelListeners)
	s.mu.RUnlock()
	for _, fn := range listeners {
		fn(label)
	}
}
