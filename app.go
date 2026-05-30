package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/StephenSHorton/wc3-forge/internal/forge"
	"github.com/StephenSHorton/wc3-forge/internal/formats/doodadsdoo"
	"github.com/StephenSHorton/wc3-forge/internal/formats/miscdata"
	"github.com/StephenSHorton/wc3-forge/internal/formats/unitsdoo"
	"github.com/StephenSHorton/wc3-forge/internal/formats/w3i"
	"github.com/StephenSHorton/wc3-forge/internal/wc3launch"
	"github.com/StephenSHorton/wc3-forge/internal/wc3path"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// Wails event names emitted from Go → JS. Defined as constants so the
// frontend strings stay in sync with what's actually emitted.
const (
	eventSelectionChanged = "wc3-forge:selection-changed"
	eventMapChanged       = "wc3-forge:map-changed"
	eventDirtyChanged     = "wc3-forge:dirty-changed"
	eventEntityChanged    = "wc3-forge:entity-changed"
	eventDevSetAnim       = "wc3-forge:dev-set-anim"
	// eventBridgeCall streams every MCP-handler dispatch to the in-page Agent
	// Console (frontend/src/BridgeConsole.svelte). Fired AFTER the handler
	// completes — payload carries timestamp, method, params summary, result
	// summary, error, and duration_micros (see forge.BridgeCallEvent).
	eventBridgeCall       = "wc3-forge:bridge-call"
	// eventTestCommand is reused by the bridge UI-command bus: MCP handlers
	// like view.set_mode emit a UI-command string through Session.EmitUICommand,
	// App forwards it on this event, and the existing App.svelte test-driver
	// hook applies the change. Same channel used by the verification automation,
	// so the contract is mature.
	eventTestCommand      = "wc3-forge:test-command"
	// eventStartupCamera carries a camera spec ("x,y,z,distance"). Used by the
	// --camera CLI flag at startup AND by the camera.set_view MCP handler at
	// runtime. The JS receiver pans + sets distance via the existing
	// startup-camera handler in App.svelte.
	eventStartupCamera    = "wc3-forge:startup-camera"
	// eventHistoryChanged fires after any undo/redo stack mutation (mutator
	// recordCommand, BeginUndoGroup commit, Undo, Redo, ClearHistory). The
	// UI uses this to refresh the Edit menu enable state + the History panel.
	// Payload mirrors HistoryList (forge.HistoryState).
	eventHistoryChanged   = "wc3-forge:history-changed"
	// eventCloseRequested fires when the user clicks the window close
	// button while the session has unsaved edits. App.svelte subscribes
	// and shows its "unsaved changes" modal; the close is aborted by the
	// onBeforeClose handler in main.go until the user explicitly chooses
	// Save & Quit, Discard & Quit, or Cancel.
	eventCloseRequested   = "wc3-forge:close-requested"
	// eventCASCRemounted fires after the user points wc3-forge at a valid
	// install via SetWC3InstallPath and the CASC mount is reopened live.
	// App.svelte subscribes and purges the viewer's resource cache + reloads
	// the current map so the stock textures/models that 404'd against the
	// previous (missing/invalid) install get re-fetched through the new mount
	// — no restart needed.
	eventCASCRemounted    = "wc3-forge:casc-remounted"
)

// App is the Wails-bindable surface exposed to the frontend. Every method
// here becomes an awaitable JS function under wailsjs/go/main/App.
//
// App is intentionally thin: it shapes the Go-JS boundary (JSON-friendly
// types, no opaque pointers) and delegates to forge.Current. The MCP bridge
// handlers in internal/forge/handlers.go cover the same surface for AI
// clients — both routes converge on forge.Session.
type App struct {
	ctx context.Context
	// forceQuitting is flipped true by ForceQuit so the next OnBeforeClose
	// callback knows the user already confirmed via the modal and lets the
	// close proceed without re-prompting.
	forceQuitting bool
	// pidTag is the cached " — PID <n>" suffix for the OS window title. The
	// PID never changes for this process so we format it once at startup.
	pidTag string
}

func NewApp() *App {
	return &App{}
}

// LogJS writes a JS-side message to wc3-forge.log next to the executable.
// Crude but reliable: GUI Wails apps have no console, so this is how
// frontend code surfaces diagnostics during early-bootstrap debugging.
func (a *App) LogJS(message string) {
	f, err := os.OpenFile("wc3-forge.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format("15:04:05.000"), message)
}

// buildWindowTitle composes the OS-visible window title from the four inputs
// that drive it. Pure so it's unit-testable without spinning up Wails.
//
// Format (matches what an agent sees as it labels its instance):
//
//	[* ]<map-or-default>[ — <label>] — PID <n>
//
// The leading "* " marks unsaved edits; <map-or-default> is the loaded map
// name or "wc3-forge" when no map is open; the label segment is omitted when
// empty (whitespace-only inputs also collapse to omitted). The PID suffix is
// always present so users can pair a window with the PID an agent reports.
func buildWindowTitle(mapName, label string, dirty bool, pidTag string) string {
	leading := ""
	if dirty {
		leading = "* "
	}
	primary := strings.TrimSpace(mapName)
	if primary == "" {
		primary = "wc3-forge"
	}
	mid := ""
	if l := strings.TrimSpace(label); l != "" {
		mid = " — " + l
	}
	return leading + primary + mid + pidTag
}

// refreshWindowTitle re-reads current session truth and pushes a new OS
// window title. Cheap to call from any of the three buses (map/dirty/label)
// because the underlying state lookups are RLock-only.
func (a *App) refreshWindowTitle() {
	if a.ctx == nil {
		return
	}
	var mapName string
	if info := forge.Current.Info(); info != nil {
		mapName = info.Name
	}
	runtime.WindowSetTitle(a.ctx, buildWindowTitle(
		mapName,
		forge.Current.AgentLabel(),
		forge.Current.IsDirty(),
		a.pidTag,
	))
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	// Forward the startup --camera spec (if any) to the frontend. App.svelte
	// listens for this event and applies the spec on the first map-load.
	if startupCameraSpec != "" {
		// Defer one tick so the frontend's EventsOn listener is registered by
		// the time we emit. runtime.EventsEmit doesn't queue for listeners
		// that haven't subscribed yet.
		go func() {
			// Wait until reasonable assumption that App.svelte's onMount has
			// registered EventsOn. Empirical: 500ms is comfortably enough
			// after Wails finishes JS bootstrapping.
			time.Sleep(500 * time.Millisecond)
			runtime.EventsEmit(a.ctx, "wc3-forge:startup-camera", map[string]any{
				"spec": startupCameraSpec,
			})
		}()
	}
	if startupPickSelfTest {
		// Fire after a longer delay than the camera spec so map-load + asset
		// streaming have had time to settle. The JS handler walks every
		// unit+doodad instance, projects its bound-center to canvas pixels,
		// calls rayPick at that pixel, and reports per-instance success/mismatch.
		go func() {
			time.Sleep(4 * time.Second)
			runtime.EventsEmit(a.ctx, "wc3-forge:pick-self-test", map[string]any{})
		}()
	}
	// Forward Go-side selection changes to the frontend.
	forge.Current.OnSelectionChanged(func(s forge.SelectionState) {
		runtime.EventsEmit(a.ctx, eventSelectionChanged, s)
	})
	// OS-visible Wails window title is the composition of (dirty marker,
	// loaded map name, agent label, PID). Three independent buses can change
	// any of those, so each one calls back into refreshWindowTitle which
	// re-reads current truth and pushes a single WindowSetTitle. The JS side's
	// document.title only updates the inner WebView2 child window, which the
	// user never sees; runtime.WindowSetTitle hits the outer Wails window so
	// the taskbar + alt-tab entry pick up changes too. The PID suffix is
	// cached once — it can't change for the life of this process.
	a.pidTag = fmt.Sprintf(" — PID %d", os.Getpid())
	// Forward map open/close from any source (App method OR bridge OR
	// --open flag at startup) to the frontend so it reloads, AND refresh
	// the window title since the map name segment just changed.
	forge.Current.OnMapChanged(func(loaded bool) {
		runtime.EventsEmit(a.ctx, eventMapChanged, map[string]any{"loaded": loaded})
		a.refreshWindowTitle()
	})
	// Forward dirty-state changes (MoveUnit edits, Save flushes) so the UI
	// can keep its modified-dot + Save-button enable state in sync without
	// polling. Payload mirrors the map-changed shape: { dirty: bool }.
	forge.Current.OnDirtyChanged(func(dirty bool) {
		runtime.EventsEmit(a.ctx, eventDirtyChanged, map[string]any{"dirty": dirty})
		a.refreshWindowTitle()
	})
	// Agents can label this wc3-forge window via the window.set_title MCP
	// handler so users running multiple parallel instances tell them apart
	// in the taskbar/alt-tab list without memorizing PIDs.
	forge.Current.OnAgentLabelChanged(func(string) {
		a.refreshWindowTitle()
	})
	// Push an initial title now so the bare "wc3-forge" Wails default is
	// replaced with our PID-tagged version even before the first map opens.
	a.refreshWindowTitle()
	// Forward entity-change notifications (MoveUnit, future SetRotation/etc.)
	// so two stale views can refresh in lockstep:
	//   - Properties panel re-fetches the primary entity to repaint inputs
	//   - Scene re-positions/re-rotates/etc. the rendered model
	// The payload rides the new position (for Field=="position") so the
	// scene subscriber doesn't need a follow-up fetch — see EntityChange.
	// Wails marshals the struct via its json tags; the JS side reads
	// camel-case-not (the tags are lowercase: kind/id/field/position).
	forge.Current.OnEntityChanged(func(c forge.EntityChange) {
		runtime.EventsEmit(a.ctx, eventEntityChanged, c)
	})
	// History-changed bus — forward snapshot of both stacks so the UI Edit
	// menu can rebuild its enabled state + labels in one round-trip. Fired
	// after every mutator, undo, redo, group-end, and ClearHistory call.
	forge.Current.OnHistoryChanged(func() {
		runtime.EventsEmit(a.ctx, eventHistoryChanged, forge.Current.HistoryList())
	})
	// Bridge-call observability — every MCP handler dispatch fires through
	// here. Forwarded to the in-page Agent Console (BridgeConsole.svelte) so
	// the user can see, live, what every connected agent is doing.
	forge.Current.OnBridgeCall(func(c forge.BridgeCallEvent) {
		runtime.EventsEmit(a.ctx, eventBridgeCall, c)
	})
	// UI-command bus — bridge handlers (view.set_mode, view.set_doodad_…,
	// camera.set_view) emit a free-form command string through here. The App
	// layer routes camera commands to the startup-camera event (the JS-side
	// camera-spec handler) and other commands to the test-command event (the
	// existing test-driver dispatch). Single layer of forwarding keeps the
	// forge package Wails-free while still letting bridge handlers reach
	// JS-owned state.
	forge.Current.OnUICommand(func(cmd string) {
		if a.ctx == nil {
			return
		}
		if rest, ok := strings.CutPrefix(cmd, "camera.set "); ok {
			runtime.EventsEmit(a.ctx, eventStartupCamera, map[string]any{"spec": rest})
			return
		}
		runtime.EventsEmit(a.ctx, eventTestCommand, map[string]any{"cmd": cmd})
	})
}

// OpenMapDialog presents an OS folder picker and returns the selected
// directory, or "" if the user cancelled.
func (a *App) OpenMapDialog() (string, error) {
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Select an extracted .w3x folder",
	})
}

