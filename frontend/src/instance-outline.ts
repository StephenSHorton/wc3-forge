// instance-outline.ts — glowing silhouette outline around a set of MDX
// instances. Used for two effects sharing one mechanism:
//   • selection — white outline around every selected unit / doodad
//   • hover     — cyan outline around the single doodad under the cursor
//
// Technique (WebGL1, mdx-m3-viewer is locked to it):
//   1. Render ONLY the target instance(s) into an offscreen RGBA mask FBO. The
//      caller supplies a `render` callback that draws exactly those instances
//      through the library's own render path (we don't reimplement MDX
//      skinning). The model writes alpha=1 where it covers a pixel, so the
//      mask's alpha channel IS the silhouette coverage.
//   2. Composite a fullscreen pass over the default framebuffer that, for each
//      pixel, samples a ring of neighbours in the mask. Where a neighbour is
//      covered but the pixel itself is not, we paint the outline colour with a
//      soft outward falloff → a glow hugging the exact model silhouette.
//
// The mask render clears its own depth, so the outline shows THROUGH occluders
// (x-ray), matching the always-on-top convention of cell-highlight.ts — the
// point is "what is selected / what will I click", which you want visible even
// behind a hill.
//
// One builder serves both effects: call draw() twice per frame with different
// colours/pulses (the mask is cleared each call). Lifecycle: buildInstanceOutline(gl)
// once per scene. setSize(w,h) on resize. draw(...) each frame an effect is
// active. dispose() on teardown. All GL state we touch is restored before
// returning so the rest of the frame's passes (cell-highlight, gizmo) are
// unaffected.

import { flog } from './debuglog'

// Fullscreen-quad vertex shader: a_pos is clip-space [-1,1]; v_uv is [0,1].
const VERT_SHADER = `
attribute vec2 a_pos;
varying vec2 v_uv;
void main() {
  v_uv = a_pos * 0.5 + 0.5;
  gl_Position = vec4(a_pos, 0.0, 1.0);
}
`.trim()

// Edge-glow fragment shader. Samples the mask's coverage (alpha) in concentric
// rings; outline intensity = nearby-coverage * (1 - self-coverage), weighted so
// nearer coverage glows brighter. u_intensity carries the time pulse.
const FRAG_SHADER = `
precision mediump float;
uniform sampler2D u_mask;
uniform vec2 u_texel;     // 1/width, 1/height
uniform vec3 u_color;
uniform float u_intensity;
uniform float u_radius;   // outline thickness in pixels
varying vec2 v_uv;

float cov(vec2 uv) {
  return texture2D(u_mask, uv).a;
}

void main() {
  float self = cov(v_uv);
  // 12 directions × 3 rings. Nearer rings weighted higher for a soft glow that
  // falls off outward from the silhouette.
  float acc = 0.0;
  const int DIRS = 12;
  for (int i = 0; i < DIRS; i++) {
    float a = (float(i) / float(DIRS)) * 6.2831853;
    vec2 dir = vec2(cos(a), sin(a)) * u_texel;
    acc = max(acc, cov(v_uv + dir * (u_radius * 0.34)) * 1.0);
    acc = max(acc, cov(v_uv + dir * (u_radius * 0.67)) * 0.7);
    acc = max(acc, cov(v_uv + dir * (u_radius * 1.0)) * 0.45);
  }
  // Only paint OUTSIDE the silhouette (self not covered) so the edge reads as
  // an outline, not a flood-fill over the model.
  float outline = acc * (1.0 - self);
  float alpha = clamp(outline, 0.0, 1.0) * u_intensity;
  if (alpha <= 0.003) discard;
  gl_FragColor = vec4(u_color, alpha);
}
`.trim()

