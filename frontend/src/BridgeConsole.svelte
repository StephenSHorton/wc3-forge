<script lang="ts">
  // BridgeConsole — dockable bottom panel that streams every MCP bridge-call
  // dispatched against this wc3-forge instance. Lets the user see, live, what
  // every connected agent is doing.
  //
  // Wire-up:
  //   - Go-side: every handler registered via forge.RegisterAll is wrapped
  //     with `instrumented`, which fires a Session.notifyBridgeCall event after
  //     the dispatch (success or error).
  //   - App.startup: subscribes to that event and forwards each
  //     forge.BridgeCallEvent through Wails as `wc3-forge:bridge-call`.
  //   - This component: subscribes to the Wails event, caps history at 500
  //     rows, renders a scrollable table, and exposes filter + clear + dock
  //     toggle + auto-scroll pause/resume.
  //
  // Persistence: panel open/closed state and bottom-pane height are stored in
  // localStorage so they survive reload (per spec). The Ctrl+` hotkey is owned
  // by App.svelte (it owns the global keydown handler and bindable `open`).

  import { onMount, onDestroy } from 'svelte'
  import { EventsOn, EventsOff } from '../wailsjs/runtime/runtime.js'
  import { GetBridgeInfo } from '../wailsjs/go/main/App.js'
  import { Button } from '$lib/components/ui/button'
  import { Input } from '$lib/components/ui/input'

  // Public component prop — bound by the parent so the parent can also drive
  // open/closed state (via header button or Ctrl+` hotkey). Two-way bind so
  // internal toggles (the X close button) propagate up.
  let {
    open = $bindable(false),
  }: {
    open?: boolean
  } = $props()

  // Each BridgeCallEvent that arrives in real time. Capped at 500 rows.
  interface BridgeCallEvent {
    timestamp: string // ISO 8601 from Go time.Time
    method: string
    params_summary: string
    result: string
    error: string
    duration_micros: number
  }
  let calls = $state<BridgeCallEvent[]>([])
  let totalSeen = $state(0) // counts ALL events ever, including dropped-from-cap
  const MAX_ROWS = 500

  let filter = $state('')
  let autoScroll = $state(true)
  let listEl: HTMLDivElement | null = $state(null)

  // Bridge identity for the top bar. Falls back to "(unknown)" before
  // GetBridgeInfo resolves.
  interface BridgeInfo {
    pid: number
    port: number
    token_short: string
    map_name: string
    map_path: string
  }
  let bridgeInfo = $state<BridgeInfo | null>(null)

  // ---- LocalStorage keys + height state ----
  const LS_HEIGHT = 'wc3forge.bridge-console.height'
  const LS_OPEN = 'wc3forge.bridge-console.open'
  // Height in pixels. Defaults to 200 per spec; clamped on drag.
  let heightPx = $state(200)
  const MIN_HEIGHT = 100
  const MAX_HEIGHT = 600

  let filtered = $derived(
    filter.trim()
      ? calls.filter((c) =>
          c.method.toLowerCase().includes(filter.trim().toLowerCase()),
        )
      : calls,
  )

  function timeOf(iso: string): string {
    // ISO 8601 from Go time.Time — extract HH:MM:SS.mmm. Local time for the
    // user (Date constructor honors timezone), millisecond-precision so
    // sub-second ordering is visible when many calls arrive within one second.
    try {
      const d = new Date(iso)
      const hh = String(d.getHours()).padStart(2, '0')
      const mm = String(d.getMinutes()).padStart(2, '0')
      const ss = String(d.getSeconds()).padStart(2, '0')
      const ms = String(d.getMilliseconds()).padStart(3, '0')
      return `${hh}:${mm}:${ss}.${ms}`
    } catch {
      return iso
    }
  }
  function durMs(us: number): string {
    return (us / 1000).toFixed(2)
  }

  // Returns Tailwind class fragments for a row based on status. Errors get a
  // destructive tint; slow (>50ms) calls highlight the method + duration in
  // amber. OK rows are neutral. We split into "row bg" classes and per-cell
  // accent classes via small helpers below.
  function rowBgClass(c: BridgeCallEvent): string {
    if (c.error) return 'bg-destructive/15 hover:bg-destructive/20'
    return 'hover:bg-muted/40'
  }
  function methodClass(c: BridgeCallEvent): string {
    if (c.error) return 'text-destructive font-medium'
    if (c.duration_micros > 50_000) return 'text-yellow-500 font-medium'
    return 'text-foreground font-medium'
  }
  function durClass(c: BridgeCallEvent): string {
    if (c.duration_micros > 50_000) return 'text-yellow-500 text-right'
    return 'text-muted-foreground text-right'
  }

  function onScroll() {
    if (!listEl) return
    // Threshold: 4px from bottom counts as "at bottom" — covers sub-pixel
    // rounding from CSS-zoom and the trailing scrollbar arrow on Windows.
    const dist = listEl.scrollHeight - listEl.scrollTop - listEl.clientHeight
    autoScroll = dist < 4
  }
  function scrollToBottom() {
    if (!listEl) return
    listEl.scrollTop = listEl.scrollHeight
  }

  function clearHistory() {
    calls = []
    // totalSeen intentionally NOT reset — the "[showing last N of M]" banner
    // tracks total stream activity since open, not since clear. Less likely
    // to mislead an agent reading the count as "we've made N calls".
  }

  // ---- Drag-to-resize the panel height ----
  let dragging = $state(false)
  let dragStartY = 0
  let dragStartHeight = 0
  function onDragStart(e: MouseEvent) {
    dragging = true
    dragStartY = e.clientY
    dragStartHeight = heightPx
    e.preventDefault()
  }
  function onDragMove(e: MouseEvent) {
    if (!dragging) return
    // Window-bottom-anchored panel: dragging up makes the panel taller, so
    // subtract the cursor delta from start height.
    const dy = dragStartY - e.clientY
    const next = Math.max(MIN_HEIGHT, Math.min(MAX_HEIGHT, dragStartHeight + dy))
    heightPx = next
  }
  function onDragEnd() {
    if (dragging) {
      dragging = false
      try {
        localStorage.setItem(LS_HEIGHT, String(heightPx))
      } catch {}
    }
  }

  // Persist open state whenever it flips (driven from inside or from parent).
  $effect(() => {
    try {
      localStorage.setItem(LS_OPEN, open ? '1' : '0')
    } catch {}
  })

  async function refreshBridgeInfo() {
    try {
      bridgeInfo = (await GetBridgeInfo()) as unknown as BridgeInfo
    } catch {
      bridgeInfo = null
    }
  }

  // Test-driver hook: external automation can set filter / clear / etc. on
  // the panel by calling these via the page's window.__bridgeConsole bag.
  // The `wc3-forge:test-command` event also routes commands prefixed with
  // `bridge_console.` here so MCP probes can drive the panel.
  function applyTestCommand(cmd: string): void {
    const [op, ...rest] = cmd.split(/\s+/, 2)
    if (op === 'filter') {
      filter = rest.join(' ')
    } else if (op === 'clear') {
      clearHistory()
    } else if (op === 'autoscroll' && rest[0] === 'off') {
      autoScroll = false
    } else if (op === 'autoscroll' && rest[0] === 'on') {
      autoScroll = true
      scrollToBottom()
    }
  }

  onMount(async () => {
    // Restore persisted height + open state. Note: parent passes `open` in
    // (so the toggle button reflects it); we still read LS so first-mount
    // (before the parent loaded LS) lines up.
    try {
      const h = parseInt(localStorage.getItem(LS_HEIGHT) || '', 10)
      if (Number.isFinite(h) && h >= MIN_HEIGHT && h <= MAX_HEIGHT) heightPx = h
    } catch {}

    // Subscribe to the bridge-call event stream from Go. Each event is
    // appended; we drop oldest when over MAX_ROWS so the live view stays
    // responsive on long sessions.
    EventsOn('wc3-forge:bridge-call', (c: BridgeCallEvent) => {
      totalSeen++
      const next = [...calls, c]
      if (next.length > MAX_ROWS) next.splice(0, next.length - MAX_ROWS)
      calls = next
      // RAF-defer the scroll so the row is in the DOM before we try to scroll
      // to it. Without this, listEl.scrollHeight reflects the pre-paint state
      // and we'd miss the new row's height.
      if (autoScroll) requestAnimationFrame(scrollToBottom)
    })
    // Map-changed events update the identifier bar's map name without
    // requiring the user to reopen the panel.
    EventsOn('wc3-forge:map-changed', () => {
      void refreshBridgeInfo()
    })
    await refreshBridgeInfo()

    // Wire up test-driver commands prefixed with `bridge_console.` — lets MCP
    // probes drive the panel's filter / clear / autoscroll state without
    // needing synthetic keyboard input (WebView2 drops it). Re-uses the
    // existing wc3-forge:test-command bus.
    EventsOn('wc3-forge:test-command', (payload: { cmd: string }) => {
      const cmd = payload?.cmd || ''
      const prefix = 'bridge_console.'
      if (cmd.startsWith(prefix + 'filter ')) {
        applyTestCommand('filter ' + cmd.slice((prefix + 'filter ').length))
      } else if (cmd === prefix + 'clear') {
        applyTestCommand('clear')
      } else if (cmd === prefix + 'autoscroll_off') {
        applyTestCommand('autoscroll off')
      } else if (cmd === prefix + 'autoscroll_on') {
        applyTestCommand('autoscroll on')
      }
    })
    ;(window as any).__bridgeConsole = { applyTestCommand }

    // Document-level mousemove/mouseup so drag continues outside the splitter.
    document.addEventListener('mousemove', onDragMove)
    document.addEventListener('mouseup', onDragEnd)
  })
  onDestroy(() => {
    EventsOff('wc3-forge:bridge-call')
    // Note: don't EventsOff('wc3-forge:map-changed') — App.svelte owns that
    // event and would silently lose its subscriber if we tear it down here.
    document.removeEventListener('mousemove', onDragMove)
    document.removeEventListener('mouseup', onDragEnd)
  })