// OpenMapFileDialog presents an OS file picker for packaged WC3 maps
// (.w3x / .w3m / .mpq) and returns the selected file, or "" if cancelled.
// MPQ-backed sessions save in place; Save only returns ErrMPQRepackFailed
// (wrapped) on the rare genuinely-unpreservable archive.
func (a *App) OpenMapFileDialog() (string, error) {
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Select a Warcraft III map file",
		Filters: []runtime.FileFilter{
			{DisplayName: "Warcraft III maps (*.w3x; *.w3m; *.mpq)", Pattern: "*.w3x;*.w3m;*.mpq"},
			{DisplayName: "All files (*.*)", Pattern: "*.*"},
		},
	})
}

// modelFileFilter is the OS file-picker filter for importable 3D model
// formats (mirrors the map-file filter shape used by OpenMapFileDialog).
var modelFileFilter = []runtime.FileFilter{
	{DisplayName: "3D models (*.obj; *.gltf; *.glb; *.stl)", Pattern: "*.obj;*.gltf;*.glb;*.stl"},
	{DisplayName: "All files (*.*)", Pattern: "*.*"},
}

// ImportModelOptions carries the conversion knobs from the frontend import
// dialog. Scale is a uniform multiplier (0 => 1.0); UpAxis is "y"/"z"/"auto"
// ("" => "y"); FlipV flips texture V (OBJ/glTF bottom-origin -> WC3 top-origin).
type ImportModelOptions struct {
	Scale  float64 `json:"scale"`
	UpAxis string  `json:"upAxis"`
	FlipV  bool    `json:"flipV"`
}

// ImportModelResult reports where the converted model + textures landed in the
// loaded map's archive plus any non-fatal conversion warnings (placeholder
// textures, collision-suffixed names, missing UVs, …) for the UI to surface.
type ImportModelResult struct {
	ModelPath    string   `json:"modelPath"`
	TexturePaths []string `json:"texturePaths"`
	Warnings     []string `json:"warnings"`
}

// ConvertAndImportModel presents an OS file picker for a 3D model
// (.obj/.gltf/.glb/.stl), converts it to a minimal WC3 MDX with baked BLP
// textures, and imports both into the loaded map under war3mapImported\. A
// cancelled dialog returns a zero ImportModelResult and no error (matching how
// OpenMapFileDialog signals cancel via an empty path).
//
// The conversion + import funnel through forge.ConvertAndImport — the SAME
// code path the models.import MCP handler uses — and Current.ImportModel fires
// the dirty/entity-changed notifications the App's startup() listeners already
// forward to the frontend, so no explicit emit is needed here.
func (a *App) ConvertAndImportModel(opts ImportModelOptions) (ImportModelResult, error) {
	path, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title:   "Select a 3D model to import",
		Filters: modelFileFilter,
	})
	if err != nil {
		return ImportModelResult{}, err
	}
	if path == "" {
		return ImportModelResult{}, nil // cancelled
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ImportModelResult{}, fmt.Errorf("read %s: %w", path, err)
	}
	res, warnings, err := forge.ConvertAndImport(path, data, opts.Scale, opts.UpAxis, opts.FlipV)
	if err != nil {
		return ImportModelResult{}, err
	}
	return ImportModelResult{
		ModelPath:    res.ModelPath,
		TexturePaths: res.TexturePaths,
		Warnings:     warnings,
	}, nil
}

// WC3InstallStatusDTO reports whether a usable Warcraft III install (a CASC
// root containing .build.info) is resolvable, plus the path that was checked.
type WC3InstallStatusDTO struct {
	Available bool   `json:"available"`
	Path      string `json:"path"`
}

// WC3InstallStatus resolves the WC3 install path (env -> user-saved -> OS
// default) and reports whether it's a valid CASC root. The frontend calls
// this on startup and prompts the user to locate their install when
// Available is false — stock models, textures, and data live in CASC, so
// without it the viewport renders without stock assets (folder-bundled maps
// still work, which is why this is a warning, not a hard block).
func (a *App) WC3InstallStatus() WC3InstallStatusDTO {
	p := wc3path.Resolve()
	return WC3InstallStatusDTO{Available: wc3path.IsValidInstall(p), Path: p}
}

// BrowseForWC3Install opens an OS folder picker for the user to point at
// their Warcraft III install ROOT (the folder containing .build.info, not a
// subfolder like _retail_/x86_64). Returns "" if cancelled. It does NOT
// persist — pass the result to SetWC3InstallPath to validate + save + remount.
func (a *App) BrowseForWC3Install() (string, error) {
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Select your Warcraft III installation folder (the one containing .build.info)",
	})
}

// SetWC3InstallPath validates that path is a real WC3 install (has
// .build.info), persists it for future sessions, and re-opens the CASC mount
// so stock assets resolve immediately — no restart. Returns the resulting
// status. If the path isn't a valid install it errors WITHOUT persisting, so
// the UI can keep the picker open with a "that's not a WC3 install" message.
func (a *App) SetWC3InstallPath(path string) (WC3InstallStatusDTO, error) {
	if !wc3path.IsValidInstall(path) {
		return WC3InstallStatusDTO{Available: false, Path: path},
			fmt.Errorf("not a Warcraft III install: %q has no .build.info — pick the install root, not a subfolder", path)
	}
	if err := wc3path.Save(path); err != nil {
		return WC3InstallStatusDTO{Available: false, Path: path}, fmt.Errorf("save install path: %w", err)
	}
	reopenCASC()
	// Tell the frontend to drop its now-stale (404/empty) cached assets and
	// reload the scene against the freshly mounted CASC — see eventCASCRemounted.
	runtime.EventsEmit(a.ctx, eventCASCRemounted, map[string]any{"path": path})
	return WC3InstallStatusDTO{Available: true, Path: path}, nil
}

// MapStatus mirrors the wire shape used by map.status / bridge.ping.
//
// Lua reflects info.Lua — true when the loaded map's script is Lua, false
// for JASS-era maps. The frontend uses it to gate the File → Convert to Lua
// menu item (disabled when already Lua) and the Test Map error toast (shows
// a "Convert to Lua…" suggestion when ErrLuaOnly surfaces from codegen).
type MapStatus struct {
	Loaded    bool   `json:"loaded"`
	Path      string `json:"path,omitempty"`
	Name      string `json:"name,omitempty"`
	UnitCount int    `json:"unit_count"`
	Lua       bool   `json:"lua"`
}

// OpenMap loads the map at the given path. Returns the post-open status so
// the UI doesn't need a separate Status() roundtrip. The map-changed event
// fires automatically via the Session listener registered in startup —
// no explicit emit here.
func (a *App) OpenMap(path string) (MapStatus, error) {
	if path == "" {
		return MapStatus{}, fmt.Errorf("path is required")
	}
	if err := forge.Current.Open(path); err != nil {
		return MapStatus{}, err
	}
	return a.Status(), nil
}

// OpenMapInNewWindow launches a new wc3-forge process with --open <path> so
// the requested map opens in its own window without disturbing the map already
// loaded here. Multi-instance is supported on the bridge side via per-PID lock
// files under ~/.wc3-forge/mcp/, so two windows can run simultaneously.
func (a *App) OpenMapInNewWindow(path string) error {
	if path == "" {
		return fmt.Errorf("path is required")
	}
	return launchNewWindow(path)
}

// NewWindow launches a new wc3-forge process with no map loaded. Same
// multi-instance story as OpenMapInNewWindow — the new process gets its own
// bridge port + PID lock.
func (a *App) NewWindow() error {
	return launchNewWindow("")
}

// launchNewWindow spawns a detached wc3-forge child. If path is non-empty,
// the child is started with --open <path>; otherwise it starts cold. The
// process is released so it outlives us; without Release the child becomes
// a zombie on Unix or a leaked handle on Windows.
func launchNewWindow(path string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	var args []string
	if path != "" {
		args = []string{"--open", path}
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launch new window: %w", err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release new window: %w", err)
	}
	return nil
}

func (a *App) CloseMap() MapStatus {
	forge.Current.Close()
	return a.Status()
}

func (a *App) Status() MapStatus {
	s := MapStatus{Loaded: forge.Current.IsLoaded()}
	if !s.Loaded {
		return s
	}
	s.Path = forge.Current.Path()
	if info := forge.Current.Info(); info != nil {
		s.Name = info.Name
		s.Lua = info.Lua
	}
	if units := forge.Current.Units(); units != nil {
		s.UnitCount = len(units.Entities)
	}
	return s
}

// UnitDTO is the JS-facing unit shape. Subset of unitsdoo.Entity — the heavy
// fields (random data blobs, ability mods) get their own per-unit fetch later.
type UnitDTO struct {
	CreationNumber uint32     `json:"creation_number"`
	TypeID         string     `json:"type_id"`
	SkinID         string     `json:"skin_id"`
	Player         uint32     `json:"player"`
	Position       [3]float32 `json:"position"`
	Rotation       float32    `json:"rotation"`
	Scale          [3]float32 `json:"scale"`
	HitPointsPct   int32      `json:"hit_points_pct"`
	ManaPct        int32      `json:"mana_pct"`
	HeroLevel      uint32     `json:"hero_level"`
	GoldAmount     uint32     `json:"gold_amount"`
}

func (a *App) ListUnits() []UnitDTO {
	units := forge.Current.Units()
	if units == nil {
		return []UnitDTO{}
	}
	out := make([]UnitDTO, 0, len(units.Entities))
	for _, e := range units.Entities {
		out = append(out, UnitDTO{
			CreationNumber: e.CreationNumber,
			TypeID:         e.TypeID,
			SkinID:         e.SkinID,
			Player:         e.Player,
			Position:       e.Position,
			Rotation:       e.Rotation,
			Scale:          e.Scale,
			HitPointsPct:   e.HitPointsPct,
			ManaPct:        e.ManaPct,
			HeroLevel:      e.HeroLevel,
			GoldAmount:     e.GoldAmount,
		})
	}
	return out
}

