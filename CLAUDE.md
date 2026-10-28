# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

Build the desktop app (this is the canonical build — `go build` alone produces a runnable binary but skips the embedded frontend and the postbuild library copy, leaving you with something that loads neither UI nor CASC):

```
wails build                               # production build
wails dev                                 # dev build with Vite HMR
```

Output paths differ per OS:
- Windows: `build/bin/wc3-forge.exe` (with `wc3-forge-dev.exe` for dev)
- macOS: `build/bin/wc3-forge.app/Contents/MacOS/wc3-forge`

**macOS only — first-time setup.** Run `./scripts/build-casclib-macos.sh` once after cloning. It downloads CascLib, builds `libcasc.dylib`, and drops it in `scripts/casclib/` (where the `darwin/*` postBuildHook expects it). The dylib is `.gitignore`d.

Run the existing binary directly (useful when you don't need a rebuild) — replace `<bin>` with the per-OS path above:

```
<bin>                                     # GUI + bridge
<bin> --open <path>                       # auto-load a map folder or .w3x on startup
<bin> --headless                          # MCP bridge only, no window
<bin> --no-bridge                         # GUI only, no MCP bridge
<bin> --reforged                          # start in HD asset mode (default is SD/Classic)
<bin> --camera x,y,z,distance             # pin startup camera for verification
```

### Checks (run locally - no CI)

There is no CI on PRs or pushes (removed to save GitHub Actions minutes). Before opening or merging a PR, or pushing to `main`, run these locally and make sure they pass:

```
cd frontend && npm ci && npm run check && npm run build && cd ..   # typecheck + build (go:embed needs frontend/dist)
cd mcp && npm ci && npm run gen:tools && cd .. && git diff --exit-code internal/mcpserver/tools.json   # MCP catalog must not drift from mcp/src/tools.ts
go vet ./...
go test ./...
go build ./...
```

On macOS, also run the race-detector suite: `CGO_ENABLED=1 go test -race ./...` (proves the GUI + MCP-bridge concurrent-writer safety, e.g. `TestSaveConcurrentMutation_NoRace`). On a memory-constrained Windows box, `GOFLAGS=-p=2` avoids errno=1455 compile OOMs.

Narrower runs while iterating:

```
go test ./internal/forge/...             # single package
go test ./internal/forge -run TestSessionSaveRoundTrip   # single test
```

The `cmd/dumpw3e` parser-replay tool exists for cross-checking renderer divergences against HiveWE — `go run ./cmd/dumpw3e <map-path>`.

## Process etiquette for parallel agents

Multiple wc3-forge instances coexist by design — the MCP bridge auto-picks an unused port and each process writes its own lockfile at `~/.wc3-forge/mcp/<pid>.lock`. **Do not run process-wide kills like `Stop-Process wc3-forge` (Windows) or `pkill wc3-forge` (macOS)** — that kills sibling agents' verification windows and the user's interactive session.

When launching your own instance for testing, capture the PID and kill only that PID on cleanup:

```powershell
# Windows
$proc = Start-Process -FilePath 'build\bin\wc3-forge.exe' -PassThru
# ... do work ...
Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
```

```bash
# macOS
build/bin/wc3-forge.app/Contents/MacOS/wc3-forge &
PID=$!
# ... do work ...
kill -TERM "$PID" 2>/dev/null
```

`wails dev` parents the dev binary under a `wails` process — when cleaning up after a dev build, filter the process-name prefix (`wc3-forge`) so the child doesn't outlive the parent and hold the CASC library locked.

If you'd otherwise launch only to drive an MCP call, read an existing PID's lockfile (`{port, token}`) and connect over JSON-RPC instead.

## Architecture

Single Wails v2 executable. Go owns map parsing/I/O and the MCP bridge; TypeScript + Svelte owns the 3D viewport and panels. Two control surfaces (the GUI and external MCP clients) converge on the same `forge.Session` singleton, so a JSON-RPC `units.move` and a viewport drag end up in the same mutator and emit the same `entity-changed` / `dirty-changed` events.

```
                ┌────────────── Go process ──────────────┐
   MCP client ──┤ bridge (NDJSON / JSON-RPC over TCP) ───┤
                │                                        │
   GUI (Wails)──┤ App (bindings) ──┐                     │
                │                  ├─► forge.Session ◄───┤
                │ assetHandler ────┘   (map state +      │
                │ /asset/<path>        history + emits)  │
                │   1) map archive                       │
                │   2) CASC mount  ◄── WC3FORGE_WC3_PATH │
                └────────────────────────────────────────┘
```

### Go side (`internal/`)

- `internal/forge` — the editor core. `Session` is the singleton; `RegisterAll(b)` in `handlers.go` is the **single registration point** for every MCP method. Adding a new MCP tool means: write `handle<Foo>`, add `reg("foo.bar", handleFoo)` to `RegisterAll`, and add the matching mutator to `session.go` so the command flows through history. All session mutations go through `recordCommand` so undo/redo + `entity-changed` events stay coherent across both control surfaces. **Also expose the tool to clients:** add it in `mcp/src/tools.ts` (the catalog source of truth), then run `cd mcp && npm run gen:tools` to regenerate `internal/mcpserver/tools.json` and commit it — that JSON is what the in-binary `--mcp` server serves via tools/list.
- `internal/bridge` — wire transport only (JSON-RPC 2.0 over NDJSON on 127.0.0.1, ephemeral port, per-pid lockfile, token auth on `params._token`). The wire contract is preserved verbatim from the HiveWE C++ fork's MCP bridge (see `CREDITS.md`). Lockdir override is `WC3FORGE_MCP_LOCK_DIR`.
- `internal/mcpserver` — the in-binary MCP server (`wc3-forge --mcp`). A thin **client** of a running editor: it discovers instances via `bridge.ListLocks` and forwards tool calls over the bridge wire, serving MCP stdio via the official `github.com/modelcontextprotocol/go-sdk`. Tool schemas come from the `go:embed`-ed `tools.json` (generated from `mcp/src/tools.ts`); only `sessions_list`/`session_select` are handled locally. This is the in-binary replacement for the standalone Node server in `mcp/` — both are interchangeable.
- `internal/formats` — pure-Go parsers for every WC3 file format the editor touches (`w3e`, `w3i`, `doodadsdoo`, `unitsdoo`, `shd`, `wts`, `w3objmod`, `wpm`, `slk`, `dds`, `miscdata`, `mpq`). Each has round-trip tests; `unitsdoo` and `w3e` have non-obvious round-trip invariants (raw-scale bits, set boundaries — see comments in the encode tests).
- `internal/casc` — purego-backed dlopen wrapper around vendored CascLib (`scripts/casclib/`). No cgo: function pointers are bound at startup via `purego.RegisterLibFunc`. Windows loads `CascLib.dll` (committed), macOS loads `libcasc.dylib` (built locally via `scripts/build-casclib-macos.sh`, gitignored). The right library is copied next to the binary by `postBuildHooks` in `wails.json`. Platform splits live in `casc_windows.go` (UTF-16 path + Win32 GetLastError) and `casc_unix.go` (UTF-8 path; errno is unrecoverable through purego so all CASC-open failures collapse to "not found" — fine for the per-prefix retry loop, real storage errors surface at `CascOpenStorage` instead). Don't import CascLib directly — go through `internal/casc.Storage`.
- `internal/wc3launch` — locates the WC3 install and launches `Warcraft III -launch -loadfile <abspath>` for "Test Map" flow. The exact bundle layout is per-OS: `launch_windows.go` resolves to `_retail_/x86_64/Warcraft III.exe`; `launch_darwin.go` to `_retail_/x86_64/Warcraft III.app/Contents/MacOS/Warcraft III`. `WC3FORGE_WC3_PATH` overrides the install root on both.
- `cmd/dumpw3e` — standalone parser-replay tool used when wc3-forge's render diverges from HiveWE.

The top-level `*.go` files (`main.go`, `app.go`, `asset_handler.go`, `reforged.go`, `sky.go`, `tileset_*.go`, `typeindex.go`) are the Wails surface and the asset HTTP handler — they don't contain editor logic, just plumbing.

### Asset resolution

The frontend's `mdx-m3-viewer` issues XHRs against `/asset/<path>` to fetch models and textures. `assetHandler.ServeHTTP` resolves each request in this order, **first hit wins**:

1. The currently-loaded map's archive/folder (custom imports + per-map overrides + `war3map.*` files).
2. The CASC mount on the user's WC3 install.

Stock asset bases must be exhausted across every candidate file extension before falling back to CASC — sibling-extension overrides at the exact CASC path are how maps retexture stock tilesets, so the order is load-bearing. The CASC install location defaults to the OS-conventional path (`C:\Program Files (x86)\Warcraft III` on Windows, `/Applications/Warcraft III` on macOS — see `asset_handler_{windows,darwin}.go`) and can be overridden with `WC3FORGE_WC3_PATH`. The lazy-init path is gated so first failures don't crash the editor — folder-extracted maps still work without CASC.

#### Pointing CASC at a Mead bottle (macOS workflow)

[Mead](https://github.com/StephenSHorton/mead) is wc3-forge's sibling project — an agent-driven Wine bottle manager built on the same Wails + MCP shape. The recommended macOS setup is to install WC3 into a Mead bottle, then point `WC3FORGE_WC3_PATH` at it:

```bash
export WC3FORGE_WC3_PATH=~/Library/Application\ Support/Mead/bottles/<bottle-id>/prefix/drive_c/Program\ Files\ \(x86\)/Warcraft\ III
```

Mead's `internal/apps` package owns the path layout — `TestIntegration_WC3PrefixPath_MatchesWC3ForgeExpectation` over there is the load-bearing contract between the two projects. If either side moves that path, the test fails and someone has to think about how to reconcile.

The bigger story: when an agent uses Mead's `apps.install` to install WC3, the install lands at the exact path wc3-forge expects. Compat issues (missing DLL, wineboot tweak) get debugged in a loop via Mead's `process.logs` MCP surface, then wc3-forge picks up the working install transparently.

### Frontend (`frontend/src/`)

Svelte 5 + TS + Vite, compiled into `frontend/dist/` and embedded into the Go binary via `//go:embed all:frontend/dist`. `App.svelte` is the only top-level component; everything else is mounted from it.

- `scene-instances.ts` is the heart of rendering. It uses `mdx-m3-viewer` **at the low level only** — `ModelViewer`, `Scene`, and the `mdx`/`blp`/`dds`/`tga` handlers. `War3MapViewer.loadMap` and the lib's terrain/cliff/water shaders are NOT used (they're brittle on modern Reforged W3X files). Terrain, cliffs, water, sky, and start-location markers are drawn by `terrain.ts`, `cliffs.ts`, `water.ts`, `sky.ts`, `sloc-markers.ts` between `viewer.startFrame()` and `viewer.render()`.
- `frontend/wailsjs/` is **generated** by `wails build` — never hand-edit. After changing any Go method bound to the Wails `App` struct, re-run `wails build` (or `wails generate module`) so the TS bindings under `frontend/wailsjs/go/main/App.js` regenerate.
- Selection is owned by `forge.Session`, not by any tool/panel. Viewport, side panels, and MCP all read from the same selection list and mutate it through the same API.

### Cross-cutting events

Go emits Wails events whose names are constants at the top of `app.go`:

- `wc3-forge:selection-changed`, `wc3-forge:entity-changed` — selection + entity mutation
- `wc3-forge:map-changed`, `wc3-forge:dirty-changed` — map lifecycle + unsaved-edits state
- `wc3-forge:history-changed` — undo/redo stack
- `wc3-forge:bridge-call` — every MCP dispatch (powers the in-page Agent Console)
- `wc3-forge:test-command`, `wc3-forge:startup-camera`, `wc3-forge:close-requested` — verification + window-close handshake

When adding a new event, declare the name as a constant in `app.go` next to the existing ones; the frontend imports the matching string in `App.svelte`.