function compileShader(gl: WebGLRenderingContext, type: number, source: string): WebGLShader {
  const sh = gl.createShader(type)!
  gl.shaderSource(sh, source)
  gl.compileShader(sh)
  if (!gl.getShaderParameter(sh, gl.COMPILE_STATUS)) {
    const log = gl.getShaderInfoLog(sh)
    gl.deleteShader(sh)
    throw new Error('instance-outline shader compile: ' + log)
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
    throw new Error('instance-outline program link: ' + log)
  }
  return {
    program,
    aPos: gl.getAttribLocation(program, 'a_pos'),
    uMask: gl.getUniformLocation(program, 'u_mask')!,
    uTexel: gl.getUniformLocation(program, 'u_texel')!,
    uColor: gl.getUniformLocation(program, 'u_color')!,
    uIntensity: gl.getUniformLocation(program, 'u_intensity')!,
    uRadius: gl.getUniformLocation(program, 'u_radius')!,
  }
}

export interface InstanceOutline {
  /** Resize the offscreen mask to match the canvas. */
  setSize(w: number, h: number): void
  /**
   * Draw a glowing outline. `render` MUST draw exactly the target instance(s)
   * into the currently-bound framebuffer (which this sets to the mask FBO
   * before invoking it). `color` is the outline RGB; `pulseAmt` in [0,1] is how
   * much the intensity breathes over time (0 = steady, ~0.45 = lively).
   */
  draw(
    render: () => void,
    timeMs: number,
    color: readonly [number, number, number],
    pulseAmt: number,
  ): void
  /** Release GL resources. */
  dispose(): void
}

// Outline thickness in pixels (shared by both effects). Tunable.
const OUTLINE_RADIUS_PX = 4.0

