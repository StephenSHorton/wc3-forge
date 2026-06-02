// scene-instances.ts — uses mdx-m3-viewer at the LOWER level, bypassing
// War3MapViewer.loadMap (which is brittle on modern Reforged W3X files;
// see project-wc3-forge memory entry for the diagnosis).
//
// What we use from the library:
//   - viewer.ModelViewer    — WebGL context, resource cache, RAF orchestration
//   - viewer.Scene          — instance container, camera, frustum culling
//   - viewer.handlers.mdx   — MDX parsing, skeletal animation, batch shaders
//   - viewer.handlers.blp   — BLP texture decoding
//   - viewer.handlers.dds   — DDS texture decoding (Reforged HD assets)
//   - viewer.handlers.tga   — TGA fallback
//
// What we DO NOT use:
//   - War3MapViewer.loadMap and parsers/w3x/* — see memory entry.
//   - The lib's terrain/cliff/water shaders. Terrain is drawn ourselves
//     into the same scene via terrain.ts, between viewer.startFrame() and
//     viewer.render(); the scene is set to alpha=true so its own startFrame
//     doesn't clear our depth buffer.
//   - SimpleOrbitCamera from clients/shared — we ship a WC3-style RTS cam.
//
// What feeds this scene:
//   - Go-parsed map data via the App.* Wails methods.
//   - Stock assets via /asset/<path> served by Go (map MPQ → CASC fallback).
//
// Lifecycle:
//   createScene(canvas) is called once on mount. The returned SceneAPI is
//   stable for the canvas's lifetime; loadMap() is called every time the
//   Wails map-changed event fires.

import * as MV_ns from 'mdx-m3-viewer'
import { flog, flogDebug, flogError, flogWarn, getLogLevel } from './debuglog'
import { collectDiag, registerDiag } from './diag-registry'
import { patchMdxParser } from './mdx-parser-patch'
import {
  ListUnits, ListDoodads, GetUnitTypeIndex, GetDoodadTypeIndex, GetTerrain,
  GetPathingMap, MoveUnit,
} from '../wailsjs/go/main/App.js'
import { EventsOn, EventsOff } from '../wailsjs/runtime/runtime.js'
import { buildTerrain, type TerrainMesh } from './terrain'
import { buildWater, type WaterMesh } from './water'
import { buildSky, type SkyRenderer } from './sky'
import { buildSkyGradient, type SkyGradient } from './sky-gradient'
import { buildPathingOverlay, type PathingOverlay } from './pathing'
import { createCamera, type RTSCamera } from './camera'
import { computeCliffPlacements, renderCliffs, type CliffRendering } from './cliffs'
import {
  buildSlocRenderer, type SlocRenderer, type SlocMarker,
} from './sloc-markers'
import { buildCellHighlight, type CellHighlight } from './cell-highlight'
import { buildRegionOverlay, type RegionOverlay, type RegionRect } from './region-overlay'
import { buildBrushCursor, type BrushCursor } from './brush-cursor'
import { buildInstanceOutline, type InstanceOutline } from './instance-outline'
import { buildBoundsDebug, type BoundsDebug } from './bounds-debug'
import { buildPickBuffer, type PickBuffer } from './pick-buffer'
import { buildPathBlockerRenderer, type PathBlockerRenderer, type PathBlockerMarker } from './path-blockers'
import { buildGizmo, type GizmoRenderer, type SelectionItem as GizmoSelectionItem } from './gizmo'

// Apply the MDX parser patch BEFORE any viewer code touches the library — the
// lib's MdxModel constructor parses on construction, and the patch fixes a
// Reforged-1.32+ format mismatch that otherwise leaves loaded models with
// zero bones / zero geosets (so the model "loads" silently but renders
// nothing). See mdx-parser-patch.ts for the failure mode and patch details.
// Runs at module-load time (before createScene is even called), guarded by
// an idempotency flag so multiple imports don't double-wrap.
patchMdxParser()

const MV: any = (MV_ns as any).default ?? MV_ns

// --- Scene diagnostics (U5 scene-load stats + U6 swallowed-error counters) ---
//
// Module-level so the registry provider (registered once at module init) reads
// stable precomputed counters WITHOUT touching gl.* or scanning the instance
// maps. createScene mutates these via its own closures (loadMap sets the load
// summary; placeUnit/placeDoodad and the viewer.on('error') handler bump the
// failure counters). One canvas/scene per process is the norm; a second
// createScene would just overwrite — acceptable, the editor only ever mounts
// one viewport.
//
// CONTRACT (see diag-registry.ts): O(1), no gl.*, no instance-map scans. Every
// field below is an integer/string already computed elsewhere. Failure
// counters carry a *Fails/*Skipped suffix so the F9 overlay auto-red-tints
// them. unitsPlaced/doodadsPlaced etc. live in `lastLoad`, set at the end of
// loadMap from counters accumulated during the placement walk.
const sceneDiag = {
  // U5 scene-load stats.
  loadingMap: false,       // true while a loadMap() is in flight
  loadGeneration: 0,       // increments once per loadMap() start
  loadMs: 0,               // wall-clock duration of the most recent loadMap()
  terrainDrawCalls: 0,     // terrain mesh draw calls (integer, set on build)
  lastLoad: {
    unitsPlaced: 0,
    unitsSkipped: 0,
    doodadsPlaced: 0,
    doodadsSkipped: 0,
    pathBlockers: 0,
    slocs: 0,
    topSkipTypeIds: [] as string[],
  },
  // U6 swallowed-error counters (the real silent-failure sites).
  unitModelFails: 0,       // placeUnit: viewer.load threw / no model
  doodadModelFails: 0,     // placeDoodad: every candidate path failed
  viewerErrors: 0,         // distinct viewer.on('error') events (deduped)
  zeroGeosetModels: 0,     // placed but geosets==0 → invisible (see flogWarn)
}
registerDiag('scene', () => ({ ...sceneDiag }))

// The previous version of this file ran a corrective re-patch here at module
// load to fix a broken readAnimations override in mdx-parser-patch.ts (commit
// dc0aaa0). That correction has been consolidated INTO mdx-parser-patch.ts
// itself — see the top-of-file imports and `patchMdxParser()` body there for
// the correct AnimationMap source and the patched readAnimations. With one
// canonical patch site, double-patching isn't possible and the failure mode
// (silent fallback to "every animation tag is unknown") can't recur.

// Per-instance frustum culling + distance/screen-size LOD.
//
// The lib's stock `ModelInstance.isVisible` tests an MDX-stored bounding
// sphere (`bounds.r * scale`) against the 6 camera planes. The sphere is
// per-sequence (the lib's MdxModelInstance.getBounds picks
// `model.sequences[this.sequence].bounds`, falling back to the model-global
// bound when the sequence's r is 0) — but in practice many WC3 MDX bounds
// are STILL tight to mesh geometry and miss attached weapons, particle
// emitters, and ribbon trails. With straight-up stock culling, the moment
// the sphere center crosses a frustum plane the entire model pops out even
// though some of its pixels would still land on-screen.
//
// The previous fix (commit ff8e2ab) disabled culling entirely. That made
// pop-out go away but means every instance in the scene runs the animation
// tick + render pipeline every frame, which gets expensive at editor scale
// (Enfo FFB has ~580 doodads + 200 units; large custom maps push 3000+).
//
// What we do instead:
//   1. Re-enable lib-style frustum culling, but with an INFLATED radius
//      (`r * CULL_RADIUS_SCALE + CULL_RADIUS_PAD`). The scale handles
//      proportional growth (a chunky idle pose extended by a weapon),
//      the pad handles particle emitters whose extent isn't in the bound
//      at all. Verified at Enfo FFB: 1024-stud pad covers every observed
//      pop-out edge case while still excluding fully-off-screen instances.
//   2. Add a SCREEN-SIZE LOD: when an instance's projected screen radius
//      is smaller than `LOD_MIN_PX`, skip rendering AND its animation
//      tick (the lib couples both inside the `update()` -> `isVisible()`
//      branch — see modelinstance.js line 103). This is the big win at
//      "frame the whole map" zoom on large maps: hundreds of distant
//      doodads project to sub-pixel disks and were eating GPU + CPU for
//      pixels nobody can see.
//
// Both behaviours are gated by `window.__wc3ForgeCulling` so a developer
// can disable them at runtime from DevTools to diagnose a regression
// (`window.__wc3ForgeCulling.frustum = false`,
//  `window.__wc3ForgeCulling.lod = false`).
//
// HiveWE reference: HiveWE does proper AABB-based culling per sequence
// (src/base/render_manager.ixx line 105-108 — uses
// `camera.inside_frustrum_transform(extent.min, extent.max, matrix)`).
// That's strictly more accurate than our sphere with inflation but needs
// per-sequence AABB plumbing through the lib's bound API, which is
// sphere-only. Deferred as a follow-up; sphere + slack covers the visible
// edge cases for now. (HiveWE does NOT do distance LOD on entity render —
// the geoset-level `lod != 0` skip in skinned_mesh.ixx is about MDX-author
// LOD geosets, not camera-distance culling.)
//
// `depth` is always written (even on the cull-fail and LOD-fail paths)
// because the scene's transparent-pass sorter reads it (scene.js line
// 189). For culled instances, the depth value won't matter for sort —
// they're not added to the render list — but the lib walks every cell's
// instance list and may briefly touch `depth` before the cull decision
// completes.
const CULL_RADIUS_SCALE = 2.0
const CULL_RADIUS_PAD = 1024 // studs — wide enough for particle emitters

// LOD: instances whose screen-projected radius is below this are skipped.
// At 0.6 px, a sub-pixel disk that wouldn't be visible to the user gets
// dropped. Set to ~2-4 px to be aggressive (visible perf gain but doodads
// "twinkle" at the LOD boundary); 0.6 px is conservative — only drops
// instances the user genuinely can't see. Camera focal estimation needs
// the canvas height to be plumbed in via the patched scene viewport (set
// by the RAF loop's `vp[3] = canvas.height`).
const LOD_MIN_PX = 0.6

interface CullingDebug {
  frustum: boolean
  lod: boolean
  // Per-frame counters reset by the RAF loop via `resetCulling()`. Read
  // from DevTools or surface in a perf overlay.
  testedCount: number
  visibleCount: number
  frustumCulledCount: number
  lodCulledCount: number
}
const culling: CullingDebug = {
  frustum: true,
  lod: true,
  testedCount: 0,
  visibleCount: 0,
  frustumCulledCount: 0,
  lodCulledCount: 0,
}
;(window as any).__wc3ForgeCulling = culling
export function resetCullingCounters() {
  culling.testedCount = 0
  culling.visibleCount = 0
  culling.frustumCulledCount = 0
  culling.lodCulledCount = 0
}
export function cullingSnapshot() {
  return { ...culling }
}

;(() => {
  const ModelInstance = MV?.viewer?.ModelInstance
  if (!ModelInstance?.prototype) {
    throw new Error('cull-patch: MV.viewer.ModelInstance.prototype missing')
  }
  ModelInstance.prototype.isVisible = function (camera: any) {
    const [x, y, z] = this.worldLocation
    const planes = camera.planes
    const near = planes[4]
    // Write depth unconditionally so the lib's transparent-pass sorter
    // never sees a stale value if something else briefly inspects it.
    const d = near[0] * x + near[1] * y + near[2] * z + near[3]
    this.depth = d
    culling.testedCount++
    if (!culling.frustum) {
      culling.visibleCount++
      return true
    }

    // Inflate the bounds sphere. Same per-instance math the stock impl
    // uses (lib's modelinstance.js line 118-131), but with our scale +
    // pad applied to the radius.
    const [sx, sy, sz] = this.worldScale
    let biggest = sx
    if (sy > biggest) biggest = sy
    if (sz > biggest) biggest = sz
    const bounds = this.getBounds()
    const inflatedR = bounds.r * biggest * CULL_RADIUS_SCALE + CULL_RADIUS_PAD
    const cx = x + bounds.x
    const cy = y + bounds.y
    const cz = z + bounds.z

    // Frustum-cull: walk all 6 planes; if the sphere is fully on the
    // negative side of any one, it's out. We keep `this.plane` cached
    // for the common "still outside the same plane" case (matches
    // testSphere's `first` argument logic in gl-matrix-addon.js).
    const startPlane = this.plane === -1 ? 0 : this.plane
    let outsidePlane = -1
    for (let i = 0; i < 6; i++) {
      const idx = (startPlane + i) % 6
      const p = planes[idx]
      const dist = p[0] * cx + p[1] * cy + p[2] * cz + p[3]
      if (dist <= -inflatedR) {
        outsidePlane = idx
        break
      }
    }
    this.plane = outsidePlane
    if (outsidePlane !== -1) {
      culling.frustumCulledCount++
      return false
    }

    // Distance / screen-size LOD. The projected screen radius of a sphere
    // at perpendicular distance D in front of the camera is roughly
    // `r * projM5 * canvasH/2 / D` where projM5 = 1/tan(fovy/2) (gl-matrix
    // perspective puts that at projectionMatrix[5]). We approximate D as
    // the distance to the near plane (`d` computed above) — for our RTS
    // view with shallow FOV (~60°) this is within a few percent of the
    // true eye-to-center distance and avoids reading `camera.location` /
    // recomputing a vec3 length per instance.
    //
    // canvasH is stashed on the camera by the RAF loop each frame
    // (camera.__wc3ForgeCanvasH) so we don't have to walk scene.viewport
    // for every instance. d<=0 means instance is at or behind the near
    // plane — never LOD it; those are extreme close-ups where the radius
    // calc would explode anyway.
    if (!culling.lod || d <= 0) {
      culling.visibleCount++
      return true
    }
    const projM5 = camera.projectionMatrix?.[5]
    const canvasH = camera.__wc3ForgeCanvasH || 0
    if (projM5 && canvasH > 0) {
      // Use the un-inflated worldR (geometry radius) for the LOD test so
      // heavy padding doesn't keep tiny doodads alive just because their
      // slack bound bumps the projected size.
      const geomR = bounds.r * biggest
      const rPx = (geomR * projM5 * canvasH * 0.5) / d
      if (rPx < LOD_MIN_PX) {
        culling.lodCulledCount++
        return false
      }
    }
    culling.visibleCount++
    return true
  }
})()

// Defensive access to the lib's namespaced classes. CJS+ESM interop is
// finicky enough that a single bundler config change can move things from
// `MV.foo` to `MV.default.foo`; we crash with a descriptive error if either
// shape is missing rather than getting "undefined is not a constructor"
// somewhere deep in user code.
function getMV() {
  const viewer = MV?.viewer
  const ModelViewer = viewer?.ModelViewer
  const handlers = viewer?.handlers
  if (!ModelViewer || !handlers?.mdx || !handlers?.blp || !handlers?.dds || !handlers?.tga) {
    const v = viewer ? Object.keys(viewer) : []
    const h = handlers ? Object.keys(handlers) : []
    throw new Error(`mdx-m3-viewer shape unexpected. viewer=[${v.join(',')}] handlers=[${h.join(',')}]`)
  }
  return { ModelViewer, handlers }
}

// pathSolver converts the library's raw asset name ("Units\Human\Footman\
// Footman.mdx") into a fetchable URL ("/asset/units/human/footman/footman.mdx").
// Wails' embedded HTTP server routes that to our Go assetHandler, which does
// map-MPQ-then-CASC lookup. mdx-m3-viewer accepts either bytes or a URL from
// pathSolver; URLs let us keep all byte-source resolution in Go.
export function pathSolver(src: unknown): unknown {
  if (typeof src !== 'string') return undefined
  return '/asset/' + src.toLowerCase().replace(/\\/g, '/')
}

// Mirrors main.UnitTypeInfo / main.DoodadTypeInfo (Wails drops them from
// models.ts when they appear as a map value — we declare the shapes locally
// instead).
interface UnitTypeInfo {
  file: string
  model_scale: number
  move_height: number
  red: number
  green: number
  blue: number
  name: string
  category: string
}

interface DoodadTypeInfo {
  file: string
  num_var: number
  fixed_rot: number
  model_scale: number
  // Per-type pitch / roll override from Doodads.slk maxPitch / maxRoll
  // (or the destructible / w3d-overlay equivalents). Stored in radians.
  // HiveWE convention:
  //   - negative value: fixed pitch/roll — apply as-is regardless of terrain
  //     (this is what custom maps use to author flame-on-sword effects whose
  //     emitters point along +X in model-local space and need a 90° tilt up).
  //   - positive value: clamp to terrain slope sampled around the doodad
  //     (terrain-follow path — currently unimplemented; we treat positive
  //     as zero, since the only stock rows we've seen use 0 here and the
  //     real consumers of this field in custom maps are all negative).
  //   - zero: no rotation (the default; everything points the way the MDX
  //     author left it).
  max_pitch: number
  max_roll: number
  name: string
  category: string
}

// Resolve the SLK 'file' column to an asset path. SLK rows for stock
// content come without an extension (we append .mdx — the lib's MDX handler
// also accepts MDL via content-magic sniffing, but stock assets are MDX).
// w3d-override rows from custom maps DO carry the extension (often .mdl
// for imported text-format models); we preserve it as-is.
function mdxPath(file: string): string {
  if (/\.(mdl|mdx)$/i.test(file)) return file
  return file + '.mdx'
}

// Axis-angle quaternion around +Z (game-space yaw). Avoids pulling gl-matrix
// in as a direct dep — it's transitive via mdx-m3-viewer, but the surface
// shape is small enough that depending on the transitive package is fragile.
function quatZ(angle: number): number[] {
  const h = angle * 0.5
  return [0, 0, Math.sin(h), Math.cos(h)]
}

// Axis-angle quaternion around +Y (model-local pitch). Used to apply per-type
// maxPitch overrides for doodads whose source MDX puts particle emitters along
// +X in model-local space and needs a fixed tilt to point them upright (see
// Enfo FFB's D002 Fire Sword Effect — maxPitch=-4.71 ≈ +π/2 after sign-flip).
function quatY(angle: number): number[] {
  const h = angle * 0.5
  return [0, Math.sin(h), 0, Math.cos(h)]
}

// Axis-angle quaternion around +X (model-local roll). Used for the maxRoll
// override sibling of maxPitch; rarely populated on stock data but custom
// maps occasionally set it.
function quatX(angle: number): number[] {
  const h = angle * 0.5
  return [Math.sin(h), 0, 0, Math.cos(h)]
}

// Hamilton product a * b — quaternion composition. Convention matches gl-matrix
// (and HiveWE's glm): the result rotates by b FIRST, then by a. The lib's
// `rotateLocal` takes a single quaternion and right-multiplies it into the
// instance's local rotation, so to apply yaw → pitch → roll in that order
// (matching HiveWE's doodad.ixx update sequence) we hand it a product where
// yaw is the leftmost factor.
function quatMul(a: number[], b: number[]): number[] {
  const ax = a[0], ay = a[1], az = a[2], aw = a[3]
  const bx = b[0], by = b[1], bz = b[2], bw = b[3]
  return [
    aw * bx + ax * bw + ay * bz - az * by,
    aw * by - ax * bz + ay * bw + az * bx,
    aw * bz + ax * by - ay * bx + az * bw,
    aw * bw - ax * bx - ay * by - az * bz,
  ]
}

// Rarity-weighted sequence picker ported from
// mdx-m3-viewer/src/viewer/handlers/w3x/standsequence.ts (default export).
// The lib doesn't re-export it from its index, so we inline the algorithm.
// Parameterized on `type` since walk/death/etc. follow the same rules.
//
// Algorithm: normalize each sequence's name ("Stand - 1" → "stand"), sort
// the filtered list by ascending rarity, then walk: rarity 0 sequences form
// the "common" pool; rarity ≥ 1 sequences are each accepted with probability
// (rarity / 10). First acceptance wins; if none, uniform-random over the
// remaining (rarity-0) common pool. MDX `rarity` ranges 0..10 in practice.
function filterSequencesByType(
  type: string,
  sequences: ReadonlyArray<{ name: string; rarity: number }>,
): { index: number; rarity: number }[] {
  const filtered: { index: number; rarity: number }[] = []
  for (let i = 0; i < sequences.length; i++) {
    const s = sequences[i]
    if (!s) continue
    const normalized = String(s.name).split('-')[0].replace(/\d/g, '').trim().toLowerCase()
    if (normalized === type) filtered.push({ index: i, rarity: s.rarity })
  }
  return filtered
}

function selectSequenceIndex(
  type: string,
  sequences: ReadonlyArray<{ name: string; rarity: number }>,
): number {
  const filtered = filterSequencesByType(type, sequences)
  if (filtered.length === 0) return -1
  filtered.sort((a, b) => a.rarity - b.rarity)
  let i = 0
  for (; i < filtered.length; i++) {
    const rarity = filtered[i].rarity
    if (rarity === 0) break
    if (Math.random() * 10 > rarity) return filtered[i].index
  }
  const sequencesLeft = filtered.length - i
  if (sequencesLeft <= 0) return filtered[filtered.length - 1].index
  return filtered[i + Math.floor(Math.random() * sequencesLeft)].index
}

function rollSequence(instance: any, type: string): boolean {
  const seqs = instance?.model?.sequences
  if (!Array.isArray(seqs)) return false
  const idx = selectSequenceIndex(type, seqs)
  if (idx < 0) return false
  instance.setSequence(idx)
  return true
}

export type PickKind = 'unit' | 'doodad'
export interface PickHit { kind: PickKind; id: number }

// Imported from terrain-picker so consumers can reference the result shape
// without a separate import. Re-exported below the interface stub.
import { pickTerrainCell, rayGroundPlane, type TerrainCellInfo } from './terrain-picker'
export type { TerrainCellInfo } from './terrain-picker'
export type TerrainPickCallback = (cell: TerrainCellInfo | null) => void
/**
 * A terrain BRUSH stroke event while the Terrain Palette is armed. `phase`
 * tracks the drag lifecycle so the caller can bracket the whole stroke in one
 * undo group:
 *   - 'start' fires on mousedown (open the undo group, paint the first dab),
 *   - 'paint' fires as the cursor crosses into a new cell during the drag,
 *   - 'end'   fires on mouseup / Escape (close the undo group). On 'end' `cell`
 *             is the last-painted cell (or null if the stroke never landed).
 * `cell` carries the footprint CENTER (BL-corner col/row + world XY).
 */
export type TerrainBrushPhase = 'start' | 'paint' | 'end'
export type TerrainBrushCallback = (cell: TerrainCellInfo | null, phase: TerrainBrushPhase) => void
/** Brush footprint descriptor the palette pushes to the scene for the cursor. */
export interface TerrainBrushShape {
  radius: number
  shape: 'circle' | 'square'
  // Fixed water-surface Z to preview under the cursor (Add Water + Fixed height).
  // null/undefined = no preview plane.
  waterPlaneZ?: number | null
}
/**
 * A click-to-place hit while placement mode is armed. `worldX`/`worldY` are
 * the game-coordinate intersection of the click ray with the ground, and `z`
 * is the terrain height sampled (bilinearly) at that point so a newly-placed
 * doodad sits on the surface instead of at sea level. `null` signals the user
 * cancelled placement (Escape) — the caller should disarm.
 */
export interface PlacementHit { worldX: number; worldY: number; z: number }
export type PlacementPickCallback = (hit: PlacementHit | null) => void
/**
 * How a pick result combines with the current selection.
 *   - 'set'    — replace selection with hits (plain click; empty hits clears)
 *   - 'add'    — union hits into the current selection (shift+click, rubber-band)
 *   - 'toggle' — XOR hits with the current selection (ctrl+click)
 */
export type SelectMode = 'set' | 'add' | 'toggle'
export type PickCallback = (hits: PickHit[], mode: SelectMode) => void