</script>

{#if open}
  <section
    class="flex flex-none flex-col bg-card text-foreground border-t border-border text-xs min-h-[100px] overflow-hidden"
    style="height: {heightPx}px"
  >
    <!-- Drag handle: 4px-tall strip across the top of the panel. Visual
         affordance is the slightly-lighter background + ns-resize cursor.
         role="separator" + aria-orientation tell ATs this is a resizable
         divider; the mousedown handler does the actual drag work. svelte-check
         flags mouse listeners on non-interactive elements but draggable
         separators are exactly the case where this is correct. -->
    <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
    <div
      role="separator"
      aria-orientation="horizontal"
      class="flex-none h-1 bg-border cursor-ns-resize transition-colors hover:bg-muted-foreground/40 {dragging
        ? 'bg-muted-foreground/40'
        : ''}"
      onmousedown={onDragStart}
      title="Drag to resize"
    ></div>

    <header
      class="flex-none flex items-center gap-2.5 px-3 py-1.5 bg-muted/40 border-b border-border text-[11px]"
    >
      <span
        class="font-semibold uppercase tracking-[0.08em] text-muted-foreground text-[10px]"
        >Agent Console</span
      >
      <span
        class="text-muted-foreground font-mono text-[11px] overflow-hidden text-ellipsis whitespace-nowrap min-w-0 flex-none"
      >
        {#if bridgeInfo}
          wc3-forge PID {bridgeInfo.pid} · bridge 127.0.0.1:{bridgeInfo.port}
          {#if bridgeInfo.token_short}
            · token {bridgeInfo.token_short}{/if}
          {#if bridgeInfo.map_name}
            · map <strong class="text-foreground font-semibold"
              >{bridgeInfo.map_name}</strong
            >{/if}
        {:else}
          (bridge info unavailable)
        {/if}
      </span>
      <span class="flex-1"></span>
      <Input
        type="text"
        placeholder="filter by method…"
        bind:value={filter}
        aria-label="Filter rows by method substring"
        class="w-[200px] h-7 px-1.5 text-[11px] font-mono"
      />
      <Button
        variant="secondary"
        size="xs"
        onclick={clearHistory}
        title="Clear local history (stream continues)">Clear</Button
      >
      <Button
        variant="ghost"
        size="icon-xs"
        onclick={() => (open = false)}
        title="Close (Ctrl+`)"
        aria-label="Close">×</Button
      >
    </header>

    <div
      class="flex-none px-3.5 py-0.5 text-[10.5px] text-muted-foreground bg-muted/20 border-b border-border"
    >
      {#if totalSeen > calls.length}
        showing last {calls.length} of {totalSeen}
      {:else}
        {calls.length} call{calls.length === 1 ? '' : 's'}
      {/if}
      {#if filter}
        · filtered: {filtered.length}
      {/if}
      {#if !autoScroll}
        · <span class="text-yellow-500">auto-scroll paused</span>
      {/if}
    </div>

    <div
      class="flex-1 min-h-0 overflow-y-auto overflow-x-hidden font-mono text-[11.5px]"
      bind:this={listEl}
      onscroll={onScroll}
    >
      {#if filtered.length === 0}
        <div class="p-5 text-center text-muted-foreground/60">
          {#if filter}
            No calls match filter <code
              class="bg-muted px-1 py-px rounded text-muted-foreground"
              >{filter}</code
            >.
          {:else}
            Waiting for bridge calls…
          {/if}
        </div>
      {:else}
        {#each filtered as c}
          <div
            class="grid gap-2 px-3.5 py-px border-b border-border/40 items-baseline {rowBgClass(
              c,
            )}"
            style="grid-template-columns: 100px 220px 1fr 1fr 80px;"
          >
            <span
              class="overflow-hidden text-ellipsis whitespace-nowrap text-muted-foreground/60"
              >{timeOf(c.timestamp)}</span
            >
            <span
              class="overflow-hidden text-ellipsis whitespace-nowrap {methodClass(
                c,
              )}">{c.method}</span
            >
            <span
              class="overflow-hidden text-ellipsis whitespace-nowrap text-muted-foreground"
              title={c.params_summary}>{c.params_summary}</span
            >
            <span
              class="overflow-hidden text-ellipsis whitespace-nowrap text-muted-foreground"
              title={c.error || c.result}
            >
              {#if c.error}
                <span class="text-destructive">{c.error}</span>
              {:else}
                {c.result}
              {/if}
            </span>
            <span
              class="overflow-hidden text-ellipsis whitespace-nowrap {durClass(
                c,
              )}">{durMs(c.duration_micros)} ms</span
            >
          </div>
        {/each}
      {/if}
    </div>
  </section>
{/if}
