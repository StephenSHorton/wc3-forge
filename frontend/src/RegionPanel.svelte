<script lang="ts">
  // Floating Region (rect) panel. Renders a small launcher pinned to the
  // bottom-left of the viewport; clicking it toggles a floating panel that
  // lists the map's regions (war3map.w3r) and lets the user create / select /
  // edit / delete them. Mirrors DoodadPalette / TerrainPalette: the component
  // surfaces the list + numeric editors and calls the Wails region bindings
  // directly (same convention as MapInfoEditor / ObjectEditor); scene wiring
  // (the overlay + the rect-draw viewport interaction) lives in App.svelte and
  // is driven through the callbacks here.
  //
  // Two create paths (per the Stage-2 roadmap floor):
  //   1. Numeric: type a name + min/max bounds and click Create.
  //   2. Rect-draw: arm the rect tool, drag a rectangle in the viewport; App
  //      calls CreateRegion with the dragged bounds and a default name, then
  //      tells us to refresh.
  // Drag-resize handles in the viewport are deferred (numeric bounds editing is
  // the required floor — see Stage-2 blockers).
  import {
    ListRegions, CreateRegion, ResizeRegion, RenameRegion, DeleteRegion,
  } from '../wailsjs/go/main/App.js'
  import type { forge } from '../wailsjs/go/models'
  import { showToast } from './toast'
  import MapPinIcon from '@lucide/svelte/icons/map-pin'
  import XIcon from '@lucide/svelte/icons/x'
  import PlusIcon from '@lucide/svelte/icons/plus'
  import Trash2Icon from '@lucide/svelte/icons/trash-2'
  import EyeIcon from '@lucide/svelte/icons/eye'
  import EyeOffIcon from '@lucide/svelte/icons/eye-off'

  let {
    loaded = false,
    selectedCN = null,
    refreshToken = 0,
    onSelectRegion,
    onRegionsChanged,
    overlayVisible = true,
    onToggleOverlay,
  }: {
    // Whether a map is loaded — gates the launcher.
    loaded?: boolean
    // The creation_number of the selected region (owned by App so the viewport
    // overlay + the panel stay in sync), or null.
    selectedCN?: number | null
    // Bumped by App whenever the region set changes outside the panel (e.g. a
    // viewport draw, an MCP edit, an undo/redo) so we re-fetch the list.
    refreshToken?: number
    // Report the selected region to App (drives the overlay highlight + lets
    // the viewport know which region is active). null clears.
    onSelectRegion: (cn: number | null) => void
    // Fired after any panel-driven create/resize/rename/delete so App can
    // re-push the overlay region list + refresh anything else.
    onRegionsChanged: () => void
    // Whether the region-rectangle overlay is currently drawn in the viewport
    // (owned + persisted by App). Drives the eye toggle's icon/state.
    overlayVisible?: boolean
    // Toggle the viewport overlay on/off (App persists the preference).
    onToggleOverlay: () => void
  } = $props()

  // Default open: this panel only mounts while in Region mode, so showing it
  // immediately (rather than behind the FAB) matches it being the mode's panel.
  let open = $state(true)
  let regions: forge.RegionInfo[] = $state([])
  let busy = $state(false)

  // New-region form state.
  let newName = $state('')
  let newMinX = $state('-256')
  let newMinY = $state('-256')
  let newMaxX = $state('256')
  let newMaxY = $state('256')

  // Per-region editable bounds buffer (string inputs, committed on blur/Enter).
  // Keyed by creation_number. We seed it from the fetched list and only read
  // it back on commit so typing doesn't fight the live list.
  type BoundsBuf = { name: string; minX: string; minY: string; maxX: string; maxY: string }
  let editBuf: Record<number, BoundsBuf> = $state({})
  // The region whose name/bounds input is currently focused (being typed).
  // refresh() preserves that region's edit buffer so an external refresh
  // (MCP / undo / another surface) can't clobber what the user is mid-typing.
  let focusedRegion: number | null = null

  // Re-fetch whenever the panel opens, the map (re)loads, or App bumps the
  // refresh token. onMount-style imperative fetch via $effect keyed on those
  // inputs — we never write the same $state we read here, so no self-ref loop
  // (the documented Svelte5 $effect footgun).
  $effect(() => {
    // Touch the deps so the effect re-runs when they change.
    void refreshToken
    void loaded
    if (open && loaded) {
      void refresh()
    } else if (!loaded) {
      regions = []
      editBuf = {}
    }
  })

  async function refresh() {
    try {
      const list = await ListRegions()
      regions = list ?? []
      // Seed/refresh the per-region edit buffer for any region not mid-edit.
      const next: Record<number, BoundsBuf> = {}
      for (const r of regions) {
        const cn = r.creation_number
        // Keep the in-progress buffer for the region the user is actively
        // typing; re-seed every other region from the backend.
        if (cn === focusedRegion && editBuf[cn]) {
          next[cn] = editBuf[cn]
          continue
        }
        next[cn] = {
          name: r.name,
          minX: String(r.min_x),
          minY: String(r.min_y),
          maxX: String(r.max_x),
          maxY: String(r.max_y),
        }
      }
      editBuf = next
    } catch (e) {
      showToast('Failed to list regions: ' + errMsg(e), 'error')
    }
  }

  function errMsg(e: unknown): string {
    return e instanceof Error ? e.message : String(e)
  }

  function num(s: string, fallback = 0): number {
    const v = parseFloat(s)
    return Number.isFinite(v) ? v : fallback
  }

  function toggleOpen() {
    open = !open
  }

  async function createNumeric() {
    const name = newName.trim()
    if (!name) {
      showToast('Enter a region name', 'error')
      return
    }
    busy = true
    try {
      const cn = await CreateRegion(
        name, num(newMinX), num(newMinY), num(newMaxX), num(newMaxY),
        '', '', [255, 255, 255],
      )
      newName = ''
      await refresh()
      onSelectRegion(cn)
      onRegionsChanged()
    } catch (e) {
      showToast('Create region failed: ' + errMsg(e), 'error')
    } finally {
      busy = false
    }
  }

  async function commitBounds(cn: number) {
    // The field just blurred/committed — clear focus tracking so the post-commit
    // refresh re-seeds the validated/normalized bounds, not the raw input.
    focusedRegion = null
    const b = editBuf[cn]
    if (!b) return
    try {
      await ResizeRegion(cn, num(b.minX), num(b.minY), num(b.maxX), num(b.maxY))
      await refresh()
      onRegionsChanged()
    } catch (e) {
      showToast('Resize region failed: ' + errMsg(e), 'error')
      await refresh()
    }
  }

  async function commitName(cn: number) {
    focusedRegion = null
    const b = editBuf[cn]
    if (!b) return
    const name = b.name.trim()
    if (!name) {
      showToast('Region name cannot be empty', 'error')
      await refresh()
      return
    }
    const cur = regions.find((r) => r.creation_number === cn)
    if (cur && cur.name === name) return // no-op
    try {
      await RenameRegion(cn, name)
      await refresh()
      onRegionsChanged()
    } catch (e) {
      showToast('Rename region failed: ' + errMsg(e), 'error')
      await refresh()
    }
  }

  async function remove(cn: number) {
    busy = true
    try {
      await DeleteRegion(cn)
      if (selectedCN === cn) onSelectRegion(null)
      await refresh()
      onRegionsChanged()
    } catch (e) {
      showToast('Delete region failed: ' + errMsg(e), 'error')
    } finally {
      busy = false
    }
  }

  function selectRow(cn: number) {
    onSelectRegion(selectedCN === cn ? null : cn)
  }

  function onKey(e: KeyboardEvent, fn: () => void) {
    if (e.key === 'Enter') {
      ;(e.target as HTMLInputElement)?.blur()
      fn()
    }
  }