export interface SceneAPI {
  /**
   * Re-populate the scene from current Go-side state. Called on map-changed.
   * Frames the camera at the loaded map by default; pass `keepCamera: true`
   * to preserve the current camera position (used when reloading the same
   * map — e.g. after a graphics-mode toggle — so the user doesn't lose
   * their viewpoint).
   */
  loadMap(opts?: { keepCamera?: boolean }): Promise<void>
  /**
   * Rebuild the sky-model renderer using `path` (a normalized editor path
   * like "environment/sky/.../....mdx", or "" to drop the dome entirely and
   * fall through to the gradient backdrop). Disposes the prior sky cleanly
   * before loading the new one. Idempotent on equal paths.
   *
   * Called by the Map Info → Sky picker after App.SetSkyModel so the user
   * sees their pick immediately, without re-running loadMap() (which would
   * rebuild terrain + every unit and doodad for what's a 1-mdx change).
   */
  reloadSky(path: string): Promise<void>
  /** Drop every instance we created. Models stay in the viewer cache. */
  clear(): void
  /** Stop the RAF loop and tear down listeners. */
  dispose(): void
  /**
   * Paint the highlight tint on selected instances. Pass split sets so the
   * scene can tint units and doodads independently — the underlying instance
   * maps are kind-separated and a single Set can't disambiguate creation
   * numbers that happen to collide across kinds.
   *
   * Sloc markers piggyback on the unit set (their type_id is "sloc" in the
   * unit DTO; they're rendered separately but selected as units).
   */
  setSelected(units: Set<number>, doodads: Set<number>): void
  /**
   * Register the picker callback. Fires for every selection-affecting gesture
   * in the viewport: plain click, shift/ctrl click, shift-drag rubber-band,
   * empty-click (hits=[], mode='set' to clear), Escape (same as empty-click).
   * Modifier-held clicks on empty space are intentionally swallowed — losing
   * a multi-step selection to a stray click in the void is more annoying than
   * losing the no-op gesture.
   */
  onPick(cb: PickCallback): void
  /**
   * Flip the MDX handler's reforged flag in-place. Drops cached MDX models +
   * team-color textures so the next loadMap() reloads them under the new mode.
   * Caller is responsible for triggering loadMap() after this.
   */
  setReforgedMode(reforged: boolean): void
  /** Current reforged flag — for UI display. */
  isReforged(): boolean
  /**
   * Drop every cached model + texture resource so the next loadMap()
   * re-fetches them. Used after a live CASC remount (the user located their
   * WC3 install at runtime): assets that 404'd against the previous missing
   * install are cached as empty/failed in the viewer's resourceMap and would
   * otherwise stay broken until a full restart. Detaches instances and
   * re-primes team textures, mirroring setReforgedMode's cache flush minus
   * the mode flip. Caller triggers loadMap() afterward.
   */
  purgeResourceCache(): void
  /** Move the camera pivot to (x, y) in world XY. Z defaults to 0 (ground plane). */
  panTo(x: number, y: number, z?: number): void
  /**
   * Camera handle. Lets the on-screen orbit gizmo read current yaw/pitch
   * and drive `setOrbit`/`resetOrbit` without poking the `window.__camera`
   * dev hook (which only exists for verification scripts).
   */
  getCamera(): RTSCamera
  /**
   * Toggle the diagnostics heartbeat: a bright bar drawn directly into the
   * canvas each frame so we can SEE whether the canvas is being presented.
   */
  setDiagnosticsMode(on: boolean): void
  /** Snapshot of live render + camera + GL state for the diagnostics overlay. */
  getDiagnostics(): {
    frame: number
    fps: number
    crashed: boolean
    crashReason: string
    crashAtFrame: number
    diagnosticsArmed: boolean
    logLevel: string
    doodads: number
    units: number
    visible: number
    terrainWidth: number
    terrainHeight: number
    centerOffset: [number, number]
    hasSky: boolean
    hasWater: boolean
    hasTerrain: boolean
    glRenderer: string
    lastGlError: number
    glErrFrame: number
    glErrStage: string
    camera: { eye: [number, number, number]; pivot: [number, number, number]; distance: number; yaw: number; pitch: number }
  } & Record<string, unknown> // registry namespaces (collectDiag) spread in at runtime
  /**
   * Re-position an already-placed unit instance to (x, y, z) in game-space
   * coordinates (the same coords stored in war3map.doo / GetUnit's Position).
   * Applies the type's move_height Z offset internally to match what placeUnit
   * does on initial placement, so callers pass in raw game coords.
   *
   * No-op when the unit isn't in the unit-instance map — slocs (rendered by
   * slocRenderer, not unitInstances), doodads, and creation_numbers from a
   * stale-but-since-removed unit all silently return. The Properties-panel
   * single-unit position-edit gate already blocks the bad cases on the
   * caller side; this is defense-in-depth.
   *
   * mdx-m3-viewer's render loop is already RAFing — the new position renders
   * on the next tick without any explicit invalidation call here.
   */
  updateUnitPosition(creationNumber: number, x: number, y: number, z: number): void
  /**
   * Dev-only: pin a unit to a named animation (e.g. 'Walk', 'Death'). Empty
   * string or 'stand' returns the unit to the idle reroll loop. Unknown names
   * are ignored. Returns true if a sequence matching `animName` was found.
   * Routed in from App.SetUnitAnimation via Wails event.
   */
  setUnitAnimation(creationNumber: number, animName: string): boolean
  /** Toggle the pathing-map overlay. Defaults to off. */
  setPathingVisible(visible: boolean): void
  /** Current pathing-overlay visibility — for UI display. */
  isPathingVisible(): boolean
  /** Toggle viewport visibility of start-location markers. Defaults to on. */
  setSlocsVisible(visible: boolean): void
  /** Current start-location-marker visibility — for UI display. */
  areSlocsVisible(): boolean
  /**
   * Toggle terrain-pick mode. When active, plain LMB clicks on the canvas
   * route to the terrain picker (onTerrainPick callback) instead of the
   * entity ray-pick. Drag-pan + rubber-band + camera controls still work.
   * Cursor changes to crosshair over the canvas when active.
   */
  setTerrainPickMode(active: boolean): void
  /** Current terrain-pick-mode flag — for UI display. */
  isTerrainPickMode(): boolean
  /**
   * Register the terrain-pick callback. Fires on canvas click when terrain
   * pick mode is active. `cell` is null when the click misses the map (e.g.
   * sky background or outside the map's bounds).
   */
  onTerrainPick(cb: TerrainPickCallback): void
  /**
   * Toggle terrain-BRUSH mode (the Terrain Palette's paint engine). When active,
   * a plain LMB press-drag-release on the canvas streams a brush stroke to the
   * onTerrainBrushStroke callback (start → paint… → end) instead of selecting.
   * Takes priority over terrain-pick + placement. Camera pan (RMB/MMB) still
   * works; Escape ends the stroke. Cursor shows the footprint ring.
   */
  setTerrainBrushMode(active: boolean): void
  /** Current terrain-brush-mode flag — for UI display. */
  isTerrainBrushMode(): boolean
  /** Register the terrain-brush stroke callback (see TerrainBrushCallback). */
  onTerrainBrushStroke(cb: TerrainBrushCallback): void
  /** Set the brush footprint (radius in corner units + shape) for the cursor. */
  setTerrainBrushShape(shape: TerrainBrushShape): void
  /**
   * Toggle doodad-placement mode. When active, a plain LMB click on the canvas
   * fires the onPlacementPick callback with the ground-plane intersection and
   * sampled terrain height (instead of entity ray-pick or terrain inspect).
   * Takes priority over terrain-pick mode. Cursor shows crosshair. Camera pan
   * (RMB/MMB drag) still works; Escape fires the callback with null so the
   * caller can disarm. Stays armed across placements so the user can drop
   * several doodads without re-selecting from the palette each time.
   */
  setPlacementMode(active: boolean): void
  /** Current placement-mode flag — for UI display. */
  isPlacementMode(): boolean
  /** Register the placement callback (see setPlacementMode / PlacementHit). */
  onPlacementPick(cb: PlacementPickCallback): void
  /**
   * Arm (or swap) the preview ghost — a tinted, non-interactive instance of
   * `typeId` that follows the cursor across the terrain while placement mode
   * is active, so the user sees the model and its footprint before clicking.
   * Pass null to drop the preview. The ghost is purely visual: it never enters
   * selection, the View-menu visibility toggles, pick tests, or the saved map.
   * Disarming placement mode (setPlacementMode(false)) also drops it.
   */
  setPlacementGhost(typeId: string | null, types: Record<string, DoodadTypeInfo>): void
  /**
   * Add a single doodad to the live scene from its DTO, without a full map
   * reload. Used by the Doodad Palette after CreateDoodad allocates a
   * creation_number: the caller pushes the DTO into its own doodads array and
   * calls this to render the new instance immediately. Resolves to true when
   * the MDX was placed (false when the type has no model / failed to load).
   * Refreshes the present-categories list so a doodad in a not-yet-present
   * category becomes toggleable in the View menu.
   */
  addDoodadLive(d: any, types: Record<string, DoodadTypeInfo>): Promise<boolean>
  /**
   * Add a single unit to the live scene from its DTO, without a full map
   * reload. Mirror of addDoodadLive for the Unit Palette after CreateUnit
   * allocates a creation_number. Resolves to true when the MDX was placed
   * (false when the type has no model / failed to load — the caller then
   * falls back to a full reload so the unit still appears in the Explorer).
   */
  addUnitLive(u: any, types: Record<string, UnitTypeInfo>): Promise<boolean>
  /**
   * Whether a unit/doodad creation_number currently has a rendered instance in
   * the scene. Used by the entity-changed `created` handler to skip a redundant
   * reload when the instance was already added live (e.g. the Doodad Palette's
   * own addDoodadLive). Returns false for kinds without per-cn MDX instances
   * (slocs, path blockers) and for any other kind.
   */
  hasInstance(kind: string, cn: number): boolean
  /**
   * Set the currently-highlighted terrain cell (yellow wireframe overlay).
   * `null` clears the highlight. The cell persists across frames until
   * replaced or cleared — caller controls when to drop it (e.g. on map open
   * or explicit Escape). Persistence here matches the Properties-panel
   * convention: the panel keeps the last-picked cell info around so the user
   * can compare cells, and the highlight does the same.
   */
  setHighlightedCell(cell: { col: number; row: number } | null): void
  /**
   * Replace the region-rectangle overlay's region list (war3map.w3r). Each
   * rect is drawn as a flat outline on the ground plane. Pass [] to clear.
   * App.svelte mirrors the Region panel's list here and re-pushes after any
   * region create/move/resize/delete so the overlay tracks edits live.
   */
  setRegionOverlay(regions: RegionRect[]): void
  /**
   * Highlight the region with this creation_number in the overlay (bright cyan),
   * or null to clear the highlight. Mirrors the panel's selected region.
   */
  setSelectedRegion(creationNumber: number | null): void
  /**
   * Show or hide the entire region-rectangle overlay in the viewport. Pure
   * render state — the regions themselves are untouched. App.svelte persists
   * the preference to localStorage and re-applies it on every map load.
   */
  setRegionOverlayVisible(visible: boolean): void
  /**
   * Enter/exit region edit mode. While active the viewport owns region editing:
   * drag = draw a new region rect, click = select the region under the cursor;
   * entity selection + box-select are suppressed.
   */
  setRegionMode(active: boolean): void
  /** Register the callback fired with world bounds when a region is drag-drawn. */
  onRegionDraw(cb: (minX: number, minY: number, maxX: number, maxY: number) => void): void
  /** Register the callback fired with the clicked region's creation_number (or null). */
  onRegionPick(cb: (cn: number | null) => void): void
  /**
   * Hide or show every doodad instance in a category. Pass "*" to affect
   * every doodad. Visibility is rendering-only — the underlying data is
   * unchanged, never persisted. Re-applied on every loadMap so hidden
   * categories stay hidden across map opens.
   *
   * Implementation: per-instance `hide()` / `show()`. The lib's
   * `setScene(null)` would crash (it calls `null.addInstance(this)`); its
   * own ModelInstance docs explicitly recommend hide/show for W3X work:
   *
   *   "When working with Warcraft 3 instances, it is preferable to use
   *    hide() and show(). These hide and show also internal instances of
   *    this instance."
   *
   * The MdxModelInstance subclass overrides hide() to recurse into
   * attachment instances too, which is exactly what we want — otherwise
   * weapons / attached effects would render free-floating.
   */
  setDoodadCategoryVisible(category: string, visible: boolean): void
  /**
   * Categories currently present in the loaded map's doodads (order matches
   * App.svelte's DOODAD_CAT_ORDER curated-first ordering). Empty when no map
   * is loaded.
   */
  getDoodadCategories(): string[]
  /**
   * Project the camera's view frustum onto the z=0 ground plane. Returns the
   * 4 world (X, Y) intersection points for the canvas corners in TL → TR →
   * BR → BL order (matching the order callers will draw the polygon outline).
   *
   * Returns null if any corner's ray fails to hit the plane (e.g. ray
   * parallel to ground, intersection behind camera, or any intermediate
   * NaN). The camera's PITCH_MIN clamp keeps us out of the parallel-ray
   * regime in normal use; the null return is defensive for any unexpected
   * camera state. Callers (the minimap overlay) skip drawing on null.
   *
   * Cheap to call per-frame: 4 screenToWorldRay calls + 4 ray/plane
   * intersections, all of which are already happening elsewhere in the
   * pipeline for terrain picking. No allocations beyond the returned array.
   */
  getViewportFrustumCorners(): Array<[number, number]> | null
  /**
   * Set the gizmo's active mode (move/rotate/scale). Cancels any in-progress
   * drag. No-op if mode is unchanged. The mode-switch UI in App.svelte routes
   * here on button click or W/E/R hotkey.
   */
  setGizmoMode(mode: 'move' | 'rotate' | 'scale'): void
  /** Current gizmo mode — for UI display. */
  getGizmoMode(): 'move' | 'rotate' | 'scale'
  /**
   * Update gizmo snap settings (Phase D). Partial — undefined keys leave
   * current values intact. Step units: move=studs, rotate=degrees, scale=
   * multiplier. Shift held during drag inverts the channel's snap state
   * for that drag only (Blender convention).
   */
  setGizmoSnap(s: {
    moveOn?: boolean; moveStep?: number
    rotateOn?: boolean; rotateStep?: number
    scaleOn?: boolean; scaleStep?: number
  }): void
  /** Read current snap settings — for UI display. */
  getGizmoSnap(): {
    moveOn: boolean; moveStep: number
    rotateOn: boolean; rotateStep: number
    scaleOn: boolean; scaleStep: number
  }
  /**
   * Frame-selected: move the camera pivot to the centroid of the current
   * selection and zoom in so the bounding box of the selection fits the
   * viewport. Orbit angles are preserved. Returns true if anything was
   * focused; false when the selection is empty or none of the selected
   * items have a live world position yet. Bound to the 'f' hotkey.
   */
  focusSelection(): boolean
  /**
   * Dev-only pick-correctness harness. Walks every visible instance, projects
   * its center to canvas pixels, rayPicks at that pixel, and tallies whether
   * the returned hit matches the queried instance. Triggered by Go's
   * --pick-self-test startup flag (App.svelte routes the
   * 'wc3-forge:pick-self-test' event here). Production code shouldn't call
   * this — it exists for verification only.
   */
  __runPickSelfTest?(): { tested: number; matched: number; mismatched: number; skipped: number; nearMatchDist: number }
}