// TerrainDTO is the JS-facing terrain shape. Heights + ground-texture indices
// as parallel slices for compact transfer (typed arrays on the JS side).
// Vertex (col, row) is at index row*Width + col, row 0 = bottom.
type TerrainDTO struct {
	Width        uint32     `json:"width"`         // vertex count along X
	Height       uint32     `json:"height"`        // vertex count along Y
	CenterOffset [2]float32 `json:"center_offset"` // game coords of vertex (0,0); bottom-left
	Heights      []float32  `json:"heights"`       // length = Width*Height, game-space Z
	// GroundTex is uint32 (not uint8/byte) so encoding/json emits a real
	// JSON array. Go's `encoding/json` special-cases []byte → base64 string,
	// which would silently turn each tile index into an ASCII character on
	// the JS side. Wire cost is tiny at these sizes.
	GroundTex []uint32 `json:"ground_tex"`  // length = Width*Height, palette idx 0..63
	Tileset   string   `json:"tileset"`     // single letter, e.g. "L" (Lordaeron)
	Palette   []string `json:"palette"`     // ground tileset FourCCs (palette key)
	// PaletteColors is one RGB triplet (0..255) per palette entry, sampled
	// from the actual WC3 tileset BLP/DDS via Terrain.slk. Lets the JS
	// renderer color tiles by their real texture average without having to
	// decode BLP/DDS itself. parallel-indexed with Palette.
	PaletteColors [][3]uint8 `json:"palette_colors"`
	// PaletteTextures is one `dir/file` asset path stem per palette FourCC
	// (e.g. `terrainart/icecrown/ice_dirt`). JS appends `.dds` (or `.blp`
	// fallback via the assetHandler's BLP↔DDS swap) and loads via
	// viewer.load. Empty string if Terrain.slk lookup failed for that entry.
	// parallel-indexed with Palette.
	PaletteTextures []string `json:"palette_textures"`
	// Per-corner cliff data. LayerHeight is the integer cliff step (0..15),
	// CliffTex is the index into CliffPalette (0..15; 15 commonly means
	// "no cliff" at this corner). CliffVar is the high 3 bits of TextureDetails
	// (per-corner variation), used in the cliff-mesh filename.
	// RampFlags packs: bit 0 = ramp, bit 1 = boundary.
	//
	// uint32 (not uint8) for the same reason as GroundTex above — Go's
	// encoding/json base64-encodes []byte/[]uint8.
	LayerHeight  []uint32 `json:"layer_height"`
	CliffTex     []uint32 `json:"cliff_tex"`
	CliffVar     []uint32 `json:"cliff_var"`
	// GroundVar is the per-corner ground variation index (0..31, low 5 bits
	// of TextureDetails). Picks the sub-tile within the palette texture's
	// 4×4 atlas; for "extended" textures (width = 2×height) the variation
	// indexes into the variation half (slots 16..31). HiveWE convention:
	//   non-extended: variation==0 → slot 0, else slot 15
	//   extended    : variation 0..15 → slot 16+v, 16 → 15, else 0
	GroundVar    []uint32 `json:"ground_var"`
	RampFlags    []uint32 `json:"ramp_flags"`
	CliffPalette []string `json:"cliff_palette"` // cliff tileset FourCCs
	// CliffPaletteTextures is one `texdir/texfile` asset path stem per cliff
	// palette FourCC, sourced from TerrainArt/CliffTypes.slk. JS appends
	// `.dds` (or `.blp` fallback) and loads via viewer.load. Empty string when
	// the SLK lookup failed. Parallel-indexed with CliffPalette. Used by the
	// custom cliff vertex/fragment shader (cliff-shader.ts), since HiveWE's
	// `vOffset.a = corner_cliff_texture` indexes into a 2D-array-texture of
	// these (one layer per palette); WebGL1 lacks sampler2DArray so we bind
	// them per-palette.
	CliffPaletteTextures []string `json:"cliff_palette_textures"`
	// Per-corner water data. WaterZ is the per-vertex water surface
	// elevation in studs (the per-tileset WaterInfo.Offset gets added
	// JS-side because Offset comes from Water.slk and lives with the
	// other rendering constants). HasWater is the per-vertex flag bit:
	// any cell whose 4 corners include at least one HasWater=1 gets a
	// water quad in the JS water mesh.
	WaterZ    []float32  `json:"water_z"`
	HasWater  []uint32   `json:"has_water"`
	Water     WaterInfo  `json:"water"`
	// Static shadow map (war3map.shd). 4× resolution per terrain tile —
	// dimensions are (Width-1)*4 × (Height-1)*4. Empty array when the map
	// doesn't ship a war3map.shd. Encoded as base64 by Wails since it's
	// []byte; JS decodes it (Uint8Array.from(atob(s), c => c.charCodeAt(0)))
	// before uploading to a GL texture. We deliberately keep the []byte
	// type — at 320KB for a typical map, the base64 wire is ~430KB which
	// is fine; expanding to []uint32 quadruples that for no benefit since
	// JS needs Uint8Array for gl.texImage2D anyway.
	ShadowMap       []byte `json:"shadow_map"`
	ShadowMapWidth  int    `json:"shadow_map_width"`
	ShadowMapHeight int    `json:"shadow_map_height"`
	// CellSkip is a per-cell flag (1 = skip terrain quad). Length =
	// (Width-1)*(Height-1), row-major (cell (i,j) at index j*(Width-1)+i).
	// Mirrors HiveWE's `gpu_ground_exists_data == 0` test — a cell is
	// "skipped" when it's a cliff cell (any 4-corner layer_height mismatch)
	// AND NOT a ramp entrance (all 4 corners ramp-flagged with non-
	// symmetric layer-height diagonals). On skipped cells, the cliff MDX
	// alone covers the vertical face; rendering the terrain quad there
	// produces diagonal slopes that poke through the cliff geometry —
	// the bug Problem 1 fixes. Stored as []uint32 for the same
	// base64-marshal-trap reason as GroundTex.
	CellSkip []uint32 `json:"cell_skip"`
	// SkyModel is the asset path for the sky dome to render behind/around the
	// terrain. Sourced from war3map.j/.lua's last SetSkyModel("...") call, or
	// a wc3-forge tileset→sky default if no script call is present. Empty
	// string means "render the flat clear color" (interior tilesets, citadel,
	// ruins, or maps that explicitly call SetSkyModel("")). Normalized to
	// lowercase + forward-slash + .mdx extension on the Go side, ready for
	// `viewer.load(skyModel, solver)`.
	SkyModel string `json:"sky_model"`
	// SkyModelFromScript is true when SkyModel came from a SetSkyModel call
	// in war3map.j/.lua, false when it came from the tileset-default table.
	// The Map Info sky picker uses this to default the override checkbox.
	SkyModelFromScript bool `json:"sky_model_from_script"`
	// CliffDisplacement is the per-corner SMOOTH height (just `Tilepoint.Z()`,
	// in studs) — i.e. the per-corner wobble WITHOUT the layer-step offset.
	// This is what the cliff vertex shader's per-vertex displacement consumes.
	//
	// CRITICAL: this is NOT the same as `Heights` (which is FinalZ = Z() +
	// (layer-2)*128). The cliff MDX geometry ALREADY encodes the full layer-step
	// height in its vertex Z range (a 1-step cliff MDX has vertices spanning
	// vPos.z = 0..128 studs). If we displaced cliffs by the full FinalZ, the
	// layer-step contribution would be applied TWICE (once via MDX geometry,
	// once via shader displacement) producing visibly 2×-tall cliff walls.
	//
	// This matches HiveWE's binding split (terrain.ixx:613 vs :652): the
	// terrain shader's SSBO binding 0 is `cliff_level_buffer` (full
	// FinalGroundHeights), but the cliff shader's SSBO binding 0 is
	// `ground_height_buffer` (CORNER_HEIGHT only — the wobble). HiveWE's cliff
	// shader pulls the layer-step contribution from `vOffset.z = min_layer - 2`
	// and from each MDX vertex's own Z; the per-corner displacement adds ONLY
	// the small wobble adjustment.
	//
	// Stored as parallel-indexed with `Heights` (length = Width*Height,
	// row-major); each entry is the corresponding Tilepoint's `Z()` in studs.
	CliffDisplacement []float32 `json:"cliff_displacement"`
}

