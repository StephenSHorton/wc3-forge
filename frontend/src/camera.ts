// WC3-style RTS camera. Operates directly on a mdx-m3-viewer Camera.
//
// The camera orbits a pivot point in the world XY plane (the ground). The
// editor user moves the pivot (pan), changes the orbit radius (zoom), and
// changes orbit angles (rotate). Pitch is clamped so the user can't look up
// from underneath the world; yaw is unbounded.
//
// Conventions match Warcraft 3:
//   - +Y is north. yaw=0 → camera looks toward +Y.
//   - +Z is up.
//   - pitch is the angle below the horizon. 0 = looking flat across the
//     map; π/2 = straight down. WC3's default sits around 45° (≈ 0.78 rad).
//
// Input bindings:
//   - mouse wheel             → zoom (change distance)
//   - right-mouse drag        → pan (drag map under cursor)
//   - middle-mouse drag       → orbit (yaw on x, pitch on y)
//   - WASD / arrow keys       → pan (keyboard fallback)
//
// In-game WC3 doesn't let players rotate the camera, so the editor defaults
// to a fixed top-down-ish orientation (DEFAULT_YAW/DEFAULT_PITCH). MMB-drag
// and the on-screen orbit gizmo let editor users tilt off-axis for spatial
// inspection; resetOrbit() snaps back to the default. Mouse-edge panning is
// disabled because the mouse can leave the window mid-pan and strand the
// camera in motion.

const PITCH_MIN = 0.18  // ~10°, prevents the floor-eye view
const PITCH_MAX = 1.45  // ~83°, prevents flipping past straight down
const DIST_MIN = 200
const DIST_MAX = 50000
const EDGE_PAN_DEADBAND = 50          // px from canvas edge
const EDGE_PAN_SPEED = 0.5            // world-units per pixel-per-second per stud of distance ratio
const KEY_PAN_SPEED = 1.0
const WHEEL_ZOOM_STEP = 1.1
const DRAG_YAW_SENS = 0.005           // radians per pixel
const DRAG_PITCH_SENS = 0.004
const FOV = Math.PI / 3
// Editor-default orientation. ~60° pitch is close to top-down with enough
// horizon to see doodad/unit silhouettes; yaw=0 has +Y (north) pointing
// "up" on screen. resetOrbit() snaps back to these.
const DEFAULT_YAW = 0
const DEFAULT_PITCH = Math.PI / 3

export interface RTSCamera {
  /** Re-center on a map: set pivot to (cx, cy) on the ground plane and frame the given span. */
  frame(centerX: number, centerY: number, span: number): void
  /** Direct setter for pivot if you have a precise spot to focus on. */
  setPivot(x: number, y: number, z?: number): void
  /**
   * Read the current pivot — the ground point the camera orbits around, which
   * is the centre of the view. Use this (not a frustum-corner average, which
   * is biased toward the horizon) to place something "at the centre of view".
   */
  getPivot(): [number, number, number]
  /**
   * Direct setter for camera-to-pivot distance. Clamped to the controller's
   * own zoom-range bounds. Useful for verification automation that needs to
   * zoom in on a specific feature without going through wheel input (which
   * WebView2 drops on synthetic mouse_event). For programmatic close-ups.
   */
  setDistance(d: number): void
  /** Update the projection aspect ratio. Cheap no-op if unchanged. */
  setAspect(aspect: number): void
  /** Detach all input listeners. */
  dispose(): void
  /** World-space eye position. Used by the gizmo to compute fixed-screen-space handle scale. */
  getEye(): [number, number, number]
  /** Current orbit angles. yaw: any radians; pitch: radians below horizon, clamped to [PITCH_MIN, PITCH_MAX]. */
  getOrbit(): { yaw: number; pitch: number }
  /** Absolute orbit setter. Pitch is clamped; yaw is taken as-is. */
  setOrbit(yaw: number, pitch: number): void
  /** Reset orbit to editor defaults (DEFAULT_YAW, DEFAULT_PITCH). Pivot and distance untouched. */
  resetOrbit(): void
  /**
   * Move the pivot to (cx, cy, cz) and pick a distance that frames a sphere
   * of the given radius around it. Used by the "frame selected" hotkey to
   * zoom in on whatever the user has selected. Orbit angles are preserved
   * so the user doesn't lose their current viewpoint.
   */
  focus(cx: number, cy: number, cz: number, radius: number): void
}