export function createScene(canvas: HTMLCanvasElement, reforged: boolean = false): SceneAPI {
  const { ModelViewer, handlers } = getMV()

  // mdx-m3-viewer reads canvas.width/height as PIXELS — keep them synced
  // with the CSS box or everything's blurry (lib README's headline gotcha).
  const sizeToBox = () => {
    const w = Math.max(1, Math.floor(canvas.clientWidth))
    const h = Math.max(1, Math.floor(canvas.clientHeight))
    if (canvas.width !== w) canvas.width = w
    if (canvas.height !== h) canvas.height = h
  }
  sizeToBox()

  const viewer = new ModelViewer(canvas)
  ;(window as any).__viewer = viewer // devtools inspection
  flog(`[scene] init reforged=${reforged}`)

  // Handler order matters: BLP/DDS/TGA before MDX so the MDX loader can
  // route texture loads to the right decoder. Texture handlers don't take
  // pathSolver/options; MDX handler does — its third arg (reforged) toggles
  //   - 28 team-color textures (vs 16 in SD)
  //   - .dds team-color URL extension (vs .blp in SD)
  //   - HD-aware sound + splat lookups in getEventObjectData
  // The shader variant (sd vs hd) is chosen per-batch at draw time based on
  // each model's own `solverParams.hd` flag, not this global.
  viewer.addHandler(handlers.blp)
  viewer.addHandler(handlers.dds)
  viewer.addHandler(handlers.tga)
  viewer.addHandler(handlers.mdx, pathSolver, /* reforged */ reforged)

  // Team colors aren't loaded automatically when using the lower-level API.
  // Pre-warm them so units have player tint from the first frame they render.
  let currentReforged = reforged
  try { handlers.mdx.loadTeamTextures(viewer) } catch (e) {
    flog('[loadTeamTextures]', e instanceof Error ? e.message : String(e))
  }

  const scene = viewer.addScene()
  // alpha=true → scene.startFrame() WON'T clear the framebuffer. We render
  // terrain ourselves between viewer.startFrame() and viewer.render(), so
  // we need the depth buffer to survive into the scene's render pass.
  ;(scene as any).alpha = true
  ;(scene as any).color = [0.07, 0.07, 0.09] // matches the app's dark chrome
  // Light MDX (HD) models from the SAME direction terrain.ts lights the ground —
  // HiveWE's normalize(1, 1, -3) — so units/doodads aren't shaded against the
  // terrain. lightPosition is a world-space point; scale the direction out far
  // so it reads as directional. (mdx-m3-viewer Scene.lightPosition; affects HD.)
  ;(scene as any).lightPosition = (() => {
    const x = 1, y = 1, z = -3
    const len = Math.sqrt(x * x + y * y + z * z)
    const dist = 10000
    return new Float32Array([(x / len) * dist, (y / len) * dist, (z / len) * dist])
  })()

  // WC3-style RTS camera. Mutates scene.camera in response to mouse/keyboard;
  // initial state has it framing the world origin from a 45°-down angle, and
  // loadMap() will re-frame whenever a new map opens.
  const camera = createCamera(canvas, scene.camera)
  ;(window as any).__camera = camera // devtools + verification-script access

  // Visible-error logger: dedupe by (message + target URL) so a single
  // missing asset doesn't spam the log thousands of times per second.
  const seenErrors = new Set<string>()
  viewer.on('error', (target: unknown, error: unknown) => {
    const msg = error instanceof Error ? error.message : String(error)
    const url = (target as any)?.fetchUrl ?? '(no url)'
    const key = msg + ' :: ' + url
    if (seenErrors.has(key)) return
    // Bump the diagnostics counter BEFORE the dedupe-gate so the FIRST
    // occurrence of each distinct (msg, url) is counted once even though the
    // log dedupes (the overlay wants "how many distinct asset failures", not
    // the per-frame spam count).
    sceneDiag.viewerErrors++
    seenErrors.add(key)
    flogWarn('[viewer error]', msg, 'url:', url)
  })

  let crashed = false
  let rafId = 0
  let terrain: TerrainMesh | null = null
  let water: WaterMesh | null = null
  let cliffs: CliffRendering | null = null
  let sky: SkyRenderer | null = null
  // Sky gradient is process-stable (doesn't depend on the map). Build once
  // here; clearInstances doesn't touch it.
  let skyGradient: SkyGradient | null = null
  try {
    skyGradient = buildSkyGradient((viewer as any).gl as WebGLRenderingContext, viewer as any)
  } catch (e) {
    flog('[sky-gradient] build failed:', e instanceof Error ? e.message : String(e))
  }
  let slocRenderer: SlocRenderer | null = null
  // Start-location marker visibility — toggled from the Explorer "Markers"
  // section eye button. Default on; remembered here so a rebuilt renderer
  // (new map load) re-applies the user's current choice.
  let slocsVisible = true
  let pathing: PathingOverlay | null = null
  // Pathing overlay visibility — toggled from the header pill. Default off
  // to match HiveWE; users opt in when debugging unit placement / arena
  // boundaries.
  let pathingVisible = false
  // Tracks the current selection so we can paint a tint on selected unit
  // instances on every render-applicable state change (selection arrives via
  // setSelected; the tint persists across loadMap since we re-apply on every
  // placeUnit call when the cn is in this set). Declared here (before the
  // render loop's first invocation) to satisfy TDZ — the loop closure
  // captures the binding name and would crash on first invocation otherwise.
  let selectedSet: Set<number> = new Set()
  // Doodad-side analogue of selectedSet. Creation_number is per-kind in WC3
  // (a unit and a doodad can share the same numeric ID), so selection state
  // for doodads lives in its own set and the renderer mirrors that split.
  let selectedDoodadSet: Set<number> = new Set()
  // Creation numbers whose animation is being driven manually via the dev
  // SetUnitAnimation hook. The per-frame stand re-roll skips these so a
  // user-pinned 'Walk' or 'Death' doesn't get clobbered on the next idle tick.
  // Doodads aren't keyed by creation_number on our side (we don't track them
  // by ID), so the dev hook is units-only — doodads always idle-reroll.
  let manualAnimCns: Set<number> = new Set()
  // Active instances we placed during the most recent loadMap. Keyed by
  // creation_number so selection events can highlight individually. Models
  // remain cached in viewer.resourceMap across loads — only instances are
  // dropped on re-open. Hoisted here (before the render loop's first
  // invocation) so the per-frame stand-reroll's iteration doesn't trip TDZ.
  // Units and doodads use separate maps because creation_number is per-kind
  // in WC3 — same reason the selected* sets above are split.
  const unitInstances = new Map<number, any>()
  const doodadInstances = new Map<number, any>()
  // Path blockers are doodads but NOT MDX instances (they're procedural boxes
  // in path-blockers.ts), so they aren't in doodadInstances and the gizmo can't
  // find them. We keep a lightweight proxy per blocker exposing just what the
  // gizmo reads — worldLocation (= the marker's position array, so live drags
  // and the gizmo centroid track it) and setLocation (mutates that array so the
  // box re-renders at the new spot). All other fields the gizmo touches have
  // safe defaults, and moveHeight is 0 for doodads, so MoveDoodad persists the
  // drag exactly like a normal doodad. Rebuilt whenever the marker list changes.
  const blockerProxies = new Map<number, { worldLocation: [number, number, number]; setLocation(p: [number, number, number]): void }>()
  function rebuildBlockerProxies(markers: PathBlockerMarker[]) {
    blockerProxies.clear()
    for (const m of markers) {
      blockerProxies.set(m.creationNumber, {
        worldLocation: m.position, // same array ref path-blockers renders from
        setLocation(p: [number, number, number]) {
          m.position[0] = p[0]; m.position[1] = p[1]; m.position[2] = p[2]
        },
      })
    }
  }
  // Doodad lookup the GIZMO sees: real MDX doodads, falling back to blocker
  // proxies. Passed to gizmo.draw / gizmo.beginDrag so path blockers move like
  // any doodad. The gizmo only ever calls .get() on this, so a thin shim is
  // enough (and avoids polluting doodadInstances, which the outline pass walks).
  const gizmoDoodadLookup = {
    get: (id: number) => doodadInstances.get(id) ?? blockerProxies.get(id),
  } as unknown as Map<number, any>
  // Creation-number of the doodad currently under the cursor (null = none).
  // Declared up here with the other instance state — the render loop reads it
  // each frame to drive the hover-outline pass, and the loop is kicked off
  // BEFORE the hover-helpers block below, so a later `let` would TDZ on frame 1.
  let hoveredDoodad: number | null = null
  // Creation-number of the unit/building under the cursor (null = none). Units
  // get the cyan hover OUTLINE but no vertex fill (a fill would clobber their
  // team-colour). At most one of hoveredDoodad / hoveredUnit is set at a time.
  let hoveredUnit: number | null = null
  // Live rubber-band preview: while a selection box is being dragged, these hold
  // the creation-numbers currently inside it so the render loop can cyan-outline
  // everything that WOULD be selected on release. Empty when not dragging.
  // (Doodad set includes path-blocker cns — they're kind 'doodad'.)
  const rubberPreviewUnits = new Set<number>()
  const rubberPreviewDoodads = new Set<number>()
  // Outline-glow colours for the two effects, read by the render loop (so they
  // live here, ahead of the loop kickoff, to dodge the same frame-1 TDZ).
  // Selection = white; hover = cyan. Tunable.
  const SELECT_OUTLINE_COLOR: readonly [number, number, number] = [1, 1, 1]
  const HOVER_OUTLINE_COLOR: readonly [number, number, number] = [0.35, 0.95, 1.0]
  // Per-doodad-instance category tag (resolved at placement time from the
  // SLK type-index). Lets setDoodadCategoryVisible flip a whole category's
  // visibility in O(visible-instances) without re-walking the type index.
  // Same set of keys as `doodadInstances`; cleared together in clearInstances.
  const doodadCategoryByCn = new Map<number, string>()
  // Per-category visibility (true = visible / default). Categories absent
  // from this map are considered visible. Persists across loadMap so hiding
  // "Trees/Destructibles" in one map keeps trees hidden when a new map opens.
  const doodadVisibility = new Map<string, boolean>()
  // Ordered list of categories present in the current map. Recomputed at the
  // end of loadMap so the View menu can render an up-to-date checkbox list.
  // Curated order first (Trees/Destructibles, Structures, …), then remainder
  // alphabetized — matches App.svelte's DOODAD_CAT_ORDER constant.
  let doodadCategoriesPresent: string[] = []
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
  // Terrain-pick mode state. When true, plain LMB clicks fire the terrain-
  // pick callback instead of the entity ray-pick. The most-recent TerrainDTO
  // is cached on loadMap so the picker reads cell data without a round-trip
  // to Go.
  let terrainPickMode = false
  let terrainPickCallback: TerrainPickCallback | null = null
  let cachedTerrainDTO: any = null
  // Terrain-BRUSH mode state (Terrain Palette paint engine). When active, a
  // plain LMB press-drag-release streams a brush stroke; `brushStroking` tracks
  // an in-progress drag, `brushLastCellKey` dedupes dabs so we only fire when
  // the cursor crosses into a new cell. brushShape drives the footprint cursor.
  let terrainBrushMode = false
  let terrainBrushCallback: TerrainBrushCallback | null = null
  let brushStroking = false
  let brushLastCellKey = -1
  let brushShape: TerrainBrushShape = { radius: 1, shape: 'circle' }
  // Last cursor client position while in brush mode, so a brush-shape change
  // (e.g. the fixed water-height slider) can refresh the preview without a move.
  let lastBrushClient: { x: number; y: number } | null = null
  // Doodad-placement mode state. When true, plain LMB clicks fire the
  // placement callback (with the ground intersection + sampled terrain Z)
  // instead of entity-pick / terrain-inspect. Checked BEFORE terrainPickMode
  // in the click handler so an armed palette brush wins over terrain mode.
  let placementMode = false
  let placementCallback: PlacementPickCallback | null = null
  // Placement ghost — a translucent-tinted preview instance of the armed
  // doodad that follows the cursor across the terrain (HiveWE / World Editor
  // behavior) so the user sees what they're about to drop and where. Built by
  // setPlacementGhost when a type is armed, repositioned in the hover handler,
  // and torn down whenever placement mode turns off. It lives OUTSIDE
  // doodadInstances/doodadCategoryByCn (it's not a real map entity) so it never
  // leaks into selection, visibility toggles, pick tests, or save. The load
  // token guards against a stale async model load landing after a re-arm or
  // disarm raced ahead of it.
  let placementGhostInst: any = null
  let placementGhostTypeId: string | null = null
  let placementGhostLoadToken = 0
  let placementGhostPlaced = false // shown only after the first positioned move
  // Cool cyan wash so the preview reads as "not placed yet." Alpha is ignored
  // by opaque doodad layers (no blend mode), so the tint — not transparency —
  // is what distinguishes the ghost from a real instance.
  const GHOST_TINT: [number, number, number, number] = [0.4, 0.85, 1.2, 1]
  // Doodad instances whose model has at least one 'stand' sequence AND that
  // sequence may end (either non-looping, or multi-variation where we want
  // to re-roll a new variation when the current one ends). These need
  // per-frame sequenceEnded checks so the idle plays continuously.
  //
  // Two cases share this path:
  //   1. Multi-variation Stand (e.g. "Stand", "Stand - 2", …):
  //      reroll picks a new variation via rarity weighting, matching the
  //      library's Widget.update idle pattern.
  //   2. Single Stand sequence marked nonLooping=1 (e.g. EFFECT doodads
  //      like Enfo FFB's "Soul Discharge"): reroll re-sets the same sequence
  //      index, which `setSequence` implements as a clean restart (frame =
  //      interval[0], event-emitters reset, forced=true). Without this the
  //      effect plays once then freezes — HiveWE's skeletal_model_instance
  //      always loops regardless of the nonLooping flag (it just wraps
  //      current_frame back to start_frame in update() — see HiveWE's
  //      src/resources/skinned_mesh/skeletal_model_instance.ixx ~line 145),
  //      so we mirror that behaviour for nonLooping-Stand doodads here.
  //
  // Doodads with no 'stand' sequence at all stay out of the set (no-op idle).
  // Doodads with a single LOOPING Stand also stay out — the library auto-loops
  // those internally and `sequenceEnded` never fires, so adding them would
  // just waste an iteration per RAF tick.
  const doodadInstancesToReroll = new Set<any>()

  // Sloc-marker renderer. Renders start locations as CircleOfPower MDX
  // instances when the viewer/scene + CASC are available, with a primitive
  // pillar fallback if the MDX fails to load. Passing unitInstances lets
  // sloc-markers register its MDX instances in the scene's master cn map
  // so the gizmo / drag-to-move / entity-changed flows below treat slocs
  // identically to real units — slocs ARE units in WC3 (type_id "sloc"
  // in war3mapUnits.doo with full position/rotation/scale fields), so any
  // mutation API that works on units should work on them. Failure to
  // build the fallback shader is non-fatal: we just won't have visible
  // markers. Built here (after `let slocRenderer = null` above) rather
  // than earlier, because assigning to a `let` before its declaration is
  // a TDZ violation in JS.
  try {
    slocRenderer = buildSlocRenderer(
      (viewer as any).gl as WebGLRenderingContext,
      viewer,
      scene,
      unitInstances,
    )
    // Re-apply the current toggle so a hidden-markers preference survives a
    // map reload (which rebuilds the renderer).
    slocRenderer?.setVisible(slocsVisible)
  } catch (e) {
    flog('[slocs] init failed:', e instanceof Error ? e.message : String(e))
  }
  // Cell-highlight overlay (yellow wireframe quad at the currently-picked
  // terrain cell). Built once per scene; the cell + terrain references are
  // updated via setHighlightedCell / loadMap. Non-fatal if build fails.
  let cellHighlight: CellHighlight | null = null
  try {
    cellHighlight = buildCellHighlight((viewer as any).gl as WebGLRenderingContext)
  } catch (e) {
    flog('[cell-highlight] init failed:', e instanceof Error ? e.message : String(e))
  }
  // Region (rect) outlines (war3map.w3r). Built once per scene; the region
  // list + selected id are pushed in via setRegionOverlay / setSelectedRegion
  // (App.svelte mirrors them from the Region panel + entity-changed events).
  // Same overlay lifetime as cellHighlight; non-fatal if build fails.
  let regionOverlay: RegionOverlay | null = null
  // When false, the region overlay is skipped in the draw loop — the regions
  // themselves are untouched. Driven by App.svelte's persisted "show regions"
  // preference (re-applied on every map load via setRegionOverlayVisible).
  let regionOverlayVisible = true
  // Region edit mode: when on, the viewport is owned by region editing — a drag
  // draws a new region rect (reusing the rubber-band overlay) and a click selects
  // the region under the cursor; entity selection + box-select are suppressed.
  // Callbacks + the rect snapshot (for click hit-testing) are set by App.svelte.
  let regionMode = false
  let regionDrawCallback: ((minX: number, minY: number, maxX: number, maxY: number) => void) | null = null
  let regionPickCallback: ((cn: number | null) => void) | null = null
  let regionRectsForPick: RegionRect[] = []
  try {
    regionOverlay = buildRegionOverlay((viewer as any).gl as WebGLRenderingContext)
  } catch (e) {
    flog('[region-overlay] init failed:', e instanceof Error ? e.message : String(e))
  }
  // Terrain-brush footprint ring (shown under the cursor while the Terrain
  // Palette is armed). Same overlay lifetime as cellHighlight; non-fatal build.
  let brushCursor: BrushCursor | null = null
  try {
    brushCursor = buildBrushCursor((viewer as any).gl as WebGLRenderingContext)
  } catch (e) {
    flog('[brush-cursor] init failed:', e instanceof Error ? e.message : String(e))
  }
  // Instance-outline overlay (glowing silhouette). Drives two effects from one
  // builder: white outline around selected units/doodads, cyan outline around
  // the hovered doodad. Built once per scene; the mask FBO is sized lazily in
  // the draw loop to match the canvas. Non-fatal if build fails — selection /
  // hover simply render without an outline.
  let outline: InstanceOutline | null = null
  try {
    outline = buildInstanceOutline((viewer as any).gl as WebGLRenderingContext)
  } catch (e) {
    flog('[instance-outline] init failed:', e instanceof Error ? e.message : String(e))
  }
  // DEBUG: when true, hovering an entity draws its rayPick bounding SPHERE
  // (magenta wireframe) instead of the cyan outline, to diagnose hover accuracy.
  // Now off — picking is pixel-accurate (see pickBuffer / rayPick narrow phase),
  // so hover shows the normal cyan outline. Toggle live from devtools via
  // window.__wc3ForgeHoverBounds = true.
  const DEBUG_HOVER_BOUNDS = false
  let boundsDebug: BoundsDebug | null = null
  try {
    boundsDebug = buildBoundsDebug((viewer as any).gl as WebGLRenderingContext)
  } catch (e) {
    flog('[bounds-debug] init failed:', e instanceof Error ? e.message : String(e))
  }
  // Pixel-accurate pick refinement: silhouette-coverage test that turns the
  // loose bounding-sphere broad phase into an on-the-model hit test. Non-fatal
  // if build fails — rayPick falls back to the nearest bounding-volume hit.
  let pickBuffer: PickBuffer | null = null
  try {
    pickBuffer = buildPickBuffer((viewer as any).gl as WebGLRenderingContext)
  } catch (e) {
    flog('[pick-buffer] init failed:', e instanceof Error ? e.message : String(e))
  }
  // Safety cap on narrow-phase silhouette tests per pick (each is a GPU render
  // + readback). Nearest-first ordering means we usually hit on the 1st; the
  // cap bounds the worst case of many overlapping bounding spheres.
  const PICK_NARROW_CAP = 16
  // Path-blocker overlay (pink/black checker quads at "Pathing Blockers"
  // category doodad positions). Built once per scene, lifetime matches the
  // other overlay renderers. Non-fatal if build fails — path blockers stay
  // invisible like they did pre-feature.
  let pathBlockers: PathBlockerRenderer | null = null
  try {
    pathBlockers = buildPathBlockerRenderer((viewer as any).gl as WebGLRenderingContext)
  } catch (e) {
    flog('[path-blockers] init failed:', e instanceof Error ? e.message : String(e))
  }
  // Transform gizmo — Phase B move-only 3-axis arrows. Drawn last (after all
  // other overlays) so it's always on top. Non-fatal if build fails; user
  // falls back to drag-to-move. Lifetime matches the other overlay renderers.
  let gizmo: GizmoRenderer | null = null
  // Axis labels (X/Y/Z) for the move gizmo. Forward-declared null here so
  // the render loop's first synchronous tick (loop() runs immediately after
  // RAF setup, before the rest of init runs) doesn't TDZ on the const that
  // used to live at the bottom of init. Populated further down once
  // overlayHost exists; updateAxisLabels null-checks before touching it.
  let axisLabelEls: Record<'x' | 'y' | 'z', HTMLDivElement> | null = null
  try {
    gizmo = buildGizmo((viewer as any).gl as WebGLRenderingContext)
    flog('[gizmo] built ok')
  } catch (e) {
    flog('[gizmo] init failed:', e instanceof Error ? e.message : String(e))
  }
  // Current selection items for the gizmo. Updated in setSelected.
  let gizmoSelectionItems: GizmoSelectionItem[] = []
  // Whether a gizmo drag is currently in progress (set in mousedown, cleared in mouseup).
  let gizmoDragging = false

  // Background color (used by our own clear call now that scene.alpha=true).
  const bg = [0.07, 0.07, 0.09]
  // Real-dt tracking for viewer.update(). Mdx-m3-viewer's contract is that
  // `viewer.update(dt)` receives MILLISECONDS elapsed since the previous
  // tick (see viewer.js line 425: `dt *= 0.001` to convert internally to
  // seconds, then each MdxModelInstance.updateAnimations multiplies back by
  // 1000 to advance KGTR/KGRT/KGSC keyframe times — which live in ms).
  //
  // The previous version passed a HARDCODED `1000 / 60` (= 16.67ms), which
  // is correct only at exactly 60 Hz. On a high-refresh display (120 Hz =
  // 8.33ms real, 144 Hz = 6.94ms real, 240 Hz = 4.17ms real) RAF fires
  // faster — but the hardcoded value still advanced the animation by 16.67ms
  // per frame, so all WC3 animations played at 2×, 2.4×, 4× speed
  // respectively. (User reported "a little faster than HiveWE" — which
  // matches the common 120Hz case.) HiveWE's `update(delta)` in
  // skeletal_model_instance.ixx uses the real frame delta from the editor's
  // QElapsedTimer; mirroring that here.
  //
  // Clamped to 250ms to keep a stalled tab from advancing animations by
  // multiple seconds on the first frame after wake (would skip past
  // non-looping idle re-rolls and look jarring). Mirrors HiveWE's
  // `std::clamp(delta, 0.0, 0.5)` on the seconds path.
  let lastFrameTs = performance.now()
  const MAX_DT_MS = 250
  // How often the always-on GL-error sample runs (ms). ~5Hz matches the
  // diagnostics push cadence and keeps the getError sync cost negligible.
  const GL_SAMPLE_MS = 200
  // Throttled culling-counters log. Emits a snapshot of the per-frame
  // tested/visible/frustum-culled/lod-culled counts every 5 seconds so we
  // can observe the cull/LOD pipeline working without spamming the log.
  // Drop the interval to 0 (set window.__wc3ForgeCullLogMs = 0) at runtime
  // to disable; bump it to 250 for finer-grained debugging.
  let lastCullLogTs = performance.now()
  ;(window as any).__wc3ForgeCullLogMs = 5000

  // --- Diagnostics instrumentation -----------------------------------------
  // frameCount increments once per render-loop body execution (i.e. once per
  // RAF tick the loop actually ran). fps is a smoothed estimate. These feed the
  // DiagnosticsOverlay so we can see whether the loop is advancing AND, via the
  // in-canvas heartbeat below, whether those frames are actually being
  // PRESENTED to screen — the crux of the "frozen viewport" investigation.
  let frameCount = 0
  let fps = 0
  let diagnosticsMode = false
  // GL error, SAMPLED at ~5Hz from the render loop (not per-frame — getError
  // forces a CPU↔GPU sync). glErrFrame is the frame the non-zero error was last
  // seen on; lastGlSampleTs paces the sampling.
  let lastGlError = 0
  let glErrFrame = 0
  let lastGlSampleTs = 0
  // Crash detail: when the render-loop body throws, we latch crashed=true plus
  // the reason + frame so diagnostics.get explains a frozen viewport instead of
  // just reporting frame-not-advancing.
  let crashReason = ''
  let crashAtFrame = 0
  // Records the FIRST render stage that produced a GL error this frame, e.g.
  // "render:0x502". Populated by glChk() probes interleaved in the loop while
  // diagnosticsMode is on, so we can pinpoint which pass is faulting.
  let glErrStage = ''
  let glRenderer = ''
  try {
    const gldbg = (viewer as any).gl as WebGLRenderingContext
    const ext = gldbg.getExtension('WEBGL_debug_renderer_info')
    glRenderer = ext ? String(gldbg.getParameter(ext.UNMASKED_RENDERER_WEBGL)) : String(gldbg.getParameter(gldbg.RENDERER))
  } catch { glRenderer = '' }


  const loop = () => {
    if (!crashed) {
      try {
        sizeToBox()
        // Push the canvas aspect through the camera controller; setAspect
        // no-ops if unchanged, so this is cheap to call every frame.
        camera.setAspect(canvas.width / Math.max(1, canvas.height))
        // Scene viewport defaults to [0,0,0,0]; keep it pinned to the canvas
        // so frustum culling + (later) screen→world ray casts use the right
        // pixel dims.
        const vp = (scene as any).viewport
        if (vp) {
          vp[0] = 0; vp[1] = 0; vp[2] = canvas.width; vp[3] = canvas.height
        }
        // Split updateAndRender so we can interleave our terrain pass:
        //   1. update — animation/particle tick (no GL state changes)
        //   2. our manual clear (scene.alpha=true means scene won't clear)
        //   3. terrain.draw — writes color + depth across the framebuffer
        //   4. scene.render — units render on top, depth-tested vs terrain
        const gl = (viewer as any).gl as WebGLRenderingContext
        const nowTs = performance.now()
        const dtMs = Math.min(MAX_DT_MS, Math.max(0, nowTs - lastFrameTs))
        lastFrameTs = nowTs
        // Diagnostics: count frames + smooth an fps estimate.
        frameCount++
        if (dtMs > 0) fps = fps === 0 ? 1000 / dtMs : fps * 0.9 + (1000 / dtMs) * 0.1
        // Stash canvas height on the camera for the per-instance LOD path
        // (the patched ModelInstance.isVisible reads this to compute pixel-
        // radius without walking scene.viewport for every instance) and
        // reset per-frame culling counters so DevTools snapshots reflect
        // the most recent frame.
        ;(scene.camera as any).__wc3ForgeCanvasH = canvas.height
        // GL-error stage probe (diagnostics only). Records the first pass whose
        // getError() returns non-zero. Clears the error each call so the stage
        // that ORIGINATES it is attributed, not a later one inheriting it.
        // GL-error stage probing is gated on diagnosticsMode: each getError()
        // forces a GPU sync, so we don't pay it per-stage in normal use. When
        // armed, glChk attributes the FIRST faulting stage AND feeds the
        // always-on lastGlError/glErrFrame (so the armed and 5Hz-sampled paths
        // report the same field). Toggle F9 / diagnostics.arm to arm.
        glErrStage = ''
        const glChk = (stage: string) => {
          if (!diagnosticsMode || glErrStage) return
          const e = gl.getError()
          if (e) { glErrStage = `${stage}:0x${e.toString(16)}`; lastGlError = e; glErrFrame = frameCount }
        }
        if (diagnosticsMode) { gl.getError(); lastGlError = 0 } // clear stale; armed path reports per-frame
        // Log the previous frame's culling tally (before resetting) so the
        // snapshot reflects what was just rendered, not zero.
        const cullLogIntervalMs = (window as any).__wc3ForgeCullLogMs || 0
        if (cullLogIntervalMs > 0 && (nowTs - lastCullLogTs) >= cullLogIntervalMs) {
          lastCullLogTs = nowTs
          flogDebug(`[culling] ${JSON.stringify(cullingSnapshot())}`)
        }
        resetCullingCounters()
        viewer.update(dtMs)
        // Re-roll stand variation when a non-looping idle finishes. Mirrors
        // mdx-m3-viewer's Widget.update (handlers/w3x/widget.ts): MDX models
        // typically have N stand variations marked nonLooping=1, so most
        // tick once then leave `sequenceEnded=true` for us to pick a new one.
        // Units pinned to a manual sequence (via setUnitAnimation) are skipped
        // so the user's poke isn't overwritten. Doodads have no per-instance
        // ID on our side, so they always idle-reroll.
        for (const [cn, inst] of unitInstances) {
          if (manualAnimCns.has(cn)) continue
          if (inst.sequence === -1 || inst.sequenceEnded) rollSequence(inst, 'stand')
        }
        for (const inst of doodadInstancesToReroll) {
          if (inst.sequenceEnded) rollSequence(inst, 'stand')
        }
        gl.viewport(0, 0, canvas.width, canvas.height)
        gl.depthMask(true)
        gl.clearColor(bg[0], bg[1], bg[2], 1)
        gl.clear(gl.COLOR_BUFFER_BIT | gl.DEPTH_BUFFER_BIT)
        // Sky stack:
        //   1. Horizon gradient — always on, fills the void with our 3-stop
        //      atmospheric backdrop. The flat bg clear above is redundant
        //      once this draws but kept for the rare case the gradient
        //      build failed (we still want SOMETHING in the framebuffer).
        //   2. SetSkyModel mdx — if a script call resolved a path. Renders
        //      ON TOP of the gradient so an authored sky still wins where
        //      it covers pixels; gradient remains visible at the edges /
        //      below the horizon for skies that are dome-only.
        // Both passes leave depth state restored for terrain (next).
        glChk('clear')
        if (skyGradient) skyGradient.draw(scene)
        if (sky) sky.draw(dtMs)
        glChk('sky')
        if (terrain) terrain.draw(scene.camera.viewProjectionMatrix)
        glChk('terrain')
        // Cliffs render AFTER terrain (so terrain's depth buffer is up so cliff
        // back-faces clip correctly) but BEFORE viewer.render() (so MDX units
        // depth-test correctly against cliff faces). Like terrain, this is a
        // custom-shader pass — the cache-invalidation below covers both.
        if (cliffs) cliffs.draw(scene.camera.viewProjectionMatrix)
        glChk('cliffs')
        // **CRITICAL**: terrain.draw calls gl.useProgram(terrainProg) directly,
        // bypassing mdx-m3-viewer's webgl.useShader() state cache. The cache
        // still thinks the LAST-USED MDX shader is bound. On the next frame,
        // when the lib does shader.use() → useShader(sameShader), it short-
        // circuits because `shader === currentShader` — but the actual GL
        // program is now the TERRAIN program. Every subsequent uniform call
        // then targets terrain's program with locations queried against the
        // MDX shader → GL_INVALID_OPERATION on every uniformXxx call → no
        // pixels painted. The lib's `webgl` is a state-caching wrapper;
        // we must invalidate its cache after any direct useProgram from our
        // own custom shaders (terrain, water, slocs, pathing). Resetting to
        // null forces the next `webgl.useShader(s)` to re-bind the program
        // and re-sync attribute indices. This is the fix for the "every MDX
        // is invisible on second frame onward" bug — see commit log.
        ;(viewer as any).webgl.currentShader = null
        viewer.render()
        glChk('render')
        // Sloc markers go AFTER viewer.render() and BEFORE water so they're
        // opaque-on-top-of-units but still get alpha-blended water in front.
        // Slocs are small and rare (typically 2-12 per map), so drawing N
        // pillars per frame is cheap enough that we don't bother with any
        // visibility culling.
        if (slocRenderer) slocRenderer.draw(scene.camera.viewProjectionMatrix, selectedSet)
        // Path-blocker checker overlays. Drawn after units/doodads/slocs so
        // they sit visually on top (the user wants them OBVIOUS — these are
        // navigation hints, not subtle ambience), before the pathing-debug
        // overlay so any path-bit flags painted by pathing.ts win the eye in
        // contested regions. Alpha-blended over the existing framebuffer;
        // depth-test on so cliffs / large doodads can still occlude them
        // when they're behind something tall. Picking treats the blockers
        // as 'doodad' kind (they ARE doodads) — selectedDoodadSet is the
        // right tint state to pass in.
        if (pathBlockers) pathBlockers.draw(scene.camera.viewProjectionMatrix, selectedDoodadSet, hoveredDoodad, rubberPreviewDoodads)
        // Pathing overlay — alpha-blended, depth-test on, depth-write off
        // (see pathing.ts). Drawn after slocs so it shades the markers' base
        // too, before water so submerged pathing flags stay readable under
        // shallow water.
        if (pathing && pathingVisible) pathing.draw(scene.camera.viewProjectionMatrix)
        // Water is translucent — must render LAST so it alpha-blends on top
        // of terrain + cliffs + opaque model passes. The lib's translucent
        // pass already ran inside viewer.render(); water sits above that.
        if (water) water.draw(scene.camera.viewProjectionMatrix)
        glChk('water')
        // Instance-outline overlays — glowing silhouettes drawn by rendering
        // the target instance(s) into an offscreen mask (via a temporary swap
        // of the scene's render list, reusing the lib's own render path — no
        // reimplemented skinning) and compositing an edge glow over the frame.
        // The lib's shader-state cache must be invalidated on BOTH sides of each
        // mask render: before, because the water/sloc/pathing custom programs
        // left a foreign GL program bound while the lib still believes its MDX
        // shader is current (same trap as the pre-render reset above); after, so
        // the next pass / frame re-binds cleanly.
        //
        // Each pass swaps scene.instances to its target array, draws, and always
        // restores it in a finally — selection (white) then hover (cyan).
        if (outline) {
          outline.setSize(canvas.width, canvas.height)
          // Selection pass (white, near-steady): every selected unit + doodad.
          const selInsts: any[] = []
          for (const cn of selectedSet) {
            const i = unitInstances.get(cn)
            if (i && i.rendered !== false) selInsts.push(i)
          }
          for (const cn of selectedDoodadSet) {
            const i = doodadInstances.get(cn)
            if (i && i.rendered !== false) selInsts.push(i)
          }
          if (selInsts.length) {
            ;(viewer as any).webgl.currentShader = null
            outline.draw(() => {
              const savedList = (scene as any).instances
              ;(scene as any).instances = selInsts
              try {
                ;(scene as any).renderOpaque()
                ;(scene as any).renderTranslucent()
              } finally {
                ;(scene as any).instances = savedList
              }
            }, nowTs, SELECT_OUTLINE_COLOR, 0.12)
            ;(viewer as any).webgl.currentShader = null
          }
          // Rubber-band preview pass (cyan): everything currently inside the
          // drag box — a live "this is what would be selected on release" hint.
          // One mask render for the whole set (path blockers wash cyan via their
          // own shader below, since they aren't MDX instances).
          if (rubberPreviewUnits.size || rubberPreviewDoodads.size) {
            const preInsts: any[] = []
            for (const cn of rubberPreviewUnits) {
              const i = unitInstances.get(cn)
              if (i && i.rendered !== false) preInsts.push(i)
            }
            for (const cn of rubberPreviewDoodads) {
              const i = doodadInstances.get(cn)
              if (i && i.rendered !== false) preInsts.push(i)
            }
            if (preInsts.length) {
              ;(viewer as any).webgl.currentShader = null
              outline.draw(() => {
                const savedList = (scene as any).instances
                ;(scene as any).instances = preInsts
                try {
                  ;(scene as any).renderOpaque()
                  ;(scene as any).renderTranslucent()
                } finally {
                  ;(scene as any).instances = savedList
                }
              }, nowTs, HOVER_OUTLINE_COLOR, 0.45)
              ;(viewer as any).webgl.currentShader = null
            }
          }
          // Hover pass (cyan, lively pulse): the single entity under the cursor
          // — a doodad or a unit/building (at most one is hovered at a time).
          const hInst = hoveredDoodad !== null
            ? doodadInstances.get(hoveredDoodad)
            : hoveredUnit !== null
              ? unitInstances.get(hoveredUnit)
              : null
          if (hInst && hInst.rendered !== false) {
            // DEBUG: draw the rayPick bounding sphere instead of the outline.
            // window.__wc3ForgeHoverBounds (true/false) overrides the compile
            // default at runtime via devtools.
            const debugBounds = (window as any).__wc3ForgeHoverBounds ?? DEBUG_HOVER_BOUNDS
            if (debugBounds && boundsDebug) {
              const wb = instanceWorldBounds(hInst)
              if (wb) {
                ;(viewer as any).webgl.currentShader = null
                boundsDebug.draw(scene.camera.viewProjectionMatrix, wb.cx, wb.cy, wb.cz, wb.r)
                ;(viewer as any).webgl.currentShader = null
              }
            } else {
              ;(viewer as any).webgl.currentShader = null
              outline.draw(() => {
                const savedList = (scene as any).instances
                ;(scene as any).instances = [hInst]
                try {
                  ;(scene as any).renderOpaque()
                  ;(scene as any).renderTranslucent()
                } finally {
                  ;(scene as any).instances = savedList
                }
              }, nowTs, HOVER_OUTLINE_COLOR, 0.45)
              ;(viewer as any).webgl.currentShader = null
            }
          }
        }
        glChk('instance-outline')
        // Cell-highlight overlay (yellow wireframe quad for picked cell).
        // Drawn LAST before the gizmo so the wireframe sits visually on top
        // of every other layer including water.
        if (cellHighlight) cellHighlight.draw(scene.camera.viewProjectionMatrix)
        if (regionOverlay && regionOverlayVisible) regionOverlay.draw(scene.camera.viewProjectionMatrix)
        // Terrain-brush footprint ring (only visible while the palette is armed
        // and the cursor is over the map). Same always-on-top overlay tier.
        if (brushCursor) brushCursor.draw(scene.camera.viewProjectionMatrix)
        // Transform gizmo — drawn LAST of all overlays. Always-on-top via
        // gl.disable(DEPTH_TEST) in draw(). Always invoked (the gate used to
        // be `gizmoSelectionItems.length > 0`) so that when selection clears,
        // the gizmo's internal lastVisible/lastOrigin state gets cleared too —
        // otherwise getMoveArrowTips() returns stale data and the X/Y/Z
        // labels float at the previously-selected entity's position even
        // though the gizmo itself isn't drawn. draw() short-circuits on
        // empty selection internally (computeOrigin returns null → return).
        if (gizmo) {
          gizmo.draw(
            (viewer as any).gl as WebGLRenderingContext,
            viewer, scene, canvas,
            gizmoSelectionItems,
            unitInstances,
            gizmoDoodadLookup, // real doodads + path-blocker proxies
            camera.getEye(),
          )
        }
        glChk('gizmo')
        updateAxisLabels()
        // GL-error capture, ALWAYS-ON but SAMPLED at ~5Hz so it never costs a
        // per-frame GPU sync (getError flushes the pipeline; the loop runs at
        // 60–240fps). The armed path (glChk above) already fed lastGlError this
        // frame, so only sample when NOT armed. This is the always-available
        // signal that caught the sky-gradient 0x502 stall.
        if (!diagnosticsMode && nowTs - lastGlSampleTs >= GL_SAMPLE_MS) {
          lastGlSampleTs = nowTs
          const e = gl.getError()
          lastGlError = e
          if (e) glErrFrame = frameCount
        }
        // Diagnostics heartbeat — drawn DIRECTLY into the main canvas (not the
        // DOM) so it proves whether the canvas is being PRESENTED to screen. A
        // bright bar sweeps left→right across the bottom edge, advancing with
        // frameCount. If the DiagnosticsOverlay's frame counter (DOM) keeps
        // climbing but this bar is frozen, the loop is running but the canvas
        // isn't presenting → present-stall. If the bar sweeps, the canvas IS
        // presenting and the "frozen" view is a camera/content issue instead.
        // Also samples gl.getError() (only while diagnostics is on, since it
        // forces a GPU sync) so the overlay can surface silent GL failures.
        if (diagnosticsMode) {
          const w = canvas.width, h = canvas.height
          const barW = 60
          const x = (frameCount * 8) % Math.max(1, w)
          gl.enable(gl.SCISSOR_TEST)
          gl.scissor(0, 0, w, 8)
          gl.clearColor(0.1, 0.0, 0.15, 1)
          gl.clear(gl.COLOR_BUFFER_BIT)
          gl.scissor(x, 0, barW, 8)
          gl.clearColor((frameCount % 30) / 30, 1, (frameCount % 60) / 60, 1)
          gl.clear(gl.COLOR_BUFFER_BIT)
          gl.scissor(0, 0, w, h)
          gl.disable(gl.SCISSOR_TEST)
          void h
        }
      } catch (e) {
        // Latch crash detail so diagnostics.get explains a frozen viewport
        // ("crashed: true, crashReason, crashAtFrame") rather than leaving an
        // agent to infer it from a non-advancing frame counter. Only record the
        // FIRST crash (frameCount stops advancing anyway once crashed).
        if (!crashed) {
          crashed = true
          crashReason = e instanceof Error ? `${e.name}: ${e.message}` : String(e)
          crashAtFrame = frameCount
          flogError('[render-loop crash]', e instanceof Error ? e.stack : String(e))
        }
      }
    }
    rafId = requestAnimationFrame(loop)
  }
  loop()

  const ro = new ResizeObserver(sizeToBox)
  ro.observe(canvas)

  let pickCallback: PickCallback | null = null
  // Note: selectedSet, selectedDoodadSet, unitInstances, doodadInstances are
  // all declared earlier in this function (before the render loop runs) to
  // avoid a TDZ trap — the loop closure captures them on first invocation.
  // Pre-click hover hint: a cyan vertex-colour fill under the cyan hover
  // outline, so the doodad under the cursor reads warm even before the glow.
  // (Selection no longer uses a vertex tint at all — it's the white outline
  // pass — so a hovered doodad's resting colour is always plain white.)
  const HOVER_TINT: [number, number, number, number] = [0.7, 1.1, 1.5, 1]
  // hoveredDoodad (the doodad painted with HOVER_TINT / cyan-outlined) is
  // declared earlier with the instance maps to avoid a frame-1 TDZ in the loop.
  function clearDoodadHover() {
    if (hoveredDoodad === null) return
    const inst = doodadInstances.get(hoveredDoodad)
    if (inst) inst.setVertexColor([1, 1, 1, 1])
    hoveredDoodad = null
  }
  function setDoodadHover(cn: number) {
    if (hoveredDoodad === cn) return
    clearDoodadHover()
    // Don't fill a doodad that's already selected — its white outline stands.
    if (selectedDoodadSet.has(cn)) return
    const inst = doodadInstances.get(cn)
    if (inst) inst.setVertexColor(HOVER_TINT)
    hoveredDoodad = cn
  }
  // Clear ALL hover state (doodad fill + outline, and the unit outline). Used
  // wherever the pointer leaves an entity or a gesture/mode takes over.
  function clearHover() {
    clearDoodadHover()
    hoveredUnit = null
  }

  // Unit type index: stock-only and process-stable (we don't yet apply
  // per-map w3u modifications), so cache for process lifetime.
  let unitTypeIndexCache: Record<string, UnitTypeInfo> | null = null
  async function getUnitTypes(): Promise<Record<string, UnitTypeInfo>> {
    if (!unitTypeIndexCache) {
      unitTypeIndexCache = (await GetUnitTypeIndex()) as unknown as Record<string, UnitTypeInfo>
    }
    return unitTypeIndexCache
  }
  // Doodad type index includes per-map w3d/w3b overlays, so it must be
  // re-fetched on every loadMap. Stored as a per-load promise so concurrent
  // placeDoodad calls share one fetch.
  async function getDoodadTypes(): Promise<Record<string, DoodadTypeInfo>> {
    return (await GetDoodadTypeIndex()) as unknown as Record<string, DoodadTypeInfo>
  }

  // Place a single unit. Pulls model via viewer.load (deduplicated via
  // viewer.resourceMap so repeat type-ids reuse the same MdxModel) and
  // returns the placed instance, or null if the type has no MDX.
  async function placeUnit(unit: any, types: Record<string, UnitTypeInfo>): Promise<any | null> {
    const info = types[unit.type_id]
    if (!info || !info.file) return null
    const path = mdxPath(info.file)
    let model: any
    try {
      model = await viewer.load(path, pathSolver)
    } catch (e) {
      sceneDiag.unitModelFails++
      flogWarn('[unit load fail]', unit.type_id, path, e instanceof Error ? e.message : String(e))
      return null
    }
    if (!model || typeof model.addInstance !== 'function') { sceneDiag.unitModelFails++; return null }

    // Placed-but-invisible guard: a model that parsed but has zero geosets
    // renders nothing yet counts as "placed". Bump a dedicated counter +
    // flogWarn so a rash of these (e.g. a parser-patch regression) is visible
    // in diagnostics rather than masked by a healthy placed=N.
    if ((model.geosets?.length ?? -1) === 0) {
      sceneDiag.zeroGeosetModels++
      flogWarn('[unit zero-geoset]', unit.type_id, path)
    }

    const inst = model.addInstance()
    inst.move([unit.position[0], unit.position[1], unit.position[2] + info.move_height])
    inst.rotateLocal(quatZ(unit.rotation))
    // Use setScale per-axis so the on-disk per-axis values (the gizmo's
    // Roblox-style face handles can write non-uniform) round-trip through
    // reload. Falls back to uniformScale when the lib lacks setScale.
    const ms = info.model_scale || 1
    if (typeof inst.setScale === 'function') {
      inst.setScale([unit.scale[0] * ms, unit.scale[1] * ms, unit.scale[2] * ms])
    } else {
      inst.uniformScale(unit.scale[0] * ms)
    }
    inst.setTeamColor(unit.player)
    // Stash the unit's type_id on the instance so later position-update calls
    // (SceneAPI.updateUnitPosition) can resolve the type's move_height without
    // needing a parallel cn → typeId map. Cheap, non-intrusive — the lib
    // never touches custom properties on its node objects.
    ;(inst as any).__wc3ForgeTypeId = unit.type_id
    // Stash current rotation + scale so the gizmo's rotate/scale drag can
    // snapshot the "orig" values without round-tripping through Go. Updated
    // by the entity-changed event subscriber when rotation/scale mutate.
    ;(inst as any).__wc3ForgeRotation = unit.rotation
    ;(inst as any).__wc3ForgeScale = [unit.scale[0], unit.scale[1], unit.scale[2]]
    // ModelScale baked-in multiplier — entity-changed scale handler reads
    // this to reconstruct the rendered value from the disk-stored unit
    // scale (the lib's worldScale = unit.scale × info.model_scale).
    ;(inst as any).__wc3ForgeModelScale = ms
    if (info.red || info.green || info.blue) {
      inst.setVertexColor([info.red / 255, info.green / 255, info.blue / 255, 1])
    }
    inst.setScene(scene)
    rollSequence(inst, 'stand')
    // Selection is shown by the white instance-outline pass (driven by
    // selectedSet in the render loop), not a vertex tint — nothing to paint here.
    return inst
  }

  // Place a single doodad. Doodads can have N variations on disk; the
  // file path picks up a numeric suffix when numVar > 1 (e.g. ATtr0.mdx,
  // ATtr1.mdx, …). Falls back to the unsuffixed name if the variant 404s.
  //
  // Custom-map overrides (war3map.w3d) often supply a fully-qualified path
  // with extension already attached (`war3mapImported\Lantern.mdl`); we
  // preserve the original extension when present and only synthesize one
  // for the stock-style "no-extension" stem case.
  //
  // Doodad scale is stored RAW in war3map.doo (memory note: no /128 divide),
  // so we pass unit.scale through verbatim, multiplied by the SLK base scale.
  // Resolve + load the MDX/MDL model for a doodad type, trying the variant
  // path, the unsuffixed fallback, and the other extension in order (see the
  // inline notes for why). Returns the loaded model and the path that worked,
  // or null when none loaded. Shared by placeDoodad (real instances) and the
  // placement ghost (cursor preview) so both honor the same extension-
  // exhaustion order.
  async function resolveDoodadModel(
    info: DoodadTypeInfo,
    variation: number,
  ): Promise<{ model: any; chosenPath: string } | null> {
    const extMatch = info.file.match(/\.(mdl|mdx)$/i)
    const declaredExt = extMatch ? extMatch[0] : '.mdx'
    const stem = extMatch ? info.file.slice(0, -extMatch[0].length) : info.file
    const variantIdx = Math.min(Math.max(0, variation), Math.max(0, info.num_var - 1))

    // Try (in order):
    //   1. variant path with the declared extension
    //   2. unsuffixed path with the declared extension
    //   3. unsuffixed path with the OTHER extension (custom maps often
    //      declare .mdl but ship .mdx, or vice versa — mdx-m3-viewer's
    //      MDX handler accepts both formats from the same handler)
    const otherExt = declaredExt.toLowerCase() === '.mdx' ? '.mdl' : '.mdx'
    const variantPath = (info.num_var > 1 ? stem + variantIdx : stem) + declaredExt
    const fallbackPath = stem + declaredExt
    const otherExtPath = stem + otherExt

    let model: any
    let chosenPath: string | undefined
    for (const p of [variantPath, fallbackPath, otherExtPath]) {
      if (model && typeof model.addInstance === 'function') break
      try { model = await viewer.load(p, pathSolver); chosenPath = p } catch { /* swallow, try next */ }
    }
    if (!model || typeof model.addInstance !== 'function') return null
    return { model, chosenPath: chosenPath! }
  }

  async function placeDoodad(d: any, types: Record<string, DoodadTypeInfo>): Promise<any | null> {
    const info = types[d.type_id]
    if (!info || !info.file) return null
    const resolved = await resolveDoodadModel(info, d.variation)
    if (!resolved) { sceneDiag.doodadModelFails++; return null }
    const { model, chosenPath } = resolved

    // Placed-but-invisible guard (same rationale as placeUnit): a doodad model
    // that parsed with zero geosets renders nothing but counts as placed.
    if ((model.geosets?.length ?? -1) === 0) {
      sceneDiag.zeroGeosetModels++
      flogWarn('[doodad zero-geoset]', d.type_id, chosenPath)
    }

    // DIAGNOSTIC: print per-doodad geometry counts so we can correlate
    // "log says placed=N" with "user sees actual MDX on screen." If geosets
    // is 0 the parser patch isn't doing what it claimed; if geosets > 0 but
    // user sees nothing, the bug is downstream (texture load, material
    // linking, etc.). Limit to first 12 to keep the log readable.
    if ((doodadInstances.size + 1) <= 12) {
      const g = model.geosets?.length ?? -1
      const b = model.batches?.length ?? -1
      const bones = model.bones?.length ?? -1
      const seqs = model.sequences?.length ?? -1
      const mats = model.materials?.length ?? -1
      flog(`[doodad geom] cn=${d.creation_number} type=${d.type_id} path=${chosenPath} geosets=${g} batches=${b} bones=${bones} mats=${mats} seqs=${seqs}`)
    }

    const inst = model.addInstance()
    inst.move([d.position[0], d.position[1], d.position[2]])
    // Composed rotation: world-Z yaw (placement rotation) → model-local Y
    // pitch (per-type maxPitch override) → model-local X roll (per-type
    // maxRoll override). Mirrors HiveWE's Doodad::update:
    //
    //   glm::quat rotation = glm::angleAxis(angle, glm::vec3(0, 0, 1));
    //   rotation *= glm::angleAxis(-pitch, glm::vec3(0, 1, 0));
    //   rotation *= glm::angleAxis(roll,  glm::vec3(1, 0, 0));
    //
    // For maxPitch / maxRoll values < 0 (the "user-set fixed" case), HiveWE
    // takes the value as-is (already-negated by the user for the Y axis;
    // unsigned for the X axis). For values > 0 it samples surrounding
    // terrain heights and pitches/rolls to follow the slope, clamped by
    // ±value. We currently only implement the fixed-negative path — that's
    // the only one custom maps in the wild actually use to author flame /
    // statue / floating-doodad effects whose source MDX is laid out flat.
    // Zero (the default for most stock doodads) is a no-op.
    let rot = quatZ(d.rotation)
    const mp = info.max_pitch || 0
    if (mp < 0) {
      // HiveWE's "-pitch" inside the angleAxis: with mp already negative,
      // the effective rotation about Y is +|mp|. The Ember Sword's mp=-4.71
      // (≈ -3π/2) produces a +3π/2 about Y, equivalent to a -π/2 (-90°)
      // rotation that flips the +X-axis emitters to point along +Z.
      rot = quatMul(rot, quatY(-mp))
    }
    const mr = info.max_roll || 0
    if (mr < 0) {
      // HiveWE flips the sign in its `roll = -max_roll` line; mirror that.
      rot = quatMul(rot, quatX(-mr))
    }
    inst.rotateLocal(rot)
    // Per-axis scale via setScale (so Roblox-style gizmo per-axis values
    // round-trip through reload); fall back to uniform if the lib lacks it.
    {
      const ms = info.model_scale || 1
      const sx = (d.scale[0] || 1) * ms
      const sy = (d.scale[1] || 1) * ms
      const sz = (d.scale[2] || 1) * ms
      if (typeof inst.setScale === 'function') {
        inst.setScale([sx, sy, sz])
      } else {
        inst.uniformScale(sx)
      }
    }
    // Stash placement rotation (yaw only, before pitch/roll composition) +
    // raw scale + model_scale on the inst so the gizmo can snapshot orig
    // values for its rotate/scale drag without needing a Go round-trip.
    // Live-preview during rotate reapplies a yaw-only quat (loses pitch/roll
    // visually until next map reload — acceptable cosmetic during drag).
    ;(inst as any).__wc3ForgeRotation = d.rotation
    ;(inst as any).__wc3ForgeScale = [d.scale[0] || 1, d.scale[1] || 1, d.scale[2] || 1]
    ;(inst as any).__wc3ForgeModelScale = info.model_scale || 1
    // Record category so visibility toggles can find this instance by cat
    // without re-walking the type index. "" → "Uncategorized" so unknown rows
    // land in a single bucket that's still toggleable via the View menu.
    const cat = (info.category && info.category.length > 0) ? info.category : 'Uncategorized'
    doodadCategoryByCn.set(d.creation_number, cat)
    // Always attach to scene so the lib's internal lists are populated;
    // visibility is controlled via show()/hide(). The previous gate of
    // `setScene(scene)` ONLY when visible meant a later show() couldn't
    // un-hide it (no scene → not in the render list at all). Calling
    // hide() before there's a scene is fine — `rendered` is just a boolean
    // flag — and the lib's update path skips instances whose `scene` is
    // null OR whose `rendered` is false.
    inst.setScene(scene)
    const visible = doodadVisibility.get(cat) !== false
    if (!visible) inst.hide()
    rollSequence(inst, 'stand')
    // Add to the per-frame reroll set when EITHER:
    //   - the model has multiple 'stand' variations (so we want variety on end), OR
    //   - the single 'stand' sequence is non-looping (so it freezes after one
    //     play without an explicit restart; the canonical case is EFFECT-type
    //     doodads like Enfo FFB's "Soul Discharge" — 1 Stand, nonLooping=1).
    //
    // Doodads with a single looping Stand can skip the reroll: the library
    // auto-wraps the frame back to interval[0] internally and never sets
    // `sequenceEnded`, so they'd hit a no-op branch every RAF tick.
    const standSeqs = filterSequencesByType('stand', model.sequences || [])
    const standCount = standSeqs.length
    const singleStandNonLooping = standCount === 1
      && (model.sequences?.[standSeqs[0].index]?.nonLooping ?? 0) !== 0
    if (standCount > 1 || singleStandNonLooping) doodadInstancesToReroll.add(inst)
    // Selection is shown by the white instance-outline pass (driven by
    // selectedDoodadSet in the render loop), not a vertex tint.
    return inst
  }

  // Tear down the placement ghost (if any). Detaches it from the scene's
  // render list — same removal path clearInstances uses for real doodads —
  // and bumps the load token so any model load still in flight discards its
  // result instead of attaching an orphan. The model itself stays in the
  // viewer cache for instant re-arm.
  function disposePlacementGhost() {
    placementGhostLoadToken++
    placementGhostTypeId = null
    placementGhostPlaced = false
    if (placementGhostInst) {
      doodadInstancesToReroll.delete(placementGhostInst)
      try { placementGhostInst.detach() } catch { /* already detached */ }
      placementGhostInst = null
    }
  }

  // (Re)build the placement ghost for `typeId`. Drops any prior ghost first, so
  // arming a different doodad swaps the preview. Passing null just disarms the
  // preview. The instance is created hidden and positioned (then shown) on the
  // first hover-move so it doesn't flash at the map origin before the cursor
  // reports a terrain cell. Transform mirrors placeDoodad's defaults for a
  // fresh drop: rotation 0, base model scale, the per-type fixed pitch/roll.
  async function rebuildPlacementGhost(typeId: string | null, types: Record<string, DoodadTypeInfo>) {
    disposePlacementGhost()
    if (!typeId) return
    const info = types[typeId]
    if (!info || !info.file) return
    placementGhostTypeId = typeId
    const token = placementGhostLoadToken
    const resolved = await resolveDoodadModel(info, 0)
    // A newer arm/disarm superseded this load while it was in flight — discard.
    if (token !== placementGhostLoadToken || placementGhostTypeId !== typeId) return
    if (!resolved) return
    const inst = resolved.model.addInstance()
    let rot = quatZ(0)
    const mp = info.max_pitch || 0
    if (mp < 0) rot = quatMul(rot, quatY(-mp))
    const mr = info.max_roll || 0
    if (mr < 0) rot = quatMul(rot, quatX(-mr))
    inst.rotateLocal(rot)
    const ms = info.model_scale || 1
    if (typeof inst.setScale === 'function') inst.setScale([ms, ms, ms])
    else inst.uniformScale(ms)
    inst.setScene(scene)
    inst.setVertexColor(GHOST_TINT)
    rollSequence(inst, 'stand')
    inst.hide() // revealed by the first positioned hover-move
    placementGhostInst = inst
    placementGhostPlaced = false
  }

  // Sample the terrain surface height at a game-coord (worldX, worldY) by
  // bilinearly interpolating the four surrounding vertex heights from the
  // cached TerrainDTO. Mirrors pickTerrainCell's grid math: vertex (0,0) sits
  // at center_offset, cells are 128 studs. Returns 0 when no terrain is cached
  // or the point is outside the vertex grid (placement at sea level is a sane
  // fallback — the doodad still lands, just not snapped to a slope). The
  // `heights` array is FinalZ per-vertex (see app.go GetTerrain), which is the
  // same Z the terrain mesh is drawn at, so a doodad placed here renders flush
  // with the ground in the viewport.
  function sampleTerrainHeight(worldX: number, worldY: number): number {
    const t = cachedTerrainDTO
    if (!t || !t.heights || !t.center_offset) return 0
    const W = t.width as number
    const H = t.height as number
    if (!(W > 1) || !(H > 1)) return 0
    const fx = (worldX - t.center_offset[0]) / 128
    const fy = (worldY - t.center_offset[1]) / 128
    let col = Math.floor(fx)
    let row = Math.floor(fy)
    if (col < 0) col = 0; else if (col > W - 2) col = W - 2
    if (row < 0) row = 0; else if (row > H - 2) row = H - 2
    const tx = Math.min(1, Math.max(0, fx - col))
    const ty = Math.min(1, Math.max(0, fy - row))
    const heights = t.heights as ArrayLike<number>
    const bl = heights[row * W + col] ?? 0
    const br = heights[row * W + col + 1] ?? bl
    const tl = heights[(row + 1) * W + col] ?? bl
    const tr = heights[(row + 1) * W + col + 1] ?? bl
    const bottom = bl + (br - bl) * tx
    const top = tl + (tr - tl) * tx
    return bottom + (top - bottom) * ty
  }

  // Rebuild the ordered present-categories list (View menu) from the current
  // doodadCategoryByCn tags. Curated DOODAD_CAT_ORDER first, then remaining
  // categories alphabetized with "Uncategorized" pinned last. Called after a
  // full loadMap and after a single live add so the menu stays in sync.
  function recomputeDoodadCategories(): void {
    const presentSet = new Set<string>()
    for (const c of doodadCategoryByCn.values()) presentSet.add(c)
    const ordered: string[] = []
    for (const c of DOODAD_CAT_ORDER) {
      if (presentSet.has(c)) { ordered.push(c); presentSet.delete(c) }
    }
    const rest = [...presentSet].sort((a, b) => {
      if (a === 'Uncategorized') return 1
      if (b === 'Uncategorized') return -1
      return a.localeCompare(b)
    })
    ordered.push(...rest)
    doodadCategoriesPresent = ordered
  }

  function clearInstances() {
    // A reload/close invalidates any armed placement ghost (its model may not
    // even exist in the next map's catalog). Drop it before detaching the rest.
    disposePlacementGhost()
    // Manual-anim pins are scoped to a loaded map — drop them so a fresh
    // map opens with all units back in the idle-reroll pool.
    manualAnimCns.clear()
    for (const inst of unitInstances.values()) {
      try { inst.detach() } catch { /* already detached */ }
    }
    unitInstances.clear()
    for (const inst of doodadInstances.values()) {
      try { inst.detach() } catch { /* already detached */ }
    }
    doodadInstances.clear()
    doodadInstancesToReroll.clear()
    // Drop the per-instance category tags. The doodadVisibility map (per-cat
    // user choices) intentionally persists across loadMap.
    doodadCategoryByCn.clear()
    doodadCategoriesPresent = []
    // Slocs hold no GL resources of their own — they live in the renderer's
    // marker list. Replace with empty list so the next loadMap re-populates.
    slocRenderer?.setMarkers([])
    // Same shape for path-blocker markers: clear the renderer's list so a
    // map close + open doesn't leave the previous map's blockers visible.
    pathBlockers?.setMarkers([])
    rebuildBlockerProxies([]) // drop stale gizmo proxies with the markers
    if (terrain) {
      terrain.dispose()
      terrain = null
    }
    if (water) {
      water.dispose()
      water = null
    }
    if (cliffs) {
      cliffs.dispose()
      cliffs = null
    }
    if (pathing) {
      pathing.dispose()
      pathing = null
    }
    if (sky) {
      sky.dispose()
      sky = null
    }
  }

  // Serializes loadMap() runs. Placement awaits viewer.load (async), so two
  // overlapping loadMap calls would BOTH clear+place into the shared instance
  // maps — and since each entry is keyed by creation_number, the later set()
  // orphans the earlier instance: it stays attached to the scene (rendering,
  // animating its own random stand sequence) but is no longer in the map, so
  // it can't be picked or detached. That's the "every entity rendered twice,
  // only one selectable" ghost. Chaining each load after the previous one
  // guarantees a full clear+place cycle finishes before the next begins, so a
  // superseded run's instances are detached by the next run's clearInstances().
  let loadMapSerial: Promise<void> = Promise.resolve()

  // --- Picking: ray vs instance bounds ---
  //
  // Each mdx-m3-viewer Bounds object is sphere-shaped (despite the type name)
  // — center + radius, in model-local space. We translate to world space
  // using the instance's worldLocation, then do ray-sphere intersection.
  // Walking units, doodads, and slocs in one pass lets us report the closest
  // hit across kinds; the picker callback receives a kind tag so the caller
  // can route through the kind-aware Go selection API.
  //
  // Click vs drag is detected by tracking mousedown/mouseup pixel distance;
  // MMB/RMB are camera drag gestures and we don't observe them here.
  // shift+LMB drag past the threshold becomes a rubber-band rectangle, also
  // handled in this module.
  const CLICK_PIXEL_THRESHOLD = 5

  // Project a world point through the current view-projection matrix into
  // canvas-pixel space (DOM-Y, origin top-left). Returns null if the point
  // is behind the camera (w <= 0). Used by rubber-band hit-testing.
  // Per-frame update for the move gizmo's X/Y/Z labels. Hides them when
  // the gizmo isn't visible in move mode; otherwise projects each arrow's
  // tip world position to canvas pixels and positions the labels there.
  function updateAxisLabels() {
    // Null-check axisLabelEls — the render loop's very first synchronous
    // tick happens BEFORE the labels are created further down in init.
    // (The let was forward-declared as null; assigned later.)
    if (!axisLabelEls) return
    const tips = gizmo?.getMoveArrowTips?.()
    if (!tips) {
      axisLabelEls.x.style.display = 'none'
      axisLabelEls.y.style.display = 'none'
      axisLabelEls.z.style.display = 'none'
      return
    }
    for (const ax of ['x', 'y', 'z'] as const) {
      const w = tips[ax]
      const sp = worldToCanvasPx(w[0], w[1], w[2])
      const el = axisLabelEls[ax]
      if (!sp) { el.style.display = 'none'; continue }
      el.style.left = `${sp.x}px`
      el.style.top = `${sp.y}px`
      el.style.display = 'block'
    }
  }

  function worldToCanvasPx(wx: number, wy: number, wz: number): { x: number; y: number } | null {
    const m = scene.camera.viewProjectionMatrix as Float32Array
    const x = m[0]*wx + m[4]*wy + m[8]*wz + m[12]
    const y = m[1]*wx + m[5]*wy + m[9]*wz + m[13]
    const w = m[3]*wx + m[7]*wy + m[11]*wz + m[15]
    if (w <= 0) return null
    const nx = x / w, ny = y / w
    const sx = (nx * 0.5 + 0.5) * canvas.clientWidth
    const syBottom = (ny * 0.5 + 0.5) * canvas.clientHeight
    return { x: sx, y: canvas.clientHeight - syBottom }
  }

  // World-space pick-bounds (sphere center + radius) for one unit/doodad
  // instance. Centralizes the "bounds.r is sphere, scale up by largest axis"
  // dance so both ray-pick and rubber-band agree on what's hittable.
  function instanceWorldBounds(inst: any): { cx: number; cy: number; cz: number; r: number } | null {
    const b = inst.getBounds?.()
    if (!b || b.r <= 0) return null
    const sx = inst.worldScale?.[0] ?? 1
    const sy = inst.worldScale?.[1] ?? 1
    const sz = inst.worldScale?.[2] ?? 1
    const biggest = Math.max(sx, sy, sz)
    return {
      cx: inst.worldLocation[0] + b.x,
      cy: inst.worldLocation[1] + b.y,
      cz: inst.worldLocation[2] + b.z,
      r: b.r * biggest,
    }
  }

  // Project a canvas-pixel (px, py) into world XY on the z=0 ground plane.
  // Returns null in the degenerate case where the camera is looking nearly
  // horizontal (dir.z ≈ 0 means the ray never hits the ground in front of
  // it) — the RTS camera's enforced pitch (PITCH_MIN ~10°) keeps us out of
  // that regime in practice, but the guard prevents NaN propagation if the
  // camera ever does end up there.
  function groundPlaneXY(px: number, py: number): [number, number] | null {
    const out = new Float32Array(6)
    const vp = (scene as any).viewport as Float32Array
    // mdx-m3-viewer's Camera.screenToWorldRay() calls into unproject() which
    // expects DOM-top-origin Y (it does `y_ndc = 1 - 2*v[1]/h` internally, so
    // v[1]=0 → top of NDC). The old "screen Y is at canvas BOTTOM" comment
    // was wrong — flipping py here mirrored picks around the canvas midpoint,
    // exactly matching the user-reported "click top selects bottom" bug.
    // Cross-check: worldToCanvasPx() (the inverse) also lives in DOM-top
    // coords, and the rubber-band picker that uses it has always worked.
    const screen = new Float32Array([px, py])
    scene.camera.screenToWorldRay(out, screen, vp)
    const nx = out[0], ny = out[1], nz = out[2]
    const dx = out[3] - nx, dy = out[4] - ny, dz = out[5] - nz
    if (Math.abs(dz) < 1e-6) return null
    // Plane equation: near.z + t * dz = 0  →  t = -near.z / dz.
    const t = -nz / dz
    if (!isFinite(t)) return null
    return [nx + dx * t, ny + dy * t]
  }

  function rayPick(px: number, py: number): PickHit | null {
    // screenToWorldRay outputs near + far points (vec3 + vec3 = 6 floats).
    const out = new Float32Array(6)
    const vp = (scene as any).viewport as Float32Array
    // mdx-m3-viewer's screenToWorldRay → unproject() uses DOM-top-origin Y
    // (`y_ndc = 1 - 2*v[1]/h`). Pass canvas-relative DOM coords AS-IS. The
    // earlier "flip with canvas.clientHeight - py" mirrored picks around the
    // canvas midpoint — that was the user-visible "vice versa" bug. See
    // worldToCanvasPx() (the inverse projection): it also lives in DOM-top
    // space and rubber-band picking — which uses only worldToCanvasPx, no
    // ray-cast — has always worked, confirming the convention.
    const screen = new Float32Array([px, py])
    scene.camera.screenToWorldRay(out, screen, vp)
    const ox = out[0], oy = out[1], oz = out[2]
    const dx = out[3] - ox, dy = out[4] - oy, dz = out[5] - oz
    const len = Math.hypot(dx, dy, dz)
    if (len < 1e-6) return null
    const rx = dx / len, ry = dy / len, rz = dz / len

    // Broad phase: collect every candidate whose bounding volume the ray hits,
    // tagged with entry distance t and (for MDX entities) the instance, so the
    // narrow phase below can refine each to pixel-accurate silhouette coverage.
    type PickCand = { t: number; hit: PickHit; inst: unknown | null }
    const cands: PickCand[] = []
    const considerSphere = (cx: number, cy: number, cz: number, r: number, hit: PickHit, inst: unknown) => {
      // Ray-sphere intersection. Standard quadratic:
      //   |o + t*d - c|² = r²  →  t² - 2t·(d·(c-o)) + |c-o|²-r² = 0
      const lx = cx - ox, ly = cy - oy, lz = cz - oz
      const tca = lx * rx + ly * ry + lz * rz
      if (tca < 0) return // behind the camera
      const d2 = lx * lx + ly * ly + lz * lz - tca * tca
      if (d2 > r * r) return // ray misses sphere
      const thc = Math.sqrt(r * r - d2)
      const t = tca - thc // nearest intersection in front of camera
      cands.push({ t, hit, inst })
    }

    for (const [cn, inst] of unitInstances) {
      // Slocs live in unitInstances as HIDDEN CircleOfPower MDX (kept for the
      // gizmo/transform) but are picked solely via slocRenderer.pickInfos()
      // below, which returns nothing while the markers are toggled hidden.
      // Skip them here so a hidden marker can't be clicked or box-selected.
      if ((inst as any).__wc3ForgeTypeId === 'sloc') continue
      const wb = instanceWorldBounds(inst)
      if (wb) considerSphere(wb.cx, wb.cy, wb.cz, wb.r, { kind: 'unit', id: cn }, inst)
    }
    for (const [cn, inst] of doodadInstances) {
      const wb = instanceWorldBounds(inst)
      if (wb) considerSphere(wb.cx, wb.cy, wb.cz, wb.r, { kind: 'doodad', id: cn }, inst)
    }

    // Path-blocker overlays — ray vs AABB (slab method). Same picker shape
    // as slocs, but routed as kind='doodad' because path blockers ARE
    // doodads (creation_numbers from war3map.doo, resolved through GetDoodad
    // on the Go side). Without this loop the checker overlays would be
    // visible but unclickable.
    const blockerInfos = pathBlockers?.pickInfos() ?? []
    for (const s of blockerInfos) {
      const cx = s.center[0], cy = s.center[1], cz = s.center[2]
      const hx = s.half[0], hy = s.half[1], hz = s.half[2]
      const minX = cx - hx, maxX = cx + hx
      const minY = cy - hy, maxY = cy + hy
      const minZ = cz - hz, maxZ = cz + hz
      let tmin = -Infinity, tmax = Infinity
      const axes: Array<[number, number, number, number]> = [
        [rx, ox, minX, maxX],
        [ry, oy, minY, maxY],
        [rz, oz, minZ, maxZ],
      ]
      let skip = false
      for (const [rd, ro, lo, hi] of axes) {
        if (Math.abs(rd) < 1e-9) {
          if (ro < lo || ro > hi) { skip = true; break }
          continue
        }
        let t1 = (lo - ro) / rd
        let t2 = (hi - ro) / rd
        if (t1 > t2) { const tmp = t1; t1 = t2; t2 = tmp }
        if (t1 > tmin) tmin = t1
        if (t2 < tmax) tmax = t2
        if (tmin > tmax) { skip = true; break }
      }
      if (skip) continue
      const t = tmin >= 0 ? tmin : tmax
      if (t < 0) continue
      cands.push({ t, hit: { kind: 'doodad', id: s.creationNumber }, inst: null })
    }

    // Sloc markers — ray vs AABB (slab method). Slocs have no MDX so they're
    // not in unitInstances; rays still need to hit them for selection. The
    // Go-side selection routes them as kind="unit" because they're entries
    // in war3mapUnits.doo.
    const slocInfos = slocRenderer?.pickInfos() ?? []
    for (const s of slocInfos) {
      // Per-axis slab intersection. Ray dir component near zero gets clamped
      // to ±Infinity so we don't divide-by-zero and the per-axis interval
      // collapses to "always inside" iff the ray origin is within the slab.
      const cx = s.center[0], cy = s.center[1], cz = s.center[2]
      const hx = s.half[0], hy = s.half[1], hz = s.half[2]
      const minX = cx - hx, maxX = cx + hx
      const minY = cy - hy, maxY = cy + hy
      const minZ = cz - hz, maxZ = cz + hz
      let tmin = -Infinity, tmax = Infinity
      const axes: Array<[number, number, number, number]> = [
        [rx, ox, minX, maxX],
        [ry, oy, minY, maxY],
        [rz, oz, minZ, maxZ],
      ]
      let skip = false
      for (const [rd, ro, lo, hi] of axes) {
        if (Math.abs(rd) < 1e-9) {
          // Ray parallel to this slab — must be inside it or no hit.
          if (ro < lo || ro > hi) { skip = true; break }
          continue
        }
        let t1 = (lo - ro) / rd
        let t2 = (hi - ro) / rd
        if (t1 > t2) { const tmp = t1; t1 = t2; t2 = tmp }
        if (t1 > tmin) tmin = t1
        if (t2 < tmax) tmax = t2
        if (tmin > tmax) { skip = true; break }
      }
      if (skip) continue
      // tmin < 0 means the ray origin is inside the box — pick anyway,
      // using tmax as the exit point. tmax < 0 means the box is entirely
      // behind the camera, skip.
      const t = tmin >= 0 ? tmin : tmax
      if (t < 0) continue
      cands.push({ t, hit: { kind: 'unit', id: s.creationNumber }, inst: null })
    }

    // Narrow phase: nearest-first, refine MDX candidates by ACTUAL silhouette
    // coverage at the cursor pixel (renders the one instance into the pick
    // buffer and reads the cursor pixel — proven by the outline that this
    // produces a correct silhouette). AABB entities (slocs / path blockers)
    // have no MDX model, so they're accepted as-is. Without a pick buffer we
    // fall back to the nearest bounding-volume hit (the old sphere behaviour).
    if (cands.length === 0) return null
    cands.sort((a, b) => a.t - b.t)
    if (!pickBuffer) return cands[0].hit
    pickBuffer.setSize(canvas.width, canvas.height)
    let tested = 0
    for (const c of cands) {
      if (c.inst === null) return c.hit // sloc / blocker: bounding box is the shape
      if (tested++ >= PICK_NARROW_CAP) return c.hit // bound worst case → accept nearest remaining
      const covered = pickBuffer.testCoverage(() => {
        ;(viewer as any).webgl.currentShader = null
        const savedList = (scene as any).instances
        ;(scene as any).instances = [c.inst]
        try {
          ;(scene as any).renderOpaque()
          ;(scene as any).renderTranslucent()
        } finally {
          ;(scene as any).instances = savedList
        }
        ;(viewer as any).webgl.currentShader = null
      }, px, py)
      if (covered) return c.hit
    }
    return null
  }

  // Rubber-band: collect every instance whose world-space bound-center
  // projects to a point inside the rectangle. Using the center (not full
  // sphere extent) matches Blender/Photoshop convention — the gesture asks
  // "what's in here?", not "what touches the edge?".
  function rubberBandPick(rect: { x: number; y: number; w: number; h: number }): PickHit[] {
    const hits: PickHit[] = []
    const inside = (p: { x: number; y: number } | null) => {
      if (!p) return false
      return p.x >= rect.x && p.x <= rect.x + rect.w
          && p.y >= rect.y && p.y <= rect.y + rect.h
    }
    for (const [cn, inst] of unitInstances) {
      // Hidden slocs (CircleOfPower MDX kept for the gizmo) are box-picked via
      // slocRenderer.pickInfos() below, which respects the hide toggle — skip
      // them here so box-select doesn't grab hidden markers.
      if ((inst as any).__wc3ForgeTypeId === 'sloc') continue
      const wb = instanceWorldBounds(inst)
      if (!wb) continue
      if (inside(worldToCanvasPx(wb.cx, wb.cy, wb.cz))) {
        hits.push({ kind: 'unit', id: cn })
      }
    }
    for (const [cn, inst] of doodadInstances) {
      const wb = instanceWorldBounds(inst)
      if (!wb) continue
      if (inside(worldToCanvasPx(wb.cx, wb.cy, wb.cz))) {
        hits.push({ kind: 'doodad', id: cn })
      }
    }
    const slocInfos = slocRenderer?.pickInfos() ?? []
    for (const s of slocInfos) {
      if (inside(worldToCanvasPx(s.center[0], s.center[1], s.center[2]))) {
        hits.push({ kind: 'unit', id: s.creationNumber })
      }
    }
    // Path-blocker overlays — same projected-center inside-rect test as the
    // other kinds. Routed as 'doodad' (matches the rayPick branch above) so
    // selection round-trips through Go's doodad-side selection state.
    const bInfos = pathBlockers?.pickInfos() ?? []
    for (const b of bInfos) {
      if (inside(worldToCanvasPx(b.center[0], b.center[1], b.center[2]))) {
        hits.push({ kind: 'doodad', id: b.creationNumber })
      }
    }
    return hits
  }

  // --- Mouse + keyboard gesture handling ---
  //
  // State machine for LMB:
  //   mousedown LMB     → record downAt + modifiers
  //   mousemove past 5px while shift was held at down → start rubber-band
  //   mousemove during rubber-band → resize the overlay rectangle
  //   mouseup LMB       → either commit rubber-band (mode=add)
  //                     OR treat as click and ray-pick (mode from modifiers)
  // Modifier-held click on empty space is a no-op (preserves selection);
  // plain click on empty space clears via mode='set' with hits=[].
  //
  // RMB/MMB drag belong to the camera and we don't observe them here.

  interface DownState {
    x: number; y: number          // canvas-relative px
    clientX: number; clientY: number // viewport px (for mousemove distance)
    shift: boolean
    ctrl: boolean
    // Did the user start a rubber-band gesture? Lazy-set on the first
    // mousemove that crosses the click-vs-drag threshold while shift is held.
    rubberBanding: boolean
    // Drag-to-move armed at mousedown: the user clicked on a unit that was
    // already in the selection without modifier keys. Stays null when the
    // hit was empty space, a non-selected unit, or any modifier was held —
    // those paths defer to the existing rubber-band / click-select code.
    // `active` flips true once motion crosses CLICK_PIXEL_THRESHOLD; before
    // that, mouseup still routes through the regular click handler (so a
    // click on a selected unit re-selects it cleanly via mode='set').
    dragMove: {
      cns: number[]                                   // unit creation_numbers
      anchorXY: [number, number]                      // ground XY at mousedown
      original: Map<number, {                         // per-cn original state
        wl: [number, number, number]                  // worldLocation (post-move_height)
        gameZ: number                                 // game-space Z (pre-move_height)
      }>
      active: boolean
    } | null
  }
  let downAt: DownState | null = null
  // Where the most recent right-mouse-button press landed (client px), or null.
  // Used to tell a right-CLICK (disarm the placement brush) from a right-DRAG
  // (camera pan) on the contextmenu event — only a near-stationary release
  // counts as a click. Camera panning owns RMB-drag and is left untouched.
  let rmbDownAt: { x: number; y: number } | null = null

  // The rubber-band rectangle is rendered as a positioned DOM div. WebGL
  // would be lower-overhead but a one-element DOM overlay is dramatically
  // simpler and doesn't conflict with any of the render-state assumptions
  // baked into our terrain/water/sloc passes.
  const overlay = document.createElement('div')
  overlay.style.cssText = [
    'position:absolute',
    'pointer-events:none',
    'border:1px solid rgba(120,180,255,0.9)',
    'background:rgba(60,120,220,0.18)',
    'display:none',
    'z-index:10',
  ].join(';')
  // The viewport container is position:relative so absolute children anchor
  // to its box. Fall back to document.body when no parent is available (unit
  // test contexts) so the listener wiring doesn't crash.
  const overlayHost = canvas.parentElement ?? document.body
  overlayHost.appendChild(overlay)

  // Per-axis labels for the move gizmo. Three absolute-positioned divs in
  // the same overlay container, repositioned per frame to sit just past
  // each arrow's cone tip. Hidden whenever the gizmo isn't in move mode
  // or no entity is selected. Pointer-events:none so they don't intercept
  // clicks. Assigned to the forward-declared axisLabelEls above (let, not
  // const) so the render loop's earlier synchronous tick sees null and
  // skips gracefully instead of TDZ-throwing.
  axisLabelEls = {
    x: document.createElement('div'),
    y: document.createElement('div'),
    z: document.createElement('div'),
  }
  const AXIS_LABEL_COLORS: Record<'x' | 'y' | 'z', string> = {
    x: 'rgb(255,80,80)',
    y: 'rgb(90,220,90)',
    z: 'rgb(120,140,255)',
  }
  for (const ax of ['x', 'y', 'z'] as const) {
    const el = axisLabelEls[ax]
    el.textContent = ax.toUpperCase()
    el.style.cssText = [
      'position:absolute',
      'pointer-events:none',
      'transform:translate(-50%,-50%)',
      'font:bold 11px system-ui,sans-serif',
      `color:${AXIS_LABEL_COLORS[ax]}`,
      'text-shadow:0 0 2px rgba(0,0,0,0.85),0 0 4px rgba(0,0,0,0.6)',
      'display:none',
      'z-index:11',
    ].join(';')
    overlayHost.appendChild(el)
  }

  function modeFromModifiers(shift: boolean, ctrl: boolean): SelectMode {
    if (ctrl) return 'toggle'
    if (shift) return 'add'
    return 'set'
  }

  function onCanvasMouseDown(e: MouseEvent) {
    // Right-click cancels an in-progress gizmo drag (industry standard:
    // Maya/Blender/Unity all use RMB to abort a transform). Restore each
    // entity to its pre-drag transform and swallow the click so it doesn't
    // also pop the WebView context menu.
    if (e.button === 2 && gizmoDragging && gizmo?.isDragging()) {
      e.preventDefault()
      gizmo.cancelDrag()
      gizmoDragging = false
      downAt = null
      return
    }
    // Remember where RMB went down so the contextmenu handler can distinguish
    // a click (disarm placement) from a pan-drag (a right-drag moves far).
    if (e.button === 2) rmbDownAt = { x: e.clientX, y: e.clientY }
    if (e.button !== 0) return
    // Gesture is starting — drop the pre-click hover hint. If this click selects
    // the entity it gets the white selection outline; otherwise the next free
    // hover move re-applies the cyan hint.
    clearHover()
    const r = canvas.getBoundingClientRect()
    const px = e.clientX - r.left
    const py = e.clientY - r.top
    const shift = e.shiftKey
    const ctrl = e.ctrlKey || e.metaKey

    // ── TERRAIN BRUSH STROKE ──────────────────────────────────────────
    // The Terrain Palette owns plain LMB press-drag-release: open a stroke
    // and paint the first dab now, then stream more dabs as the cursor crosses
    // into new cells (onDocMouseMove) until release (onDocMouseUp). We DON'T
    // set downAt — brushStroking gates the doc handlers and leaving downAt null
    // keeps the rubber-band / drag-move / gizmo paths inert. Modifier-held
    // clicks fall through so the user can briefly select without disarming.
    if (terrainBrushMode && !shift && !ctrl) {
      brushStroking = true
      brushLastCellKey = -1
      const cell = cachedTerrainDTO ? pickTerrainCell(px, py, canvas, scene, cachedTerrainDTO) : null
      terrainBrushCallback?.(cell, 'start')
      if (cell) brushLastCellKey = cell.row * 100000 + cell.col
      return
    }

    // ── REGION MODE ───────────────────────────────────────────────────
    // Region editing owns the viewport: skip the gizmo + entity-grab paths and
    // just record downAt so the move handler can rubber-band (the draw preview)
    // and the up handler can commit a region (drag) or select one (click).
    if (regionMode) {
      downAt = {
        x: px, y: py,
        clientX: e.clientX, clientY: e.clientY,
        shift: false, ctrl: false,
        rubberBanding: false,
        dragMove: null,
      }
      return
    }

    // ── GIZMO PRIORITY PICK (design §1.3) ─────────────────────────────
    // Check gizmo handle BEFORE entity selection / drag-to-move logic.
    // Shift is allowed through because it's a gizmo-side modifier (e.g.
    // uniform scale during a per-axis scale drag) — without this, a
    // shift-held click on a handle would fall through to rubber-band.
    // Ctrl stays gated as a selection-only modifier (ctrl+click an entity
    // under a handle toggles selection rather than grabbing the handle).
    if (!ctrl && gizmo && gizmoSelectionItems.length > 0) {
      const gizmoHit = gizmo.rayPick(px, py, scene, canvas)
      if (gizmoHit) {
        gizmo.beginDrag(
          gizmoHit, px, py, scene, canvas,
          gizmoSelectionItems,
          unitInstances, gizmoDoodadLookup, // real doodads + path-blocker proxies
          unitTypeIndexCache,
        )
        gizmoDragging = true
        downAt = {
          x: px, y: py,
          clientX: e.clientX, clientY: e.clientY,
          shift: false, ctrl: false,
          rubberBanding: false,
          dragMove: null,
        }
        return  // swallow — gizmo owns this gesture
      }
    }

    // Arm a drag-to-move only on a plain LMB click that lands on a unit
    // that's ALREADY in the current selection. Modifier-held clicks defer to
    // toggle/add semantics (shift+click a selected unit = deselect via
    // toggle; shift-drag empty = rubber-band) so the drag-move path stays
    // narrowly scoped to "user clicked something they had selected."
    //
    // Doodads don't get a drag-move treatment — the spec focuses on units,
    // and doodad position editing has its own deferred work (different
    // preservation fields in war3map.doo encoding).
    let dragMove: DownState['dragMove'] = null
    if (!shift && !ctrl) {
      const hit = rayPick(px, py)
      if (hit && hit.kind === 'unit' && selectedSet.has(hit.id)) {
        const anchor = groundPlaneXY(px, py)
        if (anchor) {
          const cns = [...selectedSet]
          const original = new Map<number, { wl: [number, number, number]; gameZ: number }>()
          for (const cn of cns) {
            const inst = unitInstances.get(cn)
            if (!inst) continue
            const wl = inst.worldLocation
            const typeId = (inst as any).__wc3ForgeTypeId as string | undefined
            const info = typeId ? unitTypeIndexCache?.[typeId] : undefined
            const moveHeight = info?.move_height ?? 0
            original.set(cn, {
              wl: [wl[0], wl[1], wl[2]],
              gameZ: wl[2] - moveHeight,
            })
          }
          if (original.size > 0) {
            dragMove = { cns, anchorXY: anchor, original, active: false }
          }
        }
      }
    }
    downAt = {
      x: px,
      y: py,
      clientX: e.clientX,
      clientY: e.clientY,
      shift,
      ctrl,
      rubberBanding: false,
      dragMove,
    }
  }

  function updateOverlayRect(rect: { x: number; y: number; w: number; h: number }) {
    overlay.style.left = `${rect.x}px`
    overlay.style.top = `${rect.y}px`
    overlay.style.width = `${rect.w}px`
    overlay.style.height = `${rect.h}px`
    overlay.style.display = 'block'
  }

  function currentRubberRect(curClientX: number, curClientY: number): { x: number; y: number; w: number; h: number } | null {
    if (!downAt) return null
    const r = canvas.getBoundingClientRect()
    const cx = curClientX - r.left
    const cy = curClientY - r.top
    const x = Math.min(downAt.x, cx)
    const y = Math.min(downAt.y, cy)
    const w = Math.abs(cx - downAt.x)
    const h = Math.abs(cy - downAt.y)
    return { x, y, w, h }
  }

  // Topmost region whose bounds contain the world point (smallest-area wins so a
  // region nested inside another stays selectable), or null if none.
  function pickRegionAt(wx: number, wy: number): number | null {
    let best: number | null = null
    let bestArea = Infinity
    for (const r of regionRectsForPick) {
      if (wx >= r.minX && wx <= r.maxX && wy >= r.minY && wy <= r.maxY) {
        const area = (r.maxX - r.minX) * (r.maxY - r.minY)
        if (area < bestArea) { bestArea = area; best = r.creationNumber }
      }
    }
    return best
  }

  // Document-level so we keep tracking the drag even if the cursor leaves
  // the canvas mid-gesture (rubber-band should survive a wobble over the
  // app chrome — matches what every other editor does).
  function onDocMouseMove(e: MouseEvent) {
    // ── Terrain brush stroke (no downAt) ────────────────────────────────
    // Stream a dab whenever the cursor crosses into a new cell. Painting the
    // same cell repeatedly is suppressed via brushLastCellKey so a held-still
    // brush doesn't spam identical edits (raise/lower still accumulates as the
    // user drags across cells, matching the editor's feel).
    if (brushStroking) {
      if (!terrainBrushCallback || !cachedTerrainDTO) return
      const r = canvas.getBoundingClientRect()
      const cell = pickTerrainCell(e.clientX - r.left, e.clientY - r.top, canvas, scene, cachedTerrainDTO)
      if (!cell) return
      const key = cell.row * 100000 + cell.col
      if (key === brushLastCellKey) return
      brushLastCellKey = key
      terrainBrushCallback(cell, 'paint')
      return
    }
    if (!downAt) return

    // ── Gizmo drag path ─────────────────────────────────────────────────
    if (gizmoDragging && gizmo?.isDragging()) {
      const r = canvas.getBoundingClientRect()
      gizmo.onDrag(e.clientX - r.left, e.clientY - r.top, scene, canvas, e.shiftKey)
      return
    }

    // Drag-to-move on a selected unit. Threshold-gated like rubber-band:
    // sub-threshold motion stays as a click (so the mouseup path re-selects),
    // past-threshold motion locks into a drag-move that ignores any further
    // selection events until release. We mutate setLocation on every move
    // tick — the lib's render loop picks up the new worldLocation next RAF.
    if (downAt.dragMove) {
      const dragMove = downAt.dragMove
      if (!dragMove.active) {
        const dist = Math.hypot(e.clientX - downAt.clientX, e.clientY - downAt.clientY)
        if (dist <= CLICK_PIXEL_THRESHOLD) return
        dragMove.active = true
      }
      const r = canvas.getBoundingClientRect()
      const curXY = groundPlaneXY(e.clientX - r.left, e.clientY - r.top)
      if (!curXY) return
      const dx = curXY[0] - dragMove.anchorXY[0]
      const dy = curXY[1] - dragMove.anchorXY[1]
      for (const cn of dragMove.cns) {
        const orig = dragMove.original.get(cn)
        if (!orig) continue
        const inst = unitInstances.get(cn)
        if (!inst) continue
        ;(inst as any).setLocation([orig.wl[0] + dx, orig.wl[1] + dy, orig.wl[2]])
      }
      return
    }
    if (!downAt.rubberBanding) {
      // Promote to rubber-band once the pointer has moved far enough to read as
      // a drag rather than a click. Plain drag box-selects (replace on release);
      // shift+drag adds to the existing selection. The pixel threshold keeps an
      // accidental tiny drag from clearing the selection. Gizmo drags and
      // drag-to-move are handled earlier and return before reaching here, so a
      // drag that gets this far is an empty-space / unselected-entity drag.
      const dist = Math.hypot(e.clientX - downAt.clientX, e.clientY - downAt.clientY)
      if (dist <= CLICK_PIXEL_THRESHOLD) return
      downAt.rubberBanding = true
    }
    const rect = currentRubberRect(e.clientX, e.clientY)
    if (rect) updateOverlayRect(rect) // box outline tracks every mouse event
    // Live preview: recompute what's inside the box so the render loop can
    // cyan-outline everything that would be selected on release. Throttled to
    // ~60Hz — rubberBandPick walks every instance, and raw mousemove can fire
    // at 1000Hz; the box outline above still updates every event.
    const previewNow = performance.now()
    if (rect && !regionMode && previewNow - lastRubberPreviewTs >= RUBBER_PREVIEW_MS) {
      lastRubberPreviewTs = previewNow
      rubberPreviewUnits.clear()
      rubberPreviewDoodads.clear()
      for (const hit of rubberBandPick(rect)) {
        if (hit.kind === 'unit') rubberPreviewUnits.add(hit.id)
        else rubberPreviewDoodads.add(hit.id)
      }
    }
  }
  let lastRubberPreviewTs = 0
  const RUBBER_PREVIEW_MS = 16

  function onDocMouseUp(e: MouseEvent) {
    // ── Terrain brush stroke end ────────────────────────────────────────
    // Close the stroke (the callback closes the undo group). Fires on any
    // button release while a stroke is open — only LMB starts one, so this is
    // the matching LMB-up in practice.
    if (brushStroking) {
      brushStroking = false
      brushLastCellKey = -1
      terrainBrushCallback?.(null, 'end')
      return
    }
    const d = downAt
    downAt = null
    // NOTE: we deliberately do NOT clear the rubber-band preview here. If this
    // gesture commits a selection, the cyan preview is left on screen until the
    // authoritative selection arrives (setSelected clears it) — otherwise the
    // cyan would vanish and leave a highlight-less gap during the Go round-trip,
    // which read as "lag before things get selected". Non-committing rubber-band
    // paths below clear it explicitly.

    // ── Gizmo drag commit ────────────────────────────────────────────────
    if (gizmoDragging) {
      gizmoDragging = false
      if (gizmo?.isDragging()) {
        const r = canvas.getBoundingClientRect()
        gizmo.onDragEnd(e.clientX - r.left, e.clientY - r.top, scene, canvas, e.shiftKey)
      }
      return
    }

    if (!d || e.button !== 0) return

    if (d.dragMove?.active) {
      // Commit each unit's final position via MoveUnit. Sequential (not
      // Promise.all) so the resulting entity-changed events arrive in a
      // predictable order — same reason the Properties path uses a single
      // await + commit. One failure logs via flog and we continue: a partial
      // batch is more useful than blowing the whole drag away.
      //
      // The 3D model has already been moved live during the drag via
      // setLocation, so MoveUnit's role here is to persist into Go memory +
      // mark the session dirty. The OnEntityChanged → setLocation callback
      // is idempotent so the re-paint on event delivery is a no-op.
      const dragMove = d.dragMove
      const r = canvas.getBoundingClientRect()
      const finalXY = groundPlaneXY(e.clientX - r.left, e.clientY - r.top)
      // Recompute the final position from the release coords (don't trust
      // the per-frame setLocation calls' last value — a missed RAF tick
      // could leave the visible position slightly behind the cursor).
      const dx = finalXY ? finalXY[0] - dragMove.anchorXY[0] : 0
      const dy = finalXY ? finalXY[1] - dragMove.anchorXY[1] : 0
      ;(async () => {
        for (const cn of dragMove.cns) {
          const orig = dragMove.original.get(cn)
          if (!orig) continue
          const finalX = orig.wl[0] + dx
          const finalY = orig.wl[1] + dy
          try {
            await MoveUnit(cn, finalX, finalY, orig.gameZ)
          } catch (err) {
            flog(`[drag-move] MoveUnit cn=${cn} failed: ${err instanceof Error ? err.message : String(err)}`)
          }
        }
      })()
      return
    }

    if (d.rubberBanding) {
      // Commit the rubber-band. Shift = add to the current selection; plain =
      // replace it. A degenerate (sub-2px) rect is treated as a non-gesture and
      // left alone. For 'add', an empty rect is a no-op (don't wipe selection
      // when shift-dragging over nothing); for 'set', an empty rect clears
      // (matches click-on-empty), so dragging an empty box deselects.
      overlay.style.display = 'none'
      if (regionMode) {
        // Commit a drag-drawn region: world-space bounds from down → release.
        const rb = canvas.getBoundingClientRect()
        const a = groundPlaneXY(d.x, d.y)
        const b = groundPlaneXY(e.clientX - rb.left, e.clientY - rb.top)
        if (a && b && regionDrawCallback) {
          const minX = Math.min(a[0], b[0]), maxX = Math.max(a[0], b[0])
          const minY = Math.min(a[1], b[1]), maxY = Math.max(a[1], b[1])
          // Ignore a degenerate (tiny) drag so a near-click doesn't make a sliver.
          if (maxX - minX > 8 && maxY - minY > 8) regionDrawCallback(minX, minY, maxX, maxY)
        }
        return
      }
      // Compute the rect from the CAPTURED down-state `d` — `downAt` was nulled
      // at the top of this handler, and currentRubberRect() reads `downAt`, so
      // calling it here would always return null (the bug that made box-select
      // silently select nothing). `d.x/d.y` and the release point are both
      // canvas-relative DOM-top px, matching worldToCanvasPx in rubberBandPick.
      const rr = canvas.getBoundingClientRect()
      const cx = e.clientX - rr.left
      const cy = e.clientY - rr.top
      const rect = {
        x: Math.min(d.x, cx),
        y: Math.min(d.y, cy),
        w: Math.abs(cx - d.x),
        h: Math.abs(cy - d.y),
      }
      // Helper: a non-committing exit must drop the preview itself, since no
      // setSelected will arrive to do it.
      const dropPreview = () => { rubberPreviewUnits.clear(); rubberPreviewDoodads.clear() }
      if (rect.w < 2 || rect.h < 2) { dropPreview(); return }
      const hits = rubberBandPick(rect)
      if (!pickCallback) { dropPreview(); return }
      if (d.shift) {
        // Shift = add. Empty box is a no-op (keep selection) → no setSelected,
        // so clear the preview here; otherwise setSelected clears it.
        if (hits.length > 0) pickCallback(hits, 'add')
        else dropPreview()
      } else {
        // Plain = replace (clears on empty). setSelected always fires → it
        // clears the preview, so the cyan→white handoff has no gap.
        pickCallback(hits, 'set')
      }
      return
    }

    // Click vs drag. A non-rubber-banding mouseup whose mousedown was on
    // the canvas counts as a click iff it ended close to where it started.
    const dist = Math.hypot(e.clientX - d.clientX, e.clientY - d.clientY)
    if (dist > CLICK_PIXEL_THRESHOLD) return

    // Region mode: a plain click selects the region under the cursor (or clears).
    if (regionMode) {
      const w = groundPlaneXY(d.x, d.y)
      regionPickCallback?.(w ? pickRegionAt(w[0], w[1]) : null)
      return
    }

    // Doodad-placement mode takes over plain clicks BEFORE terrain-pick.
    // We pick the terrain cell (for the ground intersection), sample the
    // surface height there so the doodad lands on the ground, and hand both
    // to the placement callback. A click that misses the map is a no-op
    // (no callback) — placement stays armed for the next click. Modifier-held
    // clicks fall through to entity-pick so the user can briefly select an
    // existing entity without disarming the brush.
    if (placementMode && !d.shift && !d.ctrl) {
      if (placementCallback && cachedTerrainDTO) {
        const cell = pickTerrainCell(d.x, d.y, canvas, scene, cachedTerrainDTO)
        if (cell) {
          const z = sampleTerrainHeight(cell.worldX, cell.worldY)
          placementCallback({ worldX: cell.worldX, worldY: cell.worldY, z })
        }
      }
      return
    }

    // Terrain-pick mode takes over plain clicks. Modifier-held clicks are
    // intentionally still entity-pick / no-op so users can briefly pop out
    // of terrain-pick to add/remove an entity from selection without leaving
    // the mode (matches the convention rubber-band mode also follows). The
    // terrain-pick callback receives null when the click misses the map
    // (sky, outside bounds) — caller decides whether to clear UI or ignore.
    if (terrainPickMode && !d.shift && !d.ctrl) {
      if (terrainPickCallback && cachedTerrainDTO) {
        const cell = pickTerrainCell(d.x, d.y, canvas, scene, cachedTerrainDTO)
        terrainPickCallback(cell)
      }
      return
    }

    const hit = rayPick(d.x, d.y)
    if (!pickCallback) return

    if (hit) {
      pickCallback([hit], modeFromModifiers(d.shift, d.ctrl))
      return
    }
    // Empty click. Modifier-held empty clicks are no-ops so a stray click
    // in the void doesn't wipe an in-progress multi-select. Plain empty
    // click clears.
    if (!d.shift && !d.ctrl) pickCallback([], 'set')
  }

  function onWindowKeyDown(e: KeyboardEvent) {
    if (e.key !== 'Escape') return
    // Don't steal Escape from text inputs / contenteditable / form controls.
    // Future inline-edit fields, search boxes, or modals need to handle their
    // own Escape (close themselves) without losing the viewport selection too.
    const a = document.activeElement as HTMLElement | null
    if (a) {
      const tag = a.tagName
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return
      if (a.isContentEditable) return
    }
    // Mid-stroke Escape closes the terrain brush stroke (ends the undo group)
    // so the partial stroke is committed as one step rather than left dangling.
    if (brushStroking) {
      brushStroking = false
      brushLastCellKey = -1
      terrainBrushCallback?.(null, 'end')
      return
    }
    // Placement mode Escape: disarm the palette brush (callback handles the
    // UI-side disarm). Takes priority over selection-clear so a user mid-place
    // who hits Escape exits placement rather than wiping their selection.
    if (placementMode) {
      placementCallback?.(null)
      return
    }
    // Region mode Escape: cancel an in-progress draw, else clear the selection.
    if (regionMode) {
      if (downAt) { downAt = null; overlay.style.display = 'none' }
      else regionPickCallback?.(null)
      return
    }
    // Mid-drag Escape: cancel any active drag without committing.
    // For gizmo drags: restore original positions. For entity drags: snap back.
    // Selection is NOT cleared — Escape cancels the drag, not the selection.
    if (gizmoDragging && gizmo?.isDragging()) {
      gizmo.cancelDrag()
      gizmoDragging = false
      downAt = null
      overlay.style.display = 'none'
      return
    }
    if (downAt?.dragMove?.active) {
      const dragMove = downAt.dragMove
      for (const cn of dragMove.cns) {
        const orig = dragMove.original.get(cn)
        if (!orig) continue
        const inst = unitInstances.get(cn)
        if (!inst) continue
        ;(inst as any).setLocation(orig.wl)
      }
      // Drop the gesture state so the impending mouseup is a no-op.
      // Rubber-band overlay (if visible) is also hidden in case the user
      // managed to be in both states somehow.
      downAt = null
      overlay.style.display = 'none'
      return
    }
    if (!pickCallback) return
    pickCallback([], 'set')
  }

  canvas.addEventListener('mousedown', onCanvasMouseDown)
  // Suppress the default WebView context menu when the user right-clicks to
  // cancel a gizmo drag. Otherwise both the cancel-drag and the menu fire,
  // and the menu lingers until dismissed.
  function onCanvasContextMenu(e: MouseEvent) {
    if (gizmoDragging && gizmo?.isDragging()) { e.preventDefault(); return }
    // Right-click while a placement brush is armed disarms it (the callback
    // does the UI-side disarm, which routes back through setPlacementMode and
    // drops the ghost). Only a stationary click counts — a right-DRAG is a
    // camera pan and must keep the brush armed. Suppress the WebView menu so
    // it doesn't pop on the disarming click.
    if (placementMode && rmbDownAt) {
      const dist = Math.hypot(e.clientX - rmbDownAt.x, e.clientY - rmbDownAt.y)
      rmbDownAt = null
      if (dist <= CLICK_PIXEL_THRESHOLD) {
        e.preventDefault()
        placementCallback?.(null)
        return
      }
    }
    rmbDownAt = null
  }
  canvas.addEventListener('contextmenu', onCanvasContextMenu)
  // Listen on document for move/up so the rubber-band gesture survives the
  // cursor leaving the canvas. Mirrors the camera controller's window-level
  // mouseup listener for the same reason.
  document.addEventListener('mousemove', onDocMouseMove)
  document.addEventListener('mouseup', onDocMouseUp)
  window.addEventListener('keydown', onWindowKeyDown)

  // Cursor feedback for hover. Picks per mousemove but throttled to ~30 Hz —
  // a full rayPick on every native mousemove event (which can fire at 1000Hz
  // on high-poll mice) walks every unit + doodad + sloc and would chug on
  // 500+-entity maps. ~33ms gating keeps the cursor responsive without
  // burning CPU. We also skip when a drag-related gesture is in progress
  // (downAt set) — once the user is committed to a drag the cursor doesn't
  // need to reflect what's under it.
  let lastHoverTs = 0
  const HOVER_THROTTLE_MS = 33
  // Reposition the brush footprint ring under the given client coords (or hide
  // it when the pointer is off the map). Sampled to terrain height each move so
  // the ring hugs the surface.
  function updateBrushCursorAt(clientX: number, clientY: number) {
    if (!brushCursor) return
    lastBrushClient = { x: clientX, y: clientY }
    const r = canvas.getBoundingClientRect()
    const xy = groundPlaneXY(clientX - r.left, clientY - r.top)
    if (!xy) {
      brushCursor.setBrush(null)
      return
    }
    // Preview plane Z must match the water RENDERER, which draws the surface at
    // water_z + the per-tileset Water.slk offset*128 (see water.ts). The dialed
    // fixed height is the pre-offset water_z, so add the same offset here — without
    // it the preview floats above (or below) where the water actually lands.
    let planeZ: number | undefined = undefined
    if (brushShape.waterPlaneZ != null) {
      const waterOffsetStuds = ((cachedTerrainDTO as any)?.water?.offset ?? 0) * 128
      planeZ = brushShape.waterPlaneZ + waterOffsetStuds
    }
    brushCursor.setBrush({
      cx: xy[0],
      cy: xy[1],
      // Match the Go circle threshold's (r+0.5) reach so the ring frames the
      // cells actually edited.
      radius: (brushShape.radius + 0.5) * 128,
      shape: brushShape.shape,
      sampleZ: (x, y) => sampleTerrainHeight(x, y),
      // Fixed-height water preview plane (Add Water + Fixed height); null = none.
      planeZ,
    })
  }

  function onCanvasHoverMove(e: MouseEvent) {
    // Terrain-brush cursor ring follows the pointer — including while painting
    // with LMB held (no downAt during a brush stroke). Owns the cursor; skips
    // the entity hover-pick entirely.
    if (terrainBrushMode) {
      updateBrushCursorAt(e.clientX, e.clientY)
      if (canvas.style.cursor !== 'crosshair') canvas.style.cursor = 'crosshair'
      return
    }
    if (downAt) return // suppress hover-pick during any active gesture
    // e.buttons bit 0=LMB, bit 1=RMB, bit 2=MMB. Any held button means an
    // ongoing drag (LMB without downAt is weird but safe to ignore; RMB/MMB
    // = camera pan, no point updating cursor under a pan).
    if (e.buttons !== 0) return
    // Terrain-pick mode owns the cursor — keep crosshair regardless of what's
    // under the pointer. The picker itself doesn't care whether anything's
    // there; it picks a terrain cell or null. Without this short-circuit the
    // hover-pick below would flip the cursor to "pointer" / "move" any time
    // the user passed over an entity, which is misleading.
    // Placement mode owns the cursor for the same reason terrain-pick does —
    // a crosshair signals "click to drop here" regardless of what's underneath.
    if (placementMode || terrainPickMode) {
      clearHover() // crosshair modes own the cursor; drop any pre-click hint
      if (canvas.style.cursor !== 'crosshair') canvas.style.cursor = 'crosshair'
      // Slide the preview ghost to the terrain cell under the cursor (same
      // pick + height-sample the click-to-place path uses, so the preview
      // lands exactly where the drop will). Throttled with the shared hover
      // gate — 30 Hz is smooth and keeps pickTerrainCell off the 1000 Hz mouse
      // firehose. A miss (cursor off the map) just freezes the ghost in place.
      if (placementMode && placementGhostInst && cachedTerrainDTO) {
        const now = performance.now()
        if (now - lastHoverTs >= HOVER_THROTTLE_MS) {
          lastHoverTs = now
          const r = canvas.getBoundingClientRect()
          const cell = pickTerrainCell(e.clientX - r.left, e.clientY - r.top, canvas, scene, cachedTerrainDTO)
          if (cell) {
            const z = sampleTerrainHeight(cell.worldX, cell.worldY)
            // setLocation (absolute), NOT move (relative += offset) — the hover
            // handler fires every frame, so a relative move would accumulate
            // and fling the ghost off the map after frame one.
            placementGhostInst.setLocation([cell.worldX, cell.worldY, z])
            if (!placementGhostPlaced) { placementGhostInst.show(); placementGhostPlaced = true }
          }
        }
      }
      return
    }
    const now = performance.now()
    if (now - lastHoverTs < HOVER_THROTTLE_MS) return
    lastHoverTs = now
    const r = canvas.getBoundingClientRect()
    const px = e.clientX - r.left
    const py = e.clientY - r.top
    // Gizmo hover takes priority over entity hover. rayPick on the gizmo also
    // updates the hoverAxis state used by draw() for handle highlighting.
    if (gizmo && gizmoSelectionItems.length > 0) {
      const gHit = gizmo.rayPick(px, py, scene, canvas)
      if (gHit) {
        clearHover() // gizmo handle under pointer — not an entity pre-click
        if (canvas.style.cursor !== 'crosshair') canvas.style.cursor = 'crosshair'
        return
      }
    }
    const hit = rayPick(px, py)
    // Pre-click highlight: glow the entity under the pointer so the user can see
    // what a click would select before committing. Doodads get a cyan fill +
    // outline; units/buildings get the cyan outline only (no fill — it'd clobber
    // their team-colour). Already-selected entities keep their white selection
    // outline (we skip hover on them); anything else clears the hint.
    if (hit && hit.kind === 'doodad') {
      setDoodadHover(hit.id)
      hoveredUnit = null
    } else if (hit && hit.kind === 'unit' && !selectedSet.has(hit.id)) {
      clearDoodadHover()
      hoveredUnit = hit.id
    } else {
      clearHover()
    }
    // Any entity under the cursor → 'pointer' (it's clickable to select). We do
    // NOT switch to 'move' for selected entities: moving is done with the gizmo
    // (which shows its own crosshair when a handle is under the pointer), so a
    // move cursor on the entity body is misleading — especially for doodads,
    // which can only be moved via the gizmo in the first place.
    const cursor = hit ? 'pointer' : 'default'
    if (canvas.style.cursor !== cursor) canvas.style.cursor = cursor
  }
  function onCanvasHoverLeave() {
    clearHover() // pointer left the canvas — drop the pre-click hint
    if (canvas.style.cursor !== '') canvas.style.cursor = ''
    // Hide the brush ring when the pointer leaves the canvas (unless a stroke
    // is mid-flight, in which case the doc-level move keeps painting and the
    // ring re-appears on re-entry).
    if (!brushStroking) brushCursor?.setBrush(null)
  }
  canvas.addEventListener('mousemove', onCanvasHoverMove)
  canvas.addEventListener('mouseleave', onCanvasHoverLeave)

  // Internal helper for re-positioning a placed unit instance to (x, y, z) in
  // game-space. Used by both the public SceneAPI.updateUnitPosition (kept for
  // direct callers) AND the entity-changed event subscriber below.
  //
  // Mirrors placeUnit's Z-offset: the type's move_height was applied at
  // placement and must be re-applied here so the instance lands at the same
  // visual offset above the ground as a freshly-placed unit at the same
  // game coords.
  function updateUnitPositionImpl(cn: number, x: number, y: number, z: number): void {
    const inst = unitInstances.get(cn)
    // Defense-in-depth: silently skip if the cn isn't a placed unit. Slocs
    // are also in this map (registered by slocRenderer); doodads and stale
    // creation_numbers from a race with map-changed all silently return.
    if (!inst) return
    const typeId = (inst as any).__wc3ForgeTypeId as string | undefined
    const info = typeId ? unitTypeIndexCache?.[typeId] : undefined
    const moveHeight = info?.move_height ?? 0
    // setLocation (not move) — move() is ADDITIVE (vec3.add into
    // localLocation). It only worked as "set" in placeUnit because the
    // instance was fresh with localLocation=[0,0,0]. Here localLocation is
    // already non-zero, so we need an absolute set.
    ;(inst as any).setLocation([x, y, z + moveHeight])
    nudgeSkeletonRefresh(inst)
  }

  // Workaround for an mdx-m3-viewer quirk: setLocation/setRotation/setScale
  // on the root MdxModelInstance correctly updates the root's worldLocation
  // and walks immediate children via Node.recalculateTransformation, but the
  // skeletal bones only refresh inside MdxModelInstance.updateAnimations when
  // their `wasDirty` flag is set — and a manual transform on the root doesn't
  // propagate wasDirty downward. Re-applying the current sequence flips
  // MdxModelInstance.forced (see MdxModelInstance.setSequence), which makes
  // the next updateAnimations pass do a full skeletal recalc.
  //
  // Without this, models with skeletal sub-geometry (CircleOfPower's rune
  // ring, units with attached weapons / particle emitters) visibly lag behind
  // a manual transform until the next animation event happens to flip
  // wasDirty organically — looks like "the center moved but the rest didn't."
  function nudgeSkeletonRefresh(inst: any): void {
    const seq = inst?.sequence
    if (typeof seq === 'number' && seq >= 0 && typeof inst.setSequence === 'function') {
      inst.setSequence(seq)
    }
  }

  // Internal helper for re-positioning a placed doodad instance. Parallel
  // to updateUnitPositionImpl, with one structural difference: doodads have
  // NO move_height offset (placeDoodad uses d.position verbatim, no SLK Z
  // adjustment unlike units), so the JS-side Z passes through unchanged.
  // setLocation (absolute), not move (additive) — same rationale as units.
  function updateDoodadPositionImpl(cn: number, x: number, y: number, z: number): void {
    const inst = doodadInstances.get(cn)
    if (!inst) return
    ;(inst as any).setLocation([x, y, z])
  }

  // Remove a deleted entity's rendered instance from the scene. Detaches the
  // mdx-m3-viewer instance (drops it from the render walk) and clears the
  // per-cn bookkeeping the same way clearInstances() does on map reload, but
  // for a single entity. Fired from the entity-changed handler on Field
  // "deleted" so GUI deletes, the MCP doodads.delete / units.delete tools, and
  // undo of a create all converge on the same teardown.
  function removeEntityImpl(kind: string, cn: number): void {
    if (kind === 'unit') {
      const inst = unitInstances.get(cn)
      if (inst) {
        try { inst.detach() } catch { /* already detached */ }
        unitInstances.delete(cn)
      }
      manualAnimCns.delete(cn)
    } else if (kind === 'doodad') {
      const inst = doodadInstances.get(cn)
      if (inst) {
        try { inst.detach() } catch { /* already detached */ }
        doodadInstances.delete(cn)
        doodadInstancesToReroll.delete(inst)
      }
      doodadCategoryByCn.delete(cn)
      // A removed doodad may have been the last of its category — refresh the
      // present-categories list so the View menu drops the now-empty entry.
      recomputeDoodadCategories()
    }
  }

  // Subscribe to Go-side entity-change events. Any mutation that fires
  // OnEntityChanged (MoveUnit + MoveDoodad today; future SetRotation/etc.)
  // flows through here, regardless of whether the mutator was the JS
  // Properties panel, the MCP bridge, or any future code path. This is what
  // keeps the 3D scene in sync with bridge-driven edits without polling.
  //
  // Field-aware: ignores changes whose Field isn't one we handle (no-op for
  // future Field values that don't map to scene state). Kind-branched so
  // unit + doodad position updates take their respective code paths.
  const ENTITY_EVENT = 'wc3-forge:entity-changed'
  type EntityChangedPayload = {
    kind: string
    id: number
    field: string
    position?: number[]
    rotation?: number
    scale?: number[]
  }
  // Update inst.localRotation to a yaw-only Z quat. Loses any pitch/roll
  // that placeDoodad composed in — acceptable, same compromise as the
  // gizmo's live-preview path. Next loadMap rebuilds the full composition.
  function applyRotation(inst: any, rot: number) {
    const h = rot / 2
    const q: [number, number, number, number] = [0, 0, Math.sin(h), Math.cos(h)]
    if (typeof inst.setRotation === 'function') {
      inst.setRotation(q)
    }
    inst.__wc3ForgeRotation = rot
    nudgeSkeletonRefresh(inst)
  }
  function applyScale(inst: any, s: number[]) {
    // Per-axis scale via setScale so the Roblox-style gizmo's per-axis
    // commits actually render correctly. Previously called uniformScale
    // (which only uses s[0]); for an X-only drag that wrote (2,1,1) to
    // disk but the uniformScale(2) ALSO grew Y and Z; for a Y-only drag
    // that wrote (1,2,1) but the uniformScale(1) snapped everything back
    // to 1× — the "size changes on mouseup" bug.
    //
    // Stash captures all three components so the gizmo's orig snapshot
    // reads what was actually committed.
    const modelScale = inst.__wc3ForgeModelScale ?? 1
    if (typeof inst.setScale === 'function') {
      inst.setScale([s[0] * modelScale, s[1] * modelScale, s[2] * modelScale])
    } else if (typeof inst.uniformScale === 'function') {
      inst.uniformScale(s[0] * modelScale)
    }
    inst.__wc3ForgeScale = [s[0], s[1], s[2]]
    nudgeSkeletonRefresh(inst)
  }
  // Live terrain re-render during brush strokes, split into two tiers by cost
  // (measured via the bridge-paint + diagnostics harness):
  //
  //   TERRAIN geometry — cheap: terrain.update() re-uploads vertex buffers and
  //     reuses the baked atlas. Runs THROTTLED (~every 90ms) so the user sees
  //     paint/height land live. On an empty 65² map this held ~78fps, no stall.
  //
  //   WATER — expensive: buildWater re-fetches the terrain DTO and rebuilds the
  //     whole water mesh. Run PER DAB this tanked the RAF to ~15fps with 200ms
  //     stalls (age_ms) — the original "flicker + lag, worse on full maps." So it
  //     runs on its OWN THROTTLE (~every 120ms, a touch slower than terrain): the
  //     user sees water fill in live as they paint, but rebuilds are capped well
  //     below the dab rate so a fast stroke can't pile them up, and the throttle
  //     coalesces — one rebuild reflects every dab since the last. A trailing
  //     rebuild after the final dab lands the settled surface. (Cliffs are
  //     similar but ride the throttled TERRAIN tier above — see runTerrain.)
  const TERRAIN_THROTTLE_MS = 90
  const WATER_THROTTLE_MS = 120
  let terrainDirty = false
  let cliffsDirty = false
  let waterDirty = false
  let terrainRunning = false
  let terrainTimer: ReturnType<typeof setTimeout> | null = null
  let lastTerrainTs = -1e9
  let heavyRunning = false
  let heavyTimer: ReturnType<typeof setTimeout> | null = null
  let lastHeavyTs = -1e9
  // Identity of the ground palette a terrain mesh was baked against. When this
  // is unchanged between rebuilds, terrain.update() can reuse the atlas; when it
  // changes (tileset swap), we rebuild the mesh so the atlas matches.
  function terrainPaletteKey(t: any): string {
    return Array.isArray(t?.palette) ? t.palette.join('|') : ''
  }
  // Translate the entity-changed field into which surfaces need rebuilding, then
  // schedule the appropriate tier. Terrain AND cliffs go in the fast throttled
  // tier and rebuild from the SAME fresh fetch, in lockstep: terrain.update
  // skips cliff cells (holes) the instant a layer changes, so the cliff walls
  // MUST rebuild at the same cadence or you get voids ("invisible cliffs").
  // Cliffs are rebuilt exactly as loadMap does (fresh GetTerrain → compute →
  // render) — reusing a cached DTO produced fewer/no placements. Only water
  // (expensive, never edited by these brushes) is deferred to the slow tier.
  function markTerrainDirty(field: string) {
    switch (field) {
      case 'tile': terrainDirty = true; break
      case 'height': terrainDirty = true; cliffsDirty = true; waterDirty = true; break // cliffs re-drape over new heights
      case 'cliff': terrainDirty = true; cliffsDirty = true; break
      case 'ramp': terrainDirty = true; cliffsDirty = true; break
      case 'water': waterDirty = true; break
      default: terrainDirty = true; cliffsDirty = true; waterDirty = true; break // unknown → all
    }
    if (terrainDirty || cliffsDirty) scheduleTerrain()
    if (waterDirty) scheduleHeavy()
  }
  // --- Tier 1: terrain geometry + cliffs (throttled, in lockstep) ---
  function scheduleTerrain() {
    if (terrainTimer !== null || terrainRunning) return
    const wait = Math.max(0, TERRAIN_THROTTLE_MS - (performance.now() - lastTerrainTs))
    terrainTimer = setTimeout(() => { terrainTimer = null; void runTerrain() }, wait)
  }
  async function runTerrain() {
    if (terrainRunning) { scheduleTerrain(); return }
    if (!terrainDirty && !cliffsDirty) return
    terrainRunning = true
    lastTerrainTs = performance.now()
    const doTerrain = terrainDirty, doCliffs = cliffsDirty
    terrainDirty = cliffsDirty = false
    try {
      const prevPaletteKey = terrainPaletteKey(cachedTerrainDTO)
      const t = await GetTerrain()
      cachedTerrainDTO = t
      cellHighlight?.setTerrain(t as unknown as any)
      const gl = (viewer as any).gl as WebGLRenderingContext
      if (doTerrain) {
        const paletteChanged = terrainPaletteKey(t) !== prevPaletteKey
        if (terrain && !paletteChanged) {
          terrain.update(t as unknown as any)
        } else {
          const newTerrain = await buildTerrain(gl, viewer as any, pathSolver, t as unknown as any)
          terrain?.dispose(); terrain = newTerrain
        }
        sceneDiag.terrainDrawCalls = terrain?.drawCallCount ?? 0
      }
      if (doCliffs) {
        // Build-then-swap (no null gap), same path + same fresh `t` as loadMap.
        const cliffPlacements = computeCliffPlacements(t as unknown as any)
        const newCliffs = cliffPlacements.length > 0
          ? await renderCliffs(gl, viewer as any, pathSolver, cliffPlacements, t as unknown as any)
          : null
        cliffs?.dispose(); cliffs = newCliffs
      }
    } catch (e) {
      flog('[terrain update]', e instanceof Error ? e.message : String(e))
    } finally {
      terrainRunning = false
      if (terrainDirty || cliffsDirty) scheduleTerrain()
    }
  }
  // --- Tier 2: water (throttled — rebuilds live during a stroke, ~every 120ms) ---
  function scheduleHeavy() {
    if (heavyTimer !== null || heavyRunning) return
    const wait = Math.max(0, WATER_THROTTLE_MS - (performance.now() - lastHeavyTs))
    heavyTimer = setTimeout(() => { heavyTimer = null; void runHeavy() }, wait)
  }
  async function runHeavy() {
    if (heavyRunning) { scheduleHeavy(); return }
    if (!waterDirty) return
    heavyRunning = true
    lastHeavyTs = performance.now()
    waterDirty = false
    try {
      // Always fetch fresh. A water-only edit (terrain.brush_water) doesn't dirty
      // the terrain tier, so cachedTerrainDTO would be stale here and the new
      // water flags/levels wouldn't render. The throttle caps how often this
      // GetTerrain + mesh rebuild runs; we also write the DTO back so terrain-cell
      // picking keeps a current snapshot.
      const t = await GetTerrain()
      cachedTerrainDTO = t
      const gl = (viewer as any).gl as WebGLRenderingContext
      const newWater = await buildWater(gl, viewer as any, pathSolver, t as unknown as any)
      water?.dispose(); water = newWater
    } catch (e) {
      flog('[terrain water rebuild]', e instanceof Error ? e.message : String(e))
    } finally {
      heavyRunning = false
      if (waterDirty) scheduleHeavy()
    }
  }

  EventsOn(ENTITY_EVENT, (payload: EntityChangedPayload) => {
    if (!payload) return
    const kind = payload.kind
    // Terrain edits (brush dab, MCP terrain.*, undo/redo). The field names what
    // changed (tile/height/cliff/ramp) so we throttle + rebuild only the
    // affected surface instead of the whole composition per event.
    if (kind === 'terrain') {
      markTerrainDirty(payload.field || '')
      return
    }
    if (payload.field === 'position') {
      const p = payload.position
      if (!p || p.length < 3) return
      if (kind === 'unit')        updateUnitPositionImpl(payload.id, p[0], p[1], p[2])
      else if (kind === 'doodad') updateDoodadPositionImpl(payload.id, p[0], p[1], p[2])
    } else if (payload.field === 'rotation') {
      const r = payload.rotation
      if (typeof r !== 'number') return
      const inst = kind === 'unit' ? unitInstances.get(payload.id) : doodadInstances.get(payload.id)
      if (inst) applyRotation(inst, r)
    } else if (payload.field === 'scale') {
      const s = payload.scale
      if (!s || s.length < 3) return
      const inst = kind === 'unit' ? unitInstances.get(payload.id) : doodadInstances.get(payload.id)
      if (inst) applyScale(inst, s)
    } else if (payload.field === 'deleted') {
      removeEntityImpl(kind, payload.id)
    } else if (payload.field === 'sky_model' && kind === 'info') {
      // Sky changed via undo/redo/discard (or any source — picker, MCP).
      // The EntityChange payload doesn't carry a string, so refetch the
      // resolved path and rebuild.
      void reloadSkyFromGo()
    }
  })

  async function reloadSkyFromGo() {
    try {
      const t = await GetTerrain()
      const path = ((t as any)?.sky_model as string | undefined) ?? ''
      if (sky) {
        sky.dispose()
        sky = null
      }
      if (path) {
        const glLocal = (viewer as any).gl as WebGLRenderingContext
        sky = await buildSky(glLocal, viewer as any, pathSolver, scene, path)
      }
    } catch (e) {
      flog('[sky undo-refresh]', e instanceof Error ? e.message : String(e))
    }
  }

  return {
    async reloadSky(path: string) {
      if (sky) {
        sky.dispose()
        sky = null
      }
      if (path) {
        try {
          const glLocal = (viewer as any).gl as WebGLRenderingContext
          sky = await buildSky(glLocal, viewer as any, pathSolver, scene, path)
          if (sky) flog(`[sky] reloaded ${path}`)
        } catch (e) {
          flog('[sky reload]', e instanceof Error ? e.message : String(e))
        }
      } else {
        flog('[sky] cleared (empty path → gradient only)')
      }
    },
    async loadMap(opts?: { keepCamera?: boolean }) {
      // Serialize against any in-flight loadMap (see loadMapSerial above) so two
      // placement walks never race and leave orphaned ghost instances. Each call
      // waits for the prior one to finish its full clear+place cycle first.
      const prior = loadMapSerial
      let releaseSerial!: () => void
      loadMapSerial = new Promise<void>((r) => { releaseSerial = r })
      try { await prior } catch { /* a prior load failing must not block this one */ }
      const keepCamera = !!opts?.keepCamera
      // U5 scene-load stats: mark in-flight, bump generation, start the timer.
      // loadMs / lastLoad are committed in the finally below so a thrown
      // terrain/asset load still reports a (partial) duration + the placement
      // tallies accumulated up to the failure point.
      sceneDiag.loadingMap = true
      sceneDiag.loadGeneration++
      const loadStartTs = performance.now()
      try {
      clearInstances()
      // Terrain first so it's visible even while units/doodads stream in.
      try {
        const t = await GetTerrain()
        // Cache for the terrain-pick path — picker reads cell data straight
        // from this without a Go round-trip per click.
        cachedTerrainDTO = t
        // Hand the same DTO to the cell-highlight overlay (reads per-corner Z
        // and the center_offset to position the wireframe at the right cell).
        // Also clear any stale cell highlight from a prior map — the (col,
        // row) it referred to may no longer exist on the new grid.
        if (cellHighlight) {
          cellHighlight.setTerrain(t as unknown as any)
          cellHighlight.setCell(null)
        }
        // Drop any stale brush ring from a prior map (it re-samples the new
        // heightfield on the next hover-move).
        brushCursor?.setBrush(null)
        const gl = (viewer as any).gl as WebGLRenderingContext
        terrain = await buildTerrain(gl, viewer as any, pathSolver, t as unknown as any)
        sceneDiag.terrainDrawCalls = terrain?.drawCallCount ?? 0
        if (terrain && !keepCamera) {
          // Frame the map. width/height in w3e are vertex counts; tile size
          // is 128, so playable span is roughly (width-1)*128 in each axis.
          // Skip when keepCamera is true — that's the case for in-place
          // reloads like a graphics-mode toggle, where re-framing would
          // throw the user back to the default view they just panned away
          // from.
          const tw = ((t as any).width - 1) * 128
          const th = ((t as any).height - 1) * 128
          const span = Math.max(tw, th)
          // Center the camera on the actual mesh center, not game-coord origin.
          // For maps with non-symmetric border padding, those differ.
          const cx = (t as any).center_offset[0] + tw / 2
          const cy = (t as any).center_offset[1] + th / 2
          camera.frame(cx, cy, span)
        }
        // Cliff transition meshes. Walks per-corner layer-heights and
        // selects the appropriate Doodads/Terrain/Cliffs/Cliffs<pattern>N.mdx
        // for each cell with a layer-height transition.
        const cliffPlacements = computeCliffPlacements(t as unknown as any)
        if (cliffPlacements.length > 0) {
          cliffs = await renderCliffs(gl, viewer as any, pathSolver, cliffPlacements, t as unknown as any)
        }
        // Water surface. Per-cell quad mesh with HiveWE-style depth-blended
        // color × animated wave texture from Water.slk. Rendered after
        // viewer.render() in the RAF loop with alpha blending — depth-test
        // ON, depth-write OFF so multiple translucent pixels at the same Z
        // don't fight.
        water = await buildWater(gl, viewer as any, pathSolver, t as unknown as any)
        if (water) {
          flog(`[water] ${water.triCount} triangles, ${water.frameCount} animation frames`)
        }
        // Sky dome — pulled from the resolved SkyModel path that Go shipped
        // with the terrain payload (script SetSkyModel call wins; tileset
        // default fallback). Empty string = no sky, flat clear remains.
        const skyPath = (t as any).sky_model as string | undefined
        if (skyPath) {
          sky = await buildSky(gl, viewer as any, pathSolver, scene, skyPath)
          if (sky) {
            flog(`[sky] loaded ${skyPath} (${(t as any).sky_model_from_script ? 'script' : 'tileset default'})`)
          }
        }
        // Pathing overlay. Built unconditionally so the toggle is
        // instant — the per-frame cost when invisible is one branch.
        // Empty pathing DTOs (map without war3map.wpm) return null and
        // the toggle becomes a no-op for that map.
        try {
          const p = await GetPathingMap()
          if (p && p.width > 0 && p.height > 0) {
            pathing = buildPathingOverlay(gl, p as unknown as any, t as unknown as any)
          }
        } catch (e) {
          flog('[pathing load]', e instanceof Error ? e.message : String(e))
        }
      } catch (e) {
        flog('[terrain load]', e instanceof Error ? e.message : String(e))
      }
      const [units, doodads, uTypes, dTypes] = await Promise.all([
        ListUnits(), ListDoodads(), getUnitTypes(), getDoodadTypes(),
      ])
      // Split slocs out of the unit list. Slocs (start-location markers)
      // have type_id "sloc" and no MDX in stock data — they're rendered as
      // colored pillars by slocRenderer instead. Filter here so placeUnit
      // doesn't try to load a non-existent MDX for each one.
      const slocMarkers: SlocMarker[] = []
      const realUnits: typeof units = []
      for (const u of units) {
        if (u.type_id === 'sloc') {
          slocMarkers.push({
            creationNumber: u.creation_number,
            player: u.player,
            position: [u.position[0], u.position[1], u.position[2]],
            rotation: u.rotation,
          })
        } else {
          realUnits.push(u)
        }
      }
      slocRenderer?.setMarkers(slocMarkers)
      // Quick diagnostic: log a sloc bounding box so we can spot maps where
      // every sloc is degenerately stacked at the same position (Enfo's FFB
      // does this — see the doodad-audit notes in the project memory).
      if (slocMarkers.length > 0) {
        let sxmin = Infinity, sxmax = -Infinity, symin = Infinity, symax = -Infinity
        for (const m of slocMarkers) {
          if (m.position[0] < sxmin) sxmin = m.position[0]
          if (m.position[0] > sxmax) sxmax = m.position[0]
          if (m.position[1] < symin) symin = m.position[1]
          if (m.position[1] > symax) symax = m.position[1]
        }
        const stacked = (sxmin === sxmax && symin === symax)
        flog(`[slocs] n=${slocMarkers.length} bbox=x[${sxmin.toFixed(0)},${sxmax.toFixed(0)}] y[${symin.toFixed(0)},${symax.toFixed(0)}]${stacked ? ' (all stacked at same pos)' : ''}`)
      }

      // Sequential to keep diagnostics legible during bring-up; once stable
      // we'll batch via Promise.allSettled for parallel asset fetching.
      let uPlaced = 0, uSkipped = 0
      for (const u of realUnits) {
        const inst = await placeUnit(u, uTypes)
        if (inst) {
          unitInstances.set(u.creation_number, inst)
          uPlaced++
        } else {
          uSkipped++
        }
      }
      // Doodad placement + audit instrumentation.
      //
      // We collect:
      //   - placed/skipped counters
      //   - per-typeID skip counts (typeIDs that 404'd or had no MDX entry,
      //     which lets us spot patterns of missing assets)
      //   - sample world data for the first few placed instances
      //     (worldLocation, worldScale, bounds.r) so we can prove they're
      //     in the right place / right size and decide if "invisible" means
      //     "broken" or just "tiny relative to map span".
      let dPlaced = 0, dSkipped = 0
      let pathBlockerCount = 0
      let xmin = Infinity, xmax = -Infinity, ymin = Infinity, ymax = -Infinity
      const skipReasons = new Map<string, number>()
      const sampleAudits: string[] = []
      // Collected path-blocker markers, handed to the overlay renderer in
      // one batch after the doodad walk completes. Sized at most by the
      // doodad count (typical maps have a few dozen blockers; Enfo's FFB
      // has ~230). Per-marker tag of the doodad's creation_number so the
      // ray-pick AABB walker can route selection through the same path
      // unit / doodad picks use.
      const pathBlockerMarkers: PathBlockerMarker[] = []
      // Per-instance scale for path-blocker box sizing. Doodad scale[0] is
      // the X-axis scale stored in war3map.doo (parsed and /128'd by the Go
      // side); multiplying our default 32-stud half-extent by it lets a map
      // author scale a blocker up to cover a larger pathing footprint and
      // have the box grow to match. SLK base scale (defScale) also multiplies
      // in — same as how placeDoodad combines uniformScale. The 32-stud
      // default = 64-stud side = 1/4 the area of one 128-stud terrain tile,
      // matching the user-spec "1/4 square unit of tile size" — large enough
      // to spot at any zoom, small enough to not cover adjacent doodads.
      const blockerHalf = (s: number, baseScale: number) => 32 * (s || 1) * (baseScale || 1)
      for (const d of doodads) {
        if (d.position[0] < xmin) xmin = d.position[0]
        if (d.position[0] > xmax) xmax = d.position[0]
        if (d.position[1] < ymin) ymin = d.position[1]
        if (d.position[1] > ymax) ymax = d.position[1]
        // Path blockers (category "Pathing Blockers", letter "P" in
        // Doodads.slk) almost always ship without an MDX — they exist only
        // to occupy pathing-map cells. Without special handling these get
        // skipped by placeDoodad (its `!info.file` guard returns null) and
        // are completely invisible in the editor. Route them to the overlay
        // renderer instead so the author can see where they are.
        //
        // We classify by the resolved category string (matches App.svelte's
        // bucketing) — `info` comes from the same dTypes index the regular
        // path consumes. Custom blockers from war3map.w3d carry the same
        // category through the per-map overlay merge.
        //
        // Note: we ALSO register the blocker into doodadInstances? No — the
        // overlay renderer owns its own pickInfos list and tints. Adding to
        // doodadInstances would mean the regular doodad ray-pick walks an
        // entity with no MDX bounds (`inst.getBounds()` would be undefined),
        // and the doodadCategoryVisible toggle would expect a setScene-able
        // instance to detach. Keeping them in a sibling list is the same
        // pattern slocs use vs unitInstances.
        const info = dTypes[d.type_id]
        const isBlocker = !!info && info.category === 'Pathing Blockers'
        if (isBlocker) {
          const half = blockerHalf(d.scale?.[0] ?? 1, info.model_scale ?? 1)
          pathBlockerMarkers.push({
            creationNumber: d.creation_number,
            position: [d.position[0], d.position[1], d.position[2]],
            halfX: half,
            halfY: half,
          })
          // Tag the category so the View-menu visibility toggle finds these
          // alongside their MDX-bearing kin (when a future map ships a path
          // blocker that DOES have an MDX, both paths cooperate). The
          // visibility toggle filters draw via setScene on doodadInstances;
          // for our overlay-only blockers we'd need to plumb a separate
          // setMarkers call to honour the toggle — left as deferred work.
          // Single-purpose feature first.
          doodadCategoryByCn.set(d.creation_number, info.category)
          pathBlockerCount++
          // Skip the regular MDX placement attempt entirely — these have no
          // file and would just go through the silent-skip path otherwise.
          continue
        }
        const inst = await placeDoodad(d, dTypes)
        if (inst) {
          doodadInstances.set(d.creation_number, inst)
          dPlaced++
          // Sample the first 5 placements + 5 from later in the list to
          // confirm bounds are sane across the map. The lib's bounds object
          // is a sphere — center + radius in MODEL-local space. World-space
          // radius = bounds.r * max(worldScale). We log both for sanity.
          if (sampleAudits.length < 10 && (dPlaced <= 5 || dPlaced % 100 === 0)) {
            const b = inst.getBounds?.()
            const wl = inst.worldLocation
            const ws = inst.worldScale
            const biggest = Math.max(ws?.[0] ?? 1, ws?.[1] ?? 1, ws?.[2] ?? 1)
            const worldR = b ? (b.r * biggest) : NaN
            sampleAudits.push(
              `#${dPlaced} ${d.type_id} loc=[${wl?.[0]?.toFixed(0)},${wl?.[1]?.toFixed(0)},${wl?.[2]?.toFixed(0)}] scale=[${ws?.[0]?.toFixed(2)},${ws?.[1]?.toFixed(2)},${ws?.[2]?.toFixed(2)}] boundsR=${b?.r?.toFixed(1)} worldR=${worldR.toFixed(1)}`
            )
          }
        } else {
          dSkipped++
          skipReasons.set(d.type_id, (skipReasons.get(d.type_id) ?? 0) + 1)
        }
      }
      // Hand the path-blocker markers to the overlay renderer in one batch.
      // setMarkers replaces (not appends) so re-loading the same map doesn't
      // double-stack blockers. Done BEFORE the audit log so its count is
      // accurate in the same flog line.
      pathBlockers?.setMarkers(pathBlockerMarkers)
      rebuildBlockerProxies(pathBlockerMarkers) // keep gizmo-movable proxies in sync
      flog(`[loadMap] units placed=${uPlaced} skipped=${uSkipped}, doodads placed=${dPlaced} skipped=${dSkipped} pathBlockers=${pathBlockerCount} doodad-bbox=x[${xmin.toFixed(0)},${xmax.toFixed(0)}] y[${ymin.toFixed(0)},${ymax.toFixed(0)}] slocs=${slocMarkers.length}`)
      // Top skipped type-ids, sorted by skip count desc — same ordering the
      // audit log uses, kept as a string[] for the diagnostics summary.
      const topSkipTypeIds = [...skipReasons.entries()]
        .sort((a, b) => b[1] - a[1])
        .slice(0, 8)
        .map(([id, n]) => `${id}×${n}`)
      if (skipReasons.size > 0) {
        flog(`[doodad audit] skipped-type-ids: ${topSkipTypeIds.join(' ')}`)
      }
      for (const line of sampleAudits) {
        flog(`[doodad audit] ${line}`)
      }
      // U5: commit the scene-load summary so diagnostics.get / the F9 overlay
      // can read the last load's placement tallies without re-walking anything.
      sceneDiag.lastLoad = {
        unitsPlaced: uPlaced,
        unitsSkipped: uSkipped,
        doodadsPlaced: dPlaced,
        doodadsSkipped: dSkipped,
        pathBlockers: pathBlockerCount,
        slocs: slocMarkers.length,
        topSkipTypeIds,
      }
      // Build the ordered present-categories list for the View menu.
      recomputeDoodadCategories()
      } finally {
        // Always commit the timing + clear the in-flight flag, even if terrain
        // or asset loading threw partway through, so the overlay never sticks
        // at loadingMap=true.
        sceneDiag.loadMs = Math.round(performance.now() - loadStartTs)
        sceneDiag.loadingMap = false
        releaseSerial() // let the next queued loadMap proceed
      }
    },
    clear() {
      clearInstances()
    },
    dispose() {
      clearInstances()
      camera.dispose()
      cancelAnimationFrame(rafId)
      ro.disconnect()
      canvas.removeEventListener('mousedown', onCanvasMouseDown)
      canvas.removeEventListener('contextmenu', onCanvasContextMenu)
      canvas.removeEventListener('mousemove', onCanvasHoverMove)
      canvas.removeEventListener('mouseleave', onCanvasHoverLeave)
      document.removeEventListener('mousemove', onDocMouseMove)
      document.removeEventListener('mouseup', onDocMouseUp)
      window.removeEventListener('keydown', onWindowKeyDown)
      // Drop the entity-changed subscription. App.svelte also calls
      // EventsOff for this name on its own unmount; the Wails runtime is
      // tolerant of duplicate offs (no-op when no listener is attached).
      EventsOff(ENTITY_EVENT)
      try { overlay.remove() } catch { /* parent already gone */ }
      if (axisLabelEls) {
        for (const ax of ['x', 'y', 'z'] as const) {
          try { axisLabelEls[ax].remove() } catch { /* parent already gone */ }
        }
      }
      if (slocRenderer) {
        slocRenderer.dispose()
        slocRenderer = null
      }
      if (cellHighlight) {
        cellHighlight.dispose()
        cellHighlight = null
      }
      if (regionOverlay) {
        regionOverlay.dispose()
        regionOverlay = null
      }
      if (brushCursor) {
        brushCursor.dispose()
        brushCursor = null
      }
      if (outline) {
        outline.dispose()
        outline = null
      }
      if (boundsDebug) {
        boundsDebug.dispose()
        boundsDebug = null
      }
      if (pickBuffer) {
        pickBuffer.dispose()
        pickBuffer = null
      }
      if (pathBlockers) {
        pathBlockers.dispose()
        pathBlockers = null
      }
      if (gizmo) {
        gizmo.dispose()
        gizmo = null
      }
    },
    setSelected(units: Set<number>, doodads: Set<number>) {
      // The authoritative selection has arrived — retire the rubber-band preview
      // now (not on mouse-up), so the cyan preview hands off to the white
      // selection outline in the same frame with no highlight-less gap.
      rubberPreviewUnits.clear()
      rubberPreviewDoodads.clear()
      // Selection is shown by the white instance-outline pass in the render
      // loop (driven straight off these sets), NOT a vertex tint — so here we
      // only track the sets. Not repainting vertex colours also stops the old
      // behaviour of clobbering unit team-colours when selecting/deselecting.
      selectedSet = new Set(units)
      // If the entity under the cursor just became selected, drop its cyan hover
      // hint so it doesn't double up under the white selection outline. The
      // click-to-select path already clears hover on mousedown; this covers
      // MCP / side-panel selection that lands here mid-hover.
      if (hoveredUnit !== null && units.has(hoveredUnit)) hoveredUnit = null
      if (hoveredDoodad !== null && doodads.has(hoveredDoodad)) clearDoodadHover()
      selectedDoodadSet = new Set(doodads)

      // Update gizmo selection items. Cancel any active drag when selection changes.
      if (gizmo?.isDragging()) {
        gizmo.cancelDrag()
        gizmoDragging = false
      }
      gizmoSelectionItems = [
        ...[...units].map(id => ({ kind: 'unit' as const, id })),
        ...[...doodads].map(id => ({ kind: 'doodad' as const, id })),
      ]
    },
    onPick(cb: PickCallback) {
      pickCallback = cb
    },
    setGizmoMode(m: 'move' | 'rotate' | 'scale') {
      gizmo?.setMode(m)
    },
    getGizmoMode(): 'move' | 'rotate' | 'scale' {
      return gizmo?.getMode() ?? 'move'
    },
    setGizmoSnap(s) {
      gizmo?.setSnap(s)
    },
    getGizmoSnap() {
      return gizmo?.getSnap() ?? {
        moveOn: false, moveStep: 32,
        rotateOn: false, rotateStep: 15,
        scaleOn: false, scaleStep: 0.1,
      }
    },
    setReforgedMode(b: boolean) {
      if (b === currentReforged) return
      flog(`[scene] reforged mode: ${currentReforged} -> ${b}`)
      currentReforged = b
      // Mutate the MDX handler's shared cache so getEventObjectData and any
      // future loadTeamTextures call see the new flag. Then drop cached
      // team colors so the next loadTeamTextures reloads the right texture
      // set (28 .dds vs 16 .blp). The shader programs themselves stay —
      // they were all built up front in the handler load() call regardless
      // of mode, and the shader-picker (getBatchShader) consults each
      // batch's own `isHd` flag, not the handler-global reforged.
      const cache: any = (viewer as any).sharedCache?.get?.('mdx')
      if (cache) {
        cache.reforged = b
        cache.teamColors.length = 0
        cache.teamGlows.length = 0
      }
      // Also flush the model + texture resource cache so the next loadMap
      // re-fetches under the new mode. Without this, units retain the SD
      // textures + skeletons they were first loaded with.
      const v: any = viewer
      if (v.resourceMap && typeof v.resourceMap.clear === 'function') {
        v.resourceMap.clear()
      }
      // Detach all instances so we don't keep stale model refs around.
      clearInstances()
      // Re-prime team textures for the new mode.
      try { handlers.mdx.loadTeamTextures(viewer) } catch (e) {
        flog('[loadTeamTextures-reload]', e instanceof Error ? e.message : String(e))
      }
    },
    isReforged() { return currentReforged },
    purgeResourceCache() {
      flog('[scene] purging resource cache (CASC remount)')
      // Flush the model + texture resource cache so the next loadMap
      // re-fetches everything through the freshly mounted CASC. This is the
      // same eviction setReforgedMode does — terrain/water/cliff textures all
      // go through viewer.load -> resourceMap, so this covers them too.
      const v: any = viewer
      if (v.resourceMap && typeof v.resourceMap.clear === 'function') {
        v.resourceMap.clear()
      }
      // Drop cached (empty) team colors so loadTeamTextures re-fetches them.
      const cache: any = (viewer as any).sharedCache?.get?.('mdx')
      if (cache) {
        cache.teamColors.length = 0
        cache.teamGlows.length = 0
      }
      // Detach all instances so we don't keep stale model refs around.
      clearInstances()
      // Re-prime team textures against the new mount.
      try { handlers.mdx.loadTeamTextures(viewer) } catch (e) {
        flog('[loadTeamTextures-remount]', e instanceof Error ? e.message : String(e))
      }
    },
    panTo(x: number, y: number, z: number = 0) {
      camera.setPivot(x, y, z)
    },
    getCamera() {
      return camera
    },
    setDiagnosticsMode(on: boolean) {
      diagnosticsMode = on
    },
    getDiagnostics() {
      const t = cachedTerrainDTO
      return {
        frame: frameCount,
        fps: Math.round(fps),
        crashed,
        crashReason,
        crashAtFrame,
        diagnosticsArmed: diagnosticsMode,
        logLevel: getLogLevel(),
        doodads: doodadInstances.size,
        units: unitInstances.size,
        visible: cullingSnapshot().visibleCount,
        terrainWidth: t ? Number(t.width) : 0,
        terrainHeight: t ? Number(t.height) : 0,
        centerOffset: t ? [Number(t.center_offset?.[0] ?? 0), Number(t.center_offset?.[1] ?? 0)] as [number, number] : [0, 0] as [number, number],
        hasSky: !!sky,
        hasWater: !!water,
        hasTerrain: !!terrain,
        glRenderer,
        // 5Hz-sampled (see render loop) — never a per-frame getError sync.
        lastGlError,
        glErrFrame,
        glErrStage,
        camera: camera.getState(),
        // Registry namespaces from every subsystem that called registerDiag().
        // Armed providers only run when diagnostics mode is on.
        ...collectDiag(diagnosticsMode),
      }
    },
    focusSelection(): boolean {
      // Build a list of world positions for everything in the current
      // selection. Units (including sloc MDX instances, which the sloc
      // renderer registers in unitInstances) and doodads expose live
      // worldLocation arrays; for slocs that don't have an MDX instance
      // yet, fall back to the marker's stored position via slocRenderer.
      const positions: Array<[number, number, number]> = []
      for (const cn of selectedSet) {
        const inst = unitInstances.get(cn)
        const wl = inst?.worldLocation
        if (wl) {
          positions.push([wl[0], wl[1], wl[2]])
          continue
        }
        // Sloc pillar-fallback: no MDX instance, look it up by cn.
        const info = slocRenderer?.pickInfos().find(p => p.creationNumber === cn)
        if (info) positions.push([info.center[0], info.center[1], info.center[2]])
      }
      for (const cn of selectedDoodadSet) {
        const inst = doodadInstances.get(cn)
        const wl = inst?.worldLocation
        if (wl) positions.push([wl[0], wl[1], wl[2]])
      }
      if (positions.length === 0) return false

      // Centroid + axis-aligned bounding box.
      let cx = 0, cy = 0, cz = 0
      let minX = Infinity, maxX = -Infinity
      let minY = Infinity, maxY = -Infinity
      let minZ = Infinity, maxZ = -Infinity
      for (const [x, y, z] of positions) {
        cx += x; cy += y; cz += z
        if (x < minX) minX = x
        if (x > maxX) maxX = x
        if (y < minY) minY = y
        if (y > maxY) maxY = y
        if (z < minZ) minZ = z
        if (z > maxZ) maxZ = z
      }
      const n = positions.length
      cx /= n; cy /= n; cz /= n
      // Half the bounding-box diagonal is a tight upper bound on the
      // farthest selected point from the centroid. Add a 256-stud pad so
      // unit silhouettes (which extend past their pivot point) aren't
      // clipped against the viewport edges, and floor at 384 so a
      // singleton doesn't zoom in past the camera's near-plane comfort.
      const halfDiag = 0.5 * Math.hypot(maxX - minX, maxY - minY, maxZ - minZ)
      const radius = Math.max(halfDiag + 256, 384)
      camera.focus(cx, cy, cz, radius)
      return true
    },
    updateUnitPosition(cn: number, x: number, y: number, z: number) {
      // Delegates to the shared impl so direct callers and the
      // entity-changed event subscriber run identical code.
      updateUnitPositionImpl(cn, x, y, z)
    },
    setUnitAnimation(cn: number, animName: string): boolean {
      const inst = unitInstances.get(cn)
      if (!inst) {
        flog(`[setUnitAnimation] no unit for cn=${cn}`)
        return false
      }
      const normalized = animName.trim().toLowerCase()
      // Empty / 'stand' → release manual control and reroll an idle.
      if (normalized === '' || normalized === 'stand') {
        manualAnimCns.delete(cn)
        const ok = rollSequence(inst, 'stand')
        flog(`[setUnitAnimation] cn=${cn} -> stand (release manual) ok=${ok}`)
        return ok
      }
      const ok = rollSequence(inst, normalized)
      if (ok) {
        manualAnimCns.add(cn)
        flog(`[setUnitAnimation] cn=${cn} -> ${normalized} ok=true`)
      } else {
        flog(`[setUnitAnimation] cn=${cn} -> ${normalized} ok=false (no matching sequence)`)
      }
      return ok
    },
    setPathingVisible(visible: boolean) {
      pathingVisible = visible
    },
    isPathingVisible() {
      return pathingVisible
    },
    setSlocsVisible(visible: boolean) {
      slocsVisible = visible
      slocRenderer?.setVisible(visible)
    },
    areSlocsVisible() {
      return slocsVisible
    },
    setTerrainPickMode(active: boolean) {
      if (active === terrainPickMode) return
      terrainPickMode = active
      // Visual feedback: crosshair cursor over the canvas. Clear any active
      // hover-pick cursor when leaving the mode so we don't leave the user
      // staring at "move" / "pointer" cursors that no longer make sense.
      if (active) canvas.style.cursor = 'crosshair'
      else canvas.style.cursor = ''
    },
    isTerrainPickMode() { return terrainPickMode },
    onTerrainPick(cb: TerrainPickCallback) {
      terrainPickCallback = cb
    },
    setTerrainBrushMode(active: boolean) {
      if (active === terrainBrushMode) return
      terrainBrushMode = active
      if (active) {
        canvas.style.cursor = 'crosshair'
      } else {
        // Leaving brush mode: drop any in-progress stroke + the footprint ring,
        // and release the cursor (unless another mode still wants the crosshair).
        if (brushStroking) {
          brushStroking = false
          brushLastCellKey = -1
          terrainBrushCallback?.(null, 'end')
        }
        brushCursor?.setBrush(null)
        if (!terrainPickMode && !placementMode && !regionMode) canvas.style.cursor = ''
      }
    },
    isTerrainBrushMode() { return terrainBrushMode },
    onTerrainBrushStroke(cb: TerrainBrushCallback) {
      terrainBrushCallback = cb
    },
    setTerrainBrushShape(shape: TerrainBrushShape) {
      // Keep the radius fractional — the footprint reach + cursor ring both use
      // it as a float (radius+0.5), so e.g. 1.25 grows the brush smoothly.
      brushShape = { radius: Math.max(0, shape.radius), shape: shape.shape, waterPlaneZ: shape.waterPlaneZ }
      // If the cursor is already on the map, refresh it so a slider change to the
      // fixed water height updates the preview plane without needing a mouse move.
      if (terrainBrushMode && lastBrushClient) updateBrushCursorAt(lastBrushClient.x, lastBrushClient.y)
    },
    setPlacementMode(active: boolean) {
      if (active === placementMode) return
      placementMode = active
      // Same cursor handling as terrain-pick. Don't stomp terrain-pick's
      // crosshair on the way out — only clear when terrain-pick isn't also
      // claiming it (both modes can't visually conflict, but the disarm path
      // shouldn't reset a cursor terrain mode still wants).
      if (active) canvas.style.cursor = 'crosshair'
      else if (!terrainPickMode) canvas.style.cursor = ''
      // Leaving placement mode always retires the preview ghost — it only
      // makes sense while a brush is armed. Re-entry rebuilds it via
      // setPlacementGhost. This is the single chokepoint every disarm path
      // (palette cancel, Escape, right-click, map close) funnels through.
      if (!active) disposePlacementGhost()
    },
    isPlacementMode() { return placementMode },
    onPlacementPick(cb: PlacementPickCallback) {
      placementCallback = cb
    },
    setPlacementGhost(typeId: string | null, types: Record<string, DoodadTypeInfo>) {
      void rebuildPlacementGhost(typeId, types)
    },
    async addDoodadLive(d: any, types: Record<string, DoodadTypeInfo>) {
      const inst = await placeDoodad(d, types)
      if (!inst) return false
      doodadInstances.set(d.creation_number, inst)
      // placeDoodad already tagged doodadCategoryByCn for this cn; refresh the
      // present-categories list so a doodad in a not-yet-present category shows
      // up (toggleable) in the View menu. Reuses the same curated-first ordering
      // loadMap builds.
      recomputeDoodadCategories()
      return true
    },
    async addUnitLive(u: any, types: Record<string, UnitTypeInfo>) {
      // Mirror of addDoodadLive: render one unit from its DTO and register it in
      // the master cn→instance map so the gizmo / drag-move / entity-changed
      // flows find it. placeUnit returns null for types with no MDX (or a load
      // failure) — the caller then falls back to a full reload so the new unit
      // still lands in the Explorer list.
      const inst = await placeUnit(u, types)
      if (!inst) return false
      unitInstances.set(u.creation_number, inst)
      return true
    },
    hasInstance(kind: string, cn: number) {
      if (kind === 'unit') return unitInstances.has(cn)
      if (kind === 'doodad') return doodadInstances.has(cn)
      return false
    },
    setHighlightedCell(cell: { col: number; row: number } | null) {
      cellHighlight?.setCell(cell)
    },
    setRegionOverlay(regions: RegionRect[]) {
      regionRectsForPick = regions
      regionOverlay?.setRegions(regions)
    },
    setSelectedRegion(creationNumber: number | null) {
      regionOverlay?.setSelected(creationNumber)
    },
    setRegionOverlayVisible(visible: boolean) {
      regionOverlayVisible = visible
    },
    setRegionMode(active: boolean) {
      regionMode = active
      canvas.style.cursor = active ? 'crosshair' : ''
    },
    onRegionDraw(cb) { regionDrawCallback = cb },
    onRegionPick(cb) { regionPickCallback = cb },
    setDoodadCategoryVisible(category: string, visible: boolean) {
      // "*" affects every category. Walk the per-instance category map and
      // flip visibility via show()/hide() — see SceneAPI doc comment for
      // why we don't use setScene(null) (it crashes — lib calls
      // null.addInstance(this)).
      let touched = 0
      if (category === '*') {
        // Mirror across all categories so future loadMap respects the choice.
        for (const c of doodadCategoriesPresent) doodadVisibility.set(c, visible)
        for (const inst of doodadInstances.values()) {
          if (visible) inst.show()
          else inst.hide()
          touched++
        }
        flog(`[setDoodadCategoryVisible] cat=* visible=${visible} touched=${touched}`)
        return
      }
      doodadVisibility.set(category, visible)
      for (const [cn, inst] of doodadInstances) {
        const c = doodadCategoryByCn.get(cn) ?? 'Uncategorized'
        if (c !== category) continue
        if (visible) inst.show()
        else inst.hide()
        touched++
      }
      flog(`[setDoodadCategoryVisible] cat=${category} visible=${visible} touched=${touched}`)
    },
    getDoodadCategories() { return [...doodadCategoriesPresent] },
    getViewportFrustumCorners(): Array<[number, number]> | null {
      // Project each canvas corner through the camera and intersect the
      // resulting world-space ray with z=0. Reuses the same ray math
      // terrain-picking already uses (rayGroundPlane / screenToWorldRay).
      //
      // Canvas dimensions: use the actual GL viewport (kept synced to
      // canvas.width/height by the RAF loop). clientWidth/Height are CSS
      // pixels which would mismatch screenToWorldRay's NDC math under a
      // non-1:1 devicePixelRatio. The viewport is also already a Float32Array
      // shaped exactly the way screenToWorldRay wants it.
      const W = canvas.width
      const H = canvas.height
      if (W <= 0 || H <= 0) return null
      const vp = (scene as any).viewport as Float32Array
      if (!vp) return null
      const cornersPx: Array<[number, number]> = [
        [0, 0],         // TL
        [W, 0],         // TR
        [W, H],         // BR
        [0, H],         // BL
      ]
      const out = new Float32Array(6)
      const screen = new Float32Array(2)
      const result: Array<[number, number]> = []
      for (const [px, py] of cornersPx) {
        screen[0] = px
        screen[1] = py
        scene.camera.screenToWorldRay(out, screen, vp)
        const ox = out[0], oy = out[1], oz = out[2]
        const dx = out[3] - ox, dy = out[4] - oy, dz = out[5] - oz
        const hit = rayGroundPlane(ox, oy, oz, dx, dy, dz)
        if (!hit) return null
        if (!isFinite(hit[0]) || !isFinite(hit[1])) return null
        result.push(hit)
      }
      return result
    },
    // Pick-correctness self-test. For each unit and doodad instance, project
    // its world-space bound-center to canvas pixels (via worldToCanvasPx),
    // then call rayPick at that pixel and compare the returned hit's id+kind
    // to the instance's own creation_number. A correctly-configured pick
    // pipeline returns the same instance for every visible-on-screen entity;
    // off-screen or behind-camera entities (worldToCanvasPx returns null) are
    // skipped. Results are logged via flog for external inspection.
    __runPickSelfTest(): { tested: number; matched: number; mismatched: number; skipped: number; nearMatchDist: number } {
      let tested = 0, matched = 0, mismatched = 0, skipped = 0
      // Cumulative distance (canvas px) between the cursor-projected center
      // of the queried instance and the cursor-projected center of whatever
      // instance rayPick returned. With a CORRECT pick pipeline, this should
      // be near zero for "matched" hits (it always is) AND small for
      // "mismatched" hits when the mismatch is just occlusion in a dense
      // cluster (the picked instance's projection sits ~near the click).
      // With a Y-MIRRORED pick pipeline, even non-null mismatches project
      // far from the cursor (across the canvas height). So this metric is
      // a clean discriminator: small avg dist for mismatches = density,
      // large = math bug.
      let mismatchDistSum = 0
      const sample: string[] = []
      const consider = (cn: number, kind: 'unit' | 'doodad', inst: any) => {
        const wb = instanceWorldBounds(inst)
        if (!wb) { skipped++; return }
        const px = worldToCanvasPx(wb.cx, wb.cy, wb.cz)
        if (!px) { skipped++; return }
        if (px.x < 0 || px.y < 0 || px.x > canvas.clientWidth || px.y > canvas.clientHeight) {
          skipped++; return
        }
        tested++
        const hit = rayPick(px.x, px.y)
        const ok = !!hit && hit.kind === kind && hit.id === cn
        if (ok) matched++
        else {
          mismatched++
          // Distance from query cursor to the picked instance's canvas
          // projection. Falls back to "way off" sentinel when hit is null.
          let dist = 99999
          if (hit) {
            const m = hit.kind === 'unit' ? unitInstances : doodadInstances
            const other = m.get(hit.id)
            if (other) {
              const wb2 = instanceWorldBounds(other)
              if (wb2) {
                const px2 = worldToCanvasPx(wb2.cx, wb2.cy, wb2.cz)
                if (px2) dist = Math.hypot(px.x - px2.x, px.y - px2.y)
              }
            }
          }
          mismatchDistSum += dist
          if (sample.length < 5) {
            sample.push(`${kind}#${cn} at canvas(${px.x.toFixed(0)},${px.y.toFixed(0)}) → ` +
              (hit ? `${hit.kind}#${hit.id} dist=${dist.toFixed(0)}px` : 'null'))
          }
        }
      }
      for (const [cn, inst] of unitInstances) consider(cn, 'unit', inst)
      for (const [cn, inst] of doodadInstances) consider(cn, 'doodad', inst)
      const nearMatchDist = mismatched > 0 ? mismatchDistSum / mismatched : 0
      const result = { tested, matched, mismatched, skipped, nearMatchDist }
      flog(`[pick-self-test] ${JSON.stringify(result)} sample-mismatches=${JSON.stringify(sample)}`)
      return result
    },
  }
}
