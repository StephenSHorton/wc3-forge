package forge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/StephenSHorton/wc3-forge/internal/bridge"
)

// packageMainBridgeMethods are bridge methods registered OUTSIDE
// forge.RegisterAll — in package main, after it — because their handlers depend
// on top-level helpers (CASC colors / tileset palette / minimap bake). They ARE
// in the tools.json catalog, so the parity test must treat them as registered.
// Keep in sync with main.go's registerMinimapBridge / registerMapNewBridge.
var packageMainBridgeMethods = []string{
	"minimap.bake",
	"map.new",
}

type catalogTool struct {
	Name   string `json:"name"`
	Method string `json:"method"` // "" (JSON null) for MCP-local tools (sessions_list/session_select)
}

// TestToolsCatalogParity asserts the MCP catalog (internal/mcpserver/tools.json,
// generated from mcp/src/tools.ts) and the registered bridge methods are in
// 1:1 correspondence:
//   - every cataloged forwarded method has a registered handler, and
//   - every registered handler (except internal "_"-prefixed escape hatches) has
//     a catalog entry.
//
// This is the guard that a handler added to RegisterAll without a tools.ts entry
// (or a tools.ts tool with no handler) fails `go test`. The local gen:tools drift check (CLAUDE.md "Checks") separately
// catches "tools.ts edited but tools.json not regenerated".
func TestToolsCatalogParity(t *testing.T) {
	// Registered methods = forge.RegisterAll + the package-main extras.
	b := bridge.New()
	RegisterAll(b)
	registered := map[string]bool{}
	for _, m := range b.ListHandlers() {
		registered[m] = true
	}
	for _, m := range packageMainBridgeMethods {
		registered[m] = true
	}

	// Catalog methods = tools.json entries with a non-null method (the 2 local
	// MCP tools — sessions_list / session_select — carry method:null and are
	// handled inside the MCP server, not forwarded to the bridge).
	toolsPath := filepath.Join("..", "mcpserver", "tools.json")
	raw, err := os.ReadFile(toolsPath)
	if err != nil {
		t.Fatalf("read %s: %v", toolsPath, err)
	}
	var tools []catalogTool
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("parse tools.json: %v", err)
	}
	catalog := map[string]string{} // method -> tool name
	for _, tl := range tools {
		if tl.Method == "" {
			continue // MCP-local tool
		}
		if prev, dup := catalog[tl.Method]; dup {
			t.Errorf("tools.json: method %q mapped by two tools (%q and %q)", tl.Method, prev, tl.Name)
		}
		catalog[tl.Method] = tl.Name
	}

	// Direction 1: every cataloged method must have a registered handler.
	var missingHandler []string
	for method := range catalog {
		if !registered[method] {
			missingHandler = append(missingHandler, method)
		}
	}
	// Direction 2: every registered handler (minus "_"-prefixed internal escape
	// hatches like _ui.send_command, deliberately uncataloged) must be cataloged.
	var missingCatalog []string
	for method := range registered {
		if strings.HasPrefix(method, "_") {
			continue
		}
		if _, ok := catalog[method]; !ok {
			missingCatalog = append(missingCatalog, method)
		}
	}

	// Log the tally so a vacuous pass (e.g. catalog parsed as empty) is obvious
	// in -v output — the catalog should be ~150 and registered ~149.
	t.Logf("catalog forwarded methods=%d, registered handlers=%d (incl. %d package-main)",
		len(catalog), len(registered), len(packageMainBridgeMethods))

	sort.Strings(missingHandler)
	sort.Strings(missingCatalog)
	if len(missingHandler) > 0 {
		t.Errorf("tools.json lists %d method(s) with NO registered handler: %v\n"+
			"  → a tool in mcp/src/tools.ts has no matching reg() in RegisterAll (or package-main registration).",
			len(missingHandler), missingHandler)
	}
	if len(missingCatalog) > 0 {
		t.Errorf("%d registered bridge method(s) have NO tools.json entry: %v\n"+
			"  → add them to mcp/src/tools.ts and run `cd mcp && npm run gen:tools` (or prefix with '_' if intentionally internal).",
			len(missingCatalog), missingCatalog)
	}
}