</script>

{#if loaded}
  <button
    class="region-fab"
    class:active={open}
    title={open ? 'Close regions' : 'Regions'}
    onclick={toggleOpen}
    aria-label="Toggle region panel"
  >
    {#if open}<XIcon size={18} />{:else}<MapPinIcon size={18} />{/if}
  </button>
{/if}

{#if open && loaded}
  <div class="region-panel">
    <div class="region-header">
      <span class="region-title">Regions</span>
      <span class="region-count">{regions.length}</span>
      <button
        class="overlay-toggle"
        class:off={!overlayVisible}
        title={overlayVisible ? 'Hide region outlines in the viewport' : 'Show region outlines in the viewport'}
        aria-label={overlayVisible ? 'Hide regions in viewport' : 'Show regions in viewport'}
        aria-pressed={!overlayVisible}
        onclick={onToggleOverlay}
      >
        {#if overlayVisible}<EyeIcon size={14} />{:else}<EyeOffIcon size={14} />{/if}
      </button>
    </div>

    <div class="region-armed">
      <span class="dot"></span>
      <span class="armed-text">Drag on the map to draw a region. Click one to select it.</span>
    </div>

    <!-- New region (numeric bounds) -->
    <div class="region-create">
      <input
        class="r-name"
        type="text"
        placeholder="New region name"
        bind:value={newName}
        spellcheck="false"
        autocomplete="off"
        onkeydown={(e) => onKey(e, createNumeric)}
      />
      <div class="r-bounds">
        <label>minX <input type="number" bind:value={newMinX} /></label>
        <label>minY <input type="number" bind:value={newMinY} /></label>
        <label>maxX <input type="number" bind:value={newMaxX} /></label>
        <label>maxY <input type="number" bind:value={newMaxY} /></label>
      </div>
      <button class="r-create-btn" disabled={busy} onclick={createNumeric}>
        <PlusIcon size={14} /> Create
      </button>
    </div>

    <div class="region-body">
      {#if regions.length === 0}
        <div class="region-empty">No regions. Create one above or draw a rectangle.</div>
      {:else}
        {#each regions as r (r.creation_number)}
          <div class="region-row" class:selected={selectedCN === r.creation_number}>
            <button class="region-row-head" onclick={() => selectRow(r.creation_number)} title="Select / focus">
              <span class="swatch" style="background: rgb({r.color[0]}, {r.color[1]}, {r.color[2]});"></span>
              {#if editBuf[r.creation_number]}
                <input
                  class="row-name"
                  type="text"
                  bind:value={editBuf[r.creation_number].name}
                  spellcheck="false"
                  onclick={(e) => e.stopPropagation()}
                  onfocus={() => (focusedRegion = r.creation_number)}
                  onblur={() => commitName(r.creation_number)}
                  onkeydown={(e) => onKey(e, () => commitName(r.creation_number))}
                />
              {/if}
              <span class="row-cn">#{r.creation_number}</span>
            </button>
            {#if selectedCN === r.creation_number && editBuf[r.creation_number]}
              <div class="row-edit">
                <label>minX
                  <input type="number" bind:value={editBuf[r.creation_number].minX}
                    onfocus={() => (focusedRegion = r.creation_number)}
                    onblur={() => commitBounds(r.creation_number)}
                    onkeydown={(e) => onKey(e, () => commitBounds(r.creation_number))} />
                </label>
                <label>minY
                  <input type="number" bind:value={editBuf[r.creation_number].minY}
                    onfocus={() => (focusedRegion = r.creation_number)}
                    onblur={() => commitBounds(r.creation_number)}
                    onkeydown={(e) => onKey(e, () => commitBounds(r.creation_number))} />
                </label>
                <label>maxX
                  <input type="number" bind:value={editBuf[r.creation_number].maxX}
                    onfocus={() => (focusedRegion = r.creation_number)}
                    onblur={() => commitBounds(r.creation_number)}
                    onkeydown={(e) => onKey(e, () => commitBounds(r.creation_number))} />
                </label>
                <label>maxY
                  <input type="number" bind:value={editBuf[r.creation_number].maxY}
                    onfocus={() => (focusedRegion = r.creation_number)}
                    onblur={() => commitBounds(r.creation_number)}
                    onkeydown={(e) => onKey(e, () => commitBounds(r.creation_number))} />
                </label>
                <button class="row-delete" disabled={busy} title="Delete region"
                  onclick={() => remove(r.creation_number)}>
                  <Trash2Icon size={13} /> Delete
                </button>
              </div>
            {/if}
          </div>
        {/each}
      {/if}
    </div>
  </div>
{/if}

<style>
  .region-fab {
    position: absolute;
    /* Region mode owns the bottom-left corner — the doodad/terrain FABs only
       render in their own modes now, so nothing else occupies this slot. */
    left: 12px;
    bottom: 12px;
    z-index: 40;
    width: 40px;
    height: 40px;
    display: flex;
    align-items: center;
    justify-content: center;
    border-radius: 9999px;
    border: 1px solid var(--border);
    background: var(--primary);
    color: var(--primary-foreground);
    box-shadow: 0 2px 8px rgb(0 0 0 / 0.35);
    cursor: pointer;
    transition: transform 0.12s ease, background 0.12s ease;
  }
  .region-fab:hover { transform: translateY(-1px); }
  .region-fab.active {
    background: var(--accent);
    color: var(--accent-foreground);
  }

  .region-panel {
    position: absolute;
    left: 12px;
    bottom: 60px;
    z-index: 40;
    width: 320px;
    max-height: min(560px, calc(100vh - 120px));
    display: flex;
    flex-direction: column;
    background: var(--card);
    color: var(--card-foreground);
    border: 1px solid var(--border);
    border-radius: var(--radius, 8px);
    box-shadow: 0 8px 28px rgb(0 0 0 / 0.45);
    overflow: hidden;
  }

  .region-header {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 10px;
    border-bottom: 1px solid var(--border);
  }
  .region-title { font-size: 0.8125rem; font-weight: 600; }
  .region-count {
    font-size: 0.6875rem;
    color: var(--muted-foreground);
    background: var(--muted);
    border-radius: 9999px;
    padding: 1px 7px;
  }
  .overlay-toggle {
    margin-left: auto; /* push the eye toggle to the right edge of the header */
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 26px;
    height: 24px;
    border-radius: 6px;
    border: 1px solid var(--border);
    background: var(--secondary);
    color: var(--secondary-foreground);
    cursor: pointer;
  }
  /* "off" = overlay hidden: dim the control so the state reads at a glance. */
  .overlay-toggle.off {
    background: var(--muted);
    color: var(--muted-foreground);
  }

  .region-armed {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 6px 10px;
    font-size: 0.6875rem;
    background: color-mix(in oklch, var(--accent) 18%, transparent);
    border-bottom: 1px solid var(--border);
  }
  .region-armed .dot {
    width: 8px; height: 8px; border-radius: 9999px;
    background: var(--accent); flex: none;
  }

  .region-create {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 8px 10px;
    border-bottom: 1px solid var(--border);
  }
  .r-name {
    width: 100%;
    font-size: 0.75rem;
    padding: 4px 6px;
    border: 1px solid var(--border);
    border-radius: 5px;
    background: var(--background);
    color: var(--foreground);
  }
  .r-bounds {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 4px 6px;
  }
  .r-bounds label, .row-edit label {
    display: flex;
    align-items: center;
    gap: 4px;
    font-size: 0.6875rem;
    color: var(--muted-foreground);
  }
  .r-bounds input, .row-edit input {
    width: 100%;
    min-width: 0;
    font-size: 0.6875rem;
    padding: 3px 5px;
    border: 1px solid var(--border);
    border-radius: 5px;
    background: var(--background);
    color: var(--foreground);
  }
  .r-create-btn {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    gap: 5px;
    font-size: 0.75rem;
    font-weight: 600;
    padding: 5px 8px;
    border-radius: 6px;
    border: 1px solid var(--border);
    background: var(--primary);
    color: var(--primary-foreground);
    cursor: pointer;
  }
  .r-create-btn:disabled { opacity: 0.5; cursor: default; }

  .region-body {
    flex: 1 1 auto;
    overflow-y: auto;
    padding: 4px 0;
  }
  .region-empty {
    padding: 14px 12px;
    font-size: 0.6875rem;
    color: var(--muted-foreground);
    text-align: center;
  }

  .region-row {
    border-bottom: 1px solid color-mix(in oklch, var(--border) 60%, transparent);
  }
  .region-row.selected {
    background: color-mix(in oklch, var(--accent) 14%, transparent);
  }
  .region-row-head {
    display: flex;
    align-items: center;
    gap: 7px;
    width: 100%;
    padding: 5px 10px;
    background: none;
    border: none;
    cursor: pointer;
    text-align: left;
  }
  .swatch {
    width: 12px; height: 12px; border-radius: 3px; flex: none;
    border: 1px solid color-mix(in oklch, var(--foreground) 25%, transparent);
  }
  .row-name {
    flex: 1 1 auto;
    min-width: 0;
    font-size: 0.75rem;
    padding: 2px 5px;
    border: 1px solid transparent;
    border-radius: 4px;
    background: transparent;
    color: var(--foreground);
  }
  .row-name:focus {
    border-color: var(--border);
    background: var(--background);
    outline: none;
  }
  .row-cn {
    font-size: 0.625rem;
    color: var(--muted-foreground);
    flex: none;
  }
  .row-edit {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 4px 6px;
    padding: 6px 10px 9px 29px;
  }
  .row-delete {
    grid-column: 1 / -1;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    gap: 4px;
    font-size: 0.6875rem;
    padding: 4px 8px;
    border-radius: 5px;
    border: 1px solid var(--border);
    background: var(--destructive, #b91c1c);
    color: #fff;
    cursor: pointer;
  }
  .row-delete:disabled { opacity: 0.5; cursor: default; }
</style>