// GetTerrain returns the terrain grid for rendering. Empty if no map loaded or
// the map has no .w3e file (older maps sometimes lack one).
func (a *App) GetTerrain() (TerrainDTO, error) {
	t := forge.Current.Terrain()
	if t == nil {
		return TerrainDTO{}, fmt.Errorf("no terrain loaded")
	}
	W := int(t.Width)
	H := int(t.Height)
	n := W * H
	heights := make([]float32, n)
	cliffDisp := make([]float32, n) // per-corner SMOOTH height (Z() only), in studs
	ground := make([]uint32, n)
	layer := make([]uint32, n)
	cliffTex := make([]uint32, n)
	cliffVar := make([]uint32, n)
	groundVar := make([]uint32, n)
	rampFlags := make([]uint32, n)
	waterZ := make([]float32, n)
	hasWater := make([]uint32, n)
	for i := 0; i < n; i++ {
		// FinalZ includes the layer_height contribution; the terrain mesh
		// renders corners at this world Z directly.
		heights[i] = t.Tiles[i].FinalZ()
		// CliffDisplacement is the per-corner SMOOTH height (just the wobble,
		// no layer-step), in studs. Used by the cliff vertex shader's
		// per-vertex displacement. HiveWE binds CORNER_HEIGHT (smooth) to the
		// cliff shader (terrain.ixx:652), NOT FinalGroundHeights — because the
		// cliff MDX geometry already encodes the layer-step rise in its
		// vertex Z values, so adding (layer-2)*128 again would double it.
		cliffDisp[i] = t.Tiles[i].Z()
		ground[i] = uint32(t.Tiles[i].GroundTexIdx())
		layer[i] = uint32(t.Tiles[i].LayerHeight)
		cliffTex[i] = uint32(t.Tiles[i].CliffTexIdx())
		cliffVar[i] = uint32((t.Tiles[i].TextureDetails >> 5) & 0x07)
		groundVar[i] = uint32(t.Tiles[i].TextureDetails & 0x1F)
		var rf uint32
		if t.Tiles[i].HasRamp() {
			rf |= 0x01
		}
		if t.Tiles[i].Boundary {
			rf |= 0x02
		}
		rampFlags[i] = rf
		waterZ[i] = t.Tiles[i].WaterZ()
		if t.Tiles[i].HasWater() {
			hasWater[i] = 1
		}
	}

	// HiveWE-compat: replace per-corner ground texture for cliff/ramp corners.
	//
	// .w3e stores `ground_texture` per corner verbatim — usually a sensible
	// "what's underneath the cliff" value, but on mixed-tileset maps (where
	// cliffs and floors come from different tilesets) it's often a stale or
	// editor-default value. HiveWE's `real_tile_texture` therefore overrides
	// the stored value with `cliff_to_ground_texture[cliff_index]` whenever
	// the corner sits on a cliff or ramp. The mapping comes from
	// TerrainArt/CliffTypes.slk's `groundtile` column (e.g. CIrb → Irbk).
	//
	// Symptom this fixes (verified vs HiveWE on Enfo's FFB v2.64f): shield
	// borders and tower edges stored as Cityscape `Ybtl` (tan brick) but
	// adjacent to Icecrown cliffs render with `Irbk` (Ice_RuneBricks blue-gray
	// stone) in HiveWE. Without the override wc3-forge showed the tan, which
	// looked wildly out of place vs the Icecrown floor.
	//
	// Single-tileset maps (the Lordaeron-Summer-only survival test fixture)
	// were unaffected because the cliff's groundtile FourCC is already the
	// floor's FourCC there — the override is a no-op.
	//
	// We compute corner_cliff per HiveWE: a cell's BL corner is "cliff" when
	// any of its 4 corners has a different layer_height than BL. Then a corner
	// is "on a cliff" when ANY of the cells it touches (at most 4 — itself
	// plus left, bottom, and bottom-left neighbors) is a cliff cell. Same
	// 4-cell window HiveWE walks in real_tile_texture.
	c2g := CliffToGroundTexture(t.CliffTilesets, t.GroundTilesets)
	cellCliff := make([]bool, n) // per-cell flag indexed by BL corner; valid for i<W-1 && j<H-1
	for j := 0; j < H-1; j++ {
		for i := 0; i < W-1; i++ {
			bl := j*W + i
			lhBL := t.Tiles[bl].LayerHeight
			if t.Tiles[bl+1].LayerHeight != lhBL ||
				t.Tiles[bl+W].LayerHeight != lhBL ||
				t.Tiles[bl+W+1].LayerHeight != lhBL {
				cellCliff[bl] = true
			}
		}
	}

	// Compute per-corner `corner_romp` flag — strict HiveWE port from
	// terrain.ixx::update_cliff_meshes (lines 1083-1187). A corner is "romp"
	// iff it sits on a ramp clifftrans piece that wc3-forge will actually
	// render. The strict rule is needed for the cell-skip pass below to match
	// HiveWE's update_ground_exists exactly.
	//
	// Two passes: vertical and horizontal ramps. Each spans 2 cells; HiveWE
	// detects them by checking 3-corner-tall (vertical) or 3-corner-wide
	// (horizontal) ramp-flag patterns with the middle-row corners at the
	// min of the endpoints' layer_height.
	//
	// We don't have access to the clifftrans MDX file existence at this stage
	// — Blizzard ships all the standard variants, so we conservatively assume
	// every spanning ramp gets a clifftrans. If the rare missing variant
	// shows up, that single ramp endpoint will show a small visible void;
	// acceptable tradeoff vs replicating the asset-existence check on the
	// Go side.
	cornerRomp := make([]bool, n)
	// Vertical ramps — span (i, j) and (i, j+1)
	for j := 0; j < H-2; j++ {
		for i := 0; i < W-1; i++ {
			bl := j*W + i
			br := bl + 1
			tl := bl + W
			tr := bl + W + 1
			ttl := bl + 2*W
			ttr := bl + 2*W + 1
			lhBL := int(t.Tiles[bl].LayerHeight)
			lhBR := int(t.Tiles[br].LayerHeight)
			lhTL := int(t.Tiles[tl].LayerHeight)
			lhTR := int(t.Tiles[tr].LayerHeight)
			lhTTL := int(t.Tiles[ttl].LayerHeight)
			lhTTR := int(t.Tiles[ttr].LayerHeight)
			ae := lhBL
			if lhTTL < ae {
				ae = lhTTL
			}
			cf := lhBR
			if lhTTR < cf {
				cf = lhTTR
			}
			if lhTL != ae || lhTR != cf {
				continue
			}
			rBL := t.Tiles[bl].HasRamp()
			rTL := t.Tiles[tl].HasRamp()
			rTTL := t.Tiles[ttl].HasRamp()
			rBR := t.Tiles[br].HasRamp()
			rTR := t.Tiles[tr].HasRamp()
			rTTR := t.Tiles[ttr].HasRamp()
			if rBL != rTL || rBL != rTTL || rBR != rTR || rBR != rTTR || rBL == rBR {
				continue
			}
			// Flat-ramp filter: all four spanning corners at same layer ⇒
			// degenerate AAAA-shape clifftrans that Blizzard doesn't ship.
			if lhBL == lhTTL && lhBL == lhBR && lhBL == lhTTR {
				continue
			}
			cornerRomp[bl] = true
			cornerRomp[tl] = true
		}
	}
	// Horizontal ramps — span (i, j) and (i+1, j)
	for j := 0; j < H-1; j++ {
		for i := 0; i < W-2; i++ {
			bl := j*W + i
			br := bl + 1
			brr := bl + 2
			tl := bl + W
			tr := bl + W + 1
			trr := bl + W + 2
			lhBL := int(t.Tiles[bl].LayerHeight)
			lhBR := int(t.Tiles[br].LayerHeight)
			lhBRR := int(t.Tiles[brr].LayerHeight)
			lhTL := int(t.Tiles[tl].LayerHeight)
			lhTR := int(t.Tiles[tr].LayerHeight)
			lhTRR := int(t.Tiles[trr].LayerHeight)
			ae := lhBL
			if lhBRR < ae {
				ae = lhBRR
			}
			bf := lhTL
			if lhTRR < bf {
				bf = lhTRR
			}
			if lhBR != ae || lhTR != bf {
				continue
			}
			rBL := t.Tiles[bl].HasRamp()
			rBR := t.Tiles[br].HasRamp()
			rBRR := t.Tiles[brr].HasRamp()
			rTL := t.Tiles[tl].HasRamp()
			rTR := t.Tiles[tr].HasRamp()
			rTRR := t.Tiles[trr].HasRamp()
			if rBL != rBR || rBL != rBRR || rTL != rTR || rTL != rTRR || rBL == rTL {
				continue
			}
			if lhBL == lhBRR && lhBL == lhTL && lhBL == lhTRR {
				continue
			}
			cornerRomp[bl] = true
			cornerRomp[br] = true
		}
	}

	// Compute per-cell "skip terrain quad" mask — STRICT HiveWE port of
	// update_ground_exists (terrain.ixx line 1043-1055):
	//   gpu_ground_exists[idx] = !(((corner_cliff[idx] || corner_romp[idx])
	//                                && !is_corner_ramp_entrance(i, j))
	//                              || corner_special_doodad[idx])
	//
	// (We don't track corner_special_doodad — its consumers are rare and the
	// data isn't surfaced in our doodadsdoo parser yet.)
	//
	// Now safe to use the strict rule because the new cliff-shader.ts
	// per-vertex displaces the cliff MDX to match `final_heights[corner]`,
	// matching HiveWE's cliff.vert line 35. The cliff/ramp meshes fully
	// cover the cells we skip; no pragmatic "keep any ramp corner" widening
	// needed (that was a978b4b's workaround for missing displacement).
	numCells := (W - 1) * (H - 1)
	cellSkip := make([]uint32, numCells)
	for j := 0; j < H-1; j++ {
		for i := 0; i < W-1; i++ {
			bl := j*W + i
			isCliff := cellCliff[bl]
			isRomp := cornerRomp[bl]
			if !isCliff && !isRomp {
				continue
			}
			br := bl + 1
			tl := bl + W
			tr := tl + 1
			rBL := t.Tiles[bl].HasRamp()
			rBR := t.Tiles[br].HasRamp()
			rTL := t.Tiles[tl].HasRamp()
			rTR := t.Tiles[tr].HasRamp()
			allRamp := rBL && rBR && rTL && rTR
			isRampEntrance := allRamp && !(t.Tiles[bl].LayerHeight == t.Tiles[tr].LayerHeight && t.Tiles[tl].LayerHeight == t.Tiles[br].LayerHeight)
			if !isRampEntrance {
				cellSkip[j*(W-1)+i] = 1
			}
		}
	}

	// Apply ramp-base +0.5-layer bump to heights, faithful port of
	// terrain.ixx::update_ground_heights lines 952-988. In HiveWE units the
	// bump is +0.5 layer-step; in our stud space one layer = 128 studs, so
	// the bump is +64 studs.
	//
	// For each cell whose all-four-corners are ramp AND non-symmetric, the
	// ramp-base corners (at the lower endpoint of the ramp's layer transition)
	// get their FinalZ lifted by +64 so the cliff vertex shader's lookup
	// returns a height that matches the actual ramp surface. Without this,
	// the cliff displacement assumes the corner sits at the lower layer
	// (which is geometrically the floor of the ramp, NOT the ramp surface),
	// and the ramp clifftrans MDX slope ends in mid-air at the ramp's base.
	//
	// The bump must be applied IDEMPOTENTLY (HiveWE iterates over multiple
	// tile-cells and writes the same corner index multiple times) — we use
	// direct assignment matching HiveWE's pattern.
	for j := 0; j < H-1; j++ {
		for i := 0; i < W-1; i++ {
			bl := j*W + i
			br := bl + 1
			tl := bl + W
			tr := bl + W + 1
			if !(t.Tiles[bl].HasRamp() && t.Tiles[tl].HasRamp() && t.Tiles[br].HasRamp() && t.Tiles[tr].HasRamp()) {
				continue
			}
			lhBL := t.Tiles[bl].LayerHeight
			lhBR := t.Tiles[br].LayerHeight
			lhTL := t.Tiles[tl].LayerHeight
			lhTR := t.Tiles[tr].LayerHeight
			if lhBL == lhTR && lhTL == lhBR {
				continue
			}
			base := lhBL
			if lhBR < base {
				base = lhBR
			}
			if lhTL < base {
				base = lhTL
			}
			if lhTR < base {
				base = lhTR
			}
			if lhBL == base {
				heights[bl] = t.Tiles[bl].Z() + (float32(lhBL)-2.0)*128.0 + 64.0
			}
			if lhBR == base {
				heights[br] = t.Tiles[br].Z() + (float32(lhBR)-2.0)*128.0 + 64.0
			}
			if lhTL == base {
				heights[tl] = t.Tiles[tl].Z() + (float32(lhTL)-2.0)*128.0 + 64.0
			}
			if lhTR == base {
				heights[tr] = t.Tiles[tr].Z() + (float32(lhTR)-2.0)*128.0 + 64.0
			}
		}
	}
	for j := 0; j < H; j++ {
		for i := 0; i < W; i++ {
			idx := j*W + i
			// Corner-touched cells: (i, j), (i-1, j), (i, j-1), (i-1, j-1).
			// Each is keyed by its BL corner index.
			anyCliff := false
			if i < W-1 && j < H-1 && cellCliff[idx] {
				anyCliff = true
			} else if i > 0 && j < H-1 && cellCliff[idx-1] {
				anyCliff = true
			} else if i < W-1 && j > 0 && cellCliff[idx-W] {
				anyCliff = true
			} else if i > 0 && j > 0 && cellCliff[idx-W-1] {
				anyCliff = true
			}
			// HiveWE's `a_romp` ORs `corner_romp[]`, NOT the raw per-corner
			// `corner_ramp` flag from war3map.w3e. `corner_romp` is set TRUE
			// only when a corner is part of an ACTUALLY-RENDERED ramp
			// clifftrans piece — much narrower than "any corner with the
			// ramp flag set."
			//
			// We previously used `t.Tiles[idx-N].HasRamp()` here (corner_ramp),
			// which over-fired the remap on plateau-interior corners adjacent
			// to ramp-flagged corners that are NOT part of a renderable ramp
			// span (e.g. the spear-tip plateau on Enfo's FFB at cells
			// (11..13, 67) — all flagged ramp=true but never matched by
			// HiveWE's vertical/horizontal ramp pre-pass because of the
			// `rBL != rBR` asymmetry requirement). The result: every plateau
			// corner touching one of those false-positive ramp corners got
			// remapped to Irbk, hiding the Itbk overlay that's supposed to
			// frame the cliff border with rune-brick. Symptom: cells (11,68)
			// and (12,68) rendered as flat Irbk in wc3-forge but as
			// Irbk-frame-with-Itbk-interior in HiveWE.
			//
			// Mirrors terrain.ixx::real_tile_texture line 755-768.
			anyRomp := cornerRomp[idx]
			if i > 0 && cornerRomp[idx-1] {
				anyRomp = true
			}
			if j > 0 && cornerRomp[idx-W] {
				anyRomp = true
			}
			if i > 0 && j > 0 && cornerRomp[idx-W-1] {
				anyRomp = true
			}
			// HiveWE-literal rule (terrain.ixx::real_tile_texture line 770):
			//   if (a_romp || (a_cliff && !corner_ramp[idx])) { remap }
			// In words: remap when this corner sits on a renderable
			// clifftrans piece (anyRomp), OR when any adjacent cell is a
			// cliff cell AND this corner is NOT itself flagged as a ramp.
			// Ramp corners are NEVER remapped on the basis of cliff
			// adjacency alone — they keep their raw ground_texture so the
			// ramp surface stays visually continuous with the floor.
			//
			// Why the previous "widened gate + narrow on (ramp AND no
			// cliff)" was wrong: it remapped ramp corners that ARE
			// cliff-adjacent (the typical plateau-edge ramp shoulder),
			// flipping them from raw Itbk to Irbk. That created bogus
			// 2-palette transition cells around the plateau edge with
			// alpha-edged variants the spec then sampled at slot 6/9
			// (diamond) instead of HiveWE's uniform Itbk. Symptom: visible
			// diamond/X artifacts on Enfo's FFB spear-tip plateau at
			// cells (11..13, 66..68) that HiveWE renders smooth. Verified
			// 2026-05-25 via the embedded HiveWE MCP bridge + a parser-side
			// real_tile_texture replay.
			remap := anyRomp || (anyCliff && !t.Tiles[idx].HasRamp())
			if !remap {
				continue
			}
			// Resolve the cliff palette index for this corner.
			// HiveWE comment: "Number 15 seems to be something" — when the
			// stored cliff_texture is 15, remap to (15-14) = 1. Mirrors the
			// `if (texture == 15) texture -= 14` in src/base/terrain.ixx.
			cti := t.Tiles[idx].CliffTexIdx()
			if cti == 15 {
				cti = 1
			}
			if int(cti) >= len(c2g) {
				continue
			}
			mapped := c2g[cti]
			if mapped < 0 {
				continue
			}
			ground[idx] = uint32(mapped)
		}
	}

	// Shadow map: optional. Empty + 0 dims when absent. The byte slice
	// rides as base64 (Go encoding/json's []byte default) — JS knows to
	// decode it before uploading as a GL texture.
	var shadowBytes []byte
	var shadowW, shadowH int
	if sm := forge.Current.ShadowMap(); sm != nil {
		shadowBytes = sm.Cells
		shadowW = sm.Width
		shadowH = sm.Height
	}
	dto := TerrainDTO{
		Width:         t.Width,
		Height:        t.Height,
		CenterOffset:  t.CenterOffset,
		Heights:       heights,
		GroundTex:     ground,
		Tileset:       string([]byte{t.Tileset}),
		Palette:         t.GroundTilesets,
		PaletteColors:   PaletteColors(t.GroundTilesets),
		PaletteTextures: PaletteTexturePaths(t.GroundTilesets),
		LayerHeight:     layer,
		CliffTex:        cliffTex,
		CliffVar:        cliffVar,
		GroundVar:       groundVar,
		RampFlags:       rampFlags,
		CliffPalette:         t.CliffTilesets,
		CliffPaletteTextures: CliffPaletteTexturePaths(t.CliffTilesets),
		WaterZ:        waterZ,
		HasWater:      hasWater,
		Water:         WaterColors(t.Tileset),
		ShadowMap:       shadowBytes,
		ShadowMapWidth:  shadowW,
		ShadowMapHeight: shadowH,
		CellSkip:          cellSkip,
		CliffDisplacement: cliffDisp,
	}
	dto.SkyModel, dto.SkyModelFromScript = ResolveSkyModel(t.Tileset)
	return dto, nil
}