export function buildInstanceOutline(gl: WebGLRenderingContext): InstanceOutline | null {
  let prog: ReturnType<typeof buildProgram>
  try {
    prog = buildProgram(gl)
  } catch (e) {
    flog('[instance-outline] program build failed:', e instanceof Error ? e.message : String(e))
    return null
  }

  // Fullscreen quad (two triangles) in clip space.
  const quad = gl.createBuffer()!
  gl.bindBuffer(gl.ARRAY_BUFFER, quad)
  gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([
    -1, -1, 1, -1, -1, 1,
    -1, 1, 1, -1, 1, 1,
  ]), gl.STATIC_DRAW)

  // Offscreen target: RGBA colour texture + depth renderbuffer, (re)sized to
  // the canvas. Created lazily in setSize.
  const fbo = gl.createFramebuffer()!
  const colorTex = gl.createTexture()!
  const depthRb = gl.createRenderbuffer()
  let texW = 0
  let texH = 0
  let ok = false

  function setSize(w: number, h: number) {
    w = Math.max(1, w | 0)
    h = Math.max(1, h | 0)
    if (w === texW && h === texH) return
    texW = w
    texH = h
    gl.bindTexture(gl.TEXTURE_2D, colorTex)
    gl.texImage2D(gl.TEXTURE_2D, 0, gl.RGBA, w, h, 0, gl.RGBA, gl.UNSIGNED_BYTE, null)
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.NEAREST)
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.NEAREST)
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE)
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE)
    gl.bindRenderbuffer(gl.RENDERBUFFER, depthRb)
    gl.renderbufferStorage(gl.RENDERBUFFER, gl.DEPTH_COMPONENT16, w, h)
    gl.bindFramebuffer(gl.FRAMEBUFFER, fbo)
    gl.framebufferTexture2D(gl.FRAMEBUFFER, gl.COLOR_ATTACHMENT0, gl.TEXTURE_2D, colorTex, 0)
    gl.framebufferRenderbuffer(gl.FRAMEBUFFER, gl.DEPTH_ATTACHMENT, gl.RENDERBUFFER, depthRb)
    ok = gl.checkFramebufferStatus(gl.FRAMEBUFFER) === gl.FRAMEBUFFER_COMPLETE
    if (!ok) flog('[instance-outline] framebuffer incomplete; outline disabled')
    gl.bindFramebuffer(gl.FRAMEBUFFER, null)
    gl.bindRenderbuffer(gl.RENDERBUFFER, null)
    gl.bindTexture(gl.TEXTURE_2D, null)
  }

  return {
    setSize,
    draw(render, timeMs, color, pulseAmt) {
      if (!ok || texW === 0) return
      // Save the framebuffer we'll restore to (normally the canvas/null).
      const prevFbo = gl.getParameter(gl.FRAMEBUFFER_BINDING) as WebGLFramebuffer | null

      // ── Pass 1: render the target instance(s) into the mask ────────────
      gl.bindFramebuffer(gl.FRAMEBUFFER, fbo)
      gl.viewport(0, 0, texW, texH)
      gl.disable(gl.SCISSOR_TEST)
      gl.clearColor(0, 0, 0, 0)
      gl.depthMask(true)
      gl.clear(gl.COLOR_BUFFER_BIT | gl.DEPTH_BUFFER_BIT)
      gl.enable(gl.DEPTH_TEST)
      // The MDX render path manages its own blend/cull per batch; we just
      // ensure depth is testable so models self-occlude correctly.
      render()

      // ── Pass 2: composite the edge glow over the scene ─────────────────
      gl.bindFramebuffer(gl.FRAMEBUFFER, prevFbo)
      gl.viewport(0, 0, texW, texH)
      gl.useProgram(prog.program)
      gl.bindBuffer(gl.ARRAY_BUFFER, quad)
      const maxAttribs = gl.getParameter(gl.MAX_VERTEX_ATTRIBS) | 0
      for (let i = 0; i < maxAttribs; i++) gl.disableVertexAttribArray(i)
      gl.enableVertexAttribArray(prog.aPos)
      gl.vertexAttribPointer(prog.aPos, 2, gl.FLOAT, false, 8, 0)
      gl.activeTexture(gl.TEXTURE0)
      gl.bindTexture(gl.TEXTURE_2D, colorTex)
      gl.uniform1i(prog.uMask, 0)
      gl.uniform2f(prog.uTexel, 1 / texW, 1 / texH)
      gl.uniform3f(prog.uColor, color[0], color[1], color[2])
      gl.uniform1f(prog.uRadius, OUTLINE_RADIUS_PX)
      // intensity breathes between (1 - pulseAmt) and 1.0.
      const pulse = (1 - pulseAmt) + pulseAmt * (0.5 + 0.5 * Math.sin(timeMs * 0.006))
      gl.uniform1f(prog.uIntensity, pulse)
      // Alpha-blend the glow on top; never write depth.
      gl.disable(gl.DEPTH_TEST)
      gl.depthMask(false)
      gl.enable(gl.BLEND)
      gl.blendFunc(gl.SRC_ALPHA, gl.ONE_MINUS_SRC_ALPHA)
      gl.disable(gl.CULL_FACE)
      gl.drawArrays(gl.TRIANGLES, 0, 6)
      // Restore state the rest of the frame expects.
      gl.disableVertexAttribArray(prog.aPos)
      gl.bindTexture(gl.TEXTURE_2D, null)
      gl.disable(gl.BLEND)
      gl.enable(gl.DEPTH_TEST)
      gl.depthMask(true)
      // Restore the full-canvas viewport. Pass 2 set it to the mask size
      // (texW×texH) when binding back to the canvas FBO; if the canvas is ever
      // a different size than the mask (e.g. a resize between setSize and this
      // pass), leaving it would clip subsequent passes. Reset to the live
      // canvas dimensions so cell-highlight / gizmo draw across the whole frame.
      gl.viewport(0, 0, gl.canvas.width, gl.canvas.height)
    },
    dispose() {
      gl.deleteBuffer(quad)
      gl.deleteFramebuffer(fbo)
      gl.deleteTexture(colorTex)
      if (depthRb) gl.deleteRenderbuffer(depthRb)
    },
  }
}