export function createCamera(canvas: HTMLCanvasElement, viewerCamera: any): RTSCamera {
  // Mutable camera state. Pivot is the ground-anchored focus point; the
  // viewer camera sits on a sphere of radius `distance` centered on pivot.
  const pivot = [0, 0, 0]
  let distance = 6000
  let pitch = DEFAULT_PITCH
  let yaw = DEFAULT_YAW
  let aspect = 1
  // Current world-space eye position, updated every applyToViewer(). Exposed
  // via getEye() so the gizmo can compute fixed-screen-space handle scale
  // without needing to access mdx-m3-viewer's internal camera.location property
  // (which is not reliably exposed in our version of the library).
  let currentEye: [number, number, number] = [0, 0, distance]

  function applyToViewer() {
    const cp = Math.cos(pitch)
    const sp = Math.sin(pitch)
    const cy = Math.cos(yaw)
    const sy = Math.sin(yaw)
    // viewDir at (yaw=0, pitch=0) is +Y. Tilting down rotates around the local
    // right axis: viewDir = (sy*cp, cy*cp, -sp). Camera sits opposite, so:
    const ox = -sy * cp * distance
    const oy = -cy * cp * distance
    const oz = sp * distance
    const eye = [pivot[0] + ox, pivot[1] + oy, pivot[2] + oz]
    currentEye = [eye[0], eye[1], eye[2]]
    viewerCamera.perspective(FOV, aspect,
      Math.max(8, distance * 0.001),
      Math.max(50000, distance * 10))
    viewerCamera.moveToAndFace(eye, pivot, [0, 0, 1])
    viewerCamera.update()
  }

  // Pan in world XY, with the offset given relative to camera's screen axes.
  // dxScreen = +right, dyScreen = +up (intuitive). Translates so dragging the
  // map feels like it sticks under the cursor.
  function pan(dxScreen: number, dyScreen: number) {
    const cy = Math.cos(yaw)
    const sy = Math.sin(yaw)
    // Screen-right axis in world XY (yaw rotation, no Z component): (cy, sy, 0)
    // Screen-up axis in world XY (forward, projected to ground): (-sy, cy, 0)
    pivot[0] += dxScreen * cy - dyScreen * sy
    pivot[1] += dxScreen * sy + dyScreen * cy
    applyToViewer()
  }

  function zoom(factor: number) {
    distance = Math.max(DIST_MIN, Math.min(DIST_MAX, distance * factor))
    applyToViewer()
  }

  function rotate(dYaw: number, dPitch: number) {
    yaw += dYaw
    pitch = Math.max(PITCH_MIN, Math.min(PITCH_MAX, pitch + dPitch))
    applyToViewer()
  }

  // ---- Input plumbing ----

  // Edge-pan needs the latest pointer position; we update it on mousemove
  // and read it from the RAF loop. Set to null when the cursor leaves the
  // canvas so we don't keep panning when focus is elsewhere.
  let pointer: { x: number; y: number } | null = null
  const keysDown = new Set<string>()

  // MMB / RMB drag state.
  let dragging: { button: number; lastX: number; lastY: number } | null = null

  function onMouseMove(e: MouseEvent) {
    const r = canvas.getBoundingClientRect()
    pointer = { x: e.clientX - r.left, y: e.clientY - r.top }
    if (dragging) {
      const dx = e.clientX - dragging.lastX
      const dy = e.clientY - dragging.lastY
      dragging.lastX = e.clientX
      dragging.lastY = e.clientY
      if (dragging.button === 1) {
        // MMB → orbit. Dragging right increases yaw (camera swings clockwise
        // around pivot when viewed from above); dragging up reduces pitch
        // (tilts toward horizontal). Sign on dy is negated so pulling the
        // mouse down — which on a top-down editor view feels like "tip the
        // camera back toward me" — increases pitch and approaches true top-
        // down, matching the muscle memory from Blender / Unity orbit cams.
        rotate(dx * DRAG_YAW_SENS, -dy * DRAG_PITCH_SENS)
      } else {
        // RMB → pan. Convert screen delta to world delta. Pan speed scales
        // with distance so dragging the same screen distance moves the world
        // by the same screen distance at any zoom. dy is flipped because
        // screen Y grows downward but our world Y grows north (up on screen
        // at yaw=0). The 0.0015 constant tunes "1 px of drag ≈ this much
        // world movement per stud of camera distance" — adjusted by feel.
        const k = -distance * 0.0015
        pan(dx * k, -dy * k)
      }
    }
  }
  function onMouseLeave() { pointer = null }

  function onMouseDown(e: MouseEvent) {
    if (e.button === 1 || e.button === 2) {
      dragging = { button: e.button, lastX: e.clientX, lastY: e.clientY }
      e.preventDefault()
    }
  }
  function onMouseUp(e: MouseEvent) {
    if (dragging && e.button === dragging.button) {
      dragging = null
    }
  }
  function onContextMenu(e: MouseEvent) {
    // RMB is a drag-pan gesture; block the native menu.
    e.preventDefault()
  }
  function onWheel(e: WheelEvent) {
    e.preventDefault()
    zoom(e.deltaY > 0 ? WHEEL_ZOOM_STEP : 1 / WHEEL_ZOOM_STEP)
  }
  function onKeyDown(e: KeyboardEvent) { keysDown.add(e.key.toLowerCase()) }
  function onKeyUp(e: KeyboardEvent) { keysDown.delete(e.key.toLowerCase()) }
  // If the window loses focus (alt-tab, click on another app) while a pan
  // key is held, the keyup never reaches us and the camera drifts forever.
  // Clear all held keys on blur / hide so the next focus starts fresh.
  function onBlur() { keysDown.clear() }

  canvas.addEventListener('mousemove', onMouseMove)
  canvas.addEventListener('mouseleave', onMouseLeave)
  canvas.addEventListener('mousedown', onMouseDown)
  window.addEventListener('mouseup', onMouseUp) // window-level so we catch release outside canvas
  canvas.addEventListener('contextmenu', onContextMenu)
  canvas.addEventListener('wheel', onWheel, { passive: false })
  window.addEventListener('keydown', onKeyDown)
  window.addEventListener('keyup', onKeyUp)
  window.addEventListener('blur', onBlur)
  document.addEventListener('visibilitychange', onBlur)

  // Per-frame: keyboard pan only (edge pan removed — the cursor leaving the
  // window would strand the camera mid-pan). Driven by RAF, throttled to
  // ~16ms by the browser; pan distance in world units per frame.
  let rafId = 0
  let lastTs = performance.now()
  function tick(ts: number) {
    const dt = Math.min(0.1, (ts - lastTs) / 1000)
    lastTs = ts
    let dx = 0, dy = 0

    if (keysDown.has('w') || keysDown.has('arrowup'))    dy += 1
    if (keysDown.has('s') || keysDown.has('arrowdown'))  dy -= 1
    if (keysDown.has('a') || keysDown.has('arrowleft'))  dx -= 1
    if (keysDown.has('d') || keysDown.has('arrowright')) dx += 1

    if (dx !== 0 || dy !== 0) {
      const speed = KEY_PAN_SPEED * distance
      pan(dx * speed * dt, dy * speed * dt)
    }
    rafId = requestAnimationFrame(tick)
  }
  rafId = requestAnimationFrame(tick)

  applyToViewer() // initial state

  return {
    frame(cx: number, cy: number, mapSpan: number) {
      pivot[0] = cx
      pivot[1] = cy
      pivot[2] = 0
      // Editor default: see a portion of the map by default rather than the
      // whole thing. WC3 maps run from tiny (~6000 studs) to huge (~30000+
      // studs). Fitting the full span makes everything 2-pixel-sized on
      // large maps; fitting a fixed slice makes tiny maps fit-to-screen
      // automatically. Compromise: target a "default view width" that's
      // a fraction of the map but clamped to a usable minimum.
      const viewedSpan = Math.max(8000, mapSpan * 0.45)
      // half-span / tan(FOV/2) = head-on fit distance; 1/cos(pitch) factor
      // because tilting down compresses ground coverage in screen-Y, so
      // we need more distance to fit the same ground span vertically.
      distance = Math.max(DIST_MIN, Math.min(DIST_MAX,
        (viewedSpan * 0.55) / Math.tan(FOV / 2) / Math.max(0.4, Math.cos(pitch))))
      applyToViewer()
    },
    setPivot(x: number, y: number, z = 0) {
      pivot[0] = x
      pivot[1] = y
      pivot[2] = z
      applyToViewer()
    },
    getPivot(): [number, number, number] {
      return [pivot[0], pivot[1], pivot[2]]
    },
    setDistance(d: number) {
      if (!isFinite(d) || d <= 0) return
      distance = Math.max(DIST_MIN, Math.min(DIST_MAX, d))
      applyToViewer()
    },
    setAspect(a: number) {
      if (!isFinite(a) || a <= 0) return
      if (Math.abs(a - aspect) < 1e-3) return
      aspect = a
      applyToViewer()
    },
    dispose() {
      cancelAnimationFrame(rafId)
      canvas.removeEventListener('mousemove', onMouseMove)
      canvas.removeEventListener('mouseleave', onMouseLeave)
      canvas.removeEventListener('mousedown', onMouseDown)
      window.removeEventListener('mouseup', onMouseUp)
      canvas.removeEventListener('contextmenu', onContextMenu)
      canvas.removeEventListener('wheel', onWheel)
      window.removeEventListener('keydown', onKeyDown)
      window.removeEventListener('keyup', onKeyUp)
      window.removeEventListener('blur', onBlur)
      document.removeEventListener('visibilitychange', onBlur)
    },
    getEye(): [number, number, number] {
      return [currentEye[0], currentEye[1], currentEye[2]]
    },
    getOrbit() {
      return { yaw, pitch }
    },
    setOrbit(y: number, p: number) {
      if (!isFinite(y) || !isFinite(p)) return
      yaw = y
      pitch = Math.max(PITCH_MIN, Math.min(PITCH_MAX, p))
      applyToViewer()
    },
    resetOrbit() {
      yaw = DEFAULT_YAW
      pitch = DEFAULT_PITCH
      applyToViewer()
    },
    focus(cx: number, cy: number, cz: number, radius: number) {
      if (!isFinite(cx) || !isFinite(cy) || !isFinite(cz) || !isFinite(radius)) return
      pivot[0] = cx
      pivot[1] = cy
      pivot[2] = cz
      // Fit a sphere of the given radius into the vertical FOV with a small
      // margin (×1.3) so the framed object doesn't kiss the screen edges.
      // The 1/cos(pitch) factor compensates for tilt — at non-zero pitch a
      // sphere of radius r projects to a taller ellipse on the ground plane,
      // so we back off proportionally. Floor at 0.4 matches frame() to avoid
      // exploding the distance near the floor-eye clamp.
      const r = Math.max(1, radius)
      const d = (r * 1.3) / Math.tan(FOV / 2) / Math.max(0.4, Math.cos(pitch))
      distance = Math.max(DIST_MIN, Math.min(DIST_MAX, d))
      applyToViewer()
    },
  }
}