// SelectionDTO mirrors forge.SelectionState for JS consumption.
type SelectionDTO struct {
	Items   []SelectionItemDTO `json:"items"`
	Primary int                `json:"primary"`
}

type SelectionItemDTO struct {
	Kind string `json:"kind"`
	ID   uint32 `json:"id"`
}

// SetSkyModel queues a sky-model change for the next Save. path is a
// normalized editor path ("environment/sky/…/….mdx") or "" to disable the
// sky. The renderer reflects the change immediately (Phase 2 picker preview);
// the actual script rewrite happens on Save.
//
// Returns the resolved path that the renderer will now use (the same value
// passed in, normalized). Empty if no map is loaded.
func (a *App) SetSkyModel(path string) (string, error) {
	p := path
	if err := forge.Current.SetSkyModel(&p); err != nil {
		return "", err
	}
	return p, nil
}

// ListSkyModels returns the WC3 stock sky-model options sourced from
// UI/WorldEditData.txt's [SkyModels] section. Used by the Map Info → Lighting
// tab's sky picker. Always includes a synthetic "None" entry at the top so
// the picker can express "no sky" as a first-class choice (writes
// SetSkyModel("") to the script, which suppresses the dome at runtime).
func (a *App) ListSkyModels() []SkyModelOption {
	stock := loadSkyModels()
	out := make([]SkyModelOption, 0, len(stock)+1)
	out = append(out, SkyModelOption{Path: "", NameKey: "", DisplayName: "None"})
	out = append(out, stock...)
	return out
}

func (a *App) GetSelection() SelectionDTO {
	s := forge.Current.Selection()
	items := make([]SelectionItemDTO, len(s.Items))
	for i, it := range s.Items {
		items[i] = SelectionItemDTO{Kind: it.Kind, ID: it.ID}
	}
	return SelectionDTO{Items: items, Primary: s.Primary}
}

// SetSelection replaces the selection. Empty items clears.
func (a *App) SetSelection(items []SelectionItemDTO) SelectionDTO {
	converted := make([]forge.SelectionItem, len(items))
	for i, it := range items {
		converted[i] = forge.SelectionItem{Kind: it.Kind, ID: it.ID}
	}
	forge.Current.SetSelection(converted, len(converted)-1)
	return a.GetSelection()
}

// SelectUnit is a convenience for the common case: select a single unit by
// creation_number. Replaces any existing selection.
func (a *App) SelectUnit(creationNumber uint32) SelectionDTO {
	forge.Current.SetSelection([]forge.SelectionItem{{Kind: "unit", ID: creationNumber}}, 0)
	return a.GetSelection()
}

func (a *App) ClearSelection() SelectionDTO {
	forge.Current.SetSelection(nil, -1)
	return a.GetSelection()
}

// DoodadDTO is the JS-facing doodad shape (trimmed for list view, like UnitDTO).
type DoodadDTO struct {
	CreationNumber uint32     `json:"creation_number"`
	TypeID         string     `json:"type_id"`
	SkinID         string     `json:"skin_id"`
	Position       [3]float32 `json:"position"`
	Rotation       float32    `json:"rotation"`
	Scale          [3]float32 `json:"scale"`
	Variation      uint32     `json:"variation"`
	Life           uint8      `json:"life"` // 0..100 for destructibles; 0xFF = N/A
	Flags          uint8      `json:"flags"`
}

func (a *App) ListDoodads() []DoodadDTO {
	dd := forge.Current.Doodads()
	if dd == nil {
		return []DoodadDTO{}
	}
	out := make([]DoodadDTO, 0, len(dd.Doodads))
	for _, d := range dd.Doodads {
		out = append(out, DoodadDTO{
			CreationNumber: d.CreationNumber,
			TypeID:         d.TypeID,
			SkinID:         d.SkinID,
			Position:       d.Position,
			Rotation:       d.Rotation,
			Scale:          d.Scale,
			Variation:      d.Variation,
			Life:           d.Life,
			Flags:          d.Flags,
		})
	}
	return out
}

// GetDoodad returns the full Doodad for one creation_number.
func (a *App) GetDoodad(creationNumber uint32) (*doodadsdoo.Doodad, error) {
	dd := forge.Current.Doodads()
	if dd == nil {
		return nil, fmt.Errorf("no map loaded")
	}
	for i := range dd.Doodads {
		if dd.Doodads[i].CreationNumber == creationNumber {
			return &dd.Doodads[i], nil
		}
	}
	return nil, fmt.Errorf("no doodad with creation_number %d", creationNumber)
}

