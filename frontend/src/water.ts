// Water surface: per-cell quad mesh at per-corner water heights, tinted by
// a depth-based blend × an animated wave texture. Mirrors HiveWE's
// `data/shaders/water.{vert,frag}`.
//
// Per-corner attributes:
//   - position (x, y, water_z + tileset_offset)
//   - color (vec4 — premultiplied at build-time from shallow/deep colors
//     blended by depth-above-ground)
//
// Texture animation: Water.slk's `texfile` column points at a base path;
// individual frames live at `<texfile>00.dds` (Classic: `.blp` via the
// asset-handler sibling swap) where the index ranges over `num_textures`.
// Each frame is 128×128. CASC lookups also try the map's
// `_tilesets/<letter>.w3mod:` mount (Reforged 2.0.3+).
// `texrate` is the playback rate in frames per second; current_frame =
// floor(time_seconds * texrate) % numTextures.
//
// Drawn after viewer.render() in the RAF loop with depth-test ON +
// depth-write OFF + alpha blending ON, so it occludes anything below the
// water surface and is partially transparent over what's above the surface.

import { flog } from './debuglog'

interface WaterDataDTO {
  width: number
  height: number
  center_offset: [number, number]
  heights: number[]     // corner_height in studs (FinalZ — includes layer_height)
  water_z: number[]     // per-corner water surface Z in studs
  has_water: number[]   // 1 = water exists at this corner
  water: {
    ok: boolean
    offset: number      // additional Z offset from Water.slk (HiveWE units; we multiply by 128)
    shallow_min: [number, number, number, number]
    shallow_max: [number, number, number, number]
    deep_min: [number, number, number, number]
    deep_max: [number, number, number, number]
    texture_file: string
    num_textures: number
    texture_rate: number
  }
}

// Vertex shader passes world XY through so the fragment shader can compute
// per-cell UVs via `fract(worldXY/128)` — that gives one texture-repeat per
// cell, matching HiveWE's per-cell-quad UV [0..1] layout, without needing
// per-vertex UV attributes (which would require duplicating corner vertices
// since adjacent cells need different UV values at the shared corner).
const VERT_SHADER = `
attribute vec3 a_position;
attribute vec4 a_color;
uniform mat4 u_viewProj;
varying vec4 v_color;
varying vec2 v_worldXY;
void main() {
  v_color = a_color;
  v_worldXY = a_position.xy;
  gl_Position = u_viewProj * vec4(a_position, 1.0);
}
`.trim()

// Texture * depth-blend color. The depth-blend already has alpha pre-baked,
// so we trust v_color.a as the final opacity envelope — the texture only
// contributes RGB modulation (multiply with rgb, keep alpha from color).
// Matches HiveWE's water.frag: outColor = texture(...) * Color.
const FRAG_SHADER = `
precision mediump float;
uniform sampler2D u_waterTex;
uniform bool u_hasTexture;
varying vec4 v_color;
varying vec2 v_worldXY;
void main() {
  vec2 uv = fract(v_worldXY / 128.0);
  vec4 tex = u_hasTexture ? texture2D(u_waterTex, uv) : vec4(1.0);
  gl_FragColor = tex * v_color;
}
`.trim()

// HiveWE's depth-blend thresholds. Values in HiveWE units (1 cell = 128 studs).
const MIN_DEPTH = 10 / 128
const DEEPLEVEL = 64 / 128
const MAXDEPTH = 72 / 128

function compileShader(gl: WebGLRenderingContext, type: number, src: string): WebGLShader {
  const sh = gl.createShader(type)!
  gl.shaderSource(sh, src)
  gl.compileShader(sh)
  if (!gl.getShaderParameter(sh, gl.COMPILE_STATUS)) {
    const log = gl.getShaderInfoLog(sh)
    gl.deleteShader(sh)
    throw new Error('water shader compile: ' + log)
  }
  return sh
}

function buildProgram(gl: WebGLRenderingContext) {
  const program = gl.createProgram()!
  gl.attachShader(program, compileShader(gl, gl.VERTEX_SHADER, VERT_SHADER))
  gl.attachShader(program, compileShader(gl, gl.FRAGMENT_SHADER, FRAG_SHADER))
  gl.linkProgram(program)
  if (!gl.getProgramParameter(program, gl.LINK_STATUS)) {
    const log = gl.getProgramInfoLog(program)
    gl.deleteProgram(program)
    throw new Error('water program link: ' + log)
  }
  return {
    program,
    aPosition: gl.getAttribLocation(program, 'a_position'),
    aColor: gl.getAttribLocation(program, 'a_color'),
    uViewProj: gl.getUniformLocation(program, 'u_viewProj')!,
    uWaterTex: gl.getUniformLocation(program, 'u_waterTex')!,
    uHasTexture: gl.getUniformLocation(program, 'u_hasTexture')!,
  }
}

