<script lang="ts">
  // Modal dialog for "Import 3D Model" — convert an external mesh file
  // (.obj / .gltf / .glb / .stl) into a WC3 .mdx (+ .blp textures) and import
  // it into the currently-loaded map's archive.
  //
  // Flow: the user first picks conversion options (uniform scale, up-axis,
  // flip-V), then clicks Import. ConvertAndImportModel opens the NATIVE OS
  // file dialog (so the .obj/.gltf/.glb/.stl is chosen AFTER confirming
  // options), converts + imports, and returns the in-archive ModelPath plus
  // any texture paths and warnings. On success we surface the warnings via the
  // app's toast mechanism and embed a live AssetPreview of the freshly-imported
  // model so the user sees it immediately.
  //
  // An empty ModelPath in the result means the user cancelled the native file
  // dialog — treat as a no-op (no error toast, dialog stays open with options).
  //
  // ConvertAndImportModel is a Wails-bound Go method added in parallel; its TS
  // binding regenerates on the next `wails build`. If svelte-check runs before
  // that, the only expected error here is the missing-binding import below.

  import * as Dialog from '$lib/components/ui/dialog'
  import { Button } from '$lib/components/ui/button'
  import { Label } from '$lib/components/ui/label'
  import { Checkbox } from '$lib/components/ui/checkbox'
  import {
    ConvertAndImportModel,
    ListObjects,
    SetObjectField,
  } from '../wailsjs/go/main/App.js'
  import type { main } from '../wailsjs/go/models'
  import AssetPreview from './AssetPreview.svelte'
  import { showToast } from './toast'

  // Mirror the SwapTilesetDialog / ConvertToLuaDialog binding convention:
  //   - `open` is bindable so App.svelte controls visibility.
  //   - onClose fires on the open→closed transition.
  //   - onImported fires once after a successful (non-cancelled) import so the
  //     caller can refresh assets so the new file is visible.
  let {
    open = $bindable(false),
    reforged = false,
    onClose,
    onImported,
    onPlace,
  }: {
    open?: boolean
    reforged?: boolean
    onClose?: () => void
    onImported?: () => void | Promise<void>
    // Drop the imported model onto the map at the centre of the current view.
    // Returns true on success (the dialog then closes). Provided by App.svelte,
    // which owns the scene/camera.
    onPlace?: (modelPath: string) => Promise<boolean>
  } = $props()

  // Conversion options. Defaults match the backend contract:
  //   - Scale 1 (positive float, uniform).
  //   - Up-axis "y" — most OBJ / glTF exports are Y-up.
  //   - Flip-V on — flips the V texture coordinate (common between exporters
  //     and WC3's convention).
  let scale: number = $state(1)
  let upAxis: 'y' | 'z' | 'auto' = $state('y')
  let flipV: boolean = $state(true)

  let importing: boolean = $state(false)

  // Result of the last successful import. modelPath drives the embedded
  // AssetPreview + the "how to use it" hint. Cleared when the dialog reopens.
  let importedModelPath: string = $state('')
  let importedTexturePaths: string[] = $state([])

  // ----- Assign to an object (optional) -----
  // After a successful import, let the user point an existing unit / doodad /
  // destructable's "Model File" at the freshly-imported .mdx, reusing the same
  // App bindings the Object Editor uses. Units (umdl), doodads (dfil) and
  // destructables (bfil) all expose the model under SLK column "file", so one
  // SetObjectField(kind, id, "file", path) call covers all three.
  type AssignKind = 'units' | 'doodads' | 'destructables'
  let assignKind: AssignKind = $state('units')
  let objectRows: main.UnitObjectListEntity[] = $state([])
  let objectFilter: string = $state('')
  let selectedObjId: string = $state('')
  let assigning: boolean = $state(false)
  let loadingObjects: boolean = $state(false)

  // Filter the (possibly large) object list client-side; cap the rendered
  // count so a 1000+-unit list stays responsive in the native <select>.
  const filteredRows = $derived.by(() => {
    const q = objectFilter.trim().toLowerCase()
    const rows = q
      ? objectRows.filter(
          (r) =>
            r.name.toLowerCase().includes(q) || r.id.toLowerCase().includes(q),
        )
      : objectRows
    return rows.slice(0, 500)
  })

  async function loadObjects() {
    loadingObjects = true
    selectedObjId = ''
    try {
      objectRows = await ListObjects(assignKind)
    } catch (e) {
      objectRows = []
      showToast(`Failed to list ${assignKind}: ${String(e)}`, 'error')
    } finally {
      loadingObjects = false
    }
  }

  async function onAssignKindChange() {
    objectFilter = ''
    await loadObjects()
  }

  async function doAssign() {
    if (assigning || !selectedObjId || !importedModelPath) return
    assigning = true
    try {
      await SetObjectField(assignKind, selectedObjId, 'file', importedModelPath)
      const row = objectRows.find((r) => r.id === selectedObjId)
      showToast(
        `Assigned model to ${row?.name ?? selectedObjId} (${selectedObjId})`,
        'success',
      )
      // Refresh assets/viewport so any placed instances repaint with the model.
      await onImported?.()
    } catch (e) {
      showToast(`Assign failed: ${String(e)}`, 'error')
    } finally {
      assigning = false
    }
  }

  // Reset transient result state on each open so a prior import's preview /
  // hint doesn't linger when the dialog is reopened. Mirrors the
  // open-transition effect pattern used by SwapTilesetDialog.
  let lastOpen = $state(false)
  $effect(() => {
    if (open && !lastOpen) {
      lastOpen = true
      importedModelPath = ''
      importedTexturePaths = []
      objectRows = []
      objectFilter = ''
      selectedObjId = ''
    } else if (!open && lastOpen) {
      lastOpen = false
      onClose?.()
    }
  })

  // Guard the numeric input: Scale must be a positive float. Disable Import
  // when it isn't (e.g. the user cleared the field → NaN, or typed 0 / a
  // negative).
  let scaleValid: boolean = $derived(
    Number.isFinite(scale) && scale > 0,
  )

  async function doImport() {
    if (importing || !scaleValid) return
    importing = true
    try {
      const result = await ConvertAndImportModel({
        scale: scale,
        upAxis: upAxis,
        flipV: flipV,
      })
      // Empty modelPath ⇒ the user cancelled the native file dialog. No-op:
      // leave the dialog open on the options view, no error toast.
      if (!result || !result.modelPath) {
        return
      }
      importedModelPath = result.modelPath
      importedTexturePaths = result.texturePaths ?? []
      // Surface each backend warning (non-fatal conversion notes) as a toast.
      for (const w of result.warnings ?? []) {
        showToast(w, 'warning')
      }
      showToast(`Imported ${result.modelPath}`, 'success')
      // Populate the "assign to object" picker for the current kind.
      void loadObjects()
      // Let the caller refresh assets so the new in-archive file resolves.
      await onImported?.()
    } catch (e) {
      showToast('Import failed: ' + String(e), 'error')
    } finally {
      importing = false
    }
  }

  // "Done" — hand the imported model to the host to drop on the map at the
  // centre of the current view, then close on success.
  let placing: boolean = $state(false)
  async function doPlace() {
    if (placing || !importedModelPath || !onPlace) return
    placing = true
    try {
      const ok = await onPlace(importedModelPath)
      if (ok) open = false
    } finally {
      placing = false
    }
  }

  function onCancel() {
    open = false
  }
