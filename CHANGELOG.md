# Changelog

All notable changes to wc3-forge are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.5] - 2026-07-06

### Added

- **Keyboard shortcuts + cheat sheet.** World Editor function keys now work:
  **F4** opens the Trigger Editor, **F6** the Object Editor, and **F2 / F3 /
  F8** switch the viewport editor mode (Doodad / Terrain / Region). Press **`?`**
  (or Help → Keyboard Shortcuts) for an overlay listing every binding.
  Previously the menu advertised F4/F6 but the keys did nothing.

### Fixed

- **Reforged custom object data now loads (units, items, abilities, …).** In
  Reforged, a custom object's data is split across two tables per kind:
  `war3map.w3*` holds the gameplay fields and `war3mapSkin.w3*` holds the
  art/skin overrides — **Name**, **Model File**, **Icon**, **Tooltip**.
  wc3-forge read only the first, so every custom unit loaded the right count
  but displayed the base type's name + default model. The skin companion tables
  are now loaded and merged on top of the base (and preserved losslessly on
  save), so custom units show their real name, model, and icon.
- **Custom-skin units render with their overridden model.** A custom unit that
  swaps its Model File (e.g. a Peasant-based unit using another model) now
  renders that model in the viewport and the Object Editor preview instead of
  falling back to the base type's model.
- **Terrain palette populates on first entry.** The ground-tile grid now loads
  the moment Terrain mode opens (it could previously show "No ground tiles." on
  a map full of terrain), and the empty state names the actual reason — a map
  with no terrain file vs. a load error you can retry.

## [1.0.4] - 2026-06-04

### Added

- **Cross-map object importer** (`objects.import_from_map`). Faithfully copy
  objects — abilities, units, items, buffs, upgrades, destructables, doodads —
  from another WC3 map into the loaded map, following the full **dependency
  graph**: an ability's referenced buffs, the units it summons and their
  abilities, and any imported icon/model files (copied to the exact paths the
  objects reference). The source map is read through a standalone reader, so the
  active map is untouched until mutation, and the whole copy is one undo group.
  Custom objects keep their source FourCC by default; pass `on_collision:
  "remap"` to give an import a fresh id (rewriting references) when a FourCC is
  already taken, so nothing is lost to cross-map id collisions.

## [1.0.3] - 2026-06-03

### Added

- **Camera bounds overlay.** A new **Camera Bounds** overlay (View → Overlays)
  darkens the unplayable border *outside* the map's camera bounds
  (`war3map.w3i` `CameraLeftBottom`..`CameraRightTop`) — the way Warcraft III
  masks it in-game — so the playable edge is obvious while editing. The shadow
  has a soft, distance-feathered edge with evenly rounded corners (computed in a
  fragment shader). On by default; the show/hide preference persists across
  reloads and is re-applied on every map load (same convention as the pathing
  and region-overlay toggles).

## [1.0.2] - 2026-06-02

### Fixed

- **Object editor — Spell Book fields.** Spell Book ability fields
  (`spb1`..`spb5`) are now keyed by their FourCC instead of collapsing onto a
  single shared column, so custom spell books edit correctly again.
- Viewport resize hardening.

## [1.0.1] - 2026-06-01

### Added

- **Region mode.** Regions are now a first-class editor mode alongside Doodad
  and Terrain (a 3-way switch in the top bar) instead of a panel floating over
  whatever mode you were in. In Region mode the viewport is owned by region
  editing: **drag to draw** a region rectangle (live preview), **click to
  select** the one under the cursor, and `Esc` cancels a draw / clears the
  selection. Entity selection and box-select are suppressed while in the mode,
  so the draw gesture no longer fights the marquee.
- **Show/hide regions toggle.** The Region panel has an eye toggle that hides
  the region-rectangle overlay in the viewport, so the orange outlines can be
  cleared out of the way without deleting the regions. Pure view preference —
  the regions are untouched — persisted across reloads and re-applied on every
  map load (same convention as the doodad-category visibility toggle).
- **Tab cycles editor modes** (Doodad → Terrain → Region; Shift+Tab reverses),
  skipping the cycle while a text field has focus.