export interface WaterMesh {
  /** Draw using the given world→clip matrix (camera.viewProjectionMatrix). */
  draw(viewProj: Float32Array): void
  /** Triangle count for stats. */
  triCount: number
  /** Number of loaded animation frames (0 if textures are static). */
  frameCount: number
  /** Release GL resources. */
  dispose(): void
}

export async function buildWater(
  gl: WebGLRenderingContext,
  viewer: any,
  pathSolver: any,
  t: WaterDataDTO,
): Promise<WaterMesh | null> {
  if (!t || !t.water_z || !t.has_water) return null

  const W = t.width
  const H = t.height
  if (W < 2 || H < 2) return null

  // Apply the per-tileset water offset (HiveWE units → studs). Each corner's
  // final water surface Z = water_z[i] + offset*128.
  const offsetStuds = (t.water?.offset ?? 0) * 128

  // Pre-compute per-corner colors. Color depends on (water_z + offset) - ground_z
  // — i.e., how deep the water is above the ground at that corner. Corners
  // outside any water cell still get a color computed (cheap, harmless;
  // they'll be skipped via the cell-water check).
  const cornerColor = new Float32Array(W * H * 4)
  const sMin = t.water.shallow_min, sMax = t.water.shallow_max
  const dMin = t.water.deep_min, dMax = t.water.deep_max
  for (let k = 0; k < W * H; k++) {
    const depthStuds = (t.water_z[k] + offsetStuds) - t.heights[k]
    const depth = Math.max(0, Math.min(1, depthStuds / 128))
    let r: number, g: number, b: number, a: number
    if (depth <= DEEPLEVEL) {
      const u = Math.max(0, depth - MIN_DEPTH) / (DEEPLEVEL - MIN_DEPTH)
      r = lerp(sMin[0], sMax[0], u)
      g = lerp(sMin[1], sMax[1], u)
      b = lerp(sMin[2], sMax[2], u)
      a = lerp(sMin[3], sMax[3], u)
    } else {
      const u = Math.min(1, Math.max(0, depth - DEEPLEVEL) / (MAXDEPTH - DEEPLEVEL))
      r = lerp(dMin[0], dMax[0], u)
      g = lerp(dMin[1], dMax[1], u)
      b = lerp(dMin[2], dMax[2], u)
      a = lerp(dMin[3], dMax[3], u)
    }
    const o = k * 4
    cornerColor[o] = r / 255
    cornerColor[o + 1] = g / 255
    cornerColor[o + 2] = b / 255
    cornerColor[o + 3] = a / 255
  }

  // Per-corner vertex array (one vertex per corner, shared by up to 4 cells).
  const cx = t.center_offset[0]
  const cy = t.center_offset[1]
  const verts = new Float32Array(W * H * 7)
  for (let j = 0; j < H; j++) {
    for (let i = 0; i < W; i++) {
      const k = j * W + i
      const o = k * 7
      verts[o] = cx + i * 128
      verts[o + 1] = cy + j * 128
      verts[o + 2] = t.water_z[k] + offsetStuds
      verts[o + 3] = cornerColor[k * 4]
      verts[o + 4] = cornerColor[k * 4 + 1]
      verts[o + 5] = cornerColor[k * 4 + 2]
      verts[o + 6] = cornerColor[k * 4 + 3]
    }
  }

  // Shared-corner mesh: one vertex per grid corner. Maps at/over ~256 tiles
  // have W*H > 65535, so we promote to uint32 indices the same way terrain
  // and pathing do. The previous uint16-only path silently returned null
  // here, which is why the Terrain Cell inspector could say "Has water: yes"
  // on a latest-Reforged large map while the viewport drew none.
  const N = W * H
  const need32 = N > 65535
  const uintExt = need32 ? gl.getExtension('OES_element_index_uint') : null
  if (need32 && !uintExt) {
    flog(`[water] vertex count ${N} exceeds 16-bit index limit and OES_element_index_uint is missing; water disabled`)
    return null
  }

  let cellCount = 0
  for (let j = 0; j < H - 1; j++) {
    for (let i = 0; i < W - 1; i++) {
      const bl = j * W + i
      const br = bl + 1
      const tl = bl + W
      const tr = tl + 1
      if (t.has_water[bl] || t.has_water[br] || t.has_water[tl] || t.has_water[tr]) {
        cellCount++
      }
    }
  }
  if (cellCount === 0) return null

  const indices: Uint16Array | Uint32Array = need32
    ? new Uint32Array(cellCount * 6)
    : new Uint16Array(cellCount * 6)
  const indexType = need32 ? gl.UNSIGNED_INT : gl.UNSIGNED_SHORT
  let idxPos = 0
  for (let j = 0; j < H - 1; j++) {
    for (let i = 0; i < W - 1; i++) {
      const bl = j * W + i
      const br = bl + 1
      const tl = bl + W
      const tr = tl + 1
      if (!(t.has_water[bl] || t.has_water[br] || t.has_water[tl] || t.has_water[tr])) continue
      indices[idxPos++] = bl
      indices[idxPos++] = br
      indices[idxPos++] = tr
      indices[idxPos++] = bl
      indices[idxPos++] = tr
      indices[idxPos++] = tl
    }
  }

  const vbo = gl.createBuffer()!
  gl.bindBuffer(gl.ARRAY_BUFFER, vbo)
  gl.bufferData(gl.ARRAY_BUFFER, verts, gl.STATIC_DRAW)

  const ibo = gl.createBuffer()!
  gl.bindBuffer(gl.ELEMENT_ARRAY_BUFFER, ibo)
  gl.bufferData(gl.ELEMENT_ARRAY_BUFFER, indices, gl.STATIC_DRAW)

  const prog = buildProgram(gl)
  const stride = 7 * 4

  // Load animation frames in parallel. Each frame is a Texture resource —
  // the lib's BLP/DDS handlers auto-pick by magic bytes, and the asset
  // handler's BLP↔DDS extension fallback covers Reforged-only-DDS installs.
  const frameTextures: (WebGLTexture | null)[] = []
  if (t.water.texture_file && t.water.num_textures > 0) {
    // Reforged CASC ships these as DDS (no BLP). Request .dds the way
    // terrain.ts does; the asset handler still sibling-swaps to .blp
    // for Classic installs. Water.slk's texfile uses backslashes.
    const stem = t.water.texture_file.replace(/\\/g, '/')
    const promises: Promise<WebGLTexture | null>[] = []
    for (let i = 0; i < t.water.num_textures; i++) {
      const num = i.toString().padStart(2, '0')
      const path = `${stem}${num}.dds`
      promises.push(
        viewer.load(path, pathSolver).then((res: any) => {
          if (!res) return null
          return (res.webglResource ?? null) as WebGLTexture | null
        }).catch(() => null),
      )
    }
    const loaded = await Promise.all(promises)
    for (const tex of loaded) frameTextures.push(tex)
    const ok = loaded.filter(Boolean).length
    flog(`[water] loaded ${ok}/${t.water.num_textures} frame textures from ${t.water.texture_file}`)
  }

  const texRate = Math.max(1, t.water.texture_rate || 12)
  const t0 = performance.now()

  return {
    triCount: cellCount * 2,
    frameCount: frameTextures.filter(Boolean).length,
    draw(viewProj: Float32Array) {
      gl.useProgram(prog.program)
      gl.bindBuffer(gl.ARRAY_BUFFER, vbo)
      gl.bindBuffer(gl.ELEMENT_ARRAY_BUFFER, ibo)

      gl.enableVertexAttribArray(prog.aPosition)
      gl.vertexAttribPointer(prog.aPosition, 3, gl.FLOAT, false, stride, 0)
      gl.enableVertexAttribArray(prog.aColor)
      gl.vertexAttribPointer(prog.aColor, 4, gl.FLOAT, false, stride, 3 * 4)

      gl.uniformMatrix4fv(prog.uViewProj, false, viewProj)

      // Pick the current animation frame and bind to texture unit 0. If no
      // frames loaded, fall back to the static color-only render.
      let currentTex: WebGLTexture | null = null
      if (frameTextures.length > 0) {
        const elapsed = (performance.now() - t0) / 1000
        const idx = Math.floor(elapsed * texRate) % frameTextures.length
        currentTex = frameTextures[idx]
      }
      if (currentTex) {
        gl.activeTexture(gl.TEXTURE0)
        gl.bindTexture(gl.TEXTURE_2D, currentTex)
        gl.uniform1i(prog.uWaterTex, 0)
        gl.uniform1i(prog.uHasTexture, 1)
      } else {
        gl.uniform1i(prog.uHasTexture, 0)
      }

      gl.enable(gl.DEPTH_TEST)
      gl.depthFunc(gl.LEQUAL)
      gl.depthMask(false)
      gl.enable(gl.BLEND)
      gl.blendFunc(gl.SRC_ALPHA, gl.ONE_MINUS_SRC_ALPHA)
      gl.disable(gl.CULL_FACE)

      gl.drawElements(gl.TRIANGLES, indices.length, indexType, 0)

      gl.depthMask(true)
      gl.disable(gl.BLEND)
      gl.disableVertexAttribArray(prog.aPosition)
      gl.disableVertexAttribArray(prog.aColor)
    },
    dispose() {
      gl.deleteBuffer(vbo)
      gl.deleteBuffer(ibo)
      gl.deleteProgram(prog.program)
      // We don't delete the frame textures — they're owned by the
      // viewer.resourceMap and may be reused on the next map open.
    },
  }
}

function lerp(a: number, b: number, u: number): number {
  return a + (b - a) * u
}
