<script lang="ts">
  import { run as run_1 } from 'svelte/legacy';

  import { onMount, onDestroy, tick } from 'svelte'
  import {
    OpenMapDialog, OpenMapFileDialog, OpenMap, OpenMapInNewWindow, NewWindow, CloseMap, ListUnits, ListDoodads, Status,
    GetSelection, SetSelection, GetUnit,
    GetReforgedMode, SetReforgedMode,
    GetUnitTypeIndex, GetDoodadTypeIndex,
    NewMapDialog as NewMapSaveDialog, CreateNewMap, WriteNewMap, ReportDiagnostics,
    MoveUnit, MoveDoodad, CreateDoodad, CreateUnit, DeleteSelection, IsDirty, SaveMap, SaveMapForce,
    SetUnitInstanceField, SetUnitInstanceItemDrops,
    ListStartLocations, CreateStartLocation, MoveStartLocation, DeleteStartLocation,
    ListRegions, CreateRegion,
    MapInfoGet,
    PaintTerrainTile, BrushTerrainHeight, BrushTerrainCliff, BrushTerrainRamp, BrushTerrainWater, WaterAddHeightAt, GetTerrainTile,
    BeginUndoGroup, EndUndoGroup,
    LaunchInWC3,
    Undo, Redo, CanUndo, CanRedo, HistoryList,
    CheckConvertToLua,
    ForceQuit,
    WC3InstallStatus,
    GetAppVersion, CheckForUpdate, DownloadAndRunInstaller,
  } from '../wailsjs/go/main/App.js'
  import { EventsOn, EventsOff } from '../wailsjs/runtime/runtime.js'
  import type { main, unitsdoo, update } from '../wailsjs/go/models'
  import {
    createScene,
    type SceneAPI, type PickHit, type SelectMode, type TerrainCellInfo,
  } from './scene-instances'
  import type { RegionRect } from './region-overlay'
  import Splitter from './Splitter.svelte'
  import Accordion from './Accordion.svelte'
  import ViewMenu from './ViewMenu.svelte'
  import AssetPreview from './AssetPreview.svelte'
  import MapInfoEditor from './MapInfoEditor.svelte'
  import NewMapDialog from './NewMapDialog.svelte'
  import ObjectEditor from './ObjectEditor.svelte'
  import TriggerEditor from './TriggerEditor.svelte'
  import SwapTilesetDialog from './SwapTilesetDialog.svelte'
  import ConvertToLuaDialog from './ConvertToLuaDialog.svelte'
  import ImportModelDialog from './ImportModelDialog.svelte'
  import ImportManager from './ImportManager.svelte'
  import WC3InstallDialog from './WC3InstallDialog.svelte'
  import GameplayConstantsEditor from './GameplayConstantsEditor.svelte'
  import BridgeConsole from './BridgeConsole.svelte'
  import Minimap from './Minimap.svelte'
  import DoodadPalette from './DoodadPalette.svelte'
  import UnitPalette, { type StartLocEntry } from './UnitPalette.svelte'
  import TerrainPalette, { type TerrainBrush } from './TerrainPalette.svelte'
  import RegionPanel from './RegionPanel.svelte'
  import UpdateDialog from './UpdateDialog.svelte'
  import CameraOrbitGizmo from './CameraOrbitGizmo.svelte'
  import DiagnosticsOverlay from './DiagnosticsOverlay.svelte'
  import { showToast } from './toast'
  import { flog, flogError, flogDebug, setLogLevel } from './debuglog'
  import { registerDiag } from './diag-registry'
  import { loadIconURL } from './icon-loader'
  import { TEAM_COLORS_RGB, PLAYER_COLOR_NAMES, neutralName } from './sloc-markers'
  import { Button } from '$lib/components/ui/button'
  import * as Dialog from '$lib/components/ui/dialog'
  import * as DropdownMenu from '$lib/components/ui/dropdown-menu'
  import { Input } from '$lib/components/ui/input'
  import { Toaster } from '$lib/components/ui/sonner'
  import ChevronDownIcon from '@lucide/svelte/icons/chevron-down'
  import EyeIcon from '@lucide/svelte/icons/eye'
  import EyeOffIcon from '@lucide/svelte/icons/eye-off'
  import FolderOpenIcon from '@lucide/svelte/icons/folder-open'
  import FilePlusIcon from '@lucide/svelte/icons/file-plus-2'
  import BotIcon from '@lucide/svelte/icons/bot'
  import CopyIcon from '@lucide/svelte/icons/copy'

  // Wails drops struct typedefs from models.ts when they appear as map values,
  // so the unit/doodad type-index shapes are declared locally here. Must stay
  // in lockstep with main.UnitTypeInfo / main.DoodadTypeInfo on the Go side.
  interface UnitTypeInfo {
    file: string; model_scale: number; move_height: number
    red: number; green: number; blue: number
    name: string; category: string
    icon_art: string
  }
  interface DoodadTypeInfo {
    file: string; num_var: number; fixed_rot: number; model_scale: number
    name: string; category: string
    icon_art: string
  }

  let status: main.MapStatus = $state({ loaded: false, unit_count: 0, lua: false })
  let units: main.UnitDTO[] = $state([])
  let doodads: main.DoodadDTO[] = $state([])
  let unitTypes: Record<string, UnitTypeInfo> = $state({})
  let doodadTypes: Record<string, DoodadTypeInfo> = $state({})
  // Selection state is split by kind because creation_number is per-kind in
  // WC3 — a unit and a doodad can share an id, so a single Set can't tell
  // them apart. The full SelectionItemDTO[] mirror (selectionItems) is what
  // we use to compose new selections on mode='add'/'toggle' picks.
  let selectedIds = $state(new Set<number>())           // unit creation numbers
  let selectedDoodadIds = $state(new Set<number>())     // doodad creation numbers
  let selectionItems: main.SelectionItemDTO[] = $state([])
  let primaryEntity: unitsdoo.Entity | null = $state(null)
  let primaryDoodad: main.DoodadDTO | null = $state(null)
  // True while the Doodad Palette is placing a doodad (CreateDoodad +
  // addDoodadLive). The entity-changed `created` handler checks this to avoid a
  // redundant full reload for palette placements — those render their instance
  // live and the event may arrive before addDoodadLive finishes. Not $state:
  // it's a plain control flag, never read in the template.
  let palettePlacing = false
  // True while a `created`-triggered reloadMap is in flight. A group undo of a
  // multi-entity delete emits one `created` event per entity; without this
  // guard each would kick its own full reload. One reloadMap rebuilds the whole
  // scene from Go's authoritative state, so the first reload already covers
  // every reinserted entity — the rest coalesce into it.
  let createdReloadPending = false
  // Persistent state errors only — currently just the scene-init-failed path
  // during onMount. Transient operational errors (Save/Open/Move/Reforged
  // toggle) go through showToast() so they auto-dismiss.
  let error: string = $state('')
  let busy: boolean = $state(false)
  let reforged: boolean = $state(false)
  let pathingVisible: boolean = $state(false)
  // Start-location markers are shown by default; the Explorer "Markers"
  // section has an eye button to hide them from the viewport.
  let slocsVisible: boolean = $state(true)

  // Terrain-pick mode state. Owned here, mirrored to the scene via
  // scene.setTerrainPickMode. When a click hits a cell, terrainCell is set
  // and the Properties panel renders an additional section. Selecting an
  // entity (or clicking outside the map) does NOT clear it on its own — the
  // user explicitly clicks elsewhere on terrain to update.
  // Top-level editor mode. 'doodad' is the default (place doodads/units, select
  // & move entities); 'terrain' paints/inspects terrain; 'region' owns the
  // viewport for drawing/selecting regions. Panels + the click pipeline gate on
  // this directly via setEditorMode (the modes are mutually exclusive).
  type EditorMode = 'doodad' | 'terrain' | 'region'
  let editorMode: EditorMode = $state('doodad')
  let terrainCell: TerrainCellInfo | null = $state(null)

  // Doodad-placement state. When a type_id is armed (from the Doodad Palette),
  // the scene is in placement mode and the next map click drops that doodad.
  // null = nothing armed. Owned here; mirrored to the scene via
  // scene.setPlacementMode and read by the palette to highlight the armed icon.
  let armedDoodadType: string | null = $state(null)

  // Unit-placement state. Mirrors the doodad-placement flow: when a unit
  // type_id is armed (from the Unit Palette), the scene is in placement mode
  // and the next map click drops that unit, owned by armedUnitPlayer. null =
  // nothing armed. startLocArmed instead arms start-location placement — the
  // next click drops a sloc at the next free start-location index. Unit,
  // start-location, doodad, region-rect, and terrain modes are all mutually
  // exclusive (they all ride the scene's single placement/brush mode), so
  // arming one disarms the others. unitPlacing is the palettePlacing analog —
  // a plain flag (never read in the template) the `created` handler checks to
  // skip the redundant full reload for live unit placements.
  let armedUnitType: string | null = $state(null)
  let armedUnitPlayer: number = $state(0)
  let startLocArmed: boolean = $state(false)
  let startLocations: StartLocEntry[] = $state([])
  let unitPlacing = false

  // Region (rect) Editor state. The RegionPanel owns its own list UI + the
  // numeric editors (it calls the Wails region bindings directly); App owns the
  // viewport-facing pieces: the region-mode drag-to-draw + click-to-select
  // (handled in the scene, wired via handleRegionDraw/handleRegionPick) and the
  // scene overlay sync. selectedRegionCN drives the overlay highlight;
  // regionRefreshToken is bumped to tell the panel + overlay to re-fetch after
  // an out-of-panel change (a viewport draw, MCP edit, undo/redo).
  let selectedRegionCN: number | null = $state(null)
  let regionRefreshToken: number = $state(0)
  // Whether the region-rectangle overlay is drawn in the viewport. Pure view
  // preference — the regions are untouched. Persisted so it survives reloads,
  // and re-applied to the scene after every map load via syncRegionOverlay.
  // Default ON so regions are visible out of the box (matches prior behavior).
  const REGION_OVERLAY_LS_KEY = 'wc3-forge:showRegions'
  let regionOverlayVisible: boolean = $state((() => {
    try {
      const raw = localStorage.getItem(REGION_OVERLAY_LS_KEY)
      return raw === null ? true : raw === '1'
    } catch { return true }
  })())
  function toggleRegionOverlay() {
    regionOverlayVisible = !regionOverlayVisible
    try { localStorage.setItem(REGION_OVERLAY_LS_KEY, regionOverlayVisible ? '1' : '0') } catch {}
    scene?.setRegionOverlayVisible(regionOverlayVisible)
  }

  // Whether the camera-bounds shadow is drawn in the viewport.
  // Pure view preference, persisted so it survives reloads and re-applied to the
  // scene after every map load via syncCameraBounds. Default ON so the playable
  // edge is visible out of the box. Toggled from the View → Overlays menu.
  const CAMERA_BOUNDS_LS_KEY = 'wc3-forge:showCameraBounds'
  let cameraBoundsVisible: boolean = $state((() => {
    try {
      const raw = localStorage.getItem(CAMERA_BOUNDS_LS_KEY)
      return raw === null ? true : raw === '1'
    } catch { return true }
  })())

  // Terrain-brush state. The Terrain Palette reports the armed brush here; we
  // mirror its size/shape to the scene's footprint cursor and put the scene in
  // brush mode. A brush stroke (mousedown→drag→mouseup) arrives via
  // handleTerrainBrushStroke, which brackets the Go brush calls in one undo
  // group. null = nothing armed.
  let terrainBrush: TerrainBrush | null = $state(null)
  // Serializes the async brush calls of a single stroke so BeginUndoGroup, the
  // dabs, and EndUndoGroup always run in order even as paint events fire fast.
  let terrainBrushChain: Promise<unknown> = Promise.resolve()
  // Flatten target Z, captured on stroke start (the height under the first dab).
  let terrainFlattenTarget = 0
  // Water surface Z for the Add Water tool, captured once on stroke start (auto
  // mode) or taken from the palette's fixed-height value, then replayed for every
  // dab so a drag paints one flat lake instead of draping over the terrain.
  let terrainWaterTarget = 0
  // True between BeginUndoGroup and EndUndoGroup so 'end' always closes the
  // group even if the brush was disarmed mid-stroke (which would otherwise leak
  // an open group and wedge undo).
  let terrainStrokeOpen = false

  // Diagnostics: top-left overlay + in-canvas heartbeat (F9). Verbose logging
  // drops the log threshold to 'debug' so per-frame/per-asset traces are
  // written. Both persist to localStorage so a debugging session survives
  // reloads. Applied to the scene/logger in onMount once they exist.
  let diagnosticsOn: boolean = $state(
    (() => { try { return localStorage.getItem('wc3-forge:diagnostics') === '1' } catch { return false } })(),
  )
  let verboseLogging: boolean = $state(
    (() => { try { return localStorage.getItem('wc3-forge:verbose-log') === '1' } catch { return false } })(),
  )
  let diagReportTimer = 0
  // Diagnostics-pump self-health, surfaced via the 'pump' registry provider.
  // reportFail (auto-red in the overlay) catches a silently-failing reporter;
  // framesAdvanced distinguishes a render-loop stall from a present-stall.
  const pumpHealth = { framesAdvanced: 0, reportOk: 0, reportFail: 0, lastError: '' }
  let pumpLastFrame = 0
  function toggleDiagnostics() {
    diagnosticsOn = !diagnosticsOn
    scene?.setDiagnosticsMode(diagnosticsOn)
    try { localStorage.setItem('wc3-forge:diagnostics', diagnosticsOn ? '1' : '0') } catch { /* ignore */ }
  }
  function toggleVerboseLogging() {
    verboseLogging = !verboseLogging
    setLogLevel(verboseLogging ? 'debug' : 'info')
    try { localStorage.setItem('wc3-forge:verbose-log', verboseLogging ? '1' : '0') } catch { /* ignore */ }
    flog(`[diag] verbose logging ${verboseLogging ? 'ON' : 'OFF'}`)
  }

  // Gizmo mode (Phase C). Mirrors scene.getGizmoMode() — the source of truth
  // lives in gizmo.ts; this is the reactive shadow that drives the header
  // mode-switch UI. Defaults match the scene's own default.
  let gizmoMode: 'move' | 'rotate' | 'scale' = $state('move')

  // Gizmo snap settings (Phase D). All channels default OFF (freeform);
  // step values match the gizmo's own defaults so a fresh "Enable" reads
  // sensibly without the user picking a value first. Step units:
  //   moveStep  = studs (128 = one terrain tile)
  //   rotateStep = degrees
  //   scaleStep  = multiplier (e.g. 0.1 = 10% increments)
  // Shift held during drag inverts each channel's enabled state for that
  // drag only (Blender convention).
  let snapMoveOn = $state(false), snapMoveStep = $state(32)
  let snapRotateOn = $state(false), snapRotateStep = $state(15)
  let snapScaleOn = $state(false), snapScaleStep = $state(0.1)
  // Preset step values per channel (design §3.2 recommendations).
  const MOVE_STEPS = [1, 5, 32, 64, 128]
  const ROTATE_STEPS = [1, 5, 15, 45, 90]
  const SCALE_STEPS = [0.05, 0.1, 0.25, 0.5]

  // Doodad-category visibility — owned here, mirrored to scene via
  // scene.setDoodadCategoryVisible. Categories absent from the map are
  // omitted from the View menu. Visibility is RENDERING-ONLY (never persists
  // to the saved map; the View-menu toggle is a viewport filter).
  let doodadVisibility: Record<string, boolean> = $state({})
  let doodadCategoriesPresent: string[] = $state([])

  let canvas: HTMLCanvasElement = $state()
  let scene: SceneAPI | null = $state(null)
  let dirty: boolean = $state(false)
  let saving: boolean = $state(false)
  let launching: boolean = $state(false)

  // Undo/redo state. Mirrors session.History — refreshed on
  // wc3-forge:history-changed events. The Edit menu reads from these to set
  // enabled state + tooltip labels ("Undo Move Footman"). Defaults to "off
  // and empty" so the menu renders correctly before the first map opens.
  let canUndo: boolean = $state(false)
  let canRedo: boolean = $state(false)
  let undoLabel: string = $state('')
  let redoLabel: string = $state('')

  // Unsaved-changes modal. Open is driven by the wc3-forge:close-requested
  // event fired from Go's OnBeforeClose handler when the user clicks the
  // window X (or Alt+F4) on a dirty session. The three buttons map to:
  //   Save & Quit    → SaveMap() then ForceQuit()
  //   Discard & Quit → ForceQuit() (in-memory edits drop on process exit)
  //   Cancel         → close the modal, do nothing
  let quitGuardOpen: boolean = $state(false)
  let quitGuardSaving: boolean = $state(false)

  async function launchTestMap() {
    if (testMapDisabled) return
    launching = true
    try {
      if (testMapIsFolder) {
        showToast('Packaging & launching…', 'info')
      }
      await LaunchInWC3()
      showToast('Launching Warcraft III…', 'info')
    } catch (e) {
      const msg = String(e)
      if (/Warcraft III\.exe not found/i.test(msg)) {
        showToast(
          'Warcraft III.exe not found. Set WC3FORGE_WC3_PATH to your WC3 install root if it\'s not at the default location.',
          'error',
        )
      } else if (/map file not found/i.test(msg)) {
        showToast('Map file not found at: ' + (status.path || '(unknown)'), 'error')
      } else {
        showToast('launch failed: ' + msg, 'error')
      }
    } finally {
      launching = false
    }
  }

  // ----- Agent Console panel (bottom dock) -----
  //
  // Streams every MCP bridge-call dispatched against this wc3-forge instance
  // so the user can see, live, what every connected agent is doing. State is
  // owned here so the header toggle button + Ctrl+` hotkey can drive it.
  // Persisted to localStorage so reload preserves open/closed state.
  const LS_BRIDGE_CONSOLE_OPEN = 'wc3forge.bridge-console.open'
  let bridgeConsoleOpen: boolean = $state((() => {
    try { return localStorage.getItem(LS_BRIDGE_CONSOLE_OPEN) === '1' } catch { return false }
  })())
  function toggleBridgeConsole() {
    bridgeConsoleOpen = !bridgeConsoleOpen
  }

  // ----- Map Info Editor modal -----
  // Conditional mount in the template (`{#if showMapInfoEditor}`) so the
  // dialog component fully unmounts on close — keeps the focus-trap +
  // global keydown listeners scoped to when the modal is actually open.
  let showMapInfoEditor: boolean = $state(false)
  function openMapInfoEditor() {
    if (!status.loaded) return
    showMapInfoEditor = true
  }
  function closeMapInfoEditor() {
    showMapInfoEditor = false
  }

  // ----- Object Editor modal (Phase 1a: units only, read-only) -----
  // Same conditional-mount pattern as MapInfoEditor so the dialog
  // component fully unmounts on close — keeps the rather large
  // tree+field-table render off the DOM until it's actually wanted.
  let showObjectEditor: boolean = $state(false)
  let objectEditorInitialId: string | null = $state(null)
  // Keyboard-shortcuts cheat sheet overlay. Toggled by '?' (Shift+/) or the
  // Help menu — the discoverable index of every binding, aimed at World Editor
  // veterans hunting for the F-keys.
  let showShortcuts: boolean = $state(false)
  // Static content for the cheat sheet. Keep in lockstep with the bindings in
  // onGlobalKeyDown (+ the camera-pan keys in camera.ts).
  const shortcutGroups: { title: string; items: { label: string; keys: string[] }[] }[] = [
    { title: 'Editors', items: [
      { label: 'Object Editor', keys: ['F6'] },
      { label: 'Trigger Editor', keys: ['F4'] },
      { label: 'Map Info', keys: ['Ctrl+Shift+I'] },
    ] },
    { title: 'Editor mode', items: [
      { label: 'Doodad / Unit mode', keys: ['F2'] },
      { label: 'Terrain mode', keys: ['F3'] },
      { label: 'Region mode', keys: ['F8'] },
      { label: 'Cycle modes', keys: ['Tab', 'Shift+Tab'] },
    ] },
    { title: 'Edit', items: [
      { label: 'Save', keys: ['Ctrl+S'] },
      { label: 'Undo', keys: ['Ctrl+Z'] },
      { label: 'Redo', keys: ['Ctrl+Shift+Z'] },
      { label: 'Delete selection', keys: ['Del', 'Backspace'] },
    ] },
    { title: 'Transform', items: [
      { label: 'Move', keys: ['1'] },
      { label: 'Rotate', keys: ['2'] },
      { label: 'Scale', keys: ['3'] },
    ] },
    { title: 'View & camera', items: [
      { label: 'Pan camera', keys: ['WASD', 'Arrows'] },
      { label: 'Frame selection', keys: ['F'] },
      { label: 'Minimap', keys: ['M'] },
      { label: 'Diagnostics', keys: ['F9'] },
      { label: 'Agent Console', keys: ['Ctrl+`'] },
    ] },
    { title: 'Help', items: [
      { label: 'This cheat sheet', keys: ['?'] },
    ] },
  ]
  function openObjectEditor(id: string | null = null) {
    if (!status.loaded) return
    objectEditorInitialId = id
    showObjectEditor = true
  }
  function closeObjectEditor() {
    showObjectEditor = false
    objectEditorInitialId = null
  }

  // ----- Trigger Editor modal (Phase 1a: read-only) -----
  // Mount-on-demand mirroring Object Editor: the trigger tree + ECA detail
  // render can be heavy (hundreds of rows on a real map), so we keep the
  // component out of the DOM until opened.
  let showTriggerEditor: boolean = $state(false)
  let triggerEditorInitialId: number | null = $state(null)
  function openTriggerEditor(id: number | null = null) {
    if (!status.loaded) return
    triggerEditorInitialId = id
    showTriggerEditor = true
  }
  function closeTriggerEditor() {
    showTriggerEditor = false
    triggerEditorInitialId = null
  }

  // ----- Convert to Lua modal -----
  //
  // The flow: user clicks File → Convert to Lua…. We synchronously call
  // CheckConvertToLua to see if the map has unconvertable JASS content; the
  // dialog renders either the confirm path (no blockers) or the blocker list
  // path. The Convert button itself goes through ConvertMapToLua on the Go
  // side. Result is undoable until Save (Ctrl+S) persists it.
  //
  // Hidden when the loaded map is already Lua (info.Lua=true). The File menu
  // item disables in that case so this never reaches the dialog, but defensive
  // guards live in the Go layer too.
  let showConvertToLua: boolean = $state(false)
  let convertBlockers: { trigger_id: number; trigger_name: string; kind: string; reason: string }[] = $state([])
  let convertChecking: boolean = $state(false)

  // WC3-install detection: on startup we check whether a usable CASC root is
  // resolvable; if not, prompt the user to locate their install (soft warning).
  let showWC3InstallDialog: boolean = $state(false)
  let wc3CheckedPath: string = $state('')
  async function openConvertToLua() {
    if (!status.loaded) return
    if (status.lua) {
      showToast('Map is already Lua — nothing to convert.', 'info')
      return
    }
    if (convertChecking) return
    convertChecking = true
    try {
      const res = await CheckConvertToLua()
      convertBlockers = (res?.blockers ?? []) as typeof convertBlockers
      showConvertToLua = true
    } catch (e) {
      showToast(`Convert check failed: ${e instanceof Error ? e.message : String(e)}`, 'error')
    } finally {
      convertChecking = false
    }
  }
  function closeConvertToLua() {
    showConvertToLua = false
    convertBlockers = []
  }
  async function onConvertedToLua() {
    // Refresh status (Lua flag flipped) + dirty flag (info dirty after convert).
    try { status = await Status() } catch {}
    try { dirty = await IsDirty() } catch {}
  }
  // Public bridge for child components (TriggerEditor's Test Map error toast)
  // that need to launch the convert flow without prop-drilling. Same pattern
  // as the existing __runPickSelfTest / __showToast globals on window.
  if (typeof window !== 'undefined') {
    ;(window as any).__openConvertToLua = openConvertToLua
  }

  // ----- Swap Tileset modal -----
  // Same mount-on-demand pattern as Map Info Editor. The terrain refresh on
  // Apply (and on Undo/Redo) is driven uniformly by the kind=='terrain'
  // entity-changed event handler — no need for a per-dialog callback.
  let showSwapTileset: boolean = $state(false)
  function openSwapTileset() {
    if (!status.loaded) return
    showSwapTileset = true
  }
  function closeSwapTileset() {
    showSwapTileset = false
  }

  // ----- New Map modal -----
  // Collects starter-map config (name/size/tileset), then the create flow
  // (Save-location picker + Go CreateNewMap) runs in handleCreateNewMap.
  let showNewMap: boolean = $state(false)
  let newMapBusy: boolean = $state(false)
  function openNewMap() {
    showNewMap = true
  }

  // First-run / Claude-Code setup: the empty-state overlay's "Set up Claude
  // Code" action opens this dialog with the one-line MCP registration command.
  let showMcpSetup: boolean = $state(false)
  let mcpCopied: boolean = $state(false)
  const mcpSetupCmd = 'claude mcp add wc3-forge --scope user -- "<install>\\wc3-forge.exe" --mcp'
  async function copyMcpCmd() {
    try {
      await navigator.clipboard.writeText(mcpSetupCmd)
      mcpCopied = true
      setTimeout(() => (mcpCopied = false), 1500)
    } catch {
      /* clipboard blocked — the command stays visible to copy manually */
    }
  }
  // Run the create flow: pick a .w3x destination, write the map, then open it.
  // When a map is already loaded we don't clobber the current session — write
  // the file and open it in a fresh window instead (mirrors how Open Map File
  // behaves with a map already open).
  async function handleCreateNewMap(cfg: { name: string; width: number; height: number; tileset: string }) {
    newMapBusy = true
    try {
      const path = await NewMapSaveDialog(cfg.name)
      if (!path) { newMapBusy = false; return }
      const params = { name: cfg.name, width: cfg.width, height: cfg.height, tileset: cfg.tileset, path }
      if (status.loaded) {
        await WriteNewMap(params)
        await OpenMapInNewWindow(path)
      } else {
        status = await CreateNewMap(params)
        mapLoadGen += 1
        await reloadMap()
        showToast('Created ' + cfg.name, 'success')
      }
      showNewMap = false
    } catch (e) {
      showToast('New map failed: ' + String(e), 'error')
    } finally {
      newMapBusy = false
    }
  }

  // ----- Import 3D Model modal -----
  // Mount-on-demand like Swap Tileset. The Go method opens the native file
  // dialog itself, converts the mesh to .mdx (+ textures), and imports it into
  // the current map's archive. On a successful import we run the same asset
  // refresh the open-map flow uses (reloadMap with keepCamera) so the new
  // in-archive file resolves and the viewport repaints without losing the
  // user's camera.
  let showImportModel: boolean = $state(false)
  function openImportModel() {
    if (!status.loaded) return
    showImportModel = true
  }
  function closeImportModel() {
    showImportModel = false
  }
  async function onModelImported() {
    if (status.loaded) await reloadMap({ keepCamera: true })
  }

  // ----- Import Manager modal -----
  // General (any-file) Import Manager: list/add/remove/rename of the files
  // registered in war3map.imp. Distinct from "Import 3D Model" (which converts
  // a mesh); this manages arbitrary imports (icons, loading screens, sounds,
  // textures). Add/remove/rename commit immediately via Wails App methods that
  // share the forge.Session mutators with the imports.* MCP verbs. After a
  // change we refresh assets (keepCamera) so the new/renamed/removed file
  // resolves in the viewport + previews.
  let showImportManager: boolean = $state(false)
  function openImportManager() {
    if (!status.loaded) return
    showImportManager = true
  }
  function closeImportManager() {
    showImportManager = false
  }
  async function onImportsChanged() {
    if (status.loaded) await reloadMap({ keepCamera: true })
  }

  // ----- Gameplay Constants Editor modal -----
  // Same lifecycle pattern as MapInfoEditor: conditional mount keeps the
  // global keydown + focus-trap listeners scoped to when the modal is
  // actually visible.
  let showGameplayConstantsEditor: boolean = $state(false)
  function openGameplayConstantsEditor() {
    if (!status.loaded) return
    showGameplayConstantsEditor = true
  }
  function closeGameplayConstantsEditor() {
    showGameplayConstantsEditor = false
  }

  // ----- In-app updater -----
  // Auto-checks GitHub Releases on launch (opt-out) and offers a one-click
  // download-and-install of the NSIS installer. Prefs persist in localStorage,
  // matching the app's other settings (theme, minimap, …). Default: auto-check
  // ON, auto-install OFF (running an installer unprompted is an explicit
  // opt-in). The Go side (update_app.go) owns the GitHub query + download +
  // elevated relaunch; this drives the UI and the dirty-state guard.
  let appVersion: string = $state('dev')
  // Help → About dialog (version + author + credits). Read-only; no Go calls.
  let showAboutDialog: boolean = $state(false)
  let showUpdateDialog: boolean = $state(false)
  let updateResult: update.CheckResult | null = $state(null)
  let updateError: string = $state('')
  let updateDownloading: boolean = $state(false)
  let updateDownloadPct: number = $state(0)
  let autoCheckUpdates: boolean = $state(localStorage.getItem('wc3-forge:auto-check-updates') !== '0')

  // Persist the toggle whenever the user flips it in the dialog.
  $effect(() => {
    localStorage.setItem('wc3-forge:auto-check-updates', autoCheckUpdates ? '1' : '0')
  })

  // Silent check fired on startup. Only surfaces something when an update
  // exists, and then always opens the dialog — the user decides whether to
  // install (the installer needs elevation, so we never elevate unprompted).
  async function autoCheckForUpdate() {
    if (!autoCheckUpdates) return
    try {
      const res = await CheckForUpdate()
      if (!res?.isNewer || !res.release?.installer) return
      updateResult = res
      updateError = ''
      showUpdateDialog = true
    } catch (e) {
      // Network hiccup / rate limit on a background check is non-fatal and
      // shouldn't nag — log it and stay quiet.
      flogDebug('auto update check failed: ' + String(e))
    }
  }

  // Manual check from the menu — always opens the dialog so the user gets
  // feedback either way ("update available" or "you're up to date").
  async function checkForUpdatesManual() {
    updateError = ''
    updateResult = null
    updateDownloading = false
    updateDownloadPct = 0
    showUpdateDialog = true
    try {
      updateResult = await CheckForUpdate()
    } catch (e) {
      updateError = String(e)
    }
  }

  // Download + run the installer. Guards unsaved work first: the installer
  // closes wc3-forge, so a dirty map would lose edits. We prompt to save.
  async function startUpdate() {
    const url = updateResult?.release?.installer?.url
    if (!url) return
    if (await IsDirty()) {
      const choice = confirm(
        'You have unsaved changes. Save before updating?\n\n' +
          'OK = save and continue, Cancel = abort update.',
      )
      if (!choice) return
      // trySaveWithForcePrompt offers the overwrite prompt if the map changed on
      // disk and toasts on failure; abort the update unless it actually saved.
      if (!(await trySaveWithForcePrompt())) return
    }
    updateDownloading = true
    updateDownloadPct = 0
    if (!showUpdateDialog) showUpdateDialog = true
    try {
      // Resolves by quitting the app (the installer takes over); if it
      // returns an error the download/launch failed and we recover.
      await DownloadAndRunInstaller(url)
    } catch (e) {
      updateDownloading = false
      updateError = String(e)
      showToast('Update failed: ' + String(e), 'error')
    }
  }

  // ----- Minimap overlay (bottom-right of viewport) -----
  //
  // Shows the map's BAKED minimap image (war3mapMap.blp / .dds / TGA fallback)
  // — author-drawn, NOT a render of terrain. Matches HiveWE's minimap panel
  // behavior. The component owns its own image-fetch + decode + click-to-pan
  // math; this page-level state controls only visibility + map-load gen.
  //
  // Visibility persists in localStorage so the user's preference survives
  // app restarts. Hotkey 'M' toggles. Click on the image pans the 3D camera.
  const MINIMAP_LS_KEY = 'wc3-forge:showMinimap'
  let showMinimap: boolean = $state((() => {
    try {
      const raw = localStorage.getItem(MINIMAP_LS_KEY)
      // Default to ON for new users — the panel is small and not in the way,
      // and being visible by default mirrors HiveWE's default.
      return raw === null ? true : raw === '1'
    } catch { return true }
  })())
  // Generation counter bumped whenever the loaded map changes (Open / Close /
  // map-changed event). The Minimap component watches this prop and reloads
  // its image + cached terrain coords on every bump. Avoids the component
  // having to subscribe to its own MAP_EVENT — single source of truth here.
  let mapLoadGen: number = $state(0)
  function toggleMinimap() {
    showMinimap = !showMinimap
    try { localStorage.setItem(MINIMAP_LS_KEY, showMinimap ? '1' : '0') } catch {}
  }

  // ----- Theme (light / dark / system) -----
  //
  // Default is `'dark'` so the shadcn dark palette is active out of the box.
  // The actual `dark` class is toggled on <html> (document.documentElement) so
  // every shadcn component that switches on the `.dark` selector flips at
  // once. ViewMenu owns the user-facing toggle; we pass `theme` + `setTheme`
  // as props (see Agent C's ViewMenu API).
  //
  // System-mode listens to prefers-color-scheme so a midday OS toggle flips
  // the app instantly; the listener is registered only while `theme === 'system'`.
  const THEME_LS_KEY = 'wc3-forge:theme'
  type Theme = 'light' | 'dark' | 'system'
  function readStoredTheme(): Theme {
    try {
      const raw = localStorage.getItem(THEME_LS_KEY)
      if (raw === 'light' || raw === 'dark' || raw === 'system') return raw
    } catch {}
    return 'dark'
  }
  function resolveTheme(t: Theme): 'light' | 'dark' {
    if (t === 'system') {
      try { return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light' }
      catch { return 'dark' }
    }
    return t
  }
  function applyTheme(t: Theme) {
    const resolved = resolveTheme(t)
    const cls = document.documentElement.classList
    if (resolved === 'dark') cls.add('dark')
    else cls.remove('dark')
  }
  let theme: Theme = $state(readStoredTheme())
  // Apply immediately at module load so the very first paint is dark — this
  // runs in the <script> body, before <main> mounts.
  applyTheme(theme)
  let systemMQ: MediaQueryList | null = null
  function onSystemThemeChange() { applyTheme(theme) }
  function setTheme(next: Theme) {
    theme = next
    try { localStorage.setItem(THEME_LS_KEY, next) } catch {}
    applyTheme(next)
    // Listener bookkeeping: only attach the prefers-color-scheme handler
    // while we're in 'system' mode; detach otherwise to avoid wasted work.
    try {
      if (next === 'system') {
        if (!systemMQ) {
          systemMQ = window.matchMedia('(prefers-color-scheme: dark)')
          systemMQ.addEventListener('change', onSystemThemeChange)
        }
      } else if (systemMQ) {
        systemMQ.removeEventListener('change', onSystemThemeChange)
        systemMQ = null
      }
    } catch {}
  }
  // Kick the listener bookkeeping once on initial load so a stored 'system'
  // preference re-attaches its MQ listener on every app start.
  if (theme === 'system') {
    try {
      systemMQ = window.matchMedia('(prefers-color-scheme: dark)')
      systemMQ.addEventListener('change', onSystemThemeChange)
    } catch {}
  }

  // ----- File menu (header dropdown) -----
  // Open/close + outside-click + Escape are owned by shadcn's DropdownMenu
  // (bits-ui under the hood); we no longer hand-roll any of that.
  let fileMenuOpen: boolean = $state(false)
  function runMenuAction(fn: () => unknown) {
    return () => {
      fileMenuOpen = false
      void fn()
    }
  }

  // ----- Right-column split (Explorer above Properties) -----
  //
  // Vertical splitter between Explorer and Properties. Tracked as a percent
  // of the right column's height so window-resize keeps the same ratio. The
  // splitter component reports dy in pixels; we convert to a percent delta
  // against the column's pixel height each drag tick.
  // Default: 50/50. Session-only (no persistence).
  let rightExplorerPct: number = $state(50)
  const RIGHT_MIN_PCT = 15
  const RIGHT_MAX_PCT = 85
  let rightColEl: HTMLDivElement | null = $state(null)
  function onRightSplitterDrag(dy: number) {
    if (!rightColEl) return
    const h = rightColEl.clientHeight
    if (h <= 0) return
    const dpct = (dy / h) * 100
    const next = Math.max(RIGHT_MIN_PCT, Math.min(RIGHT_MAX_PCT, rightExplorerPct + dpct))
    rightExplorerPct = next
    // Canvas size depends on the viewport's clientWidth (constant during this
    // drag — only the right-column SECTIONS resize vertically). No camera
    // aspect bump needed here; the viewport-width drag (rightColWidthPx)
    // handles that separately.
  }

  // ----- Right-column WIDTH (horizontal drag-resize between viewport + right col) -----
  //
  // The right column was a fixed 340px. Now it's user-resizable via a vertical
  // splitter on its LEFT edge. Width is tracked in pixels, persisted to
  // localStorage, and clamped to [MIN, MAX]. On window resize we re-clamp so
  // the viewport never gets squashed below ~400px of usable horizontal space.
  //
  // Why pixels (not percent)? The right column hosts the Explorer + Properties
  // panels — content has natural width requirements (labels, mono values).
  // The user sizing the column to "fit my Properties panel" is the dominant
  // use; relative-to-window resizing would re-shrink the panel on every window
  // resize. The viewport (left/center) flexes to absorb the remaining space.
  const RIGHT_COL_LS_KEY = 'wc3-forge:rightColWidth'
  const RIGHT_COL_MIN_PX = 240
  const RIGHT_COL_MAX_PX = 600
  const RIGHT_COL_DEFAULT_PX = 340
  // Minimum horizontal space we reserve for the viewport. If the window gets
  // narrow enough that (viewport width = window - rightColWidth) would drop
  // below this, we shrink the right column to keep the viewport usable.
  const VIEWPORT_MIN_PX = 400
  let rightColWidthPx: number = $state((() => {
    try {
      const raw = localStorage.getItem(RIGHT_COL_LS_KEY)
      if (raw === null) return RIGHT_COL_DEFAULT_PX
      const n = parseFloat(raw)
      if (!Number.isFinite(n)) return RIGHT_COL_DEFAULT_PX
      return Math.max(RIGHT_COL_MIN_PX, Math.min(RIGHT_COL_MAX_PX, n))
    } catch { return RIGHT_COL_DEFAULT_PX }
  })())
  function clampRightColWidth(px: number): number {
    let n = Math.max(RIGHT_COL_MIN_PX, Math.min(RIGHT_COL_MAX_PX, px))
    // Don't let the viewport drop below VIEWPORT_MIN_PX on narrow windows.
    // Subtract the splitter strip (4px) so the math matches reality.
    const winW = (typeof window !== 'undefined') ? window.innerWidth : 1200
    const maxByViewport = winW - VIEWPORT_MIN_PX - 4
    if (maxByViewport >= RIGHT_COL_MIN_PX && n > maxByViewport) n = maxByViewport
    return n
  }
  function onRightColWidthDrag(dx: number) {
    // The splitter is on the LEFT edge of the right column. Dragging RIGHT
    // (positive dx) makes the right column NARROWER; dragging LEFT (negative
    // dx) makes it WIDER. Hence the minus.
    rightColWidthPx = clampRightColWidth(rightColWidthPx - dx)
  }
  function onRightColWidthDragEnd() {
    try { localStorage.setItem(RIGHT_COL_LS_KEY, String(Math.round(rightColWidthPx))) } catch {}
  }
  function onWindowResize() {
    const next = clampRightColWidth(rightColWidthPx)
    if (next !== rightColWidthPx) rightColWidthPx = next
  }

  // ----- Explorer accordion state -----
  //
  // Per-section open/closed map. Keys: 'heroes' | 'units' | 'markers' |
  // 'doodads' for top-level, 'd:<category>' for the doodad sub-buckets,
  // 'p:<section>' for Properties sections.
  // Defaults open on first render; user toggles persist for the session.
  let sectionOpen: Record<string, boolean> = {}
  function isOpen(id: string, def: boolean = true): boolean {
    const v = sectionOpen[id]
    return v === undefined ? def : v
  }
  function onSectionToggle(detail: { id: string; open: boolean }) {
    sectionOpen = { ...sectionOpen, [detail.id]: detail.open }
  }

  const SEL_EVENT = 'wc3-forge:selection-changed'
  const MAP_EVENT = 'wc3-forge:map-changed'
  const DIRTY_EVENT = 'wc3-forge:dirty-changed'
  const ENTITY_EVENT = 'wc3-forge:entity-changed'
  const DEV_ANIM_EVENT = 'wc3-forge:dev-set-anim'

  // ----- Wails-event-subscription health (diagnostics) -----
  //
  // Every named EventsOn subscription bumps its {count, lastTs} here on each
  // delivery (via bumpEvent()). The 'events' diag provider exposes the map so
  // a SILENTLY-UNFIRED subscription is visible: count===0 means Go never
  // emitted (or the listener was registered against a stale event name) for an
  // event we expect to flow — e.g. selection-changed staying at 0 after a
  // click is the signature of a broken selection round-trip. O(1), no GL, no
  // scene scan: just a counter bump per delivery and a shallow copy on read.
  const eventStats: Record<string, { count: number; lastTs: number }> = {}
  function bumpEvent(name: string) {
    const s = eventStats[name] ?? (eventStats[name] = { count: 0, lastTs: 0 })
    s.count++
    s.lastTs = Date.now()
  }

  onMount(async () => {
    try {
      try { reforged = await GetReforgedMode() } catch { reforged = false }
      scene = createScene(canvas, reforged)
      scene.setPathingVisible(pathingVisible)
      scene.setSlocsVisible(slocsVisible)
      scene.onPick(handlePick)
      scene.onTerrainPick(handleTerrainPick)
      scene.onPlacementPick(handlePlacementPick)
      scene.onTerrainBrushStroke(handleTerrainBrushStroke)
      scene.onRegionDraw(handleRegionDraw)
      scene.onRegionPick(handleRegionPick)
      // Re-apply persisted diagnostics / verbose-logging settings now that the
      // scene + logger exist.
      scene.setDiagnosticsMode(diagnosticsOn)
      if (verboseLogging) setLogLevel('debug')
      // If a map is already open at startup (e.g. --open), the MAP_EVENT that
      // drives the overlay syncs can fire before this component's listener is
      // registered. Push the camera-bounds overlay once here as well so it shows
      // on first load; idempotent with the MAP_EVENT path.
      try {
        const st0 = await Status()
        if (st0.loaded) { status = st0; await syncCameraBounds() }
      } catch (e) { flogError('[camera-bounds] initial sync failed:', e) }
      // Pump-health provider — appears in both the overlay and diagnostics.get.
      registerDiag('pump', () => ({ ...pumpHealth }))
      // Event-subscription health provider. Pre-seed the events we subscribe to
      // below so a never-delivered one reports count:0 (visible "silent
      // subscription") rather than being absent from the map entirely.
      for (const n of [
        'casc-remounted', MAP_EVENT, DEV_ANIM_EVENT, DIRTY_EVENT, ENTITY_EVENT,
        SEL_EVENT, 'history-changed', 'close-requested', 'test-command',
      ]) {
        eventStats[n] = { count: 0, lastTs: 0 }
      }
      registerDiag('events', () => {
        // Shallow-copy each entry so the snapshot is an inert plain object.
        const out: Record<string, { count: number; lastTs: number }> = {}
        for (const k in eventStats) out[k] = { ...eventStats[k] }
        return out
      })
      ;(window as any).__scene = scene
      // Push diagnostics to Go ~5Hz so the diagnostics.get MCP tool can peek at
      // the live viewport numbers without a screenshot — runs regardless of the
      // overlay toggle. Cheap: getDiagnostics reads counters; one small JSON.
      diagReportTimer = window.setInterval(() => {
        try {
          const snap = scene?.getDiagnostics?.()
          if (!snap) { pumpHealth.reportFail++; pumpHealth.lastError = 'no scene snapshot'; return }
          // framesAdvanced: how many render-loop frames elapsed since the last
          // pump tick (~200ms). 0 over a tick = the render LOOP stalled/crashed;
          // a healthy nonzero count while the viewport looks frozen = a PRESENT
          // stall (loop runs, canvas not shown) — the exact distinction that
          // took hours to pin down on the sky-gradient bug. Read by the 'pump'
          // registry provider so it appears in the overlay AND diagnostics.get.
          pumpHealth.framesAdvanced = snap.frame - pumpLastFrame
          pumpLastFrame = snap.frame
          ReportDiagnostics(JSON.stringify({
            ...snap,
            doc: {
              visibility: document.visibilityState,
              focused: document.hasFocus(),
              hidden: document.hidden,
            },
            map: { name: status.name, path: status.path, loaded: status.loaded },
          }))
          pumpHealth.reportOk++
        } catch (e) {
          pumpHealth.reportFail++
          pumpHealth.lastError = e instanceof Error ? e.message : String(e)
        }
      }, 200)
      ;(window as any).__showToast = showToast
    } catch (e) {
      error = 'scene init failed: ' + (e instanceof Error ? (e.stack || e.message) : String(e))
      flogError('[scene-init]', e instanceof Error ? e.stack : String(e))
      console.error(e)
    }
    // Non-blocking: warn if no Warcraft III install (CASC root) is resolvable.
    // Stock models/textures/data come from CASC, so without it the viewport
    // renders without stock art. Soft warning — folder-bundled maps still work.
    WC3InstallStatus()
      .then((st) => {
        if (!st.available) {
          wc3CheckedPath = st.path
          showWC3InstallDialog = true
        }
      })
      .catch(() => {})
    // Stamp the running version (shown in the menu) and run the opt-out
    // auto update check in the background.
    GetAppVersion().then((v) => { appVersion = v }).catch(() => {})
    autoCheckForUpdate()
    // Installer-download progress → the update dialog's bar.
    EventsOn('wc3-forge:update-progress', (p: { pct?: number }) => {
      if (typeof p?.pct === 'number') updateDownloadPct = p.pct
    })
    // Live CASC remount (user located their WC3 install at runtime): the
    // viewer cached the stock assets that 404'd against the old missing
    // install as empty/failed, so purge that cache and reload the scene to
    // re-fetch them through the new mount — no restart needed.
    EventsOn('wc3-forge:casc-remounted', async () => {
      bumpEvent('casc-remounted')
      scene?.purgeResourceCache()
      if ((await Status()).loaded) await reloadMap({ keepCamera: true })
    })
    EventsOn(MAP_EVENT, async () => {
      bumpEvent(MAP_EVENT)
      status = await Status()
      // Bump the minimap-reload generation on every map-changed event so the
      // Minimap component refetches its image + terrain coords. Covers both
      // load (status.loaded=true → new minimap) and close (loaded=false →
      // component renders "no minimap" placeholder).
      mapLoadGen += 1
      if (status.loaded) {
        await reloadMap()
        // Push the loaded map's regions into the viewport overlay + refresh the
        // panel (a new map may have a different region set).
        await syncRegionOverlay()
        // Push the loaded map's camera bounds into the viewport overlay too.
        await syncCameraBounds()
        regionRefreshToken++
      } else {
        units = []
        doodads = []
        unitTypes = {}
        doodadTypes = {}
        selectedIds = new Set()
        selectedDoodadIds = new Set()
        selectionItems = []
        primaryEntity = null
        primaryDoodad = null
        terrainCell = null
        doodadCategoriesPresent = []
        // Map closed — disarm any doodad/unit/start-location placement so the
        // scene doesn't sit in placement mode with the palette (now unmounted)
        // gone.
        armedDoodadType = null
        armedUnitType = null
        startLocArmed = false
        startLocations = []
        scene?.setPlacementMode(false)
        // Map closed — exit region mode + clear the region overlay/selection.
        if (editorMode === 'region') setEditorMode('doodad')
        selectedRegionCN = null
        scene?.setRegionOverlay([])
        scene?.setSelectedRegion(null)
        regionRefreshToken++
        // Map closed — clear the camera-bounds shadow so the just-closed map's
        // bounds don't keep darkening the now-empty viewport.
        scene?.setCameraBounds(null)
      }
    })
    EventsOn(DEV_ANIM_EVENT, (payload: { creation_number: number; anim_name: string }) => {
      bumpEvent(DEV_ANIM_EVENT)
      scene?.setUnitAnimation(payload.creation_number, payload.anim_name)
    })
    // Verification harness. Triggered by Go-side --pick-self-test flag (which
    // emits this event ~4s after startup so map load + assets settle). The
    // scene runs a project-then-pick round-trip across every visible instance;
    // results land in wc3-forge.log via flog. WebView2 drops synthetic mouse
    // input so this is the only viable external entrypoint for pick verification.
    EventsOn('wc3-forge:pick-self-test', () => {
      const run = (scene as any)?.__runPickSelfTest
      if (typeof run === 'function') run()
    })
    // Startup map-load failure (--open / file-association / new-window). Go's
    // main() couldn't open the requested map (absent / corrupt / unsupported)
    // and stashed the message; App.startup emits it once the JS listener is up.
    // Surface it via the same error toast the interactive Open dialog uses so a
    // double-clicked corrupt/missing map shows a reason instead of a blank window.
    EventsOn('wc3-forge:startup-open-error', (payload: { message?: string }) => {
      showToast(payload?.message || 'Failed to open the map', 'error')
    })
    let startupSpec: { x: number; y: number; z: number; distance: number } | null = null
    EventsOn('wc3-forge:startup-camera', (payload: { spec: string }) => {
      const m = (payload?.spec || '').match(/^(-?\d+(?:\.\d+)?),(-?\d+(?:\.\d+)?)(?:,(-?\d+(?:\.\d+)?))?(?:,(\d+(?:\.\d+)?))?$/)
      if (!m) return
      startupSpec = {
        x: parseFloat(m[1]),
        y: parseFloat(m[2]),
        z: m[3] ? parseFloat(m[3]) : 0,
        distance: m[4] ? parseFloat(m[4]) : 0,
      }
      applyStartupCamera()
    })
    ;(window as any).__applyStartupCamera = applyStartupCamera
    function applyStartupCamera() {
      if (!startupSpec || !scene) return
      scene.panTo(startupSpec.x, startupSpec.y, startupSpec.z)
      if (startupSpec.distance > 0) {
        const camCtrl = (window as any).__camera
        if (camCtrl && typeof camCtrl.setDistance === 'function') {
          camCtrl.setDistance(startupSpec.distance)
        }
      }
    }
    EventsOn(DIRTY_EVENT, (payload: { dirty: boolean }) => {
      bumpEvent(DIRTY_EVENT)
      dirty = !!payload?.dirty
      // dirty-state consistency trace — silent unless Verbose Logging is on.
      flogDebug(`[dirty] dirty=${dirty}`)
    })
    EventsOn(ENTITY_EVENT, async (payload: { kind: string; id: number; field: string; position: number[] }) => {
      bumpEvent(ENTITY_EVENT)
      if (!payload) return
      if (payload.kind === 'region') {
        // Any region create/move/resize/rename/delete (from the panel, MCP, or
        // undo/redo) — re-push the overlay rects and tell the RegionPanel to
        // re-fetch its list. Cheap (one ListRegions call + a tiny buffer upload),
        // so no throttle needed; region counts are small.
        await syncRegionOverlay()
        regionRefreshToken++
        return
      }
      if (payload.kind === 'terrain') {
        // Only a TILESET SWAP (field "tileset": initial Apply, Undo, or Redo)
        // needs a full map reload — the palette changed, so the atlas + minimap
        // must rebake. Keep camera so undo doesn't yank the viewpoint.
        //
        // Brush edits (field tile/height/cliff/ramp) must NOT reload here: the
        // scene-instances terrain listener already re-renders them live
        // (throttled). A full reloadMap per dab re-placed every unit/doodad/
        // cliff/water ~40×/s during a drag — the terrain-tool lag + flicker,
        // worse the more content the map had.
        if (payload.field === 'tileset') {
          mapLoadGen += 1
          await reloadMap({ keepCamera: true })
        }
        return
      }
      // Entity removed (Delete key, MCP doodads.delete/units.delete, or undo of
      // a create). The scene detaches the rendered instance in its own
      // entity-changed subscription; here we just drop the row from the
      // Explorer's units/doodads arrays so the list stays in sync. The
      // selection-changed event (fired when the deleter clears selection)
      // resets primaryEntity/primaryDoodad, but null them defensively in case
      // the deleted entity was the focused primary without a selection change.
      if (payload.field === 'deleted') {
        if (payload.kind === 'unit') {
          units = units.filter(u => u.creation_number !== payload.id)
          if (primaryEntity?.CreationNumber === payload.id) primaryEntity = null
          // A deleted unit may have been a start location (slocs ARE units).
          // Re-sync the palette's list so the affordance + next-free index
          // track MCP/undo-driven start-location deletes too.
          void syncStartLocations()
        } else if (payload.kind === 'doodad') {
          doodads = doodads.filter(d => d.creation_number !== payload.id)
          if (primaryDoodad?.creation_number === payload.id) primaryDoodad = null
        }
        return
      }
      // Entity (re-)created from a source other than the live palettes — undo of
      // a delete, redo of a create, or an MCP units.create/doodads.create/
      // start_locations.create. The scene has no rendered instance for it yet,
      // so rebuild from Go's authoritative state (keepCamera so an undo doesn't
      // yank the viewpoint). The palettes place their own instance live +
      // handle the race via the palettePlacing/unitPlacing guards, so skip the
      // reload for those to keep placement fast.
      if (payload.field === 'created') {
        // A created unit may be a start location (sloc) — re-sync the palette
        // list regardless of which placement path produced it.
        if (payload.kind === 'unit') void syncStartLocations()
        if (palettePlacing || unitPlacing) return
        if (scene?.hasInstance(payload.kind, payload.id)) return
        if (createdReloadPending) return
        createdReloadPending = true
        mapLoadGen += 1
        try {
          await reloadMap({ keepCamera: true })
        } finally {
          createdReloadPending = false
        }
        return
      }
      if (payload.kind === 'unit') {
        // A position change on a unit may be a start location being moved
        // (drag/gizmo/Properties via MoveUnit, MCP start_locations.move, or
        // undo/redo). Re-sync the palette's cached list so its position +
        // next-free-index stay accurate. Cheap; no-op for non-sloc units.
        if (payload.field === 'position') void syncStartLocations()
        if (!primaryEntity || primaryEntity.CreationNumber !== payload.id) {
          // entity-changed drop: a unit other than the focused primary changed,
          // so no Properties-panel refresh is needed. Noisy → debug-level.
          flogDebug(`[entity] drop unit id=${payload.id} (not primary)`)
          return
        }
        try { primaryEntity = await GetUnit(payload.id) } catch { /* ignore */ }
      } else if (payload.kind === 'doodad') {
        if (!primaryDoodad || primaryDoodad.creation_number !== payload.id) {
          flogDebug(`[entity] drop doodad id=${payload.id} (not primary)`)
          return
        }
        const p = payload.position
        if (!p || p.length < 3) return
        primaryDoodad = { ...primaryDoodad, position: [p[0], p[1], p[2]] }
        const idx = doodads.findIndex(d => d.creation_number === payload.id)
        if (idx >= 0) {
          doodads[idx] = { ...doodads[idx], position: [p[0], p[1], p[2]] }
          doodads = doodads
        }
      }
    })
    EventsOn(SEL_EVENT, async (s: main.SelectionDTO) => {
      bumpEvent(SEL_EVENT)
      ingestSelection(s)
      const items = s.items || []
      // selection round-trip trace — silent unless Verbose Logging is on.
      flogDebug(`[selection] items=${items.length} primary=${s.primary}`)
      if (items.length === 0) {
        primaryEntity = null
        primaryDoodad = null
        return
      }
      const idx = Math.max(0, Math.min(s.primary, items.length - 1))
      const primary = items[idx]
      if (primary.kind === 'doodad') {
        primaryEntity = null
        primaryDoodad = doodads.find(d => d.creation_number === primary.id) ?? null
      } else {
        primaryDoodad = null
        try { primaryEntity = await GetUnit(primary.id) } catch { primaryEntity = null }
      }
      void scrollExplorerToPrimary(items, s.primary)
    })

    const s = await Status()
    status = s
    if (s.loaded) {
      // Initial map-already-loaded path (e.g. --open=<path> startup flag).
      // Bump the minimap-reload generation so the Minimap component (which
      // mounts after this onMount but listens on the prop reactively) picks
      // up the image without waiting for the next MAP_EVENT.
      mapLoadGen += 1
      await reloadMap()
    }
    const sel = await GetSelection()
    ingestSelection(sel)
    try { dirty = await IsDirty() } catch { dirty = false }
    await refreshHistoryState()
    EventsOn('wc3-forge:history-changed', () => {
      bumpEvent('history-changed')
      flogDebug('[history] wc3-forge:history-changed received')
      void refreshHistoryState()
    })
    // Window-close gate: Go's OnBeforeClose handler emits this event when
    // the user tries to close a dirty session. Show the confirmation modal.
    EventsOn('wc3-forge:close-requested', () => {
      bumpEvent('close-requested')
      flog('[quit-guard] close-requested fired (dirty session)')
      quitGuardOpen = true
    })
    // capture=true so we run BEFORE WebView2's default editing-shortcut
    // handlers, which can grab Ctrl+Z even with nothing focused (Chromium
    // treats the whole page as potential text edit context). Document
    // capture-phase also fires before any per-element keydown handlers.
    document.addEventListener('keydown', onGlobalKeyDown, true)
    window.addEventListener('resize', onWindowResize)
    // Test-driver hook: receives commands from Go's App.EmitTestCommand so
    // verification automation can drive UI state without needing to simulate
    // clicks (WebView2 can drop synthetic input — see memory). Subscribed
    // to the Wails event and dispatched per the simple text command format.
    EventsOn('wc3-forge:test-command', (payload: { cmd: string }) => {
      bumpEvent('test-command')
      const cmd = payload?.cmd || ''
      const [op, ...args] = cmd.trim().split(/\s+/)
      switch (op) {
        case 'diagnostics.arm': {
          // Remote arm/disarm from the diagnostics.arm MCP tool. Sets the
          // diagnostics overlay + armed probes to the requested state (idempotent).
          const want = args[0] !== 'off'
          if (want !== diagnosticsOn) toggleDiagnostics()
          break
        }
        case 'terrain.toggle':
          toggleTerrainPickMode()
          break
        case 'view.mode': {
          // Idempotent SET (from view.set_mode): args[0] is 'doodad' | 'terrain'
          // | 'region'. setEditorMode is a no-op when already in that mode.
          const m = args[0]
          if (m === 'doodad' || m === 'terrain' || m === 'region') setEditorMode(m)
          break
        }
        case 'terrain.set': {
          // Args: col row. Auto-fills info from the cached terrain DTO via
          // the picker module by simulating a click at that cell's center.
          // Simpler path: just call scene's terrain-pick on a synthetic
          // canvas-pixel center. We don't have direct cell→pixel projection
          // here, so reach into the scene's API. Easiest: skip and let the
          // user click manually; this op is a no-op in practice. For the
          // test driver we just trigger a cell via the picker directly.
          break
        }
        case 'doodad.toggle': {
          const cat = args.join(' ')
          const cur = doodadVisibility[cat]
          const next = cur === false ? true : false
          scene?.setDoodadCategoryVisible(cat, next)
          if (cat === '*') {
            const all: Record<string, boolean> = {}
            for (const c of doodadCategoriesPresent) all[c] = next
            doodadVisibility = all
          } else {
            doodadVisibility = { ...doodadVisibility, [cat]: next }
          }
          break
        }
        case 'doodad.set': {
          // Idempotent SET (from view.set_doodad_category_visible):
          // "doodad.set <true|false> <category…>". bool is first so a category
          // name containing spaces stays a single trailing token. onViewToggle
          // SETS visibility (not toggles) + handles the "*" all-categories case.
          const vis = args[0] === 'true'
          const cat = args.slice(1).join(' ')
          if (cat) onViewToggle({ category: cat, visible: vis })
          break
        }
        case 'section.toggle': {
          const id = args.join(' ')
          sectionOpen = { ...sectionOpen, [id]: !isOpen(id, true) }
          break
        }
        case 'splitter.set': {
          const pct = parseFloat(args[0])
          if (isFinite(pct) && pct > 0 && pct < 100) rightExplorerPct = pct
          break
        }
        case 'rightcol.width': {
          // Args: width in pixels. Sets + persists, same as a drag-end.
          const px = parseFloat(args[0])
          if (isFinite(px)) {
            rightColWidthPx = clampRightColWidth(px)
            onRightColWidthDragEnd()
          }
          break
        }
        case 'mapinfo.open':
          openMapInfoEditor()
          break
        case 'mapinfo.close':
          closeMapInfoEditor()
          break
        case 'objecteditor.open':
          // Optional arg: a unit FourCC to preselect (e.g. 'objecteditor.open E000').
          // Used by automated screenshots and agent-driven inspection.
          openObjectEditor(args[0] || null)
          break
        case 'objecteditor.close':
          closeObjectEditor()
          break
        case 'triggereditor.open': {
          // Optional arg: a trigger node id to preselect after opening
          // (e.g. 'triggereditor.open 42'). Numeric parse — invalid args
          // collapse to "no preselection" rather than erroring.
          const idStr = args[0]
          const id = idStr ? parseInt(idStr, 10) : NaN
          openTriggerEditor(Number.isFinite(id) ? id : null)
          break
        }
        case 'triggereditor.select': {
          const id = parseInt(args[0] ?? '', 10)
          if (Number.isFinite(id)) {
            openTriggerEditor(id)
          }
          break
        }
        case 'triggereditor.close':
          closeTriggerEditor()
          break
        case 'bridge_console.toggle':
          toggleBridgeConsole()
          break
        case 'bridge_console.open':
          bridgeConsoleOpen = true
          break
        case 'bridge_console.close':
          bridgeConsoleOpen = false
          break
        case 'minimap.toggle':
          toggleMinimap()
          break
        case 'gizmo.mode': {
          // Args: 'move' | 'rotate' | 'scale'. Drives the gizmo's mode UI +
          // scene state from automated verification (WebView2 drops some
          // synthetic input — same reason minimap.click exists).
          const m = args[0]
          if (m === 'move' || m === 'rotate' || m === 'scale') setGizmoMode(m)
          break
        }
        case 'gizmo.snap': {
          // Args: <channel> <on|off> [step]
          //   channel: 'move' | 'rotate' | 'scale'
          //   on/off:  'on' or 'off' (or 'toggle')
          //   step:    optional numeric step
          const ch = args[0] as 'move' | 'rotate' | 'scale'
          const state = args[1]
          const step = args[2] ? parseFloat(args[2]) : NaN
          if (ch !== 'move' && ch !== 'rotate' && ch !== 'scale') break
          const wantOn = state === 'on' ? true
                       : state === 'off' ? false
                       : state === 'toggle'
                         ? !(ch === 'move' ? snapMoveOn : ch === 'rotate' ? snapRotateOn : snapScaleOn)
                         : null
          if (wantOn === null) break
          if (ch === 'move') snapMoveOn = wantOn
          if (ch === 'rotate') snapRotateOn = wantOn
          if (ch === 'scale') snapScaleOn = wantOn
          if (isFinite(step) && step > 0) setSnapStep(ch, step)
          pushSnap()
          break
        }
        case 'minimap.click': {
          // Args: u v (normalized image-pixel coords, 0..1 each).
          // Synthesizes the click handler since WebView2 drops real synthetic
          // mouse input — this exposes the same panTo computation for
          // verification automation.
          const u = parseFloat(args[0])
          const v = parseFloat(args[1])
          if (isFinite(u) && isFinite(v)) {
            const fn = (window as any).__minimapClick
            if (typeof fn === 'function') fn(u, v)
          }
          break
        }
      }
    })
    // Test-driver hook (also exposed on window for in-page console use):
    ;(window as any).__app = {
      toggleTerrainPick: () => toggleTerrainPickMode(),
      setTerrainCell: (cell: TerrainCellInfo | null) => { terrainCell = cell },
      toggleDoodadCategory: (cat: string, visible: boolean) => {
        scene?.setDoodadCategoryVisible(cat, visible)
        if (cat === '*') {
          const next: Record<string, boolean> = {}
          for (const c of doodadCategoriesPresent) next[c] = visible
          doodadVisibility = next
        } else {
          doodadVisibility = { ...doodadVisibility, [cat]: visible }
        }
      },
      setSectionOpen: (id: string, open: boolean) => {
        sectionOpen = { ...sectionOpen, [id]: open }
      },
      getDoodadCategories: () => doodadCategoriesPresent,
      // Test-only: drive the Test Map button programmatically for verification
      // automation (the harness can't easily click HTML buttons via WebView2).
      launchTestMap: () => launchTestMap(),
      // Test-only: report current Test Map button state for verification.
      testMapState: () => ({ disabled: testMapDisabled, tooltip: testMapTooltip }),
    }
  })

  function onGlobalKeyDown(e: KeyboardEvent) {
    if ((e.ctrlKey || e.metaKey) && !e.shiftKey && !e.altKey && (e.key === 's' || e.key === 'S')) {
      e.preventDefault()
      void doSave()
    }
    // Undo: Ctrl+Z (no shift). Redo: Ctrl+Shift+Z (Blender/Adobe convention,
    // also matches the Cmd+Shift+Z muscle memory). We deliberately do NOT
    // bind Ctrl+Y as a Redo alias — the user explicitly asked for
    // Shift+Z-only after preferring it during evaluation.
    //
    // Skip when an input/textarea has focus so the browser's text-undo keeps
    // working. Same skip pattern the WASD-pan hotkeys use. Registered with
    // capture=true (see addEventListener call) so we run BEFORE WebView2's
    // own editing shortcuts grab Ctrl+Z/Y from the page.
    if ((e.ctrlKey || e.metaKey) && !e.altKey && (e.key === 'z' || e.key === 'Z')) {
      const tgt = e.target as HTMLElement | null
      const tagName = tgt?.tagName?.toLowerCase()
      if (tagName === 'input' || tagName === 'textarea' || tgt?.isContentEditable) return
      e.preventDefault()
      e.stopPropagation()
      flogDebug(`[hotkey] ${e.shiftKey ? 'redo' : 'undo'} canUndo=${canUndo} canRedo=${canRedo}`)
      if (e.shiftKey) void doRedo()
      else            void doUndo()
    }
    // Map Info Editor hotkey: Ctrl+Shift+I. Picked because Ctrl+I alone is
    // commonly bound (italic) and Shift differentiates without conflicting
    // with existing wc3-forge shortcuts (Ctrl+S Save).
    if ((e.ctrlKey || e.metaKey) && e.shiftKey && !e.altKey && (e.key === 'I' || e.key === 'i')) {
      e.preventDefault()
      openMapInfoEditor()
    }
    // Agent Console toggle: Ctrl+` (backtick). Doesn't collide with browser
    // shortcuts inside WebView2; not used by anything else in wc3-forge.
    // `e.key` for a literal backtick is `'`'` regardless of layout (US/UK);
    // `e.code === 'Backquote'` is the physical-key fallback for safety.
    if ((e.ctrlKey || e.metaKey) && !e.shiftKey && !e.altKey && (e.key === '`' || e.code === 'Backquote')) {
      e.preventDefault()
      toggleBridgeConsole()
    }
    // Minimap toggle hotkey: bare 'M'. No modifiers — matches the WC3 in-game
    // convention. Ignore when an input/textarea has focus so position-edit
    // typing doesn't accidentally hide the minimap, and ignore when modifiers
    // are held (so Ctrl+M / Alt+M stay free for future shortcuts).
    if ((e.key === 'm' || e.key === 'M') && !e.ctrlKey && !e.metaKey && !e.shiftKey && !e.altKey) {
      const tgt = e.target as HTMLElement | null
      const tagName = tgt?.tagName?.toLowerCase()
      if (tagName === 'input' || tagName === 'textarea' || tgt?.isContentEditable) return
      e.preventDefault()
      toggleMinimap()
    }
    // Gizmo mode hotkeys: 1=move, 2=rotate, 3=scale. (W/E/R were the
    // original Blender-style bindings; dropped per user preference for
    // the number-row form.) Bare keys only; ignore when typing in an input.
    if (!e.ctrlKey && !e.metaKey && !e.shiftKey && !e.altKey
        && (e.key === '1' || e.key === '2' || e.key === '3')) {
      const tgt = e.target as HTMLElement | null
      const tagName = tgt?.tagName?.toLowerCase()
      if (tagName === 'input' || tagName === 'textarea' || tgt?.isContentEditable) return
      const next: 'move' | 'rotate' | 'scale' =
        e.key === '1' ? 'move' : e.key === '2' ? 'rotate' : 'scale'
      e.preventDefault()
      setGizmoMode(next)
    }
    // Editor mode cycle: Tab → Doodad → Terrain → Region → (wrap); Shift+Tab
    // reverses (Blender-style mode toggling). Skip while a text field / select
    // / contenteditable has focus so Tab keeps doing normal focus traversal
    // there, and only when a map is loaded (modes are inert without one).
    if (e.key === 'Tab' && !e.ctrlKey && !e.metaKey && !e.altKey) {
      const tgt = e.target as HTMLElement | null
      const tagName = tgt?.tagName?.toLowerCase()
      if (tagName === 'input' || tagName === 'textarea' || tagName === 'select' || tgt?.isContentEditable) return
      if (!status.loaded) return
      e.preventDefault()
      const order: EditorMode[] = ['doodad', 'terrain', 'region']
      const i = order.indexOf(editorMode)
      const nextI = e.shiftKey ? (i + order.length - 1) % order.length : (i + 1) % order.length
      setEditorMode(order[nextI])
    }
    // Diagnostics overlay toggle: F9. No input-focus gate needed (function
    // keys aren't text input), no modifiers.
    if (e.key === 'F9' && !e.ctrlKey && !e.metaKey && !e.shiftKey && !e.altKey) {
      e.preventDefault()
      toggleDiagnostics()
    }
    // Editor / mode function keys — World Editor muscle memory. Function keys
    // aren't text input, so no input-focus gate; bare (no modifiers). F4 →
    // Trigger Editor, F6 → Object Editor (matches the classic WE F-key layout
    // and the shortcut labels already shown in the menu — previously those
    // labels were advertised but the keys were never wired). openTriggerEditor
    // / openObjectEditor self-gate on a loaded map.
    if (e.key === 'F4' && !e.ctrlKey && !e.metaKey && !e.shiftKey && !e.altKey) {
      e.preventDefault()
      openTriggerEditor()
    }
    if (e.key === 'F6' && !e.ctrlKey && !e.metaKey && !e.shiftKey && !e.altKey) {
      e.preventDefault()
      openObjectEditor()
    }
    // F2 / F3 / F8 switch the viewport editor mode (Doodad / Terrain / Region).
    // Like the Tab cycle, these need a loaded map (modes are inert without one).
    if ((e.key === 'F2' || e.key === 'F3' || e.key === 'F8')
        && !e.ctrlKey && !e.metaKey && !e.shiftKey && !e.altKey) {
      if (!status.loaded) return
      e.preventDefault()
      setEditorMode(e.key === 'F2' ? 'doodad' : e.key === 'F3' ? 'terrain' : 'region')
    }
    // Keyboard-shortcuts cheat sheet: '?' (Shift+/ on US layouts). Toggles the
    // overlay listing every binding. Skip while typing so '?' still types into
    // a field. Escape (handled in the overlay) also dismisses it.
    if (e.key === '?' && !e.ctrlKey && !e.metaKey && !e.altKey) {
      const tgt = e.target as HTMLElement | null
      const tagName = tgt?.tagName?.toLowerCase()
      if (tagName === 'input' || tagName === 'textarea' || tgt?.isContentEditable) return
      e.preventDefault()
      showShortcuts = !showShortcuts
    }
    // Escape closes the shortcuts overlay when it's the topmost surface.
    if (e.key === 'Escape' && showShortcuts) {
      e.preventDefault()
      showShortcuts = false
    }
    // Frame-selected hotkey: bare 'f'. Centers the camera on the current
    // selection's centroid and zooms in to fit. Matches the Blender / Maya
    // / Unity muscle memory. Same input-focus gate as 'M' so typing in the
    // Properties panel doesn't yank the camera away.
    if ((e.key === 'f' || e.key === 'F') && !e.ctrlKey && !e.metaKey && !e.shiftKey && !e.altKey) {
      const tgt = e.target as HTMLElement | null
      const tagName = tgt?.tagName?.toLowerCase()
      if (tagName === 'input' || tagName === 'textarea' || tgt?.isContentEditable) return
      e.preventDefault()
      scene?.focusSelection()
    }
    // Delete / Backspace: remove the selected units + doodads in one undo step.
    // Bare key only (no modifiers); skipped when typing in an input or when a
    // modal/editor dialog is open — those own their own Delete semantics (the
    // Trigger Editor deletes a node, Object Editor a custom row, etc.).
    // Backspace is an alias for keyboards/laptops without a dedicated Delete.
    if ((e.key === 'Delete' || e.key === 'Backspace') && !e.ctrlKey && !e.metaKey && !e.altKey && !e.shiftKey) {
      const tgt = e.target as HTMLElement | null
      const tagName = tgt?.tagName?.toLowerCase()
      if (tagName === 'input' || tagName === 'textarea' || tgt?.isContentEditable) return
      if (showObjectEditor || showTriggerEditor || showConvertToLua || showMapInfoEditor
          || showNewMap || showMcpSetup || showUpdateDialog || showWC3InstallDialog || quitGuardOpen) return
      if (selectionItems.length === 0) return
      e.preventDefault()
      void deleteSelection()
    }
  }

  // Delete every unit + doodad in the current selection as a single undo group
  // (the Go App.DeleteSelection batches them). The scene detaches the removed
  // instances and the Explorer arrays update via the entity-changed `deleted`
  // events; selection clears via the selection-changed event the delete fires.
  async function deleteSelection() {
    if (selectionItems.length === 0) return
    try {
      const n = await DeleteSelection()
      flogDebug(`[delete] removed ${n} entit${n === 1 ? 'y' : 'ies'} from selection`)
    } catch (err) {
      showToast('Delete failed: ' + String(err), 'error')
    }
  }

  function setGizmoMode(m: 'move' | 'rotate' | 'scale') {
    gizmoMode = m
    scene?.setGizmoMode(m)
  }

  async function refreshHistoryState() {
    try {
      const [u, r, list] = await Promise.all([CanUndo(), CanRedo(), HistoryList()])
      canUndo = u
      canRedo = r
      undoLabel = (list.undo && list.undo.length > 0) ? list.undo[list.undo.length - 1].label : ''
      redoLabel = (list.redo && list.redo.length > 0) ? list.redo[list.redo.length - 1].label : ''
      flog(`[history] refresh canUndo=${u} canRedo=${r} undo="${undoLabel}" redo="${redoLabel}"`)
      // Dirty may have been flipped by undo/redo too (the in-memory state
      // changed; we mark dirty defensively in the Go-side Undo/Redo path).
      // Refresh the local flag so the Save pill / OS title stay correct.
      try { dirty = await IsDirty() } catch {}
    } catch (e) {
      flog(`[history] refresh failed: ${e instanceof Error ? e.message : String(e)}`)
      canUndo = false; canRedo = false; undoLabel = ''; redoLabel = ''
    }
  }
  async function doUndo() {
    flog(`[history] doUndo called, canUndo=${canUndo}`)
    if (!canUndo) return
    try { await Undo() } catch (e) { showToast(`Undo failed: ${e instanceof Error ? e.message : String(e)}`, 'error') }
  }
  async function doRedo() {
    flog(`[history] doRedo called, canRedo=${canRedo}`)
    if (!canRedo) return
    try { await Redo() } catch (e) { showToast(`Redo failed: ${e instanceof Error ? e.message : String(e)}`, 'error') }
  }

  async function quitGuardSaveAndQuit() {
    if (quitGuardSaving) return
    quitGuardSaving = true
    try {
      // trySaveWithForcePrompt handles the external-change conflict (offers the
      // overwrite prompt + force) and the MPQ-repack failure, toasting as needed.
      // Only quit once the map is actually saved; otherwise leave the modal open
      // so the user can pick Discard, Cancel, or retry.
      if (await trySaveWithForcePrompt()) {
        quitGuardOpen = false
        await ForceQuit()
      }
    } finally {
      quitGuardSaving = false
    }
  }
  async function quitGuardDiscardAndQuit() {
    quitGuardOpen = false
    await ForceQuit()
  }
  function quitGuardCancel() {
    quitGuardOpen = false
  }

  function pushSnap() {
    scene?.setGizmoSnap({
      moveOn: snapMoveOn, moveStep: snapMoveStep,
      rotateOn: snapRotateOn, rotateStep: snapRotateStep,
      scaleOn: snapScaleOn, scaleStep: snapScaleStep,
    })
  }
  function toggleSnap(ch: 'move' | 'rotate' | 'scale') {
    if (ch === 'move') snapMoveOn = !snapMoveOn
    if (ch === 'rotate') snapRotateOn = !snapRotateOn
    if (ch === 'scale') snapScaleOn = !snapScaleOn
    pushSnap()
  }
  function setSnapStep(ch: 'move' | 'rotate' | 'scale', step: number) {
    if (!isFinite(step) || step <= 0) return
    if (ch === 'move') snapMoveStep = step
    if (ch === 'rotate') snapRotateStep = step
    if (ch === 'scale') snapScaleStep = step
    pushSnap()
  }

  // Save the map, recovering from an external-change refusal by prompting the
  // user to overwrite (then force-saving). Returns true iff the map is now saved.
  // Shared by every save entry point (Ctrl+S, Save & Quit, save-before-update)
  // so the force-overwrite prompt is offered consistently; on a declined prompt
  // or a hard failure it toasts (where appropriate) and returns false so the
  // caller can abort its flow. The marker matches ErrSourceChangedOnDisk in
  // atomic_save.go (kept stable on purpose).
  async function trySaveWithForcePrompt(): Promise<boolean> {
    try {
      await SaveMap()
      try { dirty = await IsDirty() } catch {}
      return true
    } catch (e) {
      const msg = String(e)
      if (/changed on disk since it was opened/i.test(msg)) {
        const overwrite = confirm(
          'This map was changed on disk since you opened it — another wc3-forge ' +
            'window, an agent, or you in another tool may have saved it.\n\n' +
            'Your unsaved changes are still here. The previous on-disk version ' +
            'will be backed up to a .bak file before being overwritten.\n\n' +
            'OK = overwrite with your version, Cancel = keep both (save aborted).',
        )
        if (!overwrite) return false
        try {
          await SaveMapForce()
          try { dirty = await IsDirty() } catch {}
          return true
        } catch (e2) {
          showToast('save failed: ' + String(e2), 'error')
          return false
        }
      }
      if (/MPQ repack|extract the map to a folder/i.test(msg)) {
        showToast(
          'Saving this map failed during MPQ repack. As a workaround, extract it to a folder and save that.',
          'error',
        )
        return false
      }
      showToast('save failed: ' + msg, 'error')
      return false
    }
  }

  async function doSave() {
    if (!status.loaded || saving) return
    saving = true
    try {
      await trySaveWithForcePrompt()
    } finally {
      saving = false
    }
  }

  function ingestSelection(s: main.SelectionDTO) {
    const items = s.items || []
    const u = new Set<number>()
    const d = new Set<number>()
    for (const it of items) {
      if (it.kind === 'doodad') d.add(it.id)
      // region/trigger aren't unit-or-doodad entities and carry no
      // creation_number the properties panel can resolve — routing them into the
      // unit set made the panel hang on "Loading…" forever (it awaits a unit
      // that doesn't exist). They're selected/edited via their own surfaces
      // (RegionPanel/TriggerEditor), so just keep them out of the entity sets.
      else if (it.kind === 'region' || it.kind === 'trigger') continue
      else u.add(it.id)
    }
    selectedIds = u
    selectedDoodadIds = d
    selectionItems = items
    scene?.setSelected(u, d)
  }

  // Scroll the Explorer panel to the primary (most-recent) selection so the
  // user can see which row corresponds to the entity they just clicked in the
  // viewport. Auto-opens the containing accordion section(s) when collapsed —
  // bits-ui keeps closed Content out of layout so scrollIntoView is a no-op
  // until the section is expanded.
  async function scrollExplorerToPrimary(items: main.SelectionItemDTO[], primaryIdx: number) {
    if (!items.length) return
    const idx = Math.max(0, Math.min(primaryIdx, items.length - 1))
    const primary = items[idx]
    if (!primary) return
    if (primary.kind === 'doodad') {
      const d = doodads.find(x => x.creation_number === primary.id)
      if (!d) return
      const cat = doodadCategoryFor(d) || 'Uncategorized'
      sectionOpen = { ...sectionOpen, doodads: true, [`d:${cat}`]: true }
    } else {
      const u = units.find(x => x.creation_number === primary.id)
      if (!u) return
      let groupId = 'units'
      if (u.type_id === 'sloc') {
        groupId = 'markers'
      } else {
        const info = unitTypes[u.type_id]
        const isH = info ? /Hero/i.test(info.category)
          : u.type_id.length > 0 && u.type_id[0] >= 'A' && u.type_id[0] <= 'Z'
        if (isH) groupId = 'heroes'
      }
      sectionOpen = { ...sectionOpen, [groupId]: true }
    }
    await tick()
    // Wait one more frame so the bits-ui Accordion.Content height-expand
    // transition has measured layout before we scroll.
    requestAnimationFrame(() => {
      const key = `${primary.kind}:${primary.id}`
      const el = document.querySelector(`[data-row-key="${key}"]`) as HTMLElement | null
      if (el) el.scrollIntoView({ block: 'nearest' })
    })
  }

  async function handlePick(hits: PickHit[], mode: SelectMode) {
    const next = composeSelection(selectionItems, hits, mode)
    await SetSelection(next.map(it => ({ kind: it.kind, id: it.id })))
  }

  function handleTerrainPick(cell: TerrainCellInfo | null) {
    // Update the panel state. Selection is NOT cleared — the user can keep
    // an entity selected and inspect cells side-by-side. The Properties panel
    // will display BOTH the entity properties (if any) and the cell info
    // (when set). Null cell = click missed the map; we keep the previous
    // cell info around so the user can still inspect what they last picked
    // until they pick another cell.
    if (cell) {
      terrainCell = cell
      // Mirror the pick to the 3D viewport so the user can SEE which cell
      // the panel data refers to. Persists until replaced or the mode toggles
      // off (see toggleTerrainPickMode).
      scene?.setHighlightedCell({ col: cell.col, row: cell.row })
    }
  }

  // Arm (or disarm) a doodad type for click-to-place. Called by the palette
  // when the user picks an icon (typeId) or cancels (null). Arming a type with
  // no map loaded is a no-op with a hint — placement needs a target map.
  function armDoodad(typeId: string | null) {
    if (typeId && !status.loaded) {
      showToast('Open a map before placing doodads', 'error')
      return
    }
    if (typeId) {
      onTerrainBrushChange(null) // doodad + terrain brush are exclusive
      // Doodad placement is exclusive with unit + start-location placement —
      // all three ride the scene's single placement mode. Disarm those without
      // routing back through setPlacementMode (armUnit/armStartLoc would flip it
      // off, fighting the on we're about to set).
      armedUnitType = null
      startLocArmed = false
    }
    armedDoodadType = typeId
    scene?.setPlacementMode(!!typeId)
    // Drive the cursor preview ghost: arming builds/swaps it, disarming drops
    // it (setPlacementMode(false) also drops it, so the null case is belt-and-
    // suspenders). doodadTypes is the same index the scene placed the map from.
    scene?.setPlacementGhost(typeId, doodadTypes as unknown as Record<string, any>)
  }

  // ── Unit + start-location placement wiring ───────────────────────────────
  // Mirrors armDoodad but with no preview ghost (the scene's placement ghost is
  // doodad-only; a unit/sloc just lands on click). Arming a unit disarms the
  // doodad/region/terrain/start-location modes (all share placement mode).

  // Arm (or disarm) a unit type for click-to-place. typeId null disarms.
  function armUnit(typeId: string | null) {
    if (typeId && !status.loaded) {
      showToast('Open a map before placing units', 'error')
      return
    }
    if (typeId) {
      armedDoodadType = null
      startLocArmed = false
      onTerrainBrushChange(null)
      scene?.setPlacementGhost(null, {}) // no doodad ghost for unit placement
    }
    armedUnitType = typeId
    scene?.setPlacementMode(!!typeId)
  }

  // Change the owning player slot future units are placed for.
  function setUnitPlayer(player: number) {
    armedUnitPlayer = player
  }

  // Arm (or disarm) start-location placement. The next map click drops a sloc
  // at the next free start-location index. Mutually exclusive with the other
  // placement modes.
  function armStartLoc(armed: boolean) {
    if (armed && !status.loaded) {
      showToast('Open a map before placing start locations', 'error')
      return
    }
    if (armed) {
      armedUnitType = null
      armedDoodadType = null
      onTerrainBrushChange(null)
      scene?.setPlacementGhost(null, {})
    }
    startLocArmed = armed
    scene?.setPlacementMode(armed)
  }

  // Lowest start-location index not already taken — the index a fresh sloc
  // placement claims. Start locations are conventionally dense from 0, so this
  // fills the first gap.
  function nextFreeStartLocIndex(): number {
    const used = new Set(startLocations.map((s) => s.index))
    let i = 0
    while (used.has(i)) i++
    return i
  }

  // Re-fetch the start-location list from Go (authoritative). Called on map
  // load, after a place/delete, and on undo/redo or MCP-driven changes. Feeds
  // the Unit Palette's list + the next-free-index allocator. Never writes the
  // same $state it reads.
  async function syncStartLocations() {
    try {
      const list = await ListStartLocations()
      startLocations = (list ?? []).map((s) => ({
        index: s.index,
        creationNumber: s.creation_number,
        position: [s.position[0], s.position[1], s.position[2]] as [number, number, number],
      }))
    } catch (e) {
      flogError('[startloc] list failed:', e)
    }
  }

  // Delete an existing start location by index (from the palette's list). Go
  // emits entity-changed("deleted") → the scene detaches the sloc instance;
  // we re-sync the list here.
  async function deleteStartLoc(index: number) {
    try {
      await DeleteStartLocation(index)
      await syncStartLocations()
      dirty = true
    } catch (e) {
      showToast('Delete start location failed: ' + String(e), 'error')
    }
  }

  // ── Region (rect) Editor wiring ──────────────────────────────────────────
  // The RegionPanel owns the list + numeric editors (direct Wails calls); App
  // owns the overlay sync + the viewport rect-draw tool. syncRegionOverlay
  // fetches the current regions and pushes them into the scene overlay; called
  // on map load, after any panel edit (via onRegionsChanged), and on
  // region entity-changed events from MCP/undo. Never writes the same $state it
  // reads — it only mutates the scene + bumps the refresh token.
  async function syncRegionOverlay() {
    if (!scene) return
    try {
      const list = await ListRegions()
      const rects: RegionRect[] = (list ?? []).map((r) => ({
        creationNumber: r.creation_number,
        minX: r.min_x, minY: r.min_y, maxX: r.max_x, maxY: r.max_y,
        color: [r.color[0], r.color[1], r.color[2]] as [number, number, number],
      }))
      scene.setRegionOverlay(rects)
      // Re-apply the persisted show/hide preference after every (re)load/edit.
      scene.setRegionOverlayVisible(regionOverlayVisible)
      // Drop the highlight if the selected region no longer exists.
      if (selectedRegionCN !== null && !rects.some((x) => x.creationNumber === selectedRegionCN)) {
        selectedRegionCN = null
      }
      scene.setSelectedRegion(selectedRegionCN)
    } catch (e) {
      flogError('[regions] overlay sync failed:', e)
    }
  }

  // Push the loaded map's camera bounds (war3map.w3i) into the viewport overlay.
  // Called on every map load. Robust to corner ordering via min/max. Re-applies
  // the persisted show/hide preference so the user's choice survives reloads.
  async function syncCameraBounds() {
    if (!scene) return
    try {
      if (!status.loaded) {
        scene.setCameraBounds(null)
        return
      }
      const info = await MapInfoGet()
      const lb = info?.CameraLeftBottom
      const rt = info?.CameraRightTop
      if (lb && rt) {
        scene.setCameraBounds({
          minX: Math.min(lb.X, rt.X),
          minY: Math.min(lb.Y, rt.Y),
          maxX: Math.max(lb.X, rt.X),
          maxY: Math.max(lb.Y, rt.Y),
        })
      } else {
        scene.setCameraBounds(null)
      }
      scene.setCameraBoundsVisible(cameraBoundsVisible)
    } catch (e) {
      flogError('[camera-bounds] sync failed:', e)
    }
  }

  function onSelectRegion(cn: number | null) {
    selectedRegionCN = cn
    scene?.setSelectedRegion(cn)
  }

  // Region-mode viewport handlers (registered with the scene in onMount). In
  // region mode a drag draws a new region; a click selects the one under the
  // cursor (or clears). Drawing creates on the Go side (allocates a
  // creation_number + records undo), then re-syncs the overlay and selects it.
  async function handleRegionDraw(minX: number, minY: number, maxX: number, maxY: number) {
    try {
      const name = `Region ${Date.now() % 100000}`
      const cn = await CreateRegion(name, minX, minY, maxX, maxY, '', '', [255, 255, 255])
      await syncRegionOverlay()
      onSelectRegion(cn)
      regionRefreshToken++
      dirty = true
    } catch (e) {
      showToast('Create region failed: ' + String(e), 'error')
    }
  }
  function handleRegionPick(cn: number | null) {
    onSelectRegion(cn)
    if (cn !== null) regionRefreshToken++ // nudge the panel to surface it
  }

  // Terrain Palette → scene wiring. The palette (shown only in Terrain Mode)
  // reports the armed brush (tool + size/shape/strength + selected tiles) or
  // null to disarm. We mirror the footprint to the scene's cursor and toggle
  // brush mode. The scene's mousedown prioritizes brush over the cell-inspect
  // pick, so both stay enabled in Terrain Mode: a tool selected → drag paints;
  // no tool → click inspects the cell.
  function onTerrainBrushChange(brush: TerrainBrush | null) {
    terrainBrush = brush
    if (brush) {
      // Preview the water surface only for Add Water with a fixed height — in Auto
      // mode there's no single plane (it follows the ground per stroke).
      const waterPlaneZ = brush.tool === 'water' && brush.waterFixedHeight ? brush.waterHeight : null
      scene?.setTerrainBrushShape({ radius: brush.radius, shape: brush.shape, waterPlaneZ })
      scene?.setTerrainBrushMode(true)
    } else {
      scene?.setTerrainBrushMode(false)
    }
  }

  // A terrain brush stroke: 'start' opens an undo group + paints the first dab,
  // 'paint' paints each subsequent dab as the cursor crosses cells, 'end' closes
  // the group. Calls are chained on terrainBrushChain so they apply in order
  // (BeginUndoGroup must land before any dab, EndUndoGroup after the last).
  function handleTerrainBrushStroke(cell: TerrainCellInfo | null, phase: 'start' | 'paint' | 'end') {
    // 'end' must always close an open group, even if the brush was disarmed
    // mid-stroke (so undo never wedges on a dangling group).
    if (phase === 'end') {
      if (!terrainStrokeOpen) return
      terrainStrokeOpen = false
      terrainBrushChain = terrainBrushChain.then(() => EndUndoGroup()).catch(() => {})
      return
    }
    const b = terrainBrush
    if (!b) return
    if (phase === 'start') {
      // Always open the group (even on an off-map click) so every dab in the
      // stroke is grouped; an empty group is dropped by EndUndoGroup.
      terrainStrokeOpen = true
      terrainBrushChain = terrainBrushChain.then(() => BeginUndoGroup(brushLabel(b.tool)))
      if (!cell) return
      terrainBrushChain = terrainBrushChain
        .then(async () => {
          // Flatten needs the height under the first dab as its target level.
          if (b.tool === 'flatten') {
            try { terrainFlattenTarget = (await GetTerrainTile(cell.col, cell.row)).height } catch { terrainFlattenTarget = 0 }
          }
          // Add Water locks the whole stroke to one surface height: the palette's
          // fixed value, or the auto height under the first dab (so a drag paints
          // a flat lake at the elevation where the stroke began).
          if (b.tool === 'water') {
            if (b.waterFixedHeight) {
              terrainWaterTarget = b.waterHeight
            } else {
              try { terrainWaterTarget = await WaterAddHeightAt(cell.col, cell.row) } catch { terrainWaterTarget = 128 }
            }
          }
        })
        .then(() => applyBrushDab(b, cell))
        .catch(() => {})
      return
    }
    // phase === 'paint'
    if (!cell) return
    terrainBrushChain = terrainBrushChain.then(() => applyBrushDab(b, cell)).catch(() => {})
  }

  function brushLabel(tool: TerrainBrush['tool']): string {
    if (tool === 'paint') return 'Paint terrain'
    if (tool === 'ramp' || tool === 'rampOff') return 'Edit ramp'
    if (tool === 'cliffSet') return 'Edit cliff'
    if (tool === 'water' || tool === 'waterOff') return 'Edit water'
    return 'Edit terrain height'
  }

  // Dispatch one dab to the matching Go brush. col/row are the footprint center.
  function applyBrushDab(b: TerrainBrush, cell: TerrainCellInfo): Promise<unknown> {
    const { col, row } = cell
    switch (b.tool) {
      case 'paint':
        return PaintTerrainTile(col, row, b.radius, b.shape, b.groundTileId)
      case 'raise':
        return BrushTerrainHeight(col, row, b.radius, b.shape, 'raise', b.strength, 0)
      case 'lower':
        return BrushTerrainHeight(col, row, b.radius, b.shape, 'lower', b.strength, 0)
      case 'flatten':
        return BrushTerrainHeight(col, row, b.radius, b.shape, 'flatten', 0, terrainFlattenTarget)
      case 'smooth':
        return BrushTerrainHeight(col, row, b.radius, b.shape, 'smooth', b.strength, 0)
      case 'cliffSet': {
        // The palette's level is relative to the map's default height (0); the
        // w3e layer baseline is 2, so the absolute target layer is level + 2,
        // clamped to the valid 0..15 range.
        const layer = Math.max(0, Math.min(15, b.cliffLevel + 2))
        return BrushTerrainCliff(col, row, b.radius, b.shape, 'set', layer, b.cliffTileId)
      }
      case 'ramp':
        return BrushTerrainRamp(col, row, b.radius, b.shape, true)
      case 'rampOff':
        return BrushTerrainRamp(col, row, b.radius, b.shape, false)
      case 'water':
        // terrainWaterTarget was captured at stroke start; useHeight=true so the
        // whole stroke paints one flat surface. overwrite = Fixed-height mode, so a
        // dialed level re-levels existing water; Auto preserves it.
        return BrushTerrainWater(col, row, b.radius, b.shape, 'add', terrainWaterTarget, true, b.waterFixedHeight)
      case 'waterOff':
        return BrushTerrainWater(col, row, b.radius, b.shape, 'remove', 0, false, false)
      default:
        return Promise.resolve()
    }
  }

  // Place the armed doodad at a clicked ground point. The scene gives us the
  // ground intersection (worldX/worldY) plus the terrain height (z) it sampled,
  // so the doodad lands flush with the surface. We create it on the Go side
  // (which allocates a creation_number, records undo history, marks dirty),
  // then add the rendered instance live and reflect it in our doodads array —
  // no full map reload. A null hit means the user cancelled (Escape): disarm.
  async function handlePlacementPick(hit: { worldX: number; worldY: number; z: number } | null) {
    // Start-location placement: the next click drops a sloc at the next free
    // start-location index, owned by that index (gg_start_location_<index>).
    // Stay armed for rapid placement (Escape / toggle disarms).
    if (startLocArmed) {
      if (!hit) { armStartLoc(false); return } // Escape / off-map cancel
      const index = nextFreeStartLocIndex()
      try {
        await CreateStartLocation(index, hit.worldX, hit.worldY, hit.z, 0)
        // Sloc rendering is owned by the scene's sloc renderer, which rebuilds
        // from Go's authoritative state on the entity-changed("created") full
        // reload — there's no per-cn live-add for slocs. Re-sync the palette
        // list so the new start location + the next-free index update.
        await syncStartLocations()
        dirty = true
      } catch (e) {
        showToast('Place start location failed: ' + String(e), 'error')
      }
      return
    }

    // Unit placement: mirror the doodad flow (create on Go, live-add the
    // instance, reflect in the Explorer array). Stay armed for rapid placement.
    if (armedUnitType) {
      if (!hit) { armUnit(null); return } // Escape / off-map cancel
      const utype = armedUnitType
      const player = armedUnitPlayer
      unitPlacing = true
      try {
        const cn = await CreateUnit(utype, player, hit.worldX, hit.worldY, hit.z, 0, 1)
        // Mirror what the Go session built (CreateUnit defaults: rotation 0,
        // uniform scale 1, hp/mana -1 = unit default, hero level 1).
        const dto = {
          creation_number: cn,
          type_id: utype,
          skin_id: '',
          player,
          position: [hit.worldX, hit.worldY, hit.z],
          rotation: 0,
          scale: [1, 1, 1],
          hit_points_pct: -1,
          mana_pct: -1,
          hero_level: 1,
          gold_amount: 0,
        } as unknown as main.UnitDTO
        const live = await scene?.addUnitLive(dto, unitTypes as unknown as Record<string, any>)
        units = [...units, dto]
        // If the type had no renderable MDX, addUnitLive returns false — fall
        // back to a full reload so the unit still appears (and the scene picks
        // up whatever it can). Rare for real placeable units.
        if (!live) { mapLoadGen += 1; await reloadMap({ keepCamera: true }) }
        dirty = true
      } catch (e) {
        showToast('Place unit failed: ' + String(e), 'error')
      } finally {
        unitPlacing = false
      }
      return
    }

    if (!hit) { armDoodad(null); return }
    const typeId = armedDoodadType
    if (!typeId) return
    palettePlacing = true
    try {
      const cn = await CreateDoodad(typeId, hit.worldX, hit.worldY, hit.z, 0, 1, 0)
      // Mirror what the Go session built so the live add + Explorer row match
      // (CreateDoodad defaults: rotation 0, uniform scale 1, variation 0).
      const dto = {
        creation_number: cn,
        type_id: typeId,
        skin_id: '',
        position: [hit.worldX, hit.worldY, hit.z],
        rotation: 0,
        scale: [1, 1, 1],
        variation: 0,
        life: 100,
        flags: 2,
      } as unknown as main.DoodadDTO
      await scene?.addDoodadLive(dto, doodadTypes as unknown as Record<string, any>)
      doodads = [...doodads, dto]
      // A doodad in a not-yet-present category should appear in the View menu.
      doodadCategoriesPresent = scene?.getDoodadCategories() ?? doodadCategoriesPresent
      dirty = true
    } catch (e) {
      showToast('Place doodad failed: ' + String(e), 'error')
    } finally {
      palettePlacing = false
    }
  }

  function composeSelection(
    current: main.SelectionItemDTO[],
    hits: PickHit[],
    mode: SelectMode,
  ): main.SelectionItemDTO[] {
    const key = (kind: string, id: number) => `${kind}:${id}`
    if (mode === 'set') {
      const seen = new Set<string>()
      const out: main.SelectionItemDTO[] = []
      for (const h of hits) {
        const k = key(h.kind, h.id)
        if (seen.has(k)) continue
        seen.add(k)
        out.push({ kind: h.kind, id: h.id })
      }
      return out
    }
    const map = new Map<string, main.SelectionItemDTO>()
    for (const it of current) map.set(key(it.kind, it.id), { kind: it.kind, id: it.id })
    if (mode === 'add') {
      for (const h of hits) map.set(key(h.kind, h.id), { kind: h.kind, id: h.id })
    } else {
      for (const h of hits) {
        const k = key(h.kind, h.id)
        if (map.has(k)) map.delete(k)
        else map.set(k, { kind: h.kind, id: h.id })
      }
    }
    return [...map.values()]
  }

  onDestroy(() => {
    if (diagReportTimer) clearInterval(diagReportTimer)
    EventsOff(SEL_EVENT)
    EventsOff(MAP_EVENT)
    EventsOff(DIRTY_EVENT)
    EventsOff(ENTITY_EVENT)
    EventsOff(DEV_ANIM_EVENT)
    document.removeEventListener('keydown', onGlobalKeyDown, true)
    window.removeEventListener('resize', onWindowResize)
    if (systemMQ) {
      try { systemMQ.removeEventListener('change', onSystemThemeChange) } catch {}
      systemMQ = null
    }
    scene?.dispose()
  })

  async function newWindow() {
    busy = true
    try {
      await NewWindow()
    } catch (e) {
      showToast('new window failed: ' + String(e), 'error')
    } finally {
      busy = false
    }
  }

  async function pickAndOpen() {
    busy = true
    try {
      const path = await OpenMapDialog()
      if (!path) { busy = false; return }
      if (status.loaded) {
        await OpenMapInNewWindow(path)
        return
      }
      status = await OpenMap(path)
      await reloadMap()
    } catch (e) {
      showToast('open failed: ' + String(e), 'error')
    } finally {
      busy = false
    }
  }

  async function pickAndOpenFile() {
    busy = true
    try {
      const path = await OpenMapFileDialog()
      if (!path) { busy = false; return }
      if (status.loaded) {
        await OpenMapInNewWindow(path)
        return
      }
      status = await OpenMap(path)
      await reloadMap()
    } catch (e) {
      showToast('open failed: ' + String(e), 'error')
    } finally {
      busy = false
    }
  }

  async function reloadMap(opts?: { keepCamera?: boolean }) {
    // A fresh/reloaded map invalidates any armed placement brush — the armed
    // type may not even exist in the new map's catalog. Disarm defensively.
    armedDoodadType = null
    armedUnitType = null
    startLocArmed = false
    scene?.setPlacementMode(false)
    units = await ListUnits()
    doodads = await ListDoodads()
    void syncStartLocations() // refresh the Unit Palette's start-location list
    try { unitTypes = (await GetUnitTypeIndex()) as unknown as Record<string, UnitTypeInfo> } catch { unitTypes = {} }
    try { doodadTypes = (await GetDoodadTypeIndex()) as unknown as Record<string, DoodadTypeInfo> } catch { doodadTypes = {} }
    await scene?.loadMap(opts)
    // Pull the present-categories list from the scene (populated during
    // placeDoodad). The View menu shows ONLY categories present in the map.
    doodadCategoriesPresent = scene?.getDoodadCategories() ?? []
    // Drop terrain-cell info for a new map — the old (col, row) refers to
    // a map that's no longer loaded. Scene-side highlight is also cleared via
    // loadMap's internal setCell(null) (see scene-instances.ts), but call
    // explicitly so a keep-camera reload also clears the stale wireframe.
    terrainCell = null
    scene?.setHighlightedCell(null)
    const apply = (window as any).__applyStartupCamera
    if (typeof apply === 'function') apply()
  }

  function togglePathing() {
    pathingVisible = !pathingVisible
    scene?.setPathingVisible(pathingVisible)
  }
  function toggleSlocs() {
    slocsVisible = !slocsVisible
    scene?.setSlocsVisible(slocsVisible)
  }
  // View → Overlays sub-menu dispatches `overlay-toggle` events. Route per id
  // to the relevant scene toggle. Add new overlay ids here as the menu's
  // OVERLAYS list in ViewMenu.svelte grows.
  function onOverlayToggle(detail: { overlay: string; visible: boolean }) {
    const { overlay, visible } = detail
    if (overlay === 'pathing') {
      pathingVisible = visible
      scene?.setPathingVisible(pathingVisible)
    }
    if (overlay === 'camerabounds') {
      cameraBoundsVisible = visible
      try { localStorage.setItem(CAMERA_BOUNDS_LS_KEY, cameraBoundsVisible ? '1' : '0') } catch {}
      scene?.setCameraBoundsVisible(cameraBoundsVisible)
    }
  }
  // View → top-level "extras" dispatches `extra-toggle` events. Route per id
  // to the existing toggle handler (reforged, agent-console, etc.) so the
  // menu and any header / hotkey path end up at the same code.
  function onExtraToggle(detail: { id: string; checked: boolean }) {
    const { id, checked } = detail
    if (id === 'reforged') {
      // toggleReforged flips internally; only call when the desired state
      // differs from current so the menu can't get out of sync with `busy`.
      if (checked !== reforged) toggleReforged()
    } else if (id === 'minimap') {
      if (checked !== showMinimap) toggleMinimap()
    } else if (id === 'agent-console') {
      if (checked !== bridgeConsoleOpen) toggleBridgeConsole()
    } else if (id === 'diagnostics') {
      if (checked !== diagnosticsOn) toggleDiagnostics()
    } else if (id === 'verbose-log') {
      if (checked !== verboseLogging) toggleVerboseLogging()
    }
  }

  // Single source of truth for the editor's top-level mode. Encapsulates the
  // leave-prev / enter-next side effects so the viewport's click pipeline never
  // has two modes fighting over it (placement/terrain/region are exclusive).
  function setEditorMode(next: EditorMode) {
    if (next === editorMode) return
    const prev = editorMode
    // ── Leave the previous mode ──
    if (prev === 'terrain') {
      terrainCell = null
      // Disarm any armed terrain brush (its palette unmounts) + drop the cell
      // highlight so the scene exits brush mode cleanly.
      if (terrainBrush) onTerrainBrushChange(null)
      scene?.setHighlightedCell(null)
      scene?.setTerrainPickMode(false)
    } else if (prev === 'region') {
      onSelectRegion(null)
      scene?.setRegionMode(false)
    } else {
      // Leaving Doodad mode: disarm any in-progress placement so it can't
      // swallow clicks meant for the new mode.
      if (armedDoodadType) armDoodad(null)
      if (armedUnitType) armUnit(null)
      if (startLocArmed) armStartLoc(false)
    }
    editorMode = next
    // ── Enter the next mode ──
    if (next === 'terrain') {
      scene?.setTerrainPickMode(true)
    } else if (next === 'region') {
      scene?.setRegionMode(true)
    }
  }

  // Back-compat shim: the bare hotkey + the MCP test hook still toggle between
  // Terrain and Doodad. Region is entered explicitly via setEditorMode('region').
  function toggleTerrainPickMode() {
    setEditorMode(editorMode === 'terrain' ? 'doodad' : 'terrain')
  }

  function onViewToggle(detail: { category: string; visible: boolean }) {
    const { category, visible } = detail
    scene?.setDoodadCategoryVisible(category, visible)
    if (category === '*') {
      const next: Record<string, boolean> = {}
      for (const c of doodadCategoriesPresent) next[c] = visible
      doodadVisibility = next
    } else {
      doodadVisibility = { ...doodadVisibility, [category]: visible }
    }
  }

  async function toggleReforged() {
    if (busy) return
    busy = true
    try {
      const next = !reforged
      reforged = await SetReforgedMode(next)
      scene?.setReforgedMode(reforged)
      if (status.loaded) {
        await reloadMap({ keepCamera: true })
      }
    } catch (e) {
      showToast('toggle reforged failed: ' + String(e), 'error')
    } finally {
      busy = false
    }
  }

  async function close() {
    busy = true
    try {
      status = await CloseMap()
      units = []
      doodads = []
      selectedIds = new Set()
      selectedDoodadIds = new Set()
      selectionItems = []
      primaryEntity = null
      primaryDoodad = null
      terrainCell = null
    } finally {
      busy = false
    }
  }

  async function clickRow(e: MouseEvent, kind: 'unit' | 'doodad', id: number) {
    const mode: SelectMode = e.ctrlKey || e.metaKey
      ? 'toggle'
      : (e.shiftKey ? 'add' : 'set')
    const next = composeSelection(selectionItems, [{ kind, id }], mode)
    await SetSelection(next.map(it => ({ kind: it.kind, id: it.id })))
  }

  function panToEntity(e: Event, pos: number[]) {
    e.stopPropagation()
    if (pos && pos.length >= 2) {
      scene?.panTo(pos[0], pos[1])
    }
  }

  // ----- Explorer categorization -----

  type Group = { id: string; label: string; entries: main.UnitDTO[] }
  function unitDisplayName(u: main.UnitDTO): string {
    // Slocs are start-location markers, not real units — there's no
    // UnitData.slk row for them so the type-index has no entry. Without
    // this branch every Markers row in the Explorer reads the raw "sloc"
    // FourCC with zero per-player differentiation. Resolve to a humanised
    // "Start Location (Red)" label using the existing player palette.
    if (u.type_id === 'sloc') {
      return `Start Location (${playerColorName(u.player)})`
    }
    const info = unitTypes[u.type_id]
    return info && info.name ? info.name : u.type_id
  }
  function unitCategory(u: main.UnitDTO): string {
    if (u.type_id === 'sloc') return `Player ${u.player}`
    const info = unitTypes[u.type_id]
    return info ? info.category : ''
  }
  function bucket(us: main.UnitDTO[], types: Record<string, UnitTypeInfo>): Group[] {
    const markers: main.UnitDTO[] = []
    const heroes: main.UnitDTO[] = []
    const others: main.UnitDTO[] = []
    for (const u of us) {
      if (u.type_id === 'sloc') { markers.push(u); continue }
      const info = types[u.type_id]
      const isHero = info ? /Hero/i.test(info.category)
        : u.type_id.length > 0 && u.type_id[0] >= 'A' && u.type_id[0] <= 'Z'
      if (isHero) heroes.push(u)
      else others.push(u)
    }
    const out: Group[] = []
    if (heroes.length) out.push({ id: 'heroes', label: 'Heroes', entries: heroes })
    if (others.length) out.push({ id: 'units', label: 'Units & Items', entries: others })
    if (markers.length) out.push({ id: 'markers', label: 'Markers', entries: markers })
    return out
  }


  // ----- Doodad explorer grouping -----
  type DGroup = { id: string; label: string; entries: main.DoodadDTO[] }
  function doodadDisplayName(d: main.DoodadDTO): string {
    const info = doodadTypes[d.type_id]
    return info && info.name ? info.name : d.type_id
  }
  function doodadCategoryFor(d: main.DoodadDTO): string {
    const info = doodadTypes[d.type_id]
    return info ? info.category : ''
  }
  const DOODAD_CAT_ORDER = [
    'Trees/Destructibles',
    'Structures',
    'Props',
    'Bridges/Ramps',
    'Cliff/Terrain',
    'Terrain',
    'Water',
    'Environment',
    'Pathing Blockers',
    'Cinematic',
  ]
  function bucketDoodads(ds: main.DoodadDTO[], types: Record<string, DoodadTypeInfo>): DGroup[] {
    const buckets = new Map<string, main.DoodadDTO[]>()
    for (const d of ds) {
      const info = types[d.type_id]
      const cat = (info && info.category) ? info.category : 'Uncategorized'
      let arr = buckets.get(cat)
      if (!arr) { arr = []; buckets.set(cat, arr) }
      arr.push(d)
    }
    const out: DGroup[] = []
    for (const label of DOODAD_CAT_ORDER) {
      const arr = buckets.get(label)
      if (arr && arr.length) {
        out.push({ id: 'd:' + label, label, entries: arr })
        buckets.delete(label)
      }
    }
    const rest = [...buckets.keys()].sort((a, b) => {
      if (a === 'Uncategorized') return 1
      if (b === 'Uncategorized') return -1
      return a.localeCompare(b)
    })
    for (const label of rest) {
      out.push({ id: 'd:' + label, label, entries: buckets.get(label)! })
    }
    return out
  }

  // ----- Properties helpers -----

  // Asset-preview model path. Resolves the primary selection's MDX path from
  // the SLK type index (unitTypes / doodadTypes). Mirrors the path-picking
  // logic in scene-instances.ts:
  //   - Append .mdx if no extension declared.
  //   - For multi-variant doodads (num_var > 1), use the doodad's variation
  //     index as a suffix (ATtr0.mdx etc.). Single-variant doodads use the
  //     unsuffixed path.
  //   - Returns a PRIMARY path + ordered FALLBACK list. The AssetPreview
  //     walks them in order until one loads — same as placeDoodad's variant
  //     → unsuffixed → other-extension chain in scene-instances.ts. Without
  //     the fallbacks, doodad rows whose SLK declares N variants but whose
  //     CASC ships only the unsuffixed file (e.g. "Statue 1" on Enfo's FFB
  //     → ANst.mdx exists, ANst1.mdx 404s) surface as "load failed".
  // Slocs (type_id === 'sloc') intentionally return null — they're editor-
  // only markers with no model file.
  function mdxPathFor(file: string): string {
    if (/\.(mdl|mdx)$/i.test(file)) return file
    return file + '.mdx'
  }
  interface PreviewPaths {
    primary: string
    fallbacks: string[]
  }

  function fmt(n: number, decimals: number = 0): string {
    return n.toFixed(decimals)
  }
  function fmtVec3(v: number[]): string {
    return `(${fmt(v[0])}, ${fmt(v[1])}, ${fmt(v[2])})`
  }
  function fmtScale(v: number[]): string {
    return `(${fmt(v[0], 2)}, ${fmt(v[1], 2)}, ${fmt(v[2], 2)})`
  }
  function playerLabel(p: number): string {
    if (p < PLAYER_COLOR_NAMES.length) return `${PLAYER_COLOR_NAMES[p]} (${p})`
    const neutral = neutralName(p)
    if (neutral) return `${neutral} (${p})`
    return `Player ${p}`
  }
  // playerOptions enumerates the selectable owning-player slots for the
  // Identity player picker: the 24 color slots plus the conventional neutral
  // slots. If the current entity's player is outside this set (unusual but
  // possible in hand-edited maps) it's appended so the <select> can still show
  // the truth without silently snapping to a different value.
  function playerOptions(): { value: number; label: string }[] {
    const opts: { value: number; label: string }[] = []
    for (let p = 0; p < 24; p++) opts.push({ value: p, label: playerLabel(p) })
    opts.push({ value: 24, label: 'Neutral Hostile (24)' })
    opts.push({ value: 25, label: 'Neutral Passive (25)' })
    opts.push({ value: 26, label: 'Neutral Extra (26)' })
    opts.push({ value: 27, label: 'Neutral Victim (27)' })
    const cur = primaryEntity?.Player
    if (cur !== undefined && !opts.some(o => o.value === cur)) {
      opts.push({ value: cur, label: `Player ${cur}` })
    }
    return opts
  }
  function playerColorName(p: number): string {
    if (p < PLAYER_COLOR_NAMES.length) return PLAYER_COLOR_NAMES[p]
    return neutralName(p) ?? `P${p}`
  }
  function teamColorCSS(p: number): string {
    const rgb = p < TEAM_COLORS_RGB.length ? TEAM_COLORS_RGB[p] : [0.55, 0.15, 0.15]
    const r = Math.round(rgb[0] * 255)
    const g = Math.round(rgb[1] * 255)
    const b = Math.round(rgb[2] * 255)
    return `rgb(${r}, ${g}, ${b})`
  }
  // Resolve the icon asset path for an entity. Returns "" when no icon
  // exists in the type index (custom doodads with no category, items that
  // didn't get a hit on ItemFunc.txt, slocs which are editor markers).
  function unitIconPath(u: main.UnitDTO): string {
    const info = unitTypes[u.type_id]
    return info ? info.icon_art : ''
  }
  function doodadIconPath(d: main.DoodadDTO): string {
    const info = doodadTypes[d.type_id]
    return info ? info.icon_art : ''
  }
  function isHero(e: unitsdoo.Entity): boolean {
    return e.HeroLevel > 0 || (e.TypeID.length > 0 && e.TypeID[0] >= 'A' && e.TypeID[0] <= 'Z')
  }


  let posEdit: { x: string; y: string; z: string } = $state({ x: '', y: '', z: '' })

  function primaryPosition(): [number, number, number] | null {
    if (primaryEntity) return [primaryEntity.Position[0], primaryEntity.Position[1], primaryEntity.Position[2]]
    if (primaryDoodad) return [primaryDoodad.position[0], primaryDoodad.position[1], primaryDoodad.position[2]]
    return null
  }

  async function commitPositionEdit(axis: 'x' | 'y' | 'z') {
    const primary = selectionItems[0]
    if (!primary) return
    const cn = primary.id
    const next = {
      x: parseFloat(posEdit.x),
      y: parseFloat(posEdit.y),
      z: parseFloat(posEdit.z),
    }
    if (!Number.isFinite(next[axis])) {
      const truth = primaryPosition()
      if (truth) posEdit = { x: fmt(truth[0]), y: fmt(truth[1]), z: fmt(truth[2]) }
      return
    }
    try {
      if (primary.kind === 'doodad') {
        await MoveDoodad(cn, next.x, next.y, next.z)
      } else {
        await MoveUnit(cn, next.x, next.y, next.z)
      }
    } catch (e) {
      console.error('Move failed:', e)
      showToast('move failed: ' + String(e), 'error')
      const truth = primaryPosition()
      if (truth) posEdit = { x: fmt(truth[0]), y: fmt(truth[1]), z: fmt(truth[2]) }
    }
  }

  function onPosKeydown(e: KeyboardEvent, axis: 'x' | 'y' | 'z') {
    if (e.key === 'Enter') {
      ;(e.currentTarget as HTMLInputElement).blur()
    } else if (e.key === 'Escape') {
      e.stopPropagation()
      const truth = primaryPosition()
      if (truth) {
        const idx = axis === 'x' ? 0 : axis === 'y' ? 1 : 2
        posEdit = { ...posEdit, [axis]: fmt(truth[idx]) }
      }
      ;(e.currentTarget as HTMLInputElement).blur()
    }
  }

  // ----- Per-instance unit field editing (Status / Hero / Identity rows) -----
  //
  // Mirrors the posEdit pattern: a local string buffer (`unitFieldEdit`) holds
  // the in-progress text so typing doesn't fight the entity-changed re-fetch,
  // and edits commit explicitly on blur/Enter (NOT per keystroke — see the
  // documented editor-autosave focus-loss footgun). Revert-to-truth on Escape
  // or an invalid value. The buffer is re-seeded from primaryEntity whenever the
  // focused unit changes (the run_2 effect below).
  //
  // Every scalar field flows through SetUnitInstanceField; item drops are
  // committed as a whole list via SetUnitInstanceItemDrops. Both are undo-aware
  // session mutators and fire entity-changed, so the panel re-fetches the truth
  // (via the existing GetUnit-on-entity-changed handler) after a commit.

  // 'unitFieldEditableField' is the set of scalar fields the panel edits, keyed
  // by the wire field name SetUnitInstanceField expects.
  type UnitScalarField =
    | 'gold' | 'hp_pct' | 'mana_pct' | 'player'
    | 'hero_level' | 'hero_str' | 'hero_agi' | 'hero_int'
    | 'target_acquisition' | 'custom_color'

  let unitFieldEdit: Record<UnitScalarField, string> = $state({
    gold: '', hp_pct: '', mana_pct: '', player: '',
    hero_level: '', hero_str: '', hero_agi: '', hero_int: '',
    target_acquisition: '', custom_color: '',
  })

  // The field whose input currently has focus. Re-seeding from truth (on
  // undo/redo or another agent's edit landing) skips this one field so we never
  // overwrite text the user is actively typing — the explicit-commit half of
  // the editor-autosave footgun fix.
  let focusedUnitField: UnitScalarField | null = $state(null)

  // Item-drop working buffer: the list the user is editing (committed as a
  // whole). Each row is {item_id, chance} strings; parsed on commit.
  let itemDropEdit: { item_id: string; chance: string }[] = $state([])

  // Snapshot the current per-instance scalar truth from primaryEntity, for
  // re-seeding the buffer + reverting bad/cancelled edits.
  function unitFieldTruth(field: UnitScalarField): string {
    const e = primaryEntity
    if (!e) return ''
    switch (field) {
      case 'gold': return String(e.GoldAmount)
      case 'hp_pct': return String(e.HitPointsPct)
      case 'mana_pct': return String(e.ManaPct)
      case 'player': return String(e.Player)
      case 'hero_level': return String(e.HeroLevel || 1)
      case 'hero_str': return String(e.HeroStr)
      case 'hero_agi': return String(e.HeroAgi)
      case 'hero_int': return String(e.HeroInt)
      case 'target_acquisition': return String(e.TargetAcquisition)
      case 'custom_color': return String(e.CustomColor)
    }
  }

  // itemDropFocused is true while any item-drop row input has focus — re-seeding
  // skips the whole list then, so a row the user is editing isn't yanked out.
  let itemDropFocused = $state(false)

  function seedUnitFieldBuffer() {
    const e = primaryEntity
    if (!e || e.TypeID === 'sloc') return
    const next: Record<UnitScalarField, string> = {
      gold: unitFieldTruth('gold'),
      hp_pct: unitFieldTruth('hp_pct'),
      mana_pct: unitFieldTruth('mana_pct'),
      player: unitFieldTruth('player'),
      hero_level: unitFieldTruth('hero_level'),
      hero_str: unitFieldTruth('hero_str'),
      hero_agi: unitFieldTruth('hero_agi'),
      hero_int: unitFieldTruth('hero_int'),
      target_acquisition: unitFieldTruth('target_acquisition'),
      custom_color: unitFieldTruth('custom_color'),
    }
    // Preserve the field the user is actively typing in (avoid clobber).
    if (focusedUnitField !== null) next[focusedUnitField] = unitFieldEdit[focusedUnitField]
    unitFieldEdit = next
    if (!itemDropFocused) {
      itemDropEdit = (e.ItemDrops ?? []).map(d => ({ item_id: d.ItemID, chance: String(d.Chance) }))
    }
  }

  async function commitUnitField(field: UnitScalarField) {
    const e = primaryEntity
    if (!e || e.TypeID === 'sloc') return
    const cn = e.CreationNumber
    const raw = unitFieldEdit[field]
    const v = parseFloat(raw)
    if (!Number.isFinite(v)) {
      unitFieldEdit = { ...unitFieldEdit, [field]: unitFieldTruth(field) }
      return
    }
    try {
      await SetUnitInstanceField(cn, field, v)
    } catch (err) {
      console.error('SetUnitInstanceField failed:', err)
      showToast('edit failed: ' + String(err), 'error')
      unitFieldEdit = { ...unitFieldEdit, [field]: unitFieldTruth(field) }
    }
  }

  function onUnitFieldKeydown(ev: KeyboardEvent, field: UnitScalarField) {
    if (ev.key === 'Enter') {
      ;(ev.currentTarget as HTMLInputElement).blur()
    } else if (ev.key === 'Escape') {
      ev.stopPropagation()
      unitFieldEdit = { ...unitFieldEdit, [field]: unitFieldTruth(field) }
      ;(ev.currentTarget as HTMLInputElement).blur()
    }
  }

  // Player picker commits immediately on change (a <select>, not free text — no
  // partial-typing concern, so no blur dance needed).
  async function commitUnitPlayer(ev: Event) {
    const e = primaryEntity
    if (!e || e.TypeID === 'sloc') return
    const v = parseInt((ev.currentTarget as HTMLSelectElement).value, 10)
    if (!Number.isFinite(v)) return
    try {
      await SetUnitInstanceField(e.CreationNumber, 'player', v)
    } catch (err) {
      console.error('set player failed:', err)
      showToast('set player failed: ' + String(err), 'error')
    }
  }

  // Item-drop list edits. The working buffer commits as a whole on demand
  // (add/remove row, or blur of a row input). Rows with a non-4-char item_id
  // are dropped on commit (the backend would reject them); the buffer is then
  // re-seeded from truth via the entity-changed re-fetch.
  function addItemDropRow() {
    itemDropEdit = [...itemDropEdit, { item_id: '', chance: '100' }]
  }
  function removeItemDropRow(idx: number) {
    itemDropEdit = itemDropEdit.filter((_, i) => i !== idx)
    void commitItemDrops()
  }
  async function commitItemDrops() {
    const e = primaryEntity
    if (!e || e.TypeID === 'sloc') return
    const drops = itemDropEdit
      .map(r => ({ item_id: r.item_id.trim(), chance: parseInt(r.chance, 10) }))
      .filter(r => r.item_id.length === 4 && Number.isFinite(r.chance) && r.chance >= 0)
    try {
      await SetUnitInstanceItemDrops(e.CreationNumber, drops)
    } catch (err) {
      console.error('set item drops failed:', err)
      showToast('set drops failed: ' + String(err), 'error')
      itemDropEdit = (primaryEntity?.ItemDrops ?? []).map(d => ({ item_id: d.ItemID, chance: String(d.Chance) }))
    }
  }

  // Terrain-cell info formatting. `rampFlags` is a bitfield: bit 0 = ramp,
  // bit 1 = boundary; rendered as a human-readable list for the Properties
  // panel to surface what's actually set.
  function rampFlagsLabel(rf: number): string {
    if (!rf) return 'none'
    const parts: string[] = []
    if (rf & 0x01) parts.push('ramp')
    if (rf & 0x02) parts.push('boundary')
    return parts.join(', ') || `0x${rf.toString(16)}`
  }
  function shadowLabel(s: number): string {
    if (s < 0) return '(no shadow map)'
    return s >= 0x80 ? `${s} (shadowed)` : `${s} (lit)`
  }
  // ----- Test Map button (launch WC3 with the current map preloaded) -----
  //
  // Mirrors HiveWE's ribbon `Test Map` action. Constraints (see
  // app.go::LaunchInWC3 -> forge.Session.TestMap):
  //   - Session must be loaded.
  //   - Session must NOT be dirty — save first so the launched map reflects
  //     the current edits (folder maps save in place; MPQ maps repack).
  //   - .w3x/.w3m/.mpq archives launch directly; FOLDER-backed sessions are
  //     packaged into a sibling out/<map>.w3x on launch (WC3 can't open a
  //     loose folder), so the button is enabled for them too.
  let testMapLoaded = $derived(status.loaded && !!status.path)
  let testMapIsFile = $derived((() => {
    const p = (status.path || '').toLowerCase()
    return /\.(w3x|w3m|mpq)$/.test(p)
  })())
  // Folder-backed = loaded with a path that isn't a packaged-archive extension.
  let testMapIsFolder = $derived(testMapLoaded && !testMapIsFile)
  let testMapDisabled = $derived(!testMapLoaded || dirty || launching)
  let testMapTooltip = $derived((() => {
    if (!testMapLoaded) return 'Open a map first'
    if (dirty) return 'Save changes first, then launch'
    if (launching) return 'Launching…'
    if (testMapIsFolder) return 'Package this folder and launch in WC3'
    return 'Launch Warcraft III with this map (-loadfile)'
  })())
  let groups = $derived(bucket(units, unitTypes))
  let doodadCount = $derived(doodads.length)
  let doodadGroups = $derived(bucketDoodads(doodads, doodadTypes))
  let previewPaths = $derived(((): PreviewPaths | null => {
    if (primaryEntity) {
      if (primaryEntity.TypeID === 'sloc') return null
      const info = unitTypes[primaryEntity.TypeID]
      if (!info || !info.file) return null
      const extMatch = info.file.match(/\.(mdl|mdx)$/i)
      const declaredExt = extMatch ? extMatch[0] : '.mdx'
      const stem = extMatch ? info.file.slice(0, -extMatch[0].length) : info.file
      const otherExt = declaredExt.toLowerCase() === '.mdx' ? '.mdl' : '.mdx'
      return {
        primary: mdxPathFor(info.file),
        fallbacks: [stem + otherExt],
      }
    }
    if (primaryDoodad) {
      const info = doodadTypes[primaryDoodad.type_id]
      if (!info || !info.file) return null
      const extMatch = info.file.match(/\.(mdl|mdx)$/i)
      const declaredExt = extMatch ? extMatch[0] : '.mdx'
      const stem = extMatch ? info.file.slice(0, -extMatch[0].length) : info.file
      const otherExt = declaredExt.toLowerCase() === '.mdx' ? '.mdl' : '.mdx'
      let primary: string
      const fallbacks: string[] = []
      if (info.num_var > 1) {
        const variantIdx = Math.min(Math.max(0, primaryDoodad.variation), info.num_var - 1)
        primary = stem + variantIdx + declaredExt
        // unsuffixed with declared extension — covers SLK-declares-N-but-
        // only-the-base-file-shipped cases (Statue 1).
        fallbacks.push(stem + declaredExt)
        // unsuffixed with the OTHER extension — custom maps often declare
        // .mdl but ship .mdx (or vice versa).
        fallbacks.push(stem + otherExt)
        // last resort: variant + other extension
        fallbacks.push(stem + variantIdx + otherExt)
      } else {
        primary = stem + declaredExt
        fallbacks.push(stem + otherExt)
      }
      return { primary, fallbacks }
    }
    return null
  })())
  let previewModelPath = $derived(previewPaths?.primary ?? null)
  let previewModelFallbacks = $derived(previewPaths?.fallbacks ?? [])
  let singlePositionEditable = (
    $derived(selectionItems.length === 1 &&
    (
      (selectionItems[0].kind === 'unit' && primaryEntity !== null) ||
      (selectionItems[0].kind === 'doodad' && primaryDoodad !== null)
    ))
  )
  run_1(() => {
    if (singlePositionEditable) {
      const pos = primaryEntity ? primaryEntity.Position : (primaryDoodad ? primaryDoodad.position : null)
      if (pos) {
        posEdit = {
          x: fmt(pos[0]),
          y: fmt(pos[1]),
          z: fmt(pos[2]),
        }
      }
    }
  });
  // Re-seed the per-instance field buffer when the FOCUSED unit changes
  // (selecting a different unit). We deliberately key on the creation number,
  // not on primaryEntity identity: the entity-changed handler re-fetches
  // primaryEntity after every commit, and re-seeding on each of those would
  // clobber a value the user is mid-typing in an adjacent field. Keying on the
  // CN means we re-seed on selection change + undo/redo-of-a-different-unit
  // only. Within the same focused unit, the buffer already mirrors the
  // committed truth (commit writes the same value back), so no re-seed needed.
  run_1(() => {
    const e = primaryEntity
    if (!e || e.TypeID === 'sloc') {
      itemDropEdit = []
      return
    }
    // Read the editable truth so this effect re-runs when the focused unit's
    // values change — on selection change (a new CN) AND on undo/redo or an
    // MCP/another-surface edit landing (the entity-changed handler re-fetches
    // primaryEntity, mutating these reactive fields). Same dependency-tracking
    // trick the posEdit effect uses for Position. seedUnitFieldBuffer skips the
    // actively-focused field/list, so live typing survives the re-fetch — the
    // explicit-commit, no-clobber half of the editor-autosave footgun fix.
    void [e.CreationNumber, e.GoldAmount, e.HitPointsPct, e.ManaPct, e.Player,
          e.HeroLevel, e.HeroStr, e.HeroAgi, e.HeroInt,
          e.TargetAcquisition, e.CustomColor, e.ItemDrops]
    seedUnitFieldBuffer()
  });
</script>

<main class="flex h-screen flex-col bg-background text-foreground">
  <header class="flex h-11 flex-none items-center gap-1 border-b border-border bg-card px-2">
    <DropdownMenu.Root bind:open={fileMenuOpen}>
      <DropdownMenu.Trigger>
        {#snippet child({ props })}
          <Button {...props} variant="ghost" size="sm" title="File menu">
            File
            <ChevronDownIcon class="text-muted-foreground" />
          </Button>
        {/snippet}
      </DropdownMenu.Trigger>
      <DropdownMenu.Content class="min-w-[220px]" align="start">
        <DropdownMenu.Item
          onSelect={runMenuAction(openNewMap)}
          disabled={busy || newMapBusy}
          title="Create a new blank map (flat terrain, no units or doodads)."
        >
          <span class="flex-1">New Map…</span>
        </DropdownMenu.Item>
        <DropdownMenu.Item
          onSelect={runMenuAction(newWindow)}
          disabled={busy}
          title="Launch a new wc3-forge editor window with no map loaded."
        >
          <span class="flex-1">New Window</span>
        </DropdownMenu.Item>
        <DropdownMenu.Separator />
        <DropdownMenu.Item
          onSelect={runMenuAction(pickAndOpen)}
          disabled={busy}
          title="Open an extracted .w3x folder (editable)."
        >
          <span class="flex-1">Open Map Folder</span>
        </DropdownMenu.Item>
        <DropdownMenu.Item
          onSelect={runMenuAction(pickAndOpenFile)}
          disabled={busy}
          title="Open a packaged .w3x / .w3m / .mpq archive (read-only; Save is disabled)."
        >
          <span class="flex-1">Open Map File</span>
        </DropdownMenu.Item>
        <DropdownMenu.Item
          onSelect={runMenuAction(doSave)}
          disabled={!status.loaded || !dirty || saving}
          title="Save pending edits (Ctrl+S)."
        >
          <span class="flex-1 {dirty ? 'text-amber-400' : ''}">Save{dirty ? ' •' : ''}</span>
          <DropdownMenu.Shortcut>Ctrl+S</DropdownMenu.Shortcut>
        </DropdownMenu.Item>
        <DropdownMenu.Separator />
        <DropdownMenu.Item
          onSelect={runMenuAction(openMapInfoEditor)}
          disabled={!status.loaded}
          title="Edit map name, author, description, and other metadata."
        >
          <span class="flex-1">Map Info</span>
          <DropdownMenu.Shortcut>Ctrl+Shift+I</DropdownMenu.Shortcut>
        </DropdownMenu.Item>
        <DropdownMenu.Item
          onSelect={runMenuAction(openSwapTileset)}
          disabled={!status.loaded}
          title="Swap the map's tileset, remapping each tile to a slot in the new palette."
        >
          <span class="flex-1">Swap Tileset</span>
        </DropdownMenu.Item>
        <DropdownMenu.Item
          onSelect={runMenuAction(openImportModel)}
          disabled={!status.loaded}
          title="Convert an external .obj / .gltf / .glb / .stl mesh to a WC3 .mdx and import it into the current map."
        >
          <span class="flex-1">Import 3D Model…</span>
        </DropdownMenu.Item>
        <DropdownMenu.Item
          onSelect={runMenuAction(openImportManager)}
          disabled={!status.loaded}
          title="Manage imported files (war3map.imp): add a custom icon, loading screen, sound, or texture; remove or rename an import."
        >
          <span class="flex-1">Import Manager…</span>
        </DropdownMenu.Item>
        <DropdownMenu.Item
          onSelect={runMenuAction(openGameplayConstantsEditor)}
          disabled={!status.loaded}
          title="Edit per-map gameplay constants (war3mapMisc.txt)."
        >
          <span class="flex-1">Gameplay Constants</span>
        </DropdownMenu.Item>
        <DropdownMenu.Item
          onSelect={runMenuAction(openObjectEditor)}
          disabled={!status.loaded}
          title="Browse units (Phase 1a: read-only)."
        >
          <span class="flex-1">Object Editor</span>
          <DropdownMenu.Shortcut>F6</DropdownMenu.Shortcut>
        </DropdownMenu.Item>
        <DropdownMenu.Item
          onSelect={runMenuAction(openTriggerEditor)}
          disabled={!status.loaded}
          title="Browse GUI triggers + custom scripts (Phase 1a: read-only)."
        >
          <span class="flex-1">Trigger Editor</span>
          <DropdownMenu.Shortcut>F4</DropdownMenu.Shortcut>
        </DropdownMenu.Item>
        <DropdownMenu.Item
          onSelect={runMenuAction(openConvertToLua)}
          disabled={!status.loaded || !!status.lua || convertChecking}
          title={status.lua
            ? 'Map is already Lua — nothing to convert.'
            : 'Modernize an old JASS map: regenerate war3map.lua from GUI triggers, delete war3map.j, flip MapInfo.Lua=true. Undoable until Save.'}
        >
          <span class="flex-1">
            {status.lua ? 'Convert to Lua (already Lua)' : 'Convert to Lua…'}
          </span>
        </DropdownMenu.Item>
        <DropdownMenu.Separator />
        <DropdownMenu.Item onSelect={runMenuAction(close)} disabled={!status.loaded || busy}>
          <span class="flex-1">Close</span>
        </DropdownMenu.Item>
      </DropdownMenu.Content>
    </DropdownMenu.Root>

    <!-- Edit menu (Phase: history). Hosts Undo/Redo with live labels and
         enabled state pulled from CanUndo/CanRedo. Each item's tooltip
         shows what would be undone/redone ("Undo Move doodad"). -->
    <DropdownMenu.Root>
      <DropdownMenu.Trigger>
        {#snippet child({ props })}
          <Button {...props} variant="ghost" size="sm" title="Edit menu">
            Edit
            <ChevronDownIcon class="text-muted-foreground" />
          </Button>
        {/snippet}
      </DropdownMenu.Trigger>
      <DropdownMenu.Content class="min-w-[240px]" align="start">
        <DropdownMenu.Item
          onSelect={runMenuAction(doUndo)}
          disabled={!canUndo}
          title={undoLabel ? `Undo: ${undoLabel}` : 'Nothing to undo'}
        >
          <span class="flex-1">Undo{undoLabel ? ` — ${undoLabel}` : ''}</span>
          <DropdownMenu.Shortcut>Ctrl+Z</DropdownMenu.Shortcut>
        </DropdownMenu.Item>
        <DropdownMenu.Item
          onSelect={runMenuAction(doRedo)}
          disabled={!canRedo}
          title={redoLabel ? `Redo: ${redoLabel}` : 'Nothing to redo'}
        >
          <span class="flex-1">Redo{redoLabel ? ` — ${redoLabel}` : ''}</span>
          <DropdownMenu.Shortcut>Ctrl+Shift+Z</DropdownMenu.Shortcut>
        </DropdownMenu.Item>
      </DropdownMenu.Content>
    </DropdownMenu.Root>

    <ViewMenu categories={doodadCategoriesPresent}
              visibility={doodadVisibility}
              overlays={{ pathing: pathingVisible, camerabounds: cameraBoundsVisible }}
              extras={[
                { id: 'reforged', label: 'Reforged Graphics', checked: reforged, disabled: busy,
                  title: 'Toggle Reforged graphics. Reloads the current map without resetting the camera.' },
                { id: 'minimap', label: 'Minimap', checked: showMinimap,
                  title: 'Toggle the minimap overlay (bottom-right of viewport). Shortcut: M' },
                { id: 'agent-console', label: 'Agent Console', checked: bridgeConsoleOpen,
                  title: 'Toggle the Agent Console (Ctrl+`) — live stream of every MCP bridge call.' },
                { id: 'diagnostics', label: 'Diagnostics Overlay', checked: diagnosticsOn,
                  title: 'Toggle the diagnostics overlay (top-left) + in-canvas heartbeat. Shortcut: F9' },
                { id: 'verbose-log', label: 'Verbose Logging', checked: verboseLogging,
                  title: 'Write debug-level traces (per-frame/per-asset) to wc3-forge.log.' },
              ]}
              theme={theme}
              onToggle={onViewToggle}
              onOverlayToggle={onOverlayToggle}
              onExtraToggle={onExtraToggle}
              onThemeChange={setTheme} />

    <!-- Help menu. Hosts the manual update check (moved here from File) and
         the About dialog. -->
    <DropdownMenu.Root>
      <DropdownMenu.Trigger>
        {#snippet child({ props })}
          <Button {...props} variant="ghost" size="sm" title="Help menu">
            Help
            <ChevronDownIcon class="text-muted-foreground" />
          </Button>
        {/snippet}
      </DropdownMenu.Trigger>
      <DropdownMenu.Content class="min-w-[220px]" align="start">
        <DropdownMenu.Item
          onSelect={runMenuAction(checkForUpdatesManual)}
          title="Check GitHub for a newer wc3-forge release."
        >
          <span class="flex-1">Check for Updates…</span>
          <DropdownMenu.Shortcut>v{appVersion}</DropdownMenu.Shortcut>
        </DropdownMenu.Item>
        <DropdownMenu.Separator />
        <DropdownMenu.Item
          onSelect={runMenuAction(() => { showShortcuts = true })}
          title="Show every keyboard shortcut."
        >
          <span class="flex-1">Keyboard Shortcuts…</span>
          <DropdownMenu.Shortcut>?</DropdownMenu.Shortcut>
        </DropdownMenu.Item>
        <DropdownMenu.Separator />
        <DropdownMenu.Item
          onSelect={runMenuAction(() => { showAboutDialog = true })}
          title="About wc3-forge — version, author, and credits."
        >
          <span class="flex-1">About wc3-forge</span>
        </DropdownMenu.Item>
      </DropdownMenu.Content>
    </DropdownMenu.Root>

    <div class="flex-1 truncate text-xs text-muted-foreground">
      {#if status.loaded}
        <span>{status.unit_count} entities</span>
      {/if}
    </div>

    <div class="flex items-center gap-1.5">
      {#if editorMode === 'doodad'}
      <!-- Gizmo mode toggle (Phase C): Move / Rotate / Scale.
           Bare hotkeys W / E / R. Active mode is highlighted; inactive modes
           sit in muted-secondary so the active one reads at a glance.
           Hidden in terrain pick mode — the gizmo only acts on entities, so
           the controls would be dead weight when the click pipeline is
           routed to terrain instead. -->
      <div class="flex items-center" role="group" aria-label="Gizmo mode">
        <Button
          size="sm"
          variant={gizmoMode === 'move' ? 'default' : 'secondary'}
          class="rounded-r-none {gizmoMode === 'move' ? 'bg-amber-500 text-white hover:bg-amber-600' : ''}"
          onclick={() => setGizmoMode('move')}
          title="Move (1) — drag arrows to translate"
        >Move</Button>
        <Button
          size="sm"
          variant={gizmoMode === 'rotate' ? 'default' : 'secondary'}
          class="rounded-none border-l-0 {gizmoMode === 'rotate' ? 'bg-amber-500 text-white hover:bg-amber-600' : ''}"
          onclick={() => setGizmoMode('rotate')}
          title="Rotate (2) — drag the Z ring to rotate around vertical axis"
        >Rotate</Button>
        <Button
          size="sm"
          variant={gizmoMode === 'scale' ? 'default' : 'secondary'}
          class="rounded-l-none border-l-0 {gizmoMode === 'scale' ? 'bg-amber-500 text-white hover:bg-amber-600' : ''}"
          onclick={() => setGizmoMode('scale')}
          title="Scale (3) — drag a face handle to scale on that axis"
        >Scale</Button>
      </div>
      <!-- Snap settings (Phase D). DropdownMenu popover with 3 channels.
           Each channel row: clickable toggle + step-value preset row. Hold
           Shift during a drag to invert the active channel's snap state. -->
      <DropdownMenu.Root>
        <DropdownMenu.Trigger>
          {#snippet child({ props })}
            <Button {...props} variant="secondary" size="sm"
                    class={(snapMoveOn || snapRotateOn || snapScaleOn) ? 'border border-amber-500/60' : ''}
                    title="Snap settings — Shift during drag inverts each channel">
              Snap{(snapMoveOn || snapRotateOn || snapScaleOn) ? ' •' : ''}
              <ChevronDownIcon class="text-muted-foreground" />
            </Button>
          {/snippet}
        </DropdownMenu.Trigger>
        <DropdownMenu.Content class="min-w-[260px] p-2" align="end">
          <div class="px-2 pb-1 text-xs font-semibold text-muted-foreground">Snap</div>
          {#each [
            { ch: 'move' as const,   label: 'Move',   on: snapMoveOn,   step: snapMoveStep,   steps: MOVE_STEPS,   suffix: '' },
            { ch: 'rotate' as const, label: 'Rotate', on: snapRotateOn, step: snapRotateStep, steps: ROTATE_STEPS, suffix: '°' },
            { ch: 'scale' as const,  label: 'Scale',  on: snapScaleOn,  step: snapScaleStep,  steps: SCALE_STEPS,  suffix: '×' },
          ] as row (row.ch)}
            <div class="flex items-center gap-2 px-2 py-1">
              <button type="button"
                      class="flex-1 text-left text-sm {row.on ? 'text-amber-400 font-medium' : 'text-foreground'}"
                      onclick={() => toggleSnap(row.ch)}
                      title="Toggle {row.label} snap">
                {row.on ? '✓' : '○'} {row.label}
              </button>
              <div class="flex gap-0.5" class:opacity-40={!row.on}>
                {#each row.steps as s}
                  <button type="button"
                          class="rounded px-1.5 py-0.5 text-xs {s === row.step ? 'bg-amber-500/30 text-amber-900 dark:text-amber-100' : 'hover:bg-muted'}"
                          onclick={() => setSnapStep(row.ch, s)}
                          title="{row.label} step = {s}{row.suffix}">
                    {s}{row.suffix}
                  </button>
                {/each}
              </div>
            </div>
          {/each}
          <div class="border-t border-border mt-1 pt-1.5 px-2 text-[10px] text-muted-foreground">
            Hold <kbd class="px-1 bg-muted rounded">Shift</kbd> during a drag to invert.
          </div>
        </DropdownMenu.Content>
      </DropdownMenu.Root>
      <span class="mx-1 inline-block h-5 w-px bg-border" aria-hidden="true"></span>
      {/if}
      <div class="inline-flex items-center gap-0.5 rounded-md border border-border p-0.5" role="group" aria-label="Editor mode" title="Tab / Shift+Tab to cycle modes">
        <Button size="sm"
          variant={editorMode === 'doodad' ? 'default' : 'secondary'}
          class={editorMode === 'doodad' ? 'bg-blue-600 text-white hover:bg-blue-700' : 'bg-transparent text-foreground hover:bg-muted'}
          onclick={() => setEditorMode('doodad')}
          title="Doodad mode — place doodads/units; click selects & drags entities.">Doodad</Button>
        <Button size="sm"
          variant={editorMode === 'terrain' ? 'default' : 'secondary'}
          class={editorMode === 'terrain' ? 'bg-emerald-600 text-white hover:bg-emerald-700' : 'bg-transparent text-foreground hover:bg-muted'}
          onclick={() => setEditorMode('terrain')}
          title="Terrain mode — paint terrain / inspect a cell's data.">Terrain</Button>
        <Button size="sm"
          variant={editorMode === 'region' ? 'default' : 'secondary'}
          class={editorMode === 'region' ? 'bg-amber-600 text-white hover:bg-amber-700' : 'bg-transparent text-foreground hover:bg-muted'}
          onclick={() => setEditorMode('region')}
          title="Region mode — drag to draw a region rectangle; click to select one.">Region</Button>
      </div>
      <span class="mx-1 inline-block h-5 w-px bg-border" aria-hidden="true"></span>
      <Button
        size="sm"
        onclick={launchTestMap}
        disabled={testMapDisabled}
        class="bg-emerald-600 font-semibold text-white hover:bg-emerald-700 disabled:bg-muted disabled:text-muted-foreground"
        title={testMapTooltip}
      >
        ▶ Play
      </Button>
    </div>
  </header>

  {#if error}<div class="error"><pre>{error}</pre></div>{/if}

  <!-- 3-column layout: viewport (left/center, flexes) + vertical splitter +
       right column (Explorer stacked above Properties with internal horizontal
       splitter between). The right column's width is user-resizable via the
       vertical splitter on its LEFT edge (persisted to localStorage). -->
  <div class="grid min-h-0 flex-1" style="grid-template-columns: 1fr 4px {rightColWidthPx}px;">
    <section class="viewport">
      <canvas bind:this={canvas}></canvas>
      <!-- First-run empty state: shown over the bare viewport whenever no map is
           loaded. Reuses the existing open/new handlers; auto-hides the instant
           status.loaded flips true. -->
      {#if !status.loaded}
        <div class="firstrun">
          <div class="firstrun-card">
            <h1>wc3-forge</h1>
            <p>Open a Warcraft III map to start editing — or create a new one.</p>
            <div class="firstrun-actions">
              <Button size="lg" onclick={() => void pickAndOpen()}>
                <FolderOpenIcon class="size-4" /> Open Map
              </Button>
              <Button size="lg" variant="secondary" onclick={openNewMap}>
                <FilePlusIcon class="size-4" /> New Map
              </Button>
              <Button size="lg" variant="ghost" onclick={() => (showMcpSetup = true)}>
                <BotIcon class="size-4" /> Set up Claude Code
              </Button>
            </div>
          </div>
        </div>
      {/if}
      {#if showMinimap}
        <Minimap {scene} {mapLoadGen} />
      {/if}
      <!-- View-orbit gizmo: top-right of the viewport. Lives in the same
           anchor container as the minimap (which sits bottom-right) and
           reads its camera handle from the SceneAPI so it never depends on
           window-globals. Only mounts when scene exists so the camera
           handle is guaranteed non-null. -->
      {#if scene}
        {#if diagnosticsOn}
          <DiagnosticsOverlay {scene} mapName={status.name} mapPath={status.path} />
        {/if}
        <CameraOrbitGizmo camera={scene.getCamera()} />
      {/if}
      <!-- Doodad palette: floating "+" launcher (bottom-left) + panel. Shown in
           Doodad Mode only (the terrain palette takes the same slot in Terrain
           Mode). Reports the armed type via onArm so App drives the scene's
           placement mode and the click-to-place flow. -->
      {#if status.loaded && editorMode === 'doodad'}
        <DoodadPalette armedTypeId={armedDoodadType} {reforged} onArm={armDoodad} />
      {/if}
      <!-- Unit palette: floating "users" launcher (bottom-left, above the
           Doodad FAB). Shown in Doodad Mode only (alongside the doodad palette).
           Lets the user place units (owned by a chosen player slot) and start
           locations, and lists/deletes existing start locations. App drives the
           scene's placement mode + the click-to-place flow. -->
      {#if status.loaded && editorMode === 'doodad'}
        <UnitPalette
          armedTypeId={armedUnitType}
          armedPlayer={armedUnitPlayer}
          {startLocArmed}
          {startLocations}
          {reforged}
          onArmUnit={armUnit}
          onSetPlayer={setUnitPlayer}
          onArmStartLoc={armStartLoc}
          onDeleteStartLoc={deleteStartLoc}
        />
      {/if}
      <!-- Terrain palette: floating "mountain" launcher (bottom-left). Shown in
           Terrain Mode only. Reports the armed brush via onChange; App drives
           the scene's brush mode + the Go brush calls. -->
      {#if status.loaded && editorMode === 'terrain'}
        <TerrainPalette loaded={status.loaded} onChange={onTerrainBrushChange} />
      {/if}
      <!-- Region panel: the Region-mode side panel (lists regions, numeric
           create, the show/hide overlay toggle). Shown only in Region mode —
           drawing/selecting regions is the viewport interaction that mode owns.
           Owns its list + numeric editors (direct Wails calls); App owns the
           overlay sync + the viewport draw/select wiring. -->
      {#if status.loaded && editorMode === 'region'}
        <RegionPanel
          loaded={status.loaded}
          selectedCN={selectedRegionCN}
          refreshToken={regionRefreshToken}
          onSelectRegion={onSelectRegion}
          onRegionsChanged={() => { void syncRegionOverlay(); regionRefreshToken++ }}
          overlayVisible={regionOverlayVisible}
          onToggleOverlay={toggleRegionOverlay}
        />
      {/if}
    </section>

    <Splitter direction="vertical"
              onDrag={onRightColWidthDrag}
              onDragEnd={onRightColWidthDragEnd} />

    <div class="flex min-h-0 flex-col bg-card" bind:this={rightColEl}>
      <aside class="flex min-h-0 flex-none flex-col overflow-hidden"
             style="flex: 0 0 {rightExplorerPct}%;">
        <header class="panel-header">Explorer</header>
        <div class="panel-body">
          {#if !status.loaded}
            <div class="empty">No map loaded.</div>
          {:else}
            <!-- Each top-level Explorer section is an Accordion. Doodads
                 nests sub-Accordions per category. Default-open for all
                 sections on first render; user toggles persist for session. -->
            {#each groups as g (g.id)}
              <Accordion id={g.id} label={g.label} open={isOpen(g.id, true)}
                         onToggle={onSectionToggle}>
                {#snippet headerExtras()}
                  {#if g.id === 'markers'}
                    <!-- Hide/show all start-location markers in the viewport.
                         role=button (not <button>) since headerExtras renders
                         inside the Accordion trigger button; stopPropagation
                         keeps the click from also toggling the section. -->
                    <span class="inline-flex items-center cursor-pointer opacity-70 hover:opacity-100"
                          role="button" tabindex="0"
                          aria-pressed={!slocsVisible}
                          title={slocsVisible ? 'Hide start-location markers' : 'Show start-location markers'}
                          onpointerdown={(e) => e.stopPropagation()}
                          onclick={(e) => { e.stopPropagation(); e.preventDefault(); toggleSlocs() }}
                          onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.stopPropagation(); e.preventDefault(); toggleSlocs() } }}>
                      {#if slocsVisible}<EyeIcon size={12} />{:else}<EyeOffIcon size={12} />{/if}
                    </span>
                  {/if}
                  {g.entries.length}
                {/snippet}
                <ul class="explorer-list">
                  {#each g.entries as u (u.creation_number)}
                    <li class:selected={selectedIds.has(u.creation_number)}
                        data-row-key="unit:{u.creation_number}"
                        onclick={(e) => clickRow(e, 'unit', u.creation_number)}
                        title="{u.type_id} #{u.creation_number}">
                      {#if u.type_id === 'sloc'}
                        <!-- Slocs have no command-button icon — use a colored
                             swatch in the team color so the row visually
                             telegraphs which player it belongs to. -->
                        <span class="icon-slot swatch"
                              style="background: {teamColorCSS(u.player)};"
                              aria-hidden="true"></span>
                      {:else}
                        {#await loadIconURL(unitIconPath(u)) then iconURL}
                          {#if iconURL}
                            <img class="icon-slot row-icon" src={iconURL} alt="" />
                          {:else}
                            <span class="icon-slot placeholder" aria-hidden="true"></span>
                          {/if}
                        {/await}
                      {/if}
                      <span class="name">{unitDisplayName(u)}</span>
                      <span class="cat dim">{unitCategory(u)}</span>
                      <button class="pan-btn"
                              onclick={(e) => panToEntity(e, u.position)}
                              title="Pan camera to this entity">⊕</button>
                    </li>
                  {/each}
                </ul>
              </Accordion>
            {/each}
            {#if doodadCount > 0}
              <Accordion id="doodads" label="Doodads" open={isOpen('doodads', true)}
                         onToggle={onSectionToggle}>
                {#snippet headerExtras()}{doodadCount}{/snippet}
                <div class="doodad-subs">
                  {#each doodadGroups as dg (dg.id)}
                    <Accordion id={dg.id} label={dg.label} open={isOpen(dg.id, true)}
                               onToggle={onSectionToggle}>
                      {#snippet headerExtras()}{dg.entries.length}{/snippet}
                      <ul class="explorer-list">
                        {#each dg.entries as d (d.creation_number)}
                          <li class:selected={selectedDoodadIds.has(d.creation_number)}
                              data-row-key="doodad:{d.creation_number}"
                              onclick={(e) => clickRow(e, 'doodad', d.creation_number)}
                              title="{d.type_id} #{d.creation_number}">
                            {#await loadIconURL(doodadIconPath(d)) then iconURL}
                              {#if iconURL}
                                <img class="icon-slot row-icon" src={iconURL} alt="" />
                              {:else}
                                <span class="icon-slot placeholder" aria-hidden="true"></span>
                              {/if}
                            {/await}
                            <span class="name">{doodadDisplayName(d)}</span>
                            <span class="cat dim">{doodadCategoryFor(d)}</span>
                            <button class="pan-btn"
                                    onclick={(e) => panToEntity(e, d.position)}
                                    title="Pan camera to this doodad">⊕</button>
                          </li>
                        {/each}
                      </ul>
                    </Accordion>
                  {/each}
                </div>
              </Accordion>
            {/if}
          {/if}
        </div>
      </aside>

      <Splitter onDrag={onRightSplitterDrag} />

      <aside class="flex min-h-0 flex-1 flex-col overflow-hidden">
        <header class="panel-header">Properties</header>
        <div class="panel-body">
        {#if previewModelPath}
          <Accordion id="p:preview" label="Preview" open={isOpen('p:preview', true)}
                     onToggle={onSectionToggle}>
            <AssetPreview modelPath={previewModelPath}
                          modelPathFallbacks={previewModelFallbacks}
                          {reforged}
                          teamColor={primaryEntity ? primaryEntity.Player : 0} />
          </Accordion>
        {/if}
        {#if terrainCell}
          <!-- Terrain-cell info is its own Accordion section so it survives
               an entity selection (user can keep a unit selected and inspect
               cells alongside). Default open whenever a cell exists. -->
          <Accordion id="p:cell" label="Terrain Cell" open={isOpen('p:cell', true)}
                     onToggle={onSectionToggle}>
            <dl class="props">
              <dt>Cell</dt>                <dd class="mono">({terrainCell.col}, {terrainCell.row})</dd>
              <dt>World XY</dt>            <dd class="mono">({fmt(terrainCell.worldX)}, {fmt(terrainCell.worldY)})</dd>
              <dt>Palette #</dt>           <dd class="mono">{terrainCell.paletteIdx}</dd>
              <dt>Palette FourCC</dt>      <dd class="mono">{terrainCell.paletteFourCC || '(none)'}</dd>
              <dt>Texture</dt>             <dd class="mono small">{terrainCell.paletteTexture || '(none)'}</dd>
              <dt>Corner palettes</dt>     <dd class="mono">BL={terrainCell.cornerPalettes[0]} BR={terrainCell.cornerPalettes[1]} TL={terrainCell.cornerPalettes[2]} TR={terrainCell.cornerPalettes[3]}</dd>
              <dt>Layer height</dt>        <dd class="mono">{terrainCell.layerHeight}</dd>
              <dt>Cliff tex #</dt>         <dd class="mono">{terrainCell.cliffTexIdx}</dd>
              <dt>Cliff FourCC</dt>        <dd class="mono">{terrainCell.cliffFourCC || '(none)'}</dd>
              <dt>Cliff var</dt>           <dd class="mono">{terrainCell.cliffVar}</dd>
              <dt>Ground var</dt>          <dd class="mono">{terrainCell.groundVar}</dd>
              <dt>Ramp flags</dt>          <dd>{rampFlagsLabel(terrainCell.rampFlags)}</dd>
              <dt>Has water</dt>           <dd>{terrainCell.hasWater ? `yes (Z=${fmt(terrainCell.waterZ)})` : 'no'}</dd>
              <dt>Cell skip</dt>           <dd>{terrainCell.cellSkip ? 'yes (cliff covers)' : 'no'}</dd>
              <dt>Shadow byte</dt>         <dd class="mono">{shadowLabel(terrainCell.shadow)}</dd>
            </dl>
          </Accordion>
        {/if}
        {#if primaryDoodad}
          {@const d = primaryDoodad}
          <Accordion id="p:identity" label="Identity" open={isOpen('p:identity', true)}
                     onToggle={onSectionToggle}>
            <div class="identity">
              {#await loadIconURL(doodadIconPath(d)) then iconURL}
                {#if iconURL}
                  <img class="identity-icon" src={iconURL} alt="" />
                {:else}
                  <span class="identity-icon placeholder" aria-hidden="true"></span>
                {/if}
              {/await}
              <dl class="props identity-props">
                <dt>Kind</dt>               <dd>Doodad</dd>
                <dt>Type ID</dt>            <dd class="mono">{d.type_id}</dd>
                {#if d.skin_id && d.skin_id !== d.type_id}
                  <dt>Skin ID</dt>          <dd class="mono">{d.skin_id}</dd>
                {/if}
                <dt>Creation #</dt>         <dd class="mono">{d.creation_number}</dd>
                <dt>Name</dt>               <dd>{doodadDisplayName(d)}</dd>
                <dt>Category</dt>           <dd>{doodadCategoryFor(d)}</dd>
              </dl>
            </div>
          </Accordion>
          <Accordion id="p:transform" label="Transform" open={isOpen('p:transform', true)}
                     onToggle={onSectionToggle}>
            <dl class="props">
              {#if singlePositionEditable}
                <dt>Position</dt>
                <dd class="mono pos-edit">
                  <input type="number" step="1" bind:value={posEdit.x}
                         onblur={() => commitPositionEdit('x')}
                         onkeydown={(e) => onPosKeydown(e, 'x')}
                         title="X (game coords). Enter to commit, Esc to revert." />
                  <input type="number" step="1" bind:value={posEdit.y}
                         onblur={() => commitPositionEdit('y')}
                         onkeydown={(e) => onPosKeydown(e, 'y')}
                         title="Y (game coords). Enter to commit, Esc to revert." />
                  <input type="number" step="1" bind:value={posEdit.z}
                         onblur={() => commitPositionEdit('z')}
                         onkeydown={(e) => onPosKeydown(e, 'z')}
                         title="Z (game coords). Enter to commit, Esc to revert." />
                </dd>
              {:else}
                <dt>Position</dt>         <dd class="mono">{fmtVec3(d.position)}</dd>
              {/if}
              <dt>Rotation</dt>           <dd class="mono">{fmt(d.rotation, 2)}</dd>
              <dt>Scale</dt>              <dd class="mono">{fmtScale(d.scale)}</dd>
              <dt>Variation</dt>          <dd>{d.variation}</dd>
            </dl>
          </Accordion>
          {#if d.life !== 0xFF}
            <Accordion id="p:destructible" label="Destructible" open={isOpen('p:destructible', true)}
                       onToggle={onSectionToggle}>
              <dl class="props">
                <dt>Life %</dt>           <dd>{d.life}%</dd>
              </dl>
            </Accordion>
          {/if}
        {:else if !primaryEntity}
          {#if !terrainCell}
            <div class="empty">
              {#if selectedIds.size === 0 && selectedDoodadIds.size === 0}
                Select an entity to see its properties.
              {:else}
                Loading…
              {/if}
            </div>
          {/if}
        {:else}
          {@const e = primaryEntity}
          <Accordion id="p:identity" label="Identity" open={isOpen('p:identity', true)}
                     onToggle={onSectionToggle}>
            <div class="identity">
              {#if e.TypeID === 'sloc'}
                <!-- Slocs have no command-button icon — use the team-color
                     swatch at identity-icon size so the panel echoes the
                     Explorer row's visual cue. -->
                <span class="identity-icon swatch"
                      style="background: {teamColorCSS(e.Player)};"
                      aria-hidden="true"></span>
              {:else}
                {@const iconAsset = (unitTypes[e.TypeID]?.icon_art) ?? ''}
                {#await loadIconURL(iconAsset) then iconURL}
                  {#if iconURL}
                    <img class="identity-icon" src={iconURL} alt="" />
                  {:else}
                    <span class="identity-icon placeholder" aria-hidden="true"></span>
                  {/if}
                {/await}
              {/if}
              <dl class="props identity-props">
                <dt>Type ID</dt>            <dd class="mono">{e.TypeID}</dd>
                {#if e.SkinID && e.SkinID !== e.TypeID}
                  <dt>Skin ID</dt>          <dd class="mono">{e.SkinID}</dd>
                {/if}
                <dt>Creation #</dt>         <dd class="mono">{e.CreationNumber}</dd>
                {#if e.TypeID === 'sloc'}
                  <dt>Kind</dt>             <dd>Start Location</dd>
                {:else if unitTypes[e.TypeID]?.name}
                  <dt>Name</dt>             <dd>{unitTypes[e.TypeID].name}</dd>
                {/if}
                {#if unitTypes[e.TypeID]?.category}
                  <dt>Category</dt>         <dd>{unitTypes[e.TypeID].category}</dd>
                {/if}
                <dt>Player</dt>
                {#if e.TypeID === 'sloc'}
                  <dd>{playerLabel(e.Player)}</dd>
                {:else}
                  <dd class="field-edit">
                    <select value={String(e.Player)} onchange={commitUnitPlayer}
                            title="Owning player slot.">
                      {#each playerOptions() as opt}
                        <option value={String(opt.value)}>{opt.label}</option>
                      {/each}
                    </select>
                  </dd>
                {/if}
              </dl>
            </div>
          </Accordion>
          <Accordion id="p:transform" label="Transform" open={isOpen('p:transform', true)}
                     onToggle={onSectionToggle}>
            <dl class="props">
              {#if singlePositionEditable}
                <dt>Position</dt>
                <dd class="mono pos-edit">
                  <input type="number" step="1" bind:value={posEdit.x}
                         onblur={() => commitPositionEdit('x')}
                         onkeydown={(e) => onPosKeydown(e, 'x')}
                         title="X (game coords). Enter to commit, Esc to revert." />
                  <input type="number" step="1" bind:value={posEdit.y}
                         onblur={() => commitPositionEdit('y')}
                         onkeydown={(e) => onPosKeydown(e, 'y')}
                         title="Y (game coords). Enter to commit, Esc to revert." />
                  <input type="number" step="1" bind:value={posEdit.z}
                         onblur={() => commitPositionEdit('z')}
                         onkeydown={(e) => onPosKeydown(e, 'z')}
                         title="Z (game coords). Enter to commit, Esc to revert." />
                </dd>
              {:else}
                <dt>Position</dt>         <dd class="mono">{fmtVec3(e.Position)}</dd>
              {/if}
              <dt>Rotation</dt>           <dd class="mono">{fmt(e.Rotation, 2)}</dd>
              <dt>Scale</dt>              <dd class="mono">{fmtScale(e.Scale)}</dd>
              <dt>Variation</dt>          <dd>{e.Variation}</dd>
            </dl>
          </Accordion>
          <Accordion id="p:status" label="Status" open={isOpen('p:status', true)}
                     onToggle={onSectionToggle}>
            {#if e.TypeID === 'sloc'}
              <dl class="props">
                <dt>HP %</dt>             <dd>{e.HitPointsPct < 0 ? 'default' : e.HitPointsPct + '%'}</dd>
                <dt>Mana %</dt>           <dd>{e.ManaPct < 0 ? 'default' : e.ManaPct + '%'}</dd>
              </dl>
            {:else}
              <dl class="props">
                <dt>HP %</dt>
                <dd class="field-edit">
                  <input type="number" step="1" min="-1" max="100"
                         bind:value={unitFieldEdit.hp_pct}
                         onfocus={() => (focusedUnitField = 'hp_pct')}
                         onblur={() => { focusedUnitField = null; void commitUnitField('hp_pct') }}
                         onkeydown={(ev) => onUnitFieldKeydown(ev, 'hp_pct')}
                         title="Starting HP percent. -1 = unit default. Enter to commit, Esc to revert." />
                  <span class="field-suffix">%</span>
                </dd>
                <dt>Mana %</dt>
                <dd class="field-edit">
                  <input type="number" step="1" min="-1" max="100"
                         bind:value={unitFieldEdit.mana_pct}
                         onfocus={() => (focusedUnitField = 'mana_pct')}
                         onblur={() => { focusedUnitField = null; void commitUnitField('mana_pct') }}
                         onkeydown={(ev) => onUnitFieldKeydown(ev, 'mana_pct')}
                         title="Starting mana percent. -1 = unit default. Enter to commit, Esc to revert." />
                  <span class="field-suffix">%</span>
                </dd>
                <dt>Gold</dt>
                <dd class="field-edit">
                  <input type="number" step="1" min="0"
                         bind:value={unitFieldEdit.gold}
                         onfocus={() => (focusedUnitField = 'gold')}
                         onblur={() => { focusedUnitField = null; void commitUnitField('gold') }}
                         onkeydown={(ev) => onUnitFieldKeydown(ev, 'gold')}
                         title="Gold-mine / gold-coin amount (0 = none). Enter to commit, Esc to revert." />
                </dd>
                <dt>Acquisition</dt>
                <dd class="field-edit">
                  <input type="number" step="1" min="-1"
                         bind:value={unitFieldEdit.target_acquisition}
                         onfocus={() => (focusedUnitField = 'target_acquisition')}
                         onblur={() => { focusedUnitField = null; void commitUnitField('target_acquisition') }}
                         onkeydown={(ev) => onUnitFieldKeydown(ev, 'target_acquisition')}
                         title="Target acquisition range. -1 = unit default. Enter to commit, Esc to revert." />
                </dd>
                <dt>Custom Color</dt>
                <dd class="field-edit">
                  <input type="number" step="1" min="-1" max="27"
                         bind:value={unitFieldEdit.custom_color}
                         onfocus={() => (focusedUnitField = 'custom_color')}
                         onblur={() => { focusedUnitField = null; void commitUnitField('custom_color') }}
                         onkeydown={(ev) => onUnitFieldKeydown(ev, 'custom_color')}
                         title="Player-color override (0-based color index). -1 = use player slot color. Enter to commit, Esc to revert." />
                </dd>
              </dl>
            {/if}
          </Accordion>
          {#if isHero(e)}
            <Accordion id="p:hero" label="Hero" open={isOpen('p:hero', true)}
                       onToggle={onSectionToggle}>
              <dl class="props">
                <dt>Level</dt>
                <dd class="field-edit">
                  <input type="number" step="1" min="1"
                         bind:value={unitFieldEdit.hero_level}
                         onfocus={() => (focusedUnitField = 'hero_level')}
                         onblur={() => { focusedUnitField = null; void commitUnitField('hero_level') }}
                         onkeydown={(ev) => onUnitFieldKeydown(ev, 'hero_level')}
                         title="Starting hero level (1+). Enter to commit, Esc to revert." />
                </dd>
                <dt>STR</dt>
                <dd class="field-edit">
                  <input type="number" step="1" min="0"
                         bind:value={unitFieldEdit.hero_str}
                         onfocus={() => (focusedUnitField = 'hero_str')}
                         onblur={() => { focusedUnitField = null; void commitUnitField('hero_str') }}
                         onkeydown={(ev) => onUnitFieldKeydown(ev, 'hero_str')}
                         title="Bonus strength (Reforged sub-version 11+). Enter to commit, Esc to revert." />
                </dd>
                <dt>AGI</dt>
                <dd class="field-edit">
                  <input type="number" step="1" min="0"
                         bind:value={unitFieldEdit.hero_agi}
                         onfocus={() => (focusedUnitField = 'hero_agi')}
                         onblur={() => { focusedUnitField = null; void commitUnitField('hero_agi') }}
                         onkeydown={(ev) => onUnitFieldKeydown(ev, 'hero_agi')}
                         title="Bonus agility (Reforged sub-version 11+). Enter to commit, Esc to revert." />
                </dd>
                <dt>INT</dt>
                <dd class="field-edit">
                  <input type="number" step="1" min="0"
                         bind:value={unitFieldEdit.hero_int}
                         onfocus={() => (focusedUnitField = 'hero_int')}
                         onblur={() => { focusedUnitField = null; void commitUnitField('hero_int') }}
                         onkeydown={(ev) => onUnitFieldKeydown(ev, 'hero_int')}
                         title="Bonus intelligence (Reforged sub-version 11+). Enter to commit, Esc to revert." />
                </dd>
              </dl>
            </Accordion>
          {/if}
          {#if e.Inventory && e.Inventory.length > 0}
            <Accordion id="p:inventory" label="Inventory" open={isOpen('p:inventory', true)}
                       onToggle={onSectionToggle}>
              <dl class="props">
                {#each e.Inventory as slot}
                  <dt>Slot {slot.Slot}</dt><dd class="mono">{slot.ItemID}</dd>
                {/each}
              </dl>
            </Accordion>
          {/if}
          {#if e.TypeID !== 'sloc'}
            <Accordion id="p:drops" label="Item Drops" open={isOpen('p:drops', (e.ItemDrops?.length ?? 0) > 0)}
                       onToggle={onSectionToggle}>
              <div class="drops-edit">
                {#each itemDropEdit as row, i (i)}
                  <div class="drop-row">
                    <input class="mono drop-item" type="text" maxlength="4" placeholder="iIID"
                           bind:value={row.item_id}
                           onfocus={() => (itemDropFocused = true)}
                           onblur={() => { itemDropFocused = false; void commitItemDrops() }}
                           title="Item type ID (4-char FourCC)." />
                    <input class="drop-chance" type="number" step="1" min="0"
                           bind:value={row.chance}
                           onfocus={() => (itemDropFocused = true)}
                           onblur={() => { itemDropFocused = false; void commitItemDrops() }}
                           title="Relative drop weight within the set." />
                    <span class="field-suffix">%</span>
                    <button type="button" class="drop-del" title="Remove this drop"
                            onclick={() => removeItemDropRow(i)}>×</button>
                  </div>
                {/each}
                {#if itemDropEdit.length === 0}
                  <div class="drops-empty">No item drops.</div>
                {/if}
                <button type="button" class="drop-add" onclick={addItemDropRow}>+ Add drop</button>
              </div>
            </Accordion>
          {/if}
          {#if e.AbilityModifications && e.AbilityModifications.length > 0}
            <Accordion id="p:abilities" label="Abilities" open={isOpen('p:abilities', true)}
                       onToggle={onSectionToggle}>
              <dl class="props">
                {#each e.AbilityModifications as ab}
                  <dt class="mono">{ab.AbilityID}</dt>
                  <dd>lvl {ab.Level}{ab.Autocast ? ' · autocast' : ''}</dd>
                {/each}
              </dl>
            </Accordion>
          {/if}
        {/if}
        </div>
      </aside>
    </div>
  </div>

  <BridgeConsole bind:open={bridgeConsoleOpen} />

  {#if showShortcuts}
    <!-- Keyboard-shortcuts cheat sheet. Toggled by '?' / the Help menu — the
         discoverable index of every binding (World Editor veterans expect the
         F-keys). Backdrop click, the × button, '?', or Escape dismiss it. -->
    <div
      class="fixed inset-0 z-[200] flex items-center justify-center bg-black/60 p-4"
      role="button"
      tabindex="-1"
      aria-label="Close keyboard shortcuts"
      onclick={() => (showShortcuts = false)}
    >
      <div
        class="max-h-[85vh] w-full max-w-2xl overflow-y-auto rounded-lg border border-border bg-background p-5 shadow-xl"
        role="dialog"
        tabindex="-1"
        aria-modal="true"
        aria-label="Keyboard shortcuts"
        onclick={(e) => e.stopPropagation()}
      >
        <div class="mb-3 flex items-center justify-between">
          <h2 class="text-base font-semibold">Keyboard Shortcuts</h2>
          <button type="button" class="rounded px-2 py-0.5 text-muted-foreground hover:bg-muted"
                  title="Close" onclick={() => (showShortcuts = false)}>×</button>
        </div>
        <div class="grid grid-cols-1 gap-x-8 gap-y-4 sm:grid-cols-2">
          {#each shortcutGroups as group}
            <div>
              <h3 class="mb-1.5 text-xs font-semibold uppercase tracking-wide text-muted-foreground">{group.title}</h3>
              <dl class="space-y-1">
                {#each group.items as item}
                  <div class="flex items-baseline justify-between gap-3">
                    <dt class="text-sm">{item.label}</dt>
                    <dd class="shrink-0 space-x-0.5 text-xs text-muted-foreground">
                      {#each item.keys as key, i}
                        <kbd class="rounded border border-border bg-muted px-1.5 py-0.5 font-mono">{key}</kbd>{#if i < item.keys.length - 1}<span class="text-muted-foreground">/</span>{/if}
                      {/each}
                    </dd>
                  </div>
                {/each}
              </dl>
            </div>
          {/each}
        </div>
        <p class="mt-4 text-xs text-muted-foreground">
          Press <kbd class="rounded border border-border bg-muted px-1 py-0.5 font-mono">?</kbd>
          or <kbd class="rounded border border-border bg-muted px-1 py-0.5 font-mono">Esc</kbd> to close.
        </p>
      </div>
    </div>
  {/if}

  {#if showMapInfoEditor}
    <MapInfoEditor
      bind:open={showMapInfoEditor}
      onClose={closeMapInfoEditor}
      onSkyChanged={(path) => scene?.reloadSky(path)}
    />
  {/if}

  {#if showObjectEditor}
    <ObjectEditor bind:open={showObjectEditor} initialId={objectEditorInitialId} {reforged} onClose={closeObjectEditor} />
  {/if}
  {#if showTriggerEditor}
    <TriggerEditor bind:open={showTriggerEditor} initialId={triggerEditorInitialId} onClose={closeTriggerEditor} isLuaMap={status.lua} />
  {/if}

  {#if showNewMap}
    <NewMapDialog
      bind:open={showNewMap}
      busy={newMapBusy}
      onClose={() => (showNewMap = false)}
      onCreate={handleCreateNewMap}
    />
  {/if}

  <!-- "Set up Claude Code" — the MCP registration command, copyable. Opened
       from the first-run overlay. -->
  <Dialog.Root bind:open={showMcpSetup}>
    <Dialog.Content class="max-w-xl">
      <Dialog.Header>
        <Dialog.Title>Set up Claude Code</Dialog.Title>
      </Dialog.Header>
      <div class="mcp-setup">
        <p>
          wc3-forge is also an MCP server, so you can drive the editor from
          <a href="https://claude.ai/code" target="_blank" rel="noreferrer">Claude Code</a>.
          Register it once from a terminal:
        </p>
        <div class="mcp-cmd">
          <code>{mcpSetupCmd}</code>
          <Button size="sm" variant="secondary" onclick={copyMcpCmd}>
            <CopyIcon class="size-3.5" /> {mcpCopied ? 'Copied' : 'Copy'}
          </Button>
        </div>
        <p class="mcp-note">
          Replace <code>&lt;install&gt;</code> with the folder holding
          <code>wc3-forge.exe</code>. Then run <code>claude mcp list</code> —
          wc3-forge should show <code>✓ Connected</code>. (macOS: point it at a
          locally-built binary; there is no prebuilt macOS release.)
        </p>
      </div>
      <Dialog.Footer>
        <Button onclick={() => (showMcpSetup = false)}>Done</Button>
      </Dialog.Footer>
    </Dialog.Content>
  </Dialog.Root>

  {#if showSwapTileset}
    <SwapTilesetDialog
      bind:open={showSwapTileset}
      onClose={closeSwapTileset}
    />
  {/if}

  {#if showImportModel}
    <ImportModelDialog
      bind:open={showImportModel}
      {reforged}
      onClose={closeImportModel}
      onImported={onModelImported}
    />
  {/if}

  {#if showImportManager}
    <ImportManager
      bind:open={showImportManager}
      onClose={closeImportManager}
      onChanged={onImportsChanged}
    />
  {/if}

  {#if showConvertToLua}
    <ConvertToLuaDialog
      bind:open={showConvertToLua}
      blockers={convertBlockers}
      onClose={closeConvertToLua}
      onConverted={onConvertedToLua}
      onOpenTrigger={(id) => openTriggerEditor(id)}
    />
  {/if}

  {#if showWC3InstallDialog}
    <WC3InstallDialog bind:open={showWC3InstallDialog} checkedPath={wc3CheckedPath} />
  {/if}

  {#if showGameplayConstantsEditor}
    <GameplayConstantsEditor
      bind:open={showGameplayConstantsEditor}
      onClose={closeGameplayConstantsEditor}
    />
  {/if}

  {#if showUpdateDialog}
    <UpdateDialog
      bind:open={showUpdateDialog}
      result={updateResult}
      current={appVersion}
      downloading={updateDownloading}
      downloadPct={updateDownloadPct}
      error={updateError}
      bind:autoCheck={autoCheckUpdates}
      onUpdate={startUpdate}
    />
  {/if}

  <!-- Help → About. Static info card: app identity, version, author, and the
       open-source projects wc3-forge is built on (see CREDITS.md). -->
  <Dialog.Root bind:open={showAboutDialog}>
    <Dialog.Content class="max-w-md">
      <Dialog.Header>
        <Dialog.Title>About wc3-forge</Dialog.Title>
        <Dialog.Description>
          A modern, agent-driven Warcraft III map editor.
        </Dialog.Description>
      </Dialog.Header>
      <div class="space-y-3 text-sm">
        <div class="flex items-baseline justify-between">
          <span class="text-muted-foreground">Version</span>
          <span class="font-medium">v{appVersion}</span>
        </div>
        <div class="flex items-baseline justify-between">
          <span class="text-muted-foreground">Author</span>
          <span class="font-medium">Stephen Horton</span>
        </div>
        <div class="flex items-baseline justify-between">
          <span class="text-muted-foreground">License</span>
          <span class="font-medium">GPL-3.0-or-later</span>
        </div>
        <p class="text-muted-foreground leading-relaxed">
          wc3-forge stands on the shoulders of prior open-source work —
          <span class="text-foreground">HiveWE</span> (Stijn Herfst),
          <span class="text-foreground">mdx-m3-viewer</span> (Ghostwolf),
          <span class="text-foreground">CascLib</span> &amp;
          <span class="text-foreground">StormLib</span> (Ladislav Zezula).
          See CREDITS.md for the full attribution.
        </p>
      </div>
      <Dialog.Footer>
        <Button onclick={() => { showAboutDialog = false }}>Close</Button>
      </Dialog.Footer>
    </Dialog.Content>
  </Dialog.Root>

  <!-- Unsaved-changes confirmation. Driven by the wc3-forge:close-requested
       event emitted from Go's OnBeforeClose when the user X-es a dirty
       session. interactOutsideBehavior=ignore so click-outside doesn't
       silently dismiss — the user MUST pick one of the three buttons. -->
  <Dialog.Root bind:open={quitGuardOpen}>
    <Dialog.Content interactOutsideBehavior="ignore" class="max-w-md">
      <Dialog.Header>
        <Dialog.Title>Unsaved changes</Dialog.Title>
        <Dialog.Description>
          You have unsaved edits to <span class="font-medium text-foreground">{status.name || status.path || 'this map'}</span>.
          Closing now will discard them.
        </Dialog.Description>
      </Dialog.Header>
      <Dialog.Footer class="gap-2 sm:gap-2">
        <Button variant="ghost" onclick={quitGuardCancel} disabled={quitGuardSaving}>Cancel</Button>
        <Button variant="destructive" onclick={quitGuardDiscardAndQuit} disabled={quitGuardSaving}>Discard &amp; Quit</Button>
        <Button class="bg-amber-500 text-white hover:bg-amber-600"
                onclick={quitGuardSaveAndQuit} disabled={quitGuardSaving}>
          {quitGuardSaving ? 'Saving…' : 'Save & Quit'}
        </Button>
      </Dialog.Footer>
    </Dialog.Content>
  </Dialog.Root>

  <Toaster />
</main>

<style>
  :global(body) {
    margin: 0;
    font-size: 13px;
    overflow: hidden;
  }
  :global(html), :global(body), main { height: 100vh; }

  .viewport { position: relative; min-width: 0; min-height: 0; }
  canvas { display: block; width: 100%; height: 100%; }

  /* First-run empty-state overlay (no map loaded). */
  .firstrun { position: absolute; inset: 0; display: flex; align-items: center; justify-content: center; background: color-mix(in oklab, var(--background) 72%, transparent); backdrop-filter: blur(2px); z-index: 5; }
  .firstrun-card { display: flex; flex-direction: column; align-items: center; gap: 14px; text-align: center; padding: 32px 40px; }
  .firstrun-card h1 { margin: 0; font-size: 28px; font-weight: 700; letter-spacing: 0.5px; color: var(--foreground); }
  .firstrun-card p { margin: 0; max-width: 440px; font-size: 13px; color: var(--muted-foreground); }
  .firstrun-actions { display: flex; flex-wrap: wrap; gap: 10px; justify-content: center; margin-top: 6px; }
  /* "Set up Claude Code" dialog body. */
  .mcp-setup { display: flex; flex-direction: column; gap: 14px; padding: 4px 0; font-size: 13px; }
  .mcp-setup p { margin: 0; color: var(--muted-foreground); }
  .mcp-setup a { color: var(--primary); text-decoration: underline; }
  .mcp-cmd { display: flex; align-items: center; gap: 8px; background: var(--muted); border: 1px solid var(--border); border-radius: 6px; padding: 8px 10px; }
  .mcp-cmd code { flex: 1; overflow-x: auto; white-space: nowrap; font-family: 'Cascadia Mono', Consolas, monospace; font-size: 12px; color: var(--foreground); }
  .mcp-note { font-size: 12px; }
  .mcp-note code { padding: 1px 4px; border-radius: 3px; background: var(--muted); font-family: 'Cascadia Mono', Consolas, monospace; }

  .error { background: color-mix(in oklab, var(--destructive) 18%, transparent); color: var(--destructive); padding: 6px 14px; font-family: 'Cascadia Mono', Consolas, monospace; font-size: 12px; flex: 0 0 auto; max-height: 200px; overflow: auto; }
  .error pre { margin: 0; white-space: pre-wrap; word-break: break-all; }

  /* Explorer / Properties panel header strip: tight all-caps label above each
     panel body. Tailwind utilities on the parent <aside> handle the panel
     chrome; this just styles the inner title bar. */
  .panel-header {
    padding: 3px 14px; font-size: 10px; font-weight: 600; color: var(--muted-foreground);
    text-transform: uppercase; letter-spacing: 0.08em;
    border-bottom: 1px solid var(--border); background: var(--muted);
    flex: 0 0 auto;
  }
  .panel-body {
    flex: 1 1 auto; min-height: 0; overflow-y: auto;
    display: flex; flex-direction: column;
  }
  .empty { padding: 30px 16px; text-align: center; color: var(--muted-foreground); font-size: 12px; }

  /* Explorer rows (inside Accordion bodies) */
  .explorer-list { list-style: none; margin: 0; padding: 0; }
  .explorer-list li {
    display: flex; align-items: center; gap: 8px;
    padding: 4px 14px; cursor: pointer; font-size: 12px;
    min-width: 0;
  }
  .explorer-list li:hover { background: var(--accent); }
  .explorer-list li.selected { background: var(--primary); color: var(--primary-foreground); }

  /* Row icon / placeholder slot. Reserves a fixed 18x18 box at the start
     of every row so the name column aligns whether the row has an icon,
     a team-color swatch, or just the placeholder square. The placeholder
     is a neutral dim square that's deliberately subdued — it shouldn't
     read as a missing image. */
  .explorer-list li .icon-slot {
    flex: 0 0 auto;
    width: 18px; height: 18px;
    display: inline-block;
    border-radius: 2px;
  }
  .explorer-list li .icon-slot.row-icon {
    object-fit: cover;
    background: var(--muted); /* shows briefly during decode */
  }
  .explorer-list li .icon-slot.placeholder {
    background: var(--muted);
    border: 1px solid var(--border);
  }
  .explorer-list li .icon-slot.swatch {
    border: 1px solid rgba(255,255,255,0.15);
    box-shadow: inset 0 0 0 1px rgba(0,0,0,0.4);
  }

  .explorer-list li .name {
    font-weight: 500; flex: 0 1 auto; min-width: 0;
    overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  }
  .explorer-list li .cat {
    font-size: 10.5px; flex: 0 1 auto; min-width: 0;
    overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
    text-align: left;
  }
  .explorer-list li .pan-btn {
    flex: 0 0 auto; margin-left: auto; display: none;
    background: transparent; color: var(--muted-foreground);
    border: 1px solid var(--border); border-radius: 3px;
    padding: 1px 6px; font-size: 11px; line-height: 1;
    cursor: pointer;
  }
  .explorer-list li:hover .pan-btn { display: inline-flex; }
  .explorer-list li .pan-btn:hover { background: var(--accent); color: var(--accent-foreground); }
  .dim { color: var(--muted-foreground); }

  /* Doodad sub-accordions are nested inside the Doodads accordion body.
     Slightly indent their accordion headers so the hierarchy reads. */
  .doodad-subs :global(.acc-header) {
    padding-left: 24px;
  }

  /* Identity section header: 56px icon on the left, props grid on the right.
     The icon mirrors the kind cue the Explorer row icons give, scaled up so
     the Properties panel reads "this is what's selected" at a glance. */
  .identity {
    display: grid; grid-template-columns: 56px 1fr;
    gap: 0 12px; padding: 10px 16px; align-items: start;
  }
  .identity-icon {
    width: 56px; height: 56px; border-radius: 4px;
    background: var(--muted); object-fit: cover;
    border: 1px solid var(--border);
  }
  .identity-icon.placeholder {
    background: var(--muted); border: 1px solid var(--border);
  }
  .identity-icon.swatch {
    border: 1px solid rgba(255,255,255,0.18);
    box-shadow: inset 0 0 0 1px rgba(0,0,0,0.4);
  }
  /* Props grid inside .identity nests without its own padding — the parent
     .identity already supplies the section padding. */
  .identity-props { padding: 0 !important; }

  /* Properties */
  .props {
    display: grid; grid-template-columns: max-content 1fr;
    gap: 4px 12px; padding: 10px 16px; margin: 0; font-size: 12px;
  }
  .props dt {
    color: var(--muted-foreground); font-size: 11px; padding-top: 2px;
    text-align: left; justify-self: start;
  }
  .props dd { margin: 0; color: var(--foreground); }
  .mono { font-family: 'Cascadia Mono', Consolas, monospace; }
  .mono.small { font-size: 10.5px; word-break: break-all; }

  .pos-edit { display: flex; gap: 4px; }
  .pos-edit input {
    width: 64px; padding: 2px 4px;
    background: var(--background); color: var(--foreground);
    border: 1px solid var(--border); border-radius: 3px;
    font-family: 'Cascadia Mono', Consolas, monospace; font-size: 12px;
  }
  .pos-edit input:focus {
    outline: none; border-color: var(--ring);
  }
  .pos-edit input::-webkit-inner-spin-button,
  .pos-edit input::-webkit-outer-spin-button {
    -webkit-appearance: none; margin: 0;
  }

  /* Per-instance editable field rows (gold, hp%/mana%, hero stats, custom
     color) + the player <select>. Mirrors .pos-edit's compact input look so the
     Properties panel stays visually consistent. */
  .field-edit { display: flex; align-items: center; gap: 4px; }
  .field-edit input,
  .field-edit select {
    width: 84px; padding: 2px 4px;
    background: var(--background); color: var(--foreground);
    border: 1px solid var(--border); border-radius: 3px;
    font-family: 'Cascadia Mono', Consolas, monospace; font-size: 12px;
  }
  .field-edit select { width: auto; max-width: 160px; }
  .field-edit input:focus,
  .field-edit select:focus { outline: none; border-color: var(--ring); }
  .field-edit input::-webkit-inner-spin-button,
  .field-edit input::-webkit-outer-spin-button {
    -webkit-appearance: none; margin: 0;
  }
  .field-suffix { color: var(--muted-foreground); font-size: 11px; }

  /* Item-drop editor: a vertical list of {item_id, chance, delete} rows plus an
     add button. Spans the full Properties width (not the dl grid). */
  .drops-edit { padding: 10px 16px; display: flex; flex-direction: column; gap: 6px; }
  .drop-row { display: flex; align-items: center; gap: 4px; }
  .drop-row input {
    padding: 2px 4px;
    background: var(--background); color: var(--foreground);
    border: 1px solid var(--border); border-radius: 3px;
    font-size: 12px;
  }
  .drop-row input:focus { outline: none; border-color: var(--ring); }
  .drop-item { width: 64px; }
  .drop-chance { width: 56px; }
  .drop-chance::-webkit-inner-spin-button,
  .drop-chance::-webkit-outer-spin-button { -webkit-appearance: none; margin: 0; }
  .drop-del {
    margin-left: auto; width: 20px; height: 20px; line-height: 1;
    background: transparent; color: var(--muted-foreground);
    border: 1px solid var(--border); border-radius: 3px; cursor: pointer;
  }
  .drop-del:hover { color: var(--foreground); border-color: var(--ring); }
  .drops-empty { color: var(--muted-foreground); font-size: 11px; }
  .drop-add {
    align-self: start; padding: 2px 8px; font-size: 11px; cursor: pointer;
    background: transparent; color: var(--muted-foreground);
    border: 1px solid var(--border); border-radius: 3px;
  }
  .drop-add:hover { color: var(--foreground); border-color: var(--ring); }
</style>