// GetMapBytes returns the raw .w3x bytes for the current map, base64-encoded
// for safe JSON transport. JS callers decode then pass to War3MapViewer's
// loadMap. Returns "" if the current map was opened from a folder (no
// archive bytes available) or no map is loaded.
//
// (Wails serializes []byte as base64 strings; the JS side does atob+Uint8Array
// to recover the buffer. That's intentional — base64 over the JS bridge
// performs fine at single-megabyte map sizes and avoids exposing raw
// bytes through the URL space.)
func (a *App) GetMapBytes() string {
	b := forge.Current.RawMapBytes()
	if len(b) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// MinimapDTO carries the bytes of the loaded map's baked minimap image plus
// a format hint so the JS-side decoder can dispatch correctly.
//
//   - Bytes    — base64-encoded raw image bytes (BLP1/BLP2/DDS/TGA)
//   - Ext      — "blp" | "dds" | "tga" — matches the on-disk extension of the
//                source file that was found. JS uses magic-byte sniffing for
//                BLP/DDS dispatch (mirroring icon-loader.ts), so this is
//                primarily a TGA-vs-image-formats discriminator.
//   - Found    — true when a baked minimap image was located in the map. False
//                + empty Bytes means the map ships no preview; UI should show
//                a graceful "no minimap" placeholder.
//
// (Wails serializes []byte as base64; JS does atob+Uint8Array to recover.)
type MinimapDTO struct {
	Bytes string `json:"bytes"` // base64
	Ext   string `json:"ext"`
	Found bool   `json:"found"`
}

// GetMinimapBytes returns the baked minimap image embedded in the loaded map.
// WC3 maps ship an author-drawn preview rather than a render of terrain —
// HiveWE displays it verbatim and so do we (see project_wc3_forge memory).
//
// Source-file precedence — pick the first present:
//  1. war3mapMap.blp     — Reforged-era; most common. BLP1 or BLP2.
//  2. war3mapMap.dds     — HD Reforged variant; optional, larger.
//  3. war3mapPreview.tga — legacy maps. TGA format; decoded JS-side.
//
// Returns (Found=false, "", "", nil) when no map is loaded OR the loaded map
// has none of the three files (e.g. some test fixtures). Read errors propagate
// — they're recoverable on the UI side (show placeholder + log).
func (a *App) GetMinimapBytes() (MinimapDTO, error) {
	candidates := []struct {
		name string
		ext  string
	}{
		{"war3mapMap.blp", "blp"},
		{"war3mapMap.dds", "dds"},
		{"war3mapPreview.tga", "tga"},
	}
	for _, c := range candidates {
		b, ok, err := forge.Current.ReadFile(c.name)
		if err != nil {
			return MinimapDTO{}, fmt.Errorf("read %s: %w", c.name, err)
		}
		if !ok || len(b) == 0 {
			continue
		}
		return MinimapDTO{
			Bytes: base64.StdEncoding.EncodeToString(b),
			Ext:   c.ext,
			Found: true,
		}, nil
	}
	return MinimapDTO{Found: false}, nil
}

// SetUnitAnimation is a dev-only hook for poking unit animations from the JS
// devtools console (or any MCP client). Forwards (creationNumber, animName)
// to the frontend via Wails event; the scene-instances renderer matches the
// name against the unit's MDX sequences using the same rarity-weighted picker
// used for idle stand reroll. animName of "" or "stand" returns the unit to
// the idle reroll loop. Not exposed in the UI.
func (a *App) SetUnitAnimation(creationNumber uint32, animName string) {
	runtime.EventsEmit(a.ctx, eventDevSetAnim, map[string]any{
		"creation_number": creationNumber,
		"anim_name":       animName,
	})
}

// EmitTestCommand sends a free-form test command to the JS layer via a Wails
// event. Used by verification automation to drive UI state changes that
// would otherwise require synthetic input — which WebView2 silently drops in
// some cases (see project memory). The JS side subscribes to the event and
// dispatches per command. No-op if no subscriber is wired (event just goes
// nowhere; harmless).
//
// Commands are free-form strings parsed JS-side; conventional shape:
//   "terrain.toggle"               toggle terrain-pick mode
//   "terrain.set <col> <row>"      set terrain cell (auto-fills info)
//   "doodad.toggle <cat>"          toggle doodad category visibility
//   "section.toggle <id>"          toggle accordion section open state
//   "splitter.set <pct>"           set right-column explorer pct
func (a *App) EmitTestCommand(cmd string) {
	runtime.EventsEmit(a.ctx, "wc3-forge:test-command", map[string]any{
		"cmd": cmd,
	})
}


// PathingMapDTO is the JS-facing pathing-map shape. Cells carry one byte
// per quarter-cell (4× terrain tile resolution); each byte's bits flag
// movement constraints (unwalkable / unflyable / unbuildable / …).
//
// Cells is []uint32 (not []uint8) because Go's encoding/json base64-encodes
// []byte fields. The same trap caught us with TerrainDTO.GroundTex — see
// project memory `project-wc3-forge`. Wire cost at typical sizes (~256²) is
// 256 KB which is fine for a JSON array; the JS side packs it back into a
// Uint8Array before uploading to a GL texture.
type PathingMapDTO struct {
	Width  int      `json:"width"`
	Height int      `json:"height"`
	Cells  []uint32 `json:"cells"`
}

// GetPathingMap returns the parsed war3map.wpm for the loaded map, or an
// empty DTO (width=height=0, nil cells) when the map didn't ship a .wpm
// — the renderer treats that as "no overlay to draw".
func (a *App) GetPathingMap() PathingMapDTO {
	p := forge.Current.PathingMap()
	if p == nil {
		return PathingMapDTO{}
	}
	cells := make([]uint32, len(p.Cells))
	for i, b := range p.Cells {
		cells[i] = uint32(b)
	}
	return PathingMapDTO{Width: p.Width, Height: p.Height, Cells: cells}
}

// GetUnit returns the full Entity for one creation_number. Properties panel
// uses this to render the heavier fields (inventory, ability mods, hero
// stats, item drops) that aren't included in the lighter ListUnits payload.
func (a *App) GetUnit(creationNumber uint32) (*unitsdoo.Entity, error) {
	units := forge.Current.Units()
	if units == nil {
		return nil, fmt.Errorf("no map loaded")
	}
	for i := range units.Entities {
		if units.Entities[i].CreationNumber == creationNumber {
			return &units.Entities[i], nil
		}
	}
	return nil, fmt.Errorf("no entity with creation_number %d", creationNumber)
}

// MoveUnit relocates the unit with the given creation_number to the supplied
// game coordinates. The Properties panel calls this on X/Y/Z edit-commit.
//
// Wire contract: incoming x/y/z are GAME coords (centered at 0,0); the
// unitsdoo parser stores Position verbatim so we pass them straight through.
// No conversion happens at this boundary.
func (a *App) MoveUnit(creationNumber uint32, x, y, z float32) error {
	return forge.Current.MoveUnit(creationNumber, x, y, z)
}

// MoveDoodad relocates the doodad with the given creation_number to the
// supplied game coordinates. Mirrors MoveUnit's wire contract and shape;
// the kind disambiguation lives in the Properties panel (which branches on
// the primary selection's kind before calling Move{Unit,Doodad}). The
// existing dirty-changed + entity-changed startup subscriptions fire for
// either kind, so no additional Wails wiring is needed here.
func (a *App) MoveDoodad(creationNumber uint32, x, y, z float32) error {
	return forge.Current.MoveDoodad(creationNumber, x, y, z)
}

// CreateDoodad appends a new placed doodad of typeID at game coords (x,y,z)
// and returns its creation_number. Used by the "Import 3D Model" dialog's
// "Done" action to drop the freshly-imported model onto the map at the center
// of the current view (doodads avoid the unitsdoo save round-trip hazard that
// placing a unit would hit). Thin pass-through to the session mutator, which
// records undo + fires dirty/entity-changed.
func (a *App) CreateDoodad(typeID string, x, y, z, rotation, scale float32, variation uint32) (uint32, error) {
	return forge.Current.CreateDoodad(typeID, [3]float32{x, y, z}, rotation, scale, variation)
}

// RotateUnit sets the facing angle (radians, Z-axis only) of the unit with the
// given creation_number. The gizmo's rotate-handle commits via this method.
// Mirrors MoveUnit's error-handling shape: no-op on same value, dirty-flip on
// first edit, entity-changed event fires after unlock.
func (a *App) RotateUnit(creationNumber uint32, rotation float32) error {
	return forge.Current.RotateUnit(creationNumber, rotation)
}

// RotateDoodad sets the facing angle (radians, Z-axis only) of the doodad with
// the given creation_number. Mirrors MoveDoodad's wire contract and shape.
// See session.RotateDoodad for the fixed_rot clamping contract (Phase C).
func (a *App) RotateDoodad(creationNumber uint32, rotation float32) error {
	return forge.Current.RotateDoodad(creationNumber, rotation)
}

// ScaleUnit sets the per-axis scale of the unit with the given creation_number.
// sx/sy/sz are runtime-normalized values (1.0 = default size, matching the
// BlzSetUnitScale convention). The session mutator clears the on-disk scaleRaw
// preservation field so Encode emits the new value instead of the stale bits.
func (a *App) ScaleUnit(creationNumber uint32, sx, sy, sz float32) error {
	return forge.Current.ScaleUnit(creationNumber, sx, sy, sz)
}

// ScaleDoodad sets the per-axis scale of the doodad with the given
// creation_number. Doodad scale is stored raw on disk (no /128 divide at
// Parse), so the values passed here are written verbatim by Encode.
func (a *App) ScaleDoodad(creationNumber uint32, sx, sy, sz float32) error {
	return forge.Current.ScaleDoodad(creationNumber, sx, sy, sz)
}

// Undo reverts the most-recent mutation on the session's history stack.
// No-op when the stack is empty. The reverted command is pushed onto the
// redo stack so the user can re-apply it. Bound to Ctrl+Z in the UI.
func (a *App) Undo() error {
	return forge.Current.Undo()
}

// Redo re-applies the most-recently-undone mutation. No-op when the redo
// stack is empty. Any new mutation (mouse drag, MCP call, Properties edit)
// wipes the redo stack — standard editor behavior. Bound to Ctrl+Y in the UI.
func (a *App) Redo() error {
	return forge.Current.Redo()
}

// HistoryList returns the current undo/redo stacks (oldest-first) so the
// Edit menu can show enabled state + tooltips ("Undo Move Footman" etc.).
func (a *App) HistoryList() forge.HistoryState {
	return forge.Current.HistoryList()
}

// HistoryUndoCount returns the current undo-stack depth. Modal dialogs
// (Map Info Sky picker) snapshot this at open-time so Cancel can UndoTo it.
func (a *App) HistoryUndoCount() int {
	return forge.Current.HistoryUndoCount()
}

// UndoTo undoes commands until the stack depth is at most `target`. Used by
// the Map Info dialog's Cancel button to revert any sky picks made during
// the dialog session in one round-trip.
func (a *App) UndoTo(target int) error {
	return forge.Current.UndoTo(target)
}

// DiscardTo reverts commands above `target` AND drops them from history
// entirely (no redo trail). Map Info Cancel uses this so a discarded picker
// session leaves no breadcrumb in the undo/redo UI.
func (a *App) DiscardTo(target int) error {
	return forge.Current.DiscardTo(target)
}

// CanUndo / CanRedo report whether the corresponding action is currently
// available. For menu enable/disable state.
func (a *App) CanUndo() bool { return forge.Current.CanUndo() }
func (a *App) CanRedo() bool { return forge.Current.CanRedo() }

// BeginUndoGroup starts a transaction. Subsequent mutations until
// EndUndoGroup land in a single undo step. Used by the gizmo's drag-end
// commit loop so a single drag = one Ctrl+Z. Nestable — the outermost
// begin/end pair is the one that publishes to history.
func (a *App) BeginUndoGroup(label string) {
	forge.Current.BeginUndoGroup(label)
}

// EndUndoGroup closes the outermost group (or decrements nesting).
// Returns an error if called without a matching BeginUndoGroup.
func (a *App) EndUndoGroup() error {
	return forge.Current.EndUndoGroup()
}

// IsDirty reports whether the session has unsaved edits. The header Save
// button's modified-dot indicator polls this on mount; subsequent updates
// arrive via the dirty-changed Wails event.
func (a *App) IsDirty() bool {
	return forge.Current.IsDirty()
}

// onBeforeClose is the Wails window-close gate. Returns false to allow the
// close, true to abort it. When dirty we abort + emit a JS event so
// App.svelte can put up the "unsaved changes" modal; the user's choice
// then drives either SaveMap+ForceQuit or just ForceQuit.
//
// First-call vs re-entry: when the user picks Discard/Save in the modal,
// the JS side calls ForceQuit, which calls runtime.Quit. Wails routes
// that through OnBeforeClose AGAIN — we detect this re-entry by checking
// the forceQuitting flag (set just before Quit fires) and let the close
// through unchallenged.
func (a *App) onBeforeClose(ctx context.Context) bool {
	if a.forceQuitting {
		return false
	}
	if !forge.Current.IsDirty() {
		return false
	}
	runtime.EventsEmit(ctx, eventCloseRequested)
	return true
}

// ForceQuit closes the window without re-checking dirty state. Called by
// the "unsaved changes" modal after the user picks Save & Quit or
// Discard & Quit. Sets forceQuitting first so the re-entry into
// onBeforeClose lets the close proceed.
func (a *App) ForceQuit() {
	a.forceQuitting = true
	runtime.Quit(a.ctx)
}

// SaveMap flushes all pending edits back through the source's write path.
// MPQ-backed maps save in place via the pure-Go writer; on the rare archive
// that can't be repacked, Save returns ErrMPQRepackFailed (wrapped) and the UI
// surfaces a friendly toast suggesting the folder-extraction workaround.
func (a *App) SaveMap() error {
	return forge.Current.Save()
}

// LaunchInWC3 spawns Warcraft III with the currently-loaded map preloaded
// via the `-loadfile` CLI flag. Mirrors HiveWE's "Test Map" ribbon button.
// Returns immediately after spawning — WC3 runs as a detached peer process.
//
// v1 limitations (documented in detail in internal/wc3launch/launch.go):
//   - We do NOT save the session before launching. The JS caller MUST disable
//     the Test Map button when forge.Current.IsDirty() to avoid running a
//     stale .w3x. (Folder-backed maps could save first, but WC3 only runs
//     packaged .w3x files — wc3-forge has no MPQ writer yet, so the folder
//     contents can't be re-bundled.)
//   - Only file-backed sessions can be tested. Folder-backed sessions return
//     wc3launch.ErrMapNotFound (path is a folder). The JS caller can detect
//     this from the Status().path heuristic and disable the button.
//
// Returns:
//   - wc3launch.ErrBinaryNotFound when Warcraft III.exe isn't at the expected
//     install path (env WC3FORGE_WC3_PATH overrides; else
//     `C:\Program Files (x86)\Warcraft III\_retail_\x86_64\Warcraft III.exe`).
//   - wc3launch.ErrMapNotFound when the session path doesn't resolve to a
//     real .w3x file.
//   - a fmt.Errorf for "no map loaded" / exec failures.
func (a *App) LaunchInWC3() error {
	if !forge.Current.IsLoaded() {
		return fmt.Errorf("no map loaded")
	}
	mapPath := forge.Current.Path()
	if mapPath == "" {
		return fmt.Errorf("no map path available")
	}
	return wc3launch.LaunchWithMap(mapPath)
}

// MapInfoGet returns the full parsed war3map.w3i for the loaded map. The
// Map Info Editor dialog calls this on open to pre-populate every field;
// it's heavier than Status() (which intentionally exposes only the
// name/path/unit_count needed for the header chrome) and is shaped to be
// fed directly into form inputs.
//
// Returns an error when no map is loaded — the dialog gates open on
// IsLoaded() so this is a programmer-error path, not a normal flow.
func (a *App) MapInfoGet() (*w3i.Info, error) {
	info := forge.Current.Info()
	if info == nil {
		return nil, fmt.Errorf("no map loaded")
	}
	return info, nil
}

// BridgeInfo is the JS-facing identity card for the running MCP bridge. The
// Agent Console panel (BridgeConsole.svelte) renders this in its top bar so
// the user can see at a glance WHICH wc3-forge window they're looking at
// when multiple instances are running side-by-side.
//
// Token is the SHORTENED token (first 8 hex chars) — we intentionally never
// surface the full secret in UI that might be screenshotted or screen-shared.
type BridgeInfo struct {
	PID        int    `json:"pid"`
	Port       int    `json:"port"`
	TokenShort string `json:"token_short"`
	MapName    string `json:"map_name"`
	MapPath    string `json:"map_path"`
}

// GetBridgeInfo returns the running bridge's identity card (pid, port,
// shortened auth token, currently-loaded map). Reads live from
// forge.BridgeIdentity (captured in RegisterAll) so port/token reflect the
// post-Start values, not zeros from before listen.
func (a *App) GetBridgeInfo() BridgeInfo {
	pid, port, tokenShort := forge.BridgeIdentity()
	out := BridgeInfo{
		PID:        pid,
		Port:       port,
		TokenShort: tokenShort,
	}
	if forge.Current.IsLoaded() {
		out.MapPath = forge.Current.Path()
		if info := forge.Current.Info(); info != nil {
			out.MapName = info.Name
		}
	}
	return out
}

// MapInfoApply applies a partial update DTO to the in-memory war3map.w3i.
// The Map Info Editor dialog calls this on commit; it walks `updates` and
// applies each known key to the Info struct via Session.MutateInfo so
// dirty-tracking + entity-changed events fire normally.
//
// Subset coverage for v1: the dialog's Description/General tab fields.
// See forge.ApplyInfoUpdates for the wire-key contract + how to extend.
// Unknown keys are silently ignored; type-mismatches are silently skipped.
//
// Returns the MutateInfo error (e.g. no map loaded). Successful application
// of a 0-length updates map is a no-op (returns nil without flipping dirty).
func (a *App) MapInfoApply(updates map[string]any) error {
	if len(updates) == 0 {
		return nil
	}
	return forge.Current.MutateInfo(func(info *w3i.Info) {
		forge.ApplyInfoUpdates(info, updates)
	})
}

// --- Object Editor (units) ---
//
// Phase 1a: read-only. The JS Object Editor calls these to populate its
// tree and field table. They delegate to forge.MergedUnits / the underlying
// metadata so the Wails surface and the MCP bridge stay in lockstep — both
// see the same merged base+shadow view.

// UnitObjectListEntity mirrors handlers_objects.go's objectsUnitsListEntity.
// Re-declared here because the bridge type is package-private; we don't
// want a cross-package Wails-binding dependency on internal/forge's
// JSON-shaped DTOs.
type UnitObjectListEntity struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Race      string `json:"race"`
	RaceLabel string `json:"race_label"`
	Kind      string `json:"kind"`
	Category  string `json:"category"`
	IsCustom  bool   `json:"is_custom"`
	IsEdited  bool   `json:"is_edited"`
	BaseID    string `json:"base_id,omitempty"`
	Campaign  bool   `json:"campaign"`
	IconArt   string `json:"icon_art"`
}

