# wc3-forge end-to-end test suite

Two complementary layers that drive the editor through whole workflows over its
**real MCP bridge** (JSON-RPC 2.0 over NDJSON) and assert both the wire results
and the on-disk effects. Together they replace the hand-rolled per-release smoke
testing.

| Layer | What it drives | When it runs | Covers |
|---|---|---|---|
| **Go suite** — `internal/forge/e2e_scenarios_test.go` | the bridge **in-process** (handlers → `Session` → disk) | locally before every PR, via `go test ./...` (see "Checks (run locally - no CI)" in `CLAUDE.md`; there is no PR CI) | the CASC-free surfaces: map lifecycle + save-safety, terrain, units, start locations, regions, imports, history (undo/redo/groups), view, gameplay constants, diagnostics |
| **PS release runner** — `test/e2e/run_release.ps1` | the **actual built `wc3-forge.exe`** over a headless bridge | manually, before cutting a release (on a machine with Warcraft III installed) | everything the Go suite covers **plus** the CASC-dependent Object Editor (all 7 kinds) which a bare CI runner can't load |

The split mirrors the "ci" vs "casc" tiering: the Go suite is the always-on
regression gate; the runner validates the shipped artifact + the surfaces that
need the stock SLK base data from a CASC install.

## Running it

### Go suite (local pre-PR gate — no setup)

```bash
go test ./internal/forge/ -run '^TestE2E_' -v   # just the e2e scenarios
go test ./...                                    # the whole suite (incl. these)
```

These run on a bare machine: no Warcraft III, no CASC, no GUI. They use the
committed folder fixture at `internal/forge/testdata/extracted-map`.

### Release runner (validates the real .exe)

```powershell
# Build a fresh binary and run everything (needs WC3 installed for the CASC tier):
pwsh test/e2e/run_release.ps1 -Build

# Use an already-built / installed binary:
pwsh test/e2e/run_release.ps1 -Bin 'C:\Program Files\wc3-forge\wc3-forge\wc3-forge.exe'

# No Warcraft III on this machine? Skip the Object Editor (CASC) scenarios:
pwsh test/e2e/run_release.ps1 -SkipCasc
```

It prints `[PASS]`/`[FAIL]` per check and a final `N passed, M failed` tally, and
exits with the failure count (0 = all green) so it can gate a script.

## Layout

```
test/e2e/
  run_release.ps1        # orchestrator: launch headless bridge, run scenarios, tally
  lib/bridge.ps1         # reusable driver: Start-ForgeBridge / Send-ForgeRpc /
                         #   Stop-ForgeBridge + the Check/Write-ForgeSummary harness
  scenarios/
    01-save-safety.ps1   # atomic save, backup, external-change refusal + force
    02-terrain.ps1
    03-units-slocs.ps1
    04-regions-imports.ps1
    05-history-view-diag.ps1
    06-objects-casc.ps1  # CASC tier — skipped under -SkipCasc
  README.md
```

Each scenario opens its **own fresh copy** of the fixture (so they don't
interfere) and the bridge runs on an **isolated lock dir** with only its own PID
killed on teardown — multiple wc3-forge instances coexist by design, so this
never disturbs a sibling agent or your interactive session.

## Writing a new scenario

**PowerShell:** dot-source `lib/bridge.ps1`, then:

```powershell
$b = Start-ForgeBridge -Bin <path>
try {
    $folder = Open-FreshFixture $b                 # (defined by run_release.ps1)
    $r = Send-ForgeRpc $b 'units.move' @{ creation_number = 21; x = 0; y = 0; z = 0 }
    Check 'move ok' (-not $r.error) $r.error.message
    Test-ForgeRpcOK  $b 'save'        'map.save' @{}                     # asserts no error
    Test-ForgeRpcErr $b 'bad cn errs' 'units.move' @{ creation_number = 9 } 'no unit'
} finally { Stop-ForgeBridge $b }
```

Add the function to a `scenarios/NN-*.ps1` file and call it from `run_release.ps1`.

**Go:** add a `TestE2E_*` func to `internal/forge/e2e_scenarios_test.go` using the
shared `startBridge`/`rpc`/`expectOK`/`mustErrContains`/`openExtracted` helpers.

## Wire-contract notes (current behavior the suite pins)

These are intentional or known quirks the tests lock in so they don't drift
silently — not bugs, but worth knowing when reading results:

- **`units.list` returns its array under the key `entities`** (not `units`).
- **`*.get` returns the raw Go struct (PascalCase keys)** — e.g. `units.get` gives
  `{"TypeID":"Hpal","Position":[...],"CreationNumber":21}` — while `*.list`
  returns snake_case DTOs (`type_id`/`position`/`creation_number`).
- **`map.info_get` returns the raw `w3i.Info` (PascalCase keys** like `Name`).
- **The save external-change guard only checks files in *this save's* dirty write
  set** — a stale-save test must dirty the same file it tampers (e.g.
  `terrain.set_height` to touch `war3map.w3e`). The refusal error contains the
  stable marker `changed on disk since it was opened`.
- **`map.save` takes an optional `{ "force": true }`** to override that guard.

## Optionally gating a release on the artifact runner

`run_release.ps1 -SkipCasc` is CASC-free and self-contained, so it *can* be wired
into `.github/workflows/release.yml` as a post-`wails build` smoke gate (the
binary is already built there; no WC3 needed). It's left opt-in because it
launches the GUI binary in `--headless` mode on the runner — validate that path
on your CI image before making it a hard gate. The Go suite is the local pre-PR
gate (there is no PR CI).