</script>

<Dialog.Root bind:open>
  <Dialog.Content
    class="w-[min(720px,calc(100%-2rem))] max-w-[min(720px,calc(100%-2rem))] sm:max-w-[min(720px,calc(100%-2rem))] gap-0 p-0 overflow-hidden"
  >
    <Dialog.Header class="px-4 py-3 border-b">
      <Dialog.Title>Import 3D Model</Dialog.Title>
      <Dialog.Description class="sr-only">
        Convert an external .obj / .gltf / .glb / .stl mesh into a WC3 .mdx and
        import it into the current map.
      </Dialog.Description>
    </Dialog.Header>

    <div class="flex flex-col gap-4 p-5 max-h-[calc(100vh-12rem)] overflow-y-auto">
      <p class="text-sm text-muted-foreground">
        Set conversion options, then click Import — you'll pick the source mesh
        (<code class="text-xs bg-muted px-1 py-0.5 rounded">.obj</code>,
        <code class="text-xs bg-muted px-1 py-0.5 rounded">.gltf</code>,
        <code class="text-xs bg-muted px-1 py-0.5 rounded">.glb</code>,
        <code class="text-xs bg-muted px-1 py-0.5 rounded">.stl</code>) in the
        next dialog. The mesh is converted to a WC3
        <code class="text-xs bg-muted px-1 py-0.5 rounded">.mdx</code> (plus any
        textures) and imported into the current map.
      </p>

      <div class="grid grid-cols-[auto_1fr] gap-x-3 gap-y-3 items-center">
        <Label for="import-scale" class="shrink-0">Scale</Label>
        <input
          id="import-scale"
          type="number"
          min="0"
          step="0.1"
          class="flex h-9 w-32 rounded-md border border-input bg-background px-3 py-1 text-sm shadow-sm focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50 {scaleValid ? '' : 'border-destructive'}"
          bind:value={scale}
          disabled={importing}
        />

        <Label for="import-upaxis" class="shrink-0">Up axis</Label>
        <select
          id="import-upaxis"
          class="flex h-9 w-40 rounded-md border border-input bg-background px-3 py-1 text-sm shadow-sm focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50"
          bind:value={upAxis}
          disabled={importing}
        >
          <option value="y">Y-up</option>
          <option value="z">Z-up</option>
          <option value="auto">Auto</option>
        </select>

        <span class="shrink-0 text-sm">Flip V</span>
        <label class="flex items-center gap-2 cursor-pointer">
          <Checkbox bind:checked={flipV} disabled={importing} />
          <span class="text-xs text-muted-foreground">
            Flip the V texture coordinate (recommended for most exporters).
          </span>
        </label>
      </div>

      <p class="text-xs text-muted-foreground">
        The model is auto-fit to a sensible in-game size, so
        <strong>Scale 1</strong> is a good default. Scale is a multiplier on top
        of that (2 = twice as big).
      </p>

      {#if !scaleValid}
        <p class="text-xs text-destructive">Scale must be a positive number.</p>
      {/if}

      {#if importedModelPath}
        <section class="flex flex-col gap-2 border-t pt-4">
          <h3 class="text-sm font-semibold">Imported model</h3>
          <AssetPreview modelPath={importedModelPath} {reforged} />
          <p class="text-xs text-muted-foreground">
            Imported as
            <code class="text-xs bg-muted px-1 py-0.5 rounded break-all">{importedModelPath}</code>.
            {#if importedTexturePaths.length > 0}
              ({importedTexturePaths.length} texture{importedTexturePaths.length === 1 ? '' : 's'} imported.)
            {/if}
          </p>

          <div class="flex flex-col gap-2 border-t pt-3 mt-1">
            <h4 class="text-sm font-semibold">
              Assign to an object
              <span class="text-xs font-normal text-muted-foreground">(optional)</span>
            </h4>
            <p class="text-xs text-muted-foreground">
              Point an existing unit / doodad / destructable's
              <strong>Model File</strong> at this import. You can also do this
              later in the <strong>Object Editor</strong>.
            </p>

            <div class="grid grid-cols-[auto_1fr] gap-x-3 gap-y-2 items-center">
              <Label for="assign-kind" class="shrink-0">Type</Label>
              <select
                id="assign-kind"
                class="flex h-9 w-40 rounded-md border border-input bg-background px-3 py-1 text-sm shadow-sm focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50"
                bind:value={assignKind}
                onchange={onAssignKindChange}
                disabled={assigning || loadingObjects}
              >
                <option value="units">Units</option>
                <option value="doodads">Doodads</option>
                <option value="destructables">Destructables</option>
              </select>

              <Label for="assign-filter" class="shrink-0">Find</Label>
              <input
                id="assign-filter"
                type="text"
                placeholder="filter by name or id…"
                class="flex h-9 w-full rounded-md border border-input bg-background px-3 py-1 text-sm shadow-sm focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50"
                bind:value={objectFilter}
                disabled={assigning}
              />
            </div>

            <select
              size="6"
              class="w-full rounded-md border border-input bg-background px-1 py-1 text-sm shadow-sm focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50"
              bind:value={selectedObjId}
              disabled={assigning || loadingObjects}
            >
              {#each filteredRows as r (r.kind + ':' + r.id)}
                <option value={r.id}>
                  {r.name}{r.is_custom ? ' (custom)' : ''} — {r.id}
                </option>
              {/each}
            </select>

            <div class="flex items-center gap-3">
              <Button onclick={doAssign} disabled={assigning || !selectedObjId}>
                {assigning ? 'Assigning…' : 'Assign model'}
              </Button>
              <span class="text-xs text-muted-foreground">
                {#if loadingObjects}
                  Loading…
                {:else}
                  {filteredRows.length} shown{objectRows.length > filteredRows.length ? ` of ${objectRows.length}` : ''}
                {/if}
              </span>
            </div>
          </div>
        </section>
      {/if}
    </div>

    <Dialog.Footer class="px-4 py-3 border-t bg-muted/30 rounded-none mx-0 mb-0">
      {#if importedModelPath}
        <Button variant="ghost" onclick={onCancel} disabled={placing}>
          Close
        </Button>
        <Button onclick={doPlace} disabled={placing || !onPlace}>
          {placing ? 'Placing…' : 'Done — place at view'}
        </Button>
      {:else}
        <Button variant="ghost" onclick={onCancel} disabled={importing}>
          Cancel
        </Button>
        <Button onclick={doImport} disabled={importing || !scaleValid}>
          {importing ? 'Importing…' : 'Import…'}
        </Button>
      {/if}
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>