// ListUnitObjects returns the merged-units tree for the Object Editor.
// Empty slice (not error) when no map is loaded or CASC isn't reachable —
// the UI shows an empty-state instead of an error toast.
func (a *App) ListUnitObjects() []UnitObjectListEntity {
	out, err := forge.ListUnitObjects()
	if err != nil {
		return []UnitObjectListEntity{}
	}
	dtos := make([]UnitObjectListEntity, 0, len(out))
	for _, r := range out {
		dtos = append(dtos, UnitObjectListEntity{
			ID:        r.ID,
			Name:      r.Name,
			Race:      r.Race,
			RaceLabel: r.RaceLabel,
			Kind:      r.Kind,
			Category:  r.Category,
			IsCustom:  r.IsCustom,
			IsEdited:  r.IsEdited,
			BaseID:    r.BaseID,
			Campaign:  r.Campaign,
			IconArt:   r.IconArt,
		})
	}
	return dtos
}

// UnitObjectField mirrors handlers_objects.go's objectsUnitsField.
type UnitObjectField struct {
	ID          string `json:"id"`
	Field       string `json:"field"`
	DisplayName string `json:"display_name"`
	Category    string `json:"category"`
	Type        string `json:"type"`
	Value       string `json:"value"`
	Display     string `json:"display"`
	// DisplayRaw is the resolved value with WC3 color codes preserved
	// (|cAARRGGBB ... |r). Frontend renders this through wc3color.ts for
	// inline-colored display while keeping Display as the stripped fallback.
	DisplayRaw string `json:"display_raw"`
	Overridden bool   `json:"overridden"`
	// Levels carries per-level override values for leveled (opt-format) kinds:
	// level (1-based) → raw value. Empty/omitted for non-leveled fields. Must
	// mirror objectsUnitsField so the struct conversion in toUnitObjectDetail
	// stays valid.
	Levels map[uint32]string `json:"levels,omitempty"`
}

// UnitObjectDetail mirrors handlers_objects.go's objectsUnitsGetResult.
type UnitObjectDetail struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	BaseID         string            `json:"base_id,omitempty"`
	IsCustom       bool              `json:"is_custom"`
	IsEdited       bool              `json:"is_edited"`
	Race           string            `json:"race"`
	Kind           string            `json:"kind"`
	IconArt        string            `json:"icon_art"`
	ModelPath      string            `json:"model_path"`
	ModelFallbacks []string          `json:"model_fallbacks"`
	Fields         []UnitObjectField `json:"fields"`
}

// GetUnitObject returns the full field table for one unit. Returns nil
// (not error) when the id is missing or no map loaded — the UI shows an
// inline empty state in that case.
func (a *App) GetUnitObject(id string) *UnitObjectDetail {
	if id == "" {
		return nil
	}
	d, err := forge.GetUnitObject(id)
	if err != nil || d == nil {
		return nil
	}
	return toUnitObjectDetail(d)
}

// toUnitObjectDetail converts the forge-package detail shape to the
// Wails-bound shape. Re-declared in app.go because the bridge type lives
// inside internal/forge — Wails bindings can't depend on that package's
// private types, so we round-trip through the public alias.
func toUnitObjectDetail(d *forge.UnitObjectDetail) *UnitObjectDetail {
	if d == nil {
		return nil
	}
	fields := make([]UnitObjectField, 0, len(d.Fields))
	for _, f := range d.Fields {
		fields = append(fields, UnitObjectField(f))
	}
	return &UnitObjectDetail{
		ID: d.ID, Name: d.Name, BaseID: d.BaseID, IsCustom: d.IsCustom,
		IsEdited: d.IsEdited, Race: d.Race, Kind: d.Kind, IconArt: d.IconArt,
		ModelPath: d.ModelPath, ModelFallbacks: d.ModelFallbacks,
		Fields: fields,
	}
}

// SetUnitObjectField writes a single field override. column may be FourCC
// (e.g. "unam") or column-name (e.g. "name") — the mutator normalizes via
// UnitMetaData. Returns the post-mutation detail so the JS side can
// re-render the Overridden flag in-place. Returns nil + error when the
// edit fails (no map / unknown id / unknown field).
func (a *App) SetUnitObjectField(id, column, value string) (*UnitObjectDetail, error) {
	d, err := forge.SetUnitObjectField(id, column, value)
	if err != nil {
		return nil, err
	}
	return toUnitObjectDetail(d), nil
}

// CreateCustomUnit appends a new custom unit row derived from baseID and
// returns its full detail. id is optional — when empty the allocator picks
// the next free FourCC starting from baseID's first character.
//
// Returns the chosen id PLUS the detail in a flat object so the JS side
// gets both in one round-trip without an extra Get call.
type CreateCustomUnitResult struct {
	ID     string            `json:"id"`
	Detail *UnitObjectDetail `json:"detail"`
}

func (a *App) CreateCustomUnit(baseID, id string) (*CreateCustomUnitResult, error) {
	chosenID, d, err := forge.CreateCustomUnitObject(baseID, id)
	if err != nil {
		return nil, err
	}
	return &CreateCustomUnitResult{ID: chosenID, Detail: toUnitObjectDetail(d)}, nil
}

// DeleteCustomUnit removes a custom unit by id. Errors if id isn't a
// custom row (stock units aren't deletable).
func (a *App) DeleteCustomUnit(id string) error {
	return forge.DeleteCustomUnitObject(id)
}

