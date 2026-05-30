<script lang="ts">
  import { run as run_1 } from 'svelte/legacy';

  import { onMount, onDestroy, tick } from 'svelte'
  import {
    OpenMapDialog, OpenMapFileDialog, OpenMap, OpenMapInNewWindow, NewWindow, CloseMap, ListUnits, ListDoodads, Status,
    GetSelection, SetSelection, GetUnit,
    GetReforgedMode, SetReforgedMode,
    GetUnitTypeIndex, GetDoodadTypeIndex,
    MoveUnit, MoveDoodad, IsDirty, SaveMap,
    LaunchInWC3,
    Undo, Redo, CanUndo, CanRedo, HistoryList,
    CheckConvertToLua,
    ForceQuit,
    WC3InstallStatus,
    ListObjects, CreateCustomObject, SetObjectField, CreateDoodad,
  } from '../wailsjs/go/main/App.js'
  import { GetObject, type ObjectKind } from './object-editor-bindings'
  import { EventsOn, EventsOff } from '../wailsjs/runtime/runtime.js'
  import type { main, unitsdoo } from '../wailsjs/go/models'
  import {
    createScene,
    type SceneAPI, type PickHit, type SelectMode, type TerrainCellInfo,
  } from './scene-instances'
  import Splitter from './Splitter.svelte'
  import Accordion from './Accordion.svelte'
  import ViewMenu from './ViewMenu.svelte'
  import AssetPreview from './AssetPreview.svelte'
  import MapInfoEditor from './MapInfoEditor.svelte'
  import ObjectEditor from './ObjectEditor.svelte'
  import TriggerEditor from './TriggerEditor.svelte'
  import SwapTilesetDialog from './SwapTilesetDialog.svelte'
  import ConvertToLuaDialog from './ConvertToLuaDialog.svelte'
  import ImportModelDialog from './ImportModelDialog.svelte'
  import WC3InstallDialog from './WC3InstallDialog.svelte'
  import GameplayConstantsEditor from './GameplayConstantsEditor.svelte'
  import BridgeConsole from './BridgeConsole.svelte'
  import Minimap from './Minimap.svelte'
  import CameraOrbitGizmo from './CameraOrbitGizmo.svelte'
  import { showToast } from './toast'
  import { flog } from './debuglog'
  import { loadIconURL } from './icon-loader'
  import { TEAM_COLORS_RGB } from './sloc-markers'
  import { Button } from '$lib/components/ui/button'
  import * as Dialog from '$lib/components/ui/dialog'
  import * as DropdownMenu from '$lib/components/ui/dropdown-menu'
  import { Input } from '$lib/components/ui/input'
  import { Toaster } from '$lib/components/ui/sonner'
  import ChevronDownIcon from '@lucide/svelte/icons/chevron-down'

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
  // Type-level (Object Editor) detail for the primary selection's TYPE, so a
  // curated subset (Name, Model File) is editable inline and a quick-link can
  // jump straight to the full Object Editor for that type. Refreshed whenever
  // the primary selection changes (see refreshPrimaryType).
  let primaryType: main.UnitObjectDetail | null = $state(null)
  let primaryTypeKind: ObjectKind | null = $state(null)
  let typeNameEdit: string = $state('')
  let typeModelEdit: string = $state('')
  let typeCategoryEdit: string = $state('')
  // Doodad category letter → label (Blizzard stock data; mirrors
  // typeindex.go doodadCategoryKeys). Drives the inline category dropdown.
  const DOODAD_CATEGORIES: { letter: string; label: string }[] = [
    { letter: 'O', label: 'Props' },
    { letter: 'E', label: 'Environment' },
    { letter: 'D', label: 'Trees/Destructibles' },
    { letter: 'S', label: 'Structures' },
    { letter: 'B', label: 'Bridges/Ramps' },
    { letter: 'W', label: 'Water' },
    { letter: 'C', label: 'Cliff/Terrain' },
    { letter: 'T', label: 'Terrain' },
    { letter: 'P', label: 'Pathing Blockers' },
    { letter: 'Z', label: 'Cinematic' },
  ]
  // Persistent state errors only — currently just the scene-init-failed path
  // during onMount. Transient operational errors (Save/Open/Move/Reforged
  // toggle) go through showToast() so they auto-dismiss.
  let error: string = $state('')
  let busy: boolean = $state(false)
  let reforged: boolean = $state(false)
  let pathingVisible: boolean = $state(false)

  // Terrain-pick mode state. Owned here, mirrored to the scene via
  // scene.setTerrainPickMode. When a click hits a cell, terrainCell is set
  // and the Properties panel renders an additional section. Selecting an
  // entity (or clicking outside the map) does NOT clear it on its own — the
  // user explicitly clicks elsewhere on terrain to update.
  let terrainPickModeOn: boolean = $state(false)
  let terrainCell: TerrainCellInfo | null = $state(null)

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
  let objectEditorInitialKind: ObjectKind = $state('units')
  function openObjectEditor(id: string | null = null, kind: ObjectKind = 'units') {
    if (!status.loaded) return
    objectEditorInitialId = id
    objectEditorInitialKind = kind
    showObjectEditor = true
  }
  function closeObjectEditor() {
    showObjectEditor = false
    objectEditorInitialId = null
  }

  // --- Inline "Type" (Object Editor) fields for the primary selection ---
  // The Properties panel surfaces a curated subset of the selected entity's
  // TYPE-level object-editor fields (Name, Model File) for quick editing, plus
  // a link into the full Object Editor. SLK column names: 'name', 'file'.
  function typeFieldValue(col: string): string {
    const f = (primaryType?.fields ?? []).find((x) => x.field === col)
    return f?.value ?? ''
  }
  async function refreshPrimaryType() {
    const tid = primaryDoodad?.type_id ?? primaryEntity?.TypeID ?? null
    const kind: ObjectKind | null = primaryDoodad
      ? 'doodads'
      : primaryEntity
        ? 'units'
        : null
    // No type-level fields for start locations (no object-editor row).
    if (!tid || !kind || tid === 'sloc') {
      primaryType = null
      primaryTypeKind = null
      return
    }
    primaryTypeKind = kind
    try {
      primaryType = await GetObject(kind, tid)
    } catch {
      primaryType = null
    }
  }
  async function commitTypeField(col: 'name' | 'file' | 'category', val: string) {
    if (!primaryType || !primaryTypeKind) return
    if (typeFieldValue(col) === val) return // no-op — don't churn history
    try {
      primaryType = await SetObjectField(primaryTypeKind, primaryType.id, col, val)
      if (col === 'name') {
        // Name changed — refresh the panels/lists that show display names.
        units = await ListUnits()
        doodads = await ListDoodads()
      } else {
        // Model or category changed — reload so the view re-renders / the
        // Explorer re-buckets under the new category.
        await reloadMap({ keepCamera: true })
      }
    } catch (e) {
      showToast('Edit failed: ' + String(e), 'error')
    }
  }
  function editTypeInObjectEditor() {
    if (primaryType && primaryTypeKind) openObjectEditor(primaryType.id, primaryTypeKind)
  }
  // Refresh the type detail whenever the primary selection changes (covers both
  // select and deselect — refreshPrimaryType clears when nothing is selected).
  $effect(() => {
    const _deps = [primaryDoodad, primaryEntity]
    void _deps
    void refreshPrimaryType()
  })
  // Reset the inline edit buffers when the selected type changes so the inputs
  // reflect current values (reads primaryType via typeFieldValue → re-runs on
  // change; never reads the buffers, so no feedback loop).
  $effect(() => {
    typeNameEdit = typeFieldValue('name')
    typeModelEdit = typeFieldValue('file')
    typeCategoryEdit = typeFieldValue('category')
  })

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
  // "Done" action of the import dialog: drop the freshly-imported model onto
  // the map at the centre of the current view. We auto-create a custom doodad
  // type whose Model File is the import (cloning the first available doodad as
  // a base) and place one instance at the view centroid. Doodads — not units —
  // avoid the unitsdoo save round-trip hazard. The model path is stored as a
  // stem (no .mdx) so both placeUnit (appends .mdx) and placeDoodad
  // (extension-aware) resolve it. Returns true on success so the dialog can
  // close itself.
  async function placeImportedModelOnMap(modelPath: string, name?: string): Promise<boolean> {
    if (!status.loaded || !scene) return false
    // Centre of view = the camera's orbit pivot on the ground. (NOT a
    // frustum-corner average — those project toward the horizon and bias the
    // point far from where the user is actually looking.)
    const [cx, cy] = scene.getCamera().getPivot()
    const stem = modelPath.replace(/\.(mdl|mdx)$/i, '')
    const trimmedName = name?.trim() ?? ''
    try {
      const doodads = await ListObjects('doodads')
      // Prefer a prop-like base so the inherited fields (pathing/scale) are
      // sensible; fall back to the first doodad. (ListObjects may report the
      // category as a letter OR a resolved label, so match both.) Category is
      // overridden to Props below regardless, so the import never lands under
      // Cliff/Terrain.
      const propLike = new Set([
        'O', 'E', 'D',
        'Props', 'Environment', 'Trees/Destructibles',
      ])
      const base =
        doodads.find((d) => propLike.has(d.category))?.id ?? doodads[0]?.id
      if (!base) {
        showToast('No doodad type available to base the imported model on.', 'error')
        return false
      }
      const created = await CreateCustomObject('doodads', base, '')
      // 'O' = Props — keeps the import out of the Cliff/Terrain bucket.
      await SetObjectField('doodads', created.id, 'category', 'O')
      if (trimmedName) await SetObjectField('doodads', created.id, 'name', trimmedName)
      await SetObjectField('doodads', created.id, 'file', stem)
      const cn = await CreateDoodad(created.id, cx, cy, 0, 0, 1, 0)
      // Re-read + re-render so the new doodad type's model resolves and the
      // instance is drawn.
      await reloadMap({ keepCamera: true })
      // Select it with the move gizmo so it's ready to nudge into place.
      setGizmoMode('move')
      ingestSelection(await SetSelection([{ kind: 'doodad', id: cn }]))
      showToast(`Placed “${trimmedName || stem}” at the centre of the view.`, 'success')
      return true
    } catch (e) {
      showToast('Place on map failed: ' + String(e), 'error')
      return false
    }
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

  onMount(async () => {
    try {
      try { reforged = await GetReforgedMode() } catch { reforged = false }
      scene = createScene(canvas, reforged)
      scene.setPathingVisible(pathingVisible)
      scene.onPick(handlePick)
      scene.onTerrainPick(handleTerrainPick)
      ;(window as any).__scene = scene
      ;(window as any).__showToast = showToast
    } catch (e) {
      error = 'scene init failed: ' + (e instanceof Error ? (e.stack || e.message) : String(e))
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
    // Live CASC remount (user located their WC3 install at runtime): the
    // viewer cached the stock assets that 404'd against the old missing
    // install as empty/failed, so purge that cache and reload the scene to
    // re-fetch them through the new mount — no restart needed.
    EventsOn('wc3-forge:casc-remounted', async () => {
      scene?.purgeResourceCache()
      if ((await Status()).loaded) await reloadMap({ keepCamera: true })
    })
    EventsOn(MAP_EVENT, async () => {
      status = await Status()
      // Bump the minimap-reload generation on every map-changed event so the
      // Minimap component refetches its image + terrain coords. Covers both
      // load (status.loaded=true → new minimap) and close (loaded=false →
      // component renders "no minimap" placeholder).
      mapLoadGen += 1
      if (status.loaded) {
        await reloadMap()
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
      }
    })
    EventsOn(DEV_ANIM_EVENT, (payload: { creation_number: number; anim_name: string }) => {
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
      dirty = !!payload?.dirty
    })
    EventsOn(ENTITY_EVENT, async (payload: { kind: string; id: number; field: string; position: number[] }) => {
      if (!payload) return
      if (payload.kind === 'terrain') {
        // Tileset swap (initial Apply, Undo, or Redo) — palette + per-tile
        // texture indices have changed. Rebuild the terrain renderer's atlas
        // + minimap so the viewport reflects the new state. Keep camera so
        // the undo doesn't yank the user's viewpoint.
        mapLoadGen += 1
        await reloadMap({ keepCamera: true })
        return
      }
      if (payload.kind === 'unit') {
        if (!primaryEntity || primaryEntity.CreationNumber !== payload.id) return
        try { primaryEntity = await GetUnit(payload.id) } catch { /* ignore */ }
      } else if (payload.kind === 'doodad') {
        if (!primaryDoodad || primaryDoodad.creation_number !== payload.id) return
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
      ingestSelection(s)
      const items = s.items || []
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
      flog('[history] wc3-forge:history-changed received')
      void refreshHistoryState()
    })
    // Window-close gate: Go's OnBeforeClose handler emits this event when
    // the user tries to close a dirty session. Show the confirmation modal.
    EventsOn('wc3-forge:close-requested', () => {
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
      const cmd = payload?.cmd || ''
      const [op, ...args] = cmd.trim().split(/\s+/)
      switch (op) {
        case 'terrain.toggle':
          toggleTerrainPickMode()
          break
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
      flog(`[hotkey] ${e.shiftKey ? 'redo' : 'undo'} canUndo=${canUndo} canRedo=${canRedo}`)
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
      await SaveMap()
      // Save succeeded → drop the modal and ForceQuit. If Save fails (e.g.
      // ErrMPQRepackFailed on an unpreservable archive), show the toast and
      // leave the modal open so the user can pick Discard or Cancel instead.
      quitGuardOpen = false
      await ForceQuit()
    } catch (e) {
      showToast(`Save failed: ${e instanceof Error ? e.message : String(e)}`, 'error')
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

  async function doSave() {
    if (!status.loaded || saving) return
    saving = true
    try {
      await SaveMap()
      try { dirty = await IsDirty() } catch {}
    } catch (e) {
      const msg = String(e)
      if (/MPQ repack|extract the map to a folder/i.test(msg)) {
        showToast(
          'Saving this map failed during MPQ repack. As a workaround, extract it to a folder and save that.',
          'error',
        )
      } else {
        showToast('save failed: ' + msg, 'error')
      }
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
    units = await ListUnits()
    doodads = await ListDoodads()
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
  // View → Overlays sub-menu dispatches `overlay-toggle` events. Route per id
  // to the relevant scene toggle. Add new overlay ids here as the menu's
  // OVERLAYS list in ViewMenu.svelte grows.
  function onOverlayToggle(detail: { overlay: string; visible: boolean }) {
    const { overlay, visible } = detail
    if (overlay === 'pathing') {
      pathingVisible = visible
      scene?.setPathingVisible(pathingVisible)
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
    }
  }

  function toggleTerrainPickMode() {
    terrainPickModeOn = !terrainPickModeOn
    scene?.setTerrainPickMode(terrainPickModeOn)
    if (!terrainPickModeOn) {
      terrainCell = null
      // Drop the yellow wireframe overlay when leaving terrain mode — the
      // user has switched to doodad mode and the persistent highlight would
      // otherwise sit at a cell they're no longer working with.
      scene?.setHighlightedCell(null)
    }
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
    const colors = ['Red', 'Blue', 'Teal', 'Purple', 'Yellow', 'Orange', 'Green',
                    'Pink', 'Gray', 'LightBlue', 'DarkGreen', 'Brown']
    if (p === 15) return 'Neutral Passive (15)'
    if (p === 12) return 'Neutral Aggressive (12)'
    if (p < colors.length) return `${colors[p]} (${p})`
    return `Player ${p}`
  }
  function playerColorName(p: number): string {
    const colors = ['Red', 'Blue', 'Teal', 'Purple', 'Yellow', 'Orange', 'Green',
                    'Pink', 'Gray', 'LightBlue', 'DarkGreen', 'Brown']
    if (p < colors.length) return colors[p]
    if (p === 15) return 'Neutral Passive'
    if (p === 12) return 'Neutral Aggressive'
    return `P${p}`
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
  // Mirrors HiveWE's ribbon `Test Map` action. v1 constraints (see
  // app.go::LaunchInWC3 + internal/wc3launch/launch.go):
  //   - Session must be loaded.
  //   - Session must NOT be dirty — wc3-forge has no MPQ writer yet, so
  //     edits can't be packaged into the .w3x that WC3 needs to load. The
  //     button is gated below; future work: save + repackage + launch.
  //   - Session path must be a .w3x/.w3m/.mpq archive, NOT a folder.
  //     Folder-backed sessions can't be launched directly because WC3 only
  //     opens packaged archives.
  let testMapLoaded = $derived(status.loaded && !!status.path)
  let testMapIsFile = $derived((() => {
    const p = (status.path || '').toLowerCase()
    return /\.(w3x|w3m|mpq)$/.test(p)
  })())
  let testMapDisabled = $derived(!testMapLoaded || !testMapIsFile || dirty || launching)
  let testMapTooltip = $derived((() => {
    if (!testMapLoaded) return 'Open a map first'
    if (!testMapIsFile) return 'Folder-backed maps can\'t be tested — open a packaged .w3x first'
    if (dirty) return 'Save changes first (note: saving to MPQ archives is not yet supported, so edits must be discarded or applied externally before testing)'
    if (launching) return 'Launching…'
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
              overlays={{ pathing: pathingVisible }}
              extras={[
                { id: 'reforged', label: 'Reforged Graphics', checked: reforged, disabled: busy,
                  title: 'Toggle Reforged graphics. Reloads the current map without resetting the camera.' },
                { id: 'minimap', label: 'Minimap', checked: showMinimap,
                  title: 'Toggle the minimap overlay (bottom-right of viewport). Shortcut: M' },
                { id: 'agent-console', label: 'Agent Console', checked: bridgeConsoleOpen,
                  title: 'Toggle the Agent Console (Ctrl+`) — live stream of every MCP bridge call.' },
              ]}
              theme={theme}
              onToggle={onViewToggle}
              onOverlayToggle={onOverlayToggle}
              onExtraToggle={onExtraToggle}
              onThemeChange={setTheme} />

    <div class="flex-1 truncate text-xs text-muted-foreground">
      {#if status.loaded}
        <span>{status.unit_count} entities</span>
      {/if}
    </div>

    <div class="flex items-center gap-1.5">
      {#if !terrainPickModeOn}
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
      <Button
        size="sm"
        variant={terrainPickModeOn ? 'default' : 'secondary'}
        class={terrainPickModeOn
          ? 'bg-emerald-600 text-white hover:bg-emerald-700'
          : 'bg-blue-600 text-white hover:bg-blue-700'}
        onclick={toggleTerrainPickMode}
        title={terrainPickModeOn
          ? 'Terrain mode active — clicking a cell shows its data. Click to switch to Doodad mode.'
          : 'Doodad mode active — clicking selects entities. Click to switch to Terrain mode.'}
      >
        {terrainPickModeOn ? 'Terrain Mode' : 'Doodad Mode'}
      </Button>
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
      {#if showMinimap}
        <Minimap {scene} {mapLoadGen} />
      {/if}
      <!-- View-orbit gizmo: top-right of the viewport. Lives in the same
           anchor container as the minimap (which sits bottom-right) and
           reads its camera handle from the SceneAPI so it never depends on
           window-globals. Only mounts when scene exists so the camera
           handle is guaranteed non-null. -->
      {#if scene}
        <CameraOrbitGizmo camera={scene.getCamera()} />
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
                {#snippet headerExtras()}{g.entries.length}{/snippet}
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
            {#if primaryType}
              <button type="button" class="oe-chip" onclick={editTypeInObjectEditor}
                      title="Edit this type in the Object Editor">⤢ Object Editor</button>
            {/if}
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
                <dt>Player</dt>             <dd>{playerLabel(e.Player)}</dd>
              </dl>
            </div>
            {#if primaryType}
              <button type="button" class="oe-chip" onclick={editTypeInObjectEditor}
                      title="Edit this type in the Object Editor">⤢ Object Editor</button>
            {/if}
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
            <dl class="props">
              <dt>HP %</dt>               <dd>{e.HitPointsPct < 0 ? 'default' : e.HitPointsPct + '%'}</dd>
              <dt>Mana %</dt>             <dd>{e.ManaPct < 0 ? 'default' : e.ManaPct + '%'}</dd>
              {#if e.GoldAmount > 0}
                <dt>Gold</dt>             <dd>{e.GoldAmount}</dd>
              {/if}
              {#if e.TargetAcquisition !== 0}
                <dt>Acquisition</dt>      <dd class="mono">{fmt(e.TargetAcquisition, 1)}</dd>
              {/if}
            </dl>
          </Accordion>
          {#if isHero(e)}
            <Accordion id="p:hero" label="Hero" open={isOpen('p:hero', true)}
                       onToggle={onSectionToggle}>
              <dl class="props">
                <dt>Level</dt>            <dd>{e.HeroLevel || 1}</dd>
                {#if e.HeroStr > 0 || e.HeroAgi > 0 || e.HeroInt > 0}
                  <dt>Stats</dt>          <dd class="mono">STR {e.HeroStr} · AGI {e.HeroAgi} · INT {e.HeroInt}</dd>
                {/if}
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
          {#if e.ItemDrops && e.ItemDrops.length > 0}
            <Accordion id="p:drops" label="Item Drops" open={isOpen('p:drops', true)}
                       onToggle={onSectionToggle}>
              <dl class="props">
                {#each e.ItemDrops as drop}
                  <dt class="mono">{drop.ItemID}</dt><dd>{drop.Chance}%</dd>
                {/each}
              </dl>
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
        {#if primaryType}
          <Accordion id="p:type" label="Type" open={isOpen('p:type', true)}
                     onToggle={onSectionToggle}>
            <dl class="props">
              <dt>Type ID</dt>
              <dd class="mono">{primaryType.id}{primaryType.is_custom ? ' · custom' : primaryType.is_edited ? ' · edited' : ''}</dd>
              <dt>Name</dt>
              <dd>
                <input class="type-edit" type="text" bind:value={typeNameEdit}
                       onblur={() => commitTypeField('name', typeNameEdit)}
                       onkeydown={(e) => { if (e.key === 'Enter') e.currentTarget.blur() }}
                       title="Type display name. Enter to commit." />
              </dd>
              <dt>Model</dt>
              <dd>
                <input class="type-edit mono" type="text" bind:value={typeModelEdit}
                       onblur={() => commitTypeField('file', typeModelEdit)}
                       onkeydown={(e) => { if (e.key === 'Enter') e.currentTarget.blur() }}
                       title="Art - Model File (path stem, no .mdx). Enter to commit; the view reloads to show the new model." />
              </dd>
              {#if primaryTypeKind === 'doodads'}
                <dt>Category</dt>
                <dd>
                  <select class="type-edit" bind:value={typeCategoryEdit}
                          onchange={() => commitTypeField('category', typeCategoryEdit)}
                          title="Doodad category — change it from Cliff/Terrain to Props, etc.">
                    {#each DOODAD_CATEGORIES as c (c.letter)}
                      <option value={c.letter}>{c.label}</option>
                    {/each}
                  </select>
                </dd>
              {/if}
            </dl>
          </Accordion>
        {/if}
        </div>
      </aside>
    </div>
  </div>

  <BridgeConsole bind:open={bridgeConsoleOpen} />

  {#if showMapInfoEditor}
    <MapInfoEditor
      bind:open={showMapInfoEditor}
      onClose={closeMapInfoEditor}
      onSkyChanged={(path) => scene?.reloadSky(path)}
    />
  {/if}

  {#if showObjectEditor}
    <ObjectEditor bind:open={showObjectEditor} initialId={objectEditorInitialId} initialKind={objectEditorInitialKind} {reforged} onClose={closeObjectEditor} />
  {/if}
  {#if showTriggerEditor}
    <TriggerEditor bind:open={showTriggerEditor} initialId={triggerEditorInitialId} onClose={closeTriggerEditor} />
  {/if}

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
      onPlace={placeImportedModelOnMap}
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

  /* Inline Type (Object Editor) field editors */
  .type-edit {
    width: 100%; box-sizing: border-box; padding: 2px 4px;
    background: var(--background); color: var(--foreground);
    border: 1px solid var(--border); border-radius: 3px; font-size: 12px;
  }
  .type-edit.mono { font-family: 'Cascadia Mono', Consolas, monospace; }
  .type-edit:focus { outline: none; border-color: var(--ring); }
  /* Small "open in Object Editor" chip in the Identity section */
  .oe-chip {
    margin-top: 6px; padding: 2px 8px;
    background: transparent; color: var(--muted-foreground);
    border: 1px solid var(--border); border-radius: 999px;
    font-size: 11px; cursor: pointer; line-height: 1.6;
  }
  .oe-chip:hover { background: var(--accent); color: var(--foreground); }
</style>