- The Terrain palette now **auto-opens on entering Terrain mode** (matching the
  Region panel), instead of hiding behind its launcher.

### Changed

- The single Doodad/Terrain mode toggle is now a three-way Doodad / Terrain /
  Region switch (Tab / Shift+Tab cycles). `view.set_mode` accepts `"region"`
  in addition to `"doodad"` and `"terrain"`.

### Removed

- The old two-click "Draw" region tool (which fought the viewport's box-select);
  superseded by Region mode's drag-to-draw.

## [1.0.0] - 2026-06-01

Phase 0 — the data-integrity floor. 1.0 is the promise that your map survives a
crash, a second editor, and malformed input: saves are now atomic and
all-or-nothing, a concurrent save is detected instead of silently clobbered, the
prior bytes are always kept, and a corrupt map can no longer take the window down.

### Added

- **Atomic, all-or-nothing saves.** Folder-backed maps encode every dirty file
  first, stage each into an `fsync`'d temp, and only once *all* are written rename
  them into place — a crash, disk-full, or encode error before that point leaves
  every original file byte-for-byte intact (never a torn map with new units over
  old terrain). MPQ-backed maps already repack the whole `.w3x` in one atomic
  temp+rename. Single-file direct writes (model import, Convert-to-Lua, script
  save, sky) now go through the same temp+`fsync`+rename primitive instead of a
  truncating write.
- **Backup-on-save.** Before a file's bytes are replaced its prior contents are
  copied to a sibling `.bak`, so the previous version stays recoverable.
- **External-change detection.** Each source file's identity (mtime+size) is
  recorded at open; a save that would overwrite a file another wc3-forge window,
  an agent, or a human changed on disk since then is **refused** with an
  actionable error (your unsaved edits are kept). `map.save` takes an optional
  `{"force": true}` to overwrite anyway (the on-disk bytes are still backed up
  first); the GUI surfaces this as a "saved elsewhere — overwrite?" prompt.

### Fixed

- **Malformed maps can't crash the window.** The map-open path recovers from a
  parser panic on corrupt/truncated/protected input and surfaces it as a normal
  "map appears corrupt or unsupported" error instead of vanishing the window.
- **Out-of-memory on hostile input.** The `war3map.w3i` and `war3map.wtg` parsers
  now cap untrusted element counts read off the wire, so a bogus length field
  can't drive a multi-gigabyte allocation.
- **Lossy MPQ saves are refused.** When a repack would write fewer files than the
  archive declared (the import-drop class — e.g. a 240-file map shrinking to 14),
  the save is refused before the destructive rename and the original `.w3x` is
  left untouched, rather than overwriting the user's only copy with a lossy one.

### Internal

- CI commits the round-trip fixtures so the losslessness tests actually **run**
  (not skip) in CI, and exercises the suite under the race detector on the macOS
  leg.

## [0.9.0] - 2026-06-01

Phase 3 of the road to 1.0 — honesty polish: docs that match reality, update
integrity, and the never-compiled macOS paths under CI.

### Added

- **First-run empty state.** With no map loaded the viewport now offers Open Map,
  New Map, and a "Set up Claude Code" dialog with the copyable MCP command.
- **macOS CI leg.** A `macos-latest` job compiles, vets, and tests the
  darwin/unix Go paths that the Windows-only CI never built.
- **`SECURITY.md`** documenting the reporting channel, the unsigned-binary
  posture, and how to verify a download.

### Changed

- **Documentation reconciled to reality** — the tool reference now lists all 152
  MCP tools (regions, start locations, imports, gameplay constants, view,
  diagnostics, models, minimap, and the Phase 2 map/history additions); the
  CHANGELOG backfills every release; `CREDITS.md` corrected (CascLib is bound via
  purego, not cgo).
- **macOS support is described as build-from-source** across the README and site
  docs — there is no prebuilt macOS binary and the in-app updater is Windows-only.

### Fixed

- **Update integrity.** Releases now publish a `SHA256SUMS` asset and the in-app
  updater verifies the downloaded installer's SHA-256 before it elevates and runs
  it, refusing to launch on a mismatch.
- **MDX model lighting** now matches the terrain's light direction
  (`normalize(1, 1, -3)`) so units and doodads aren't shaded against the ground.
- **Cliff walls re-drape during height edits** — the height brush now rebuilds
  cliffs in lockstep with the terrain instead of leaving them over stale heights.

## [0.8.0] - 2026-06-01

Phase 2 — complete the agent loop, finish save-safety, lock contracts in CI.

### Added

- **Begin & persist a map from MCP**: `map.new`, `map.save_as`, and
  `map.extract_to_folder` (unpack a `.w3x` into the editable folder workflow).
- **`history.abort_group`** + an `open_group_depth` field on `history.list` to
  recover a session whose undo/redo is wedged by an agent's dangling group.
- **Byte-canonical `war3map.wts` encoder.**
- **CI contract guards**: regenerate `tools.json` and fail on drift, plus a
  handler↔catalog parity test.

### Changed

- `view.set_mode` and `view.set_doodad_category_visible` are now idempotent
  **SET** operations (not toggles) and return the resulting state.

### Fixed

- **Localized Map Info no longer orphans `war3map.wts`.** Editing a TRIGSTR-backed
  field (name/author/description/suggested players) updates the referenced string
  entry and rewrites `war3map.wts`, instead of inlining a literal into `war3map.w3i`.
- **Concurrent-save data race.** `Save()` encodes every dirty file and clears its
  dirty flag under one lock, so a simultaneous edit from the other surface can't
  tear a half-written `.w3x` or have its edit dropped from the next save.
- A region/trigger selection no longer hangs the properties panel on "Loading…".

## [0.7.0] - 2026-05-31

Phase 1 — a real editor across both the GUI and the MCP surface.

### Added

- **Region (rect) editor** — create/move/resize/rename/delete regions on both
  surfaces, with a WebGL viewport overlay; byte-faithful `war3map.w3r` encoder.
- **Unit hand-placement & start-location authoring**, and **placed-unit instance
  field editing** (player, HP/mana %, hero level, gold, …) with a live model
  preview.
- **Import Manager** over `war3map.imp` (add/remove/rename), wiring three
  previously GUI-only edits to MCP.

### Fixed

- **24-player start-location colors.** High-slot players (12–23) render in their
  correct colors via custom uniform rune-pad markers (the engine has no SD
  team-color texture for those slots); added a hide-all toggle, and hidden
  markers are no longer box-selectable.

## [0.6.7] - 2026-05-31

### Fixed

- Start-location markers for 24-player maps render correct per-player colors;
  added a Markers hide-all toggle; hidden markers are no longer box-selectable.

## [0.6.6] - 2026-05-31

### Fixed

- The in-app updater no longer reports "you're up to date" on an unstamped/dev
  build — it surfaces the dev build and still offers the latest release.

## [0.6.5] - 2026-05-31

### Added

- **Water brush** — add/remove water with a fixed height and a live preview.

## [0.6.4] - 2026-05-31

### Fixed

- **Item object field editing.** Items read the shared `UnitMetaData.slk`
  (selected by the `useItem` column); the previously-referenced
  `ItemMetaData.slk` doesn't exist, so every item-field edit had failed.

## [0.6.3] - 2026-05-31

### Added

- **Test Map for folder-backed maps** — package the working folder into a
  `.w3x` (synthesized HM3W header + lossless MPQ) and launch it in Warcraft III,
  no external build script required.

## [0.6.2] - 2026-05-31

### Fixed

- The updater always shows its dialog (even on a dev build), and the installer
  renames a locked `wc3-forge.exe` aside instead of failing to overwrite it —
  fixing the file-lock race when the `--mcp` server holds the binary open.

## [0.6.1] - 2026-05-31

### Fixed

- Documentation site fixes for GitHub Pages: trailing-slash routing so `/docs/`
  resolves, base-path-prefixed images, and the official Claude logomark in the
  hero.

## [0.6.0] - 2026-05-31

### Added

- **Terrain Palette.** A dedicated Terrain Mode panel to paint tiles, raise and
  lower height, and edit cliffs by hand. Tiles render as real texture thumbnails
  (not flat color swatches), and the brush supports a fractional radius.
- **Cliff editing.** A single Cliff tool raises or lowers terrain by a chosen
  signed number of levels from the default, rippling edits into a valid
  one-level staircase and rebuilding cliffs in lockstep with the terrain.
- **Entity deletion by keyboard.** Press Delete to remove the selected units and
  doodads.
- **Placement ghost preview.** Doodad and unit placement now shows a cursor
  preview ghost, with right-click to disarm.
- **Viewport selection polish.** Glowing hover and selection outlines,
  model-accurate picking, and box-select.
- **New Map tileset picker** with live tileset thumbnails.
- **Help menu** with Check for Updates and an About dialog.
- **Project website + documentation** at
  [the GitHub Pages site](https://github.com/StephenSHorton/wc3-forge) — landing
  page, download, and full docs.

### Fixed

- **Reforged HD rendering.** Post-1100 HD models no longer stretch from the
  origin (skin-stretch fix), HD per-slot textures resolve correctly for "tree"
  replaceable ids, and the HD render path honors per-layer blending.
- **Minimap.** New maps bake a correct `war3mapMap.blp` with a live terrain PNG
  render; fixed the garbled new-map minimap and added a terrain fallback.
- **Terrain brush performance.** Brush edits no longer trigger a full map reload
  on every dab — live rebuilds are throttled and differentiated, meshes update
  in place, and an open undo group can no longer leak across a stroke. Together
  these remove the brush lag and flicker.

## [0.5.1] - 2026-05-30

### Fixed

- Bounded the `unitsdoo` count fields so a malformed `war3mapUnits.doo` can no
  longer trigger a 16 GiB allocation (OOM); rejects implausible counts up front.

## [0.5.0] - 2026-05-30

### Added

- **In-app updater.** Checks GitHub Releases for a newer version and offers a
  one-click download + elevated install of the NSIS installer. This is the
  updater baseline — builds from here on can self-update.

## [0.4.1] - 2026-05-30

### Fixed

- The installer now bundles the CASC DLLs, fixing clean installs that rendered
  every map black (the binary couldn't mount CASC). The portable zip was fine.

## [0.4.0] - 2026-05-30

### Added

- **Doodad palette** + **New Map** (name/size/tileset → a real `.w3x`).
- **Diagnostics system** — an F9 overlay and `diagnostics.get` / `diagnostics.arm`
  MCP tools, fed by a registry any subsystem extends in one line.
- **Embedded MCP server** — `wc3-forge --mcp` makes the binary itself the MCP
  stdio server, so registration needs no Node or repo checkout. (#17)

### Fixed

- Sky-gradient `GL_INVALID_OPERATION` that froze the viewport (leftover enabled
  vertex-attrib arrays under ANGLE). (#19)
- Object-mutation deadlock and a `unitsdoo` save-corruption bug. (#16)

## [0.3.0] - 2026-05-29

### Added

- **Native JASS map support.** Older JASS maps now open, edit, save, and Test
  Map directly against `war3map.j` — no conversion to Lua required. Hand-rolled
  script maps round-trip verbatim; GUI-backed JASS maps get a full GUI→JASS
  codegen backend mirroring the Lua emitter (typed `globals` block, `call`/`set`
  statements, `if/then/else/endif`, `loop/exitwhen/endloop`, `'xxxx'` rawcodes).
  The Trigger Editor highlights script triggers as JASS in JASS maps. (#14)
- **Convert-to-Lua review & repair.** The conversion dialog surfaces failed /
  untranslatable sections at the top and lets you hand-write the Lua for them,
  written verbatim into the output. Per-failure gap markers, prev/next
  navigation, and inline error decorations show exactly where to fill in.
  (#11, #13)
- **Cross-trigger vJASS module inlining** in Convert-to-Lua, unblocking maps
  whose modules span multiple triggers. (#7)
- **3D model import** — bring in OBJ / glTF / STL and auto-convert to MDX. (#12)
- **macOS support (build from source)** — the darwin code paths (launch, CASC,
  asset resolution) plus the [Mead](https://github.com/StephenSHorton/mead)
  Wine-bottle workflow for installing Warcraft III. No prebuilt macOS binary is
  published; build with `wails build`. (#1)
- **MPQ patch-chain stock-asset source** for Classic / non-CASC installs
  (layered `War3Patch.mpq` over `War3.mpq`). (#2)
- **WC3 install detection** — prompts you to locate Warcraft III when it isn't
  at the conventional path. (#3)
- Pure-Go MPQ writer with lossless `.w3x` repack (preserves unlisted custom
  imports), terrain `set_tile` / `set_height` brushes, entity create + delete
  for units and doodads, and a full MCP wire surface across triggers, all seven
  object kinds, terrain, and entity lifecycle.

### Changed

- Test Map preserves an imported, unedited `war3map.j` / `war3map.lua` instead
  of regenerating it — old maps launch with their original compiled script, and
  regeneration only happens when you actually edit triggers. (#14)
- Convert-to-Lua transpiler hardened across a wide map corpus (most now convert
  to zero errors): multi-line string literals, `keyword` forward-declaration
  stripping, dangling `requires` clauses, integer-division typing, typed array
  defaults, and block-aware error recovery.

### Fixed

- `save_script` no longer clobbers a hand-authored script — it refuses without
  `overwrite`, and backs up to a `.bak` sidecar when overwriting.
- MPQ `.w3x` repack guarded against dropping unlisted custom files; `unitsdoo`
  subver-11 skin-id presence resolved per-file by trial rather than by
  subversion.
- CASC symbol loading split so Windows builds (purego has no `Dlopen` there).
- Save-failure toast restored after the MPQ error-sentinel rename.

## [0.2.0] - 2026-05-28

### Added

- Object Editor for all seven definition kinds (units, items, abilities, buffs,
  destructables, doodads, upgrades) — read + write, custom and stock objects.
- Trigger Editor — GUI tree + Monaco code view + WC3 IntelliSense + GUI→Lua
  codegen + Test Map.
- Convert Map to Lua — full vJASS preprocessor (textmacros, library/scope,
  structs, modules) with a side-by-side diff preview before committing.

## [0.1.0] - 2026-05-26

### Added

- Initial alpha: native `.w3x` / MPQ rendering (terrain, cliffs, water,
  doodads), read-only Object Editor, and the CASC stock-asset pipeline.
- Relicensed to GPL-3.0-or-later with `CREDITS.md` enumerating the ported
  HiveWE subsystems.

[0.9.0]: https://github.com/StephenSHorton/wc3-forge/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/StephenSHorton/wc3-forge/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/StephenSHorton/wc3-forge/compare/v0.6.7...v0.7.0
[0.6.7]: https://github.com/StephenSHorton/wc3-forge/compare/v0.6.6...v0.6.7
[0.6.6]: https://github.com/StephenSHorton/wc3-forge/compare/v0.6.5...v0.6.6
[0.6.5]: https://github.com/StephenSHorton/wc3-forge/compare/v0.6.4...v0.6.5
[0.6.4]: https://github.com/StephenSHorton/wc3-forge/compare/v0.6.3...v0.6.4
[0.6.3]: https://github.com/StephenSHorton/wc3-forge/compare/v0.6.2...v0.6.3
[0.6.2]: https://github.com/StephenSHorton/wc3-forge/compare/v0.6.1...v0.6.2
[0.6.1]: https://github.com/StephenSHorton/wc3-forge/compare/v0.6.0...v0.6.1
[0.6.0]: https://github.com/StephenSHorton/wc3-forge/compare/v0.5.1...v0.6.0
[0.5.1]: https://github.com/StephenSHorton/wc3-forge/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/StephenSHorton/wc3-forge/compare/v0.4.1...v0.5.0
[0.4.1]: https://github.com/StephenSHorton/wc3-forge/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/StephenSHorton/wc3-forge/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/StephenSHorton/wc3-forge/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/StephenSHorton/wc3-forge/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/StephenSHorton/wc3-forge/releases/tag/v0.1.0