// ---------------------------------------------------------------------------
// Generic Object Editor surface — Phase 2b. The kind-typed wrappers above
// (ListUnitObjects, GetUnitObject, ...) stay as backwards-compat aliases.
// New JS code should target the generic methods below, which accept a kind
// string ("units"/"items"/"abilities"/"buffs"/"destructables"/"doodads"/
// "upgrades") and dispatch through the registered KindConfig.
//
// Wails surface choice was option B (six generic methods) over option A
// (42 hand-typed per-kind methods). Wire shapes are identical across kinds
// — the same UnitObjectListEntity / UnitObjectDetail structures serve every
// editor; only the kind tag changes. The generic path also lets future
// kinds light up without touching app.go.
// ---------------------------------------------------------------------------

// ListObjects returns the merged tree for the given kind. Empty slice (not
// error) when no map is loaded or CASC isn't reachable for that kind's
// MetaData — the UI shows an empty-state instead of an error toast.
func (a *App) ListObjects(kind string) []UnitObjectListEntity {
	out, err := forge.ListObjects(kind)
	if err != nil {
		return []UnitObjectListEntity{}
	}
	dtos := make([]UnitObjectListEntity, 0, len(out))
	for _, r := range out {
		dtos = append(dtos, UnitObjectListEntity{
			ID:        r.ID,
			Name:      r.Name,
			Race:      r.Race,
			RaceLabel: r.RaceLabel,
			Kind:      r.Kind,
			Category:  r.Category,
			IsCustom:  r.IsCustom,
			IsEdited:  r.IsEdited,
			BaseID:    r.BaseID,
			Campaign:  r.Campaign,
			IconArt:   r.IconArt,
		})
	}
	return dtos
}

// GetObject returns the full field table for one object of the given kind.
// Returns nil (not error) when the id is missing or no map loaded — the UI
// shows an inline empty state in that case.
func (a *App) GetObject(kind, id string) *UnitObjectDetail {
	if id == "" {
		return nil
	}
	d, err := forge.GetObject(kind, id)
	if err != nil || d == nil {
		return nil
	}
	return toUnitObjectDetail(d)
}

// SetObjectField writes a single field override on an object of the given
// kind. column may be FourCC (e.g. "unam") or column-name (e.g. "name") —
// the mutator normalizes via the kind's MetaData. Returns the post-mutation
// detail so the JS side can re-render the Overridden flag in-place.
func (a *App) SetObjectField(kind, id, column, value string) (*UnitObjectDetail, error) {
	d, err := forge.SetObjectField(kind, id, column, value)
	if err != nil {
		return nil, err
	}
	return toUnitObjectDetail(d), nil
}

// CreateCustomObjectResult mirrors CreateCustomUnitResult for the generic
// surface — chosen ID + full detail in one round-trip.
type CreateCustomObjectResult struct {
	ID     string            `json:"id"`
	Detail *UnitObjectDetail `json:"detail"`
}

// CreateCustomObject appends a new custom row of the given kind derived from
// baseID and returns its full detail. id is optional — when empty the
// allocator picks the next free FourCC starting from baseID's first
// character.
func (a *App) CreateCustomObject(kind, baseID, id string) (*CreateCustomObjectResult, error) {
	chosenID, d, err := forge.CreateCustomObject(kind, baseID, id)
	if err != nil {
		return nil, err
	}
	return &CreateCustomObjectResult{ID: chosenID, Detail: toUnitObjectDetail(d)}, nil
}

// DeleteCustomObject removes a custom row of the given kind by id. Errors
// if id isn't a custom row (stock rows aren't deletable).
func (a *App) DeleteCustomObject(kind, id string) error {
	return forge.DeleteCustomObject(kind, id)
}

// ConvertObjectResult mirrors CreateCustomObjectResult's shape — the new id
// in the destination kind + its full detail payload, so the caller can switch
// tabs + select without an extra round-trip.
type ConvertObjectResult struct {
	ID     string            `json:"id"`
	Detail *UnitObjectDetail `json:"detail"`
}

// ConvertObject converts an object from srcKind to dstKind. Currently only
// the doodads↔destructables pair is accepted (other pairs return a clear
// error). Stock-source rows are NOT deleted (the converted result is a new
// custom in dst); custom-source rows ARE deleted so the user ends up with
// one replacement, not two near-duplicates. Recorded as one undo step.
func (a *App) ConvertObject(srcKind, srcID, dstKind string) (*ConvertObjectResult, error) {
	id, err := forge.ConvertObject(srcKind, srcID, dstKind)
	if err != nil {
		return nil, err
	}
	d, derr := forge.GetObject(dstKind, id)
	if derr != nil {
		// The conversion succeeded but the follow-up Get failed. Return the id
		// without a detail rather than rolling back; the JS layer can fetch
		// again or just refresh the tree.
		return &ConvertObjectResult{ID: id}, nil
	}
	return &ConvertObjectResult{ID: id, Detail: toUnitObjectDetail(d)}, nil
}

// CommandButtonEntry is one icon listed by ListCommandButtons. Path is
// lowercase + forward-slash, ready to pass to /asset/<path>; Name is the
// basename without extension (the picker uses Name to filter by typed text).
type CommandButtonEntry struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// commandButtonsCache holds the per-process enumeration of CASC's command-
// button icons. Built once on first request via commandButtonsOnce; CASC
// contents don't change during a session so caching is safe.
var (
	commandButtonsOnce  sync.Once
	commandButtonsList  []CommandButtonEntry
	commandButtonsErr   error
)

// ListCommandButtons enumerates every BLP/DDS icon under CASC's
// replaceabletextures/commandbuttons/ prefix (plus the disabled-state
// commandbuttonsdisabled/ for completeness — the picker filters/groups in
// the UI). Lazy-built + cached for the session.
//
// Returns an empty slice + nil error when CASC isn't reachable (the editor
// degrades gracefully — the picker shows a single "no icons available" row).
func (a *App) ListCommandButtons() []CommandButtonEntry {
	commandButtonsOnce.Do(func() {
		c, err := getCASC()
		if err != nil || c == nil {
			commandButtonsErr = err
			return
		}
		var all []string
		for _, prefix := range []string{
			"replaceabletextures/commandbuttons/",
			"replaceabletextures/commandbuttonsdisabled/",
		} {
			entries, err := c.ListByPrefix(prefix)
			if err != nil {
				commandButtonsErr = err
				return
			}
			all = append(all, entries...)
		}
		// Dedupe + filter to image extensions. Some CASC entries can list both
		// .blp and .dds variants of the same icon; we keep one entry per stem,
		// preferring .blp (the broader-compat variant — the asset handler
		// transparently swaps .blp ↔ .dds on miss).
		seen := map[string]int{}
		out := make([]CommandButtonEntry, 0, len(all))
		for _, p := range all {
			lp := strings.ToLower(p)
			ext := ""
			if i := strings.LastIndexByte(lp, '.'); i > 0 {
				ext = lp[i:]
			}
			if ext != ".blp" && ext != ".dds" {
				continue
			}
			stem := lp[:len(lp)-len(ext)]
			// Prefer .blp over .dds on collision.
			if idx, ok := seen[stem]; ok {
				if ext == ".blp" && strings.HasSuffix(out[idx].Path, ".dds") {
					out[idx].Path = lp
				}
				continue
			}
			base := stem
			if i := strings.LastIndexByte(base, '/'); i >= 0 {
				base = base[i+1:]
			}
			out = append(out, CommandButtonEntry{Path: lp, Name: base})
			seen[stem] = len(out) - 1
		}
		// Sort by name for deterministic UI ordering (the picker grid renders
		// in this order; no point shuffling per call).
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		commandButtonsList = out
	})
	if commandButtonsErr != nil {
		return []CommandButtonEntry{}
	}
	return commandButtonsList
}

// GameplayConstantRow is the JSON-friendly shape for one row in the
// Gameplay Constants Editor. Carries the section the row belongs to (only
// [Misc] in practice today) and the raw value as stored in war3mapMisc.txt
// — for list-valued constants like DamageBonusNormal, that's the
// comma-separated value verbatim ("1.00,2.00,1.00,...").
type GameplayConstantRow struct {
	Section string `json:"section"`
	Key     string `json:"key"`
	Value   string `json:"value"`
}

// GameplayConstantsGet returns the per-map gameplay-constants overrides
// currently in memory (parsed from war3mapMisc.txt at Open, plus any
// pending edits). Returns an empty slice (not an error) for maps that
// don't ship any overrides — the editor lets the user add new ones from
// there.
//
// Returns an error only when no map is loaded; the dialog gates open on
// IsLoaded() so this is a programmer-error path, not a normal flow.
func (a *App) GameplayConstantsGet() ([]GameplayConstantRow, error) {
	if !forge.Current.IsLoaded() {
		return nil, fmt.Errorf("no map loaded")
	}
	g := forge.Current.Gameplay()
	if g == nil {
		return []GameplayConstantRow{}, nil
	}
	out := make([]GameplayConstantRow, 0, 32)
	for _, s := range g.Sections {
		if s == nil {
			continue
		}
		for _, e := range s.Entries {
			if e == nil || e.Key == "" {
				continue
			}
			out = append(out, GameplayConstantRow{
				Section: s.Name,
				Key:     e.Key,
				Value:   e.Value,
			})
		}
	}
	return out, nil
}

// GameplayConstantsApply replaces the in-memory war3mapMisc.txt with a new
// row set. The dialog's commit path sends the FULL post-edit row list (not
// a diff) — the file is small and a snapshot replace is simpler than
// diffing, while still letting the user freely add/edit/delete rows
// without any binding-side state.
//
// Rows are written in the order received, so the editor's row order is
// what ends up on disk. An empty `rows` slice clears all overrides
// (leaving an empty [Misc] section on save).
//
// Returns the MutateGameplay error (e.g. no map loaded).
func (a *App) GameplayConstantsApply(rows []GameplayConstantRow) error {
	return forge.Current.MutateGameplay(func(f *miscdata.File) {
		// Replace contents wholesale. Group rows by section, preserving
		// first-seen order so the UI's order survives the round-trip.
		f.Sections = f.Sections[:0]
		sectionsByName := map[string]*miscdata.Section{}
		var order []string
		for _, r := range rows {
			section := strings.TrimSpace(r.Section)
			if section == "" {
				section = "Misc"
			}
			key := strings.TrimSpace(r.Key)
			if key == "" {
				continue
			}
			s, ok := sectionsByName[section]
			if !ok {
				s = &miscdata.Section{Name: section}
				sectionsByName[section] = s
				order = append(order, section)
			}
			s.Entries = append(s.Entries, &miscdata.Entry{
				Key:   key,
				Value: r.Value,
			})
		}
		for _, name := range order {
			f.Sections = append(f.Sections, sectionsByName[name])
		}
		// Always guarantee a [Misc] section exists so a re-Get from the
		// editor shows the canonical structure even when the user cleared
		// every row.
		if _, ok := sectionsByName["Misc"]; !ok {
			f.Sections = append(f.Sections, &miscdata.Section{Name: "Misc"})
		}
	})
}
