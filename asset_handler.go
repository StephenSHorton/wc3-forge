package main

import (
	"log"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/StephenSHorton/wc3-forge/internal/casc"
	"github.com/StephenSHorton/wc3-forge/internal/forge"
	"github.com/StephenSHorton/wc3-forge/internal/mpqchain"
	"github.com/StephenSHorton/wc3-forge/internal/wc3path"
)

// stockSource is the seam behind which a WC3 install's STOCK assets are read.
// Two implementations slot in interchangeably:
//   - *casc.Storage     — Reforged / modern installs (CASC storage).
//   - *mpqchain.Chain   — Classic / pre-Reforged installs (layered War3*.mpq).
//
// Both already satisfy this shape. The asset handler talks only to this
// interface, so the resolution order and sibling-extension logic are
// identical regardless of which kind of install is mounted. The contract for
// ReadFile is exactly casc.Storage's: (nil, false, nil) on a clean miss, a
// non-nil error only on a genuine failure — so the handler's "map source
// exhausted before stock" ordering is preserved byte-for-byte.
type stockSource interface {
	ReadFile(name string) ([]byte, bool, error)
	ListByPrefix(prefix string) ([]string, error)
	SetReforged(b bool)
	Reforged() bool
	SetTileset(ts byte)
	Close() error
}

// applyMapTileset points the stock source at the loaded map's w3e tileset
// letter so CASC ReadFile tries `_tilesets/<letter>.w3mod:` mounts. Classic
// MPQ chains no-op this. Safe when no map is loaded (tileset 0).
func applyMapTileset(c stockSource) {
	var ts byte
	if t := forge.Current.Terrain(); t != nil {
		ts = t.Tileset
	}
	c.SetTileset(ts)
}

var (
	_ stockSource = (*casc.Storage)(nil)
	_ stockSource = (*mpqchain.Chain)(nil)
)

// assetHandler serves /asset/<path> requests from Go-side asset sources.
// mdx-m3-viewer's pathSolver returns "/asset/<src>" strings; the viewer
// then XHR-fetches via Wails' embedded HTTP server, which routes here.
//
// Resolution order (first hit wins):
//  1. The currently-loaded map's archive/folder (custom imports + per-map
//     overrides + the map's own files like war3map.w3i if requested).
//  2. CASC mount on the user's WC3 install — for stock unit/doodad
//     models, tileset textures, team-color BLPs, etc.
type assetHandler struct{}

func newAssetHandler() http.Handler {
	return &assetHandler{}
}

// Lazy-init the WC3 stock-asset source. We don't fail wc3-forge to start if
// it isn't available — folder-based maps still work, and the user may not
// have WC3 installed yet (the GUI prompts them to locate it). reopenCASC lets
// the user point at a new install without restarting.
//
// The source is either a CASC storage (Reforged/modern install) or an MPQ
// patch chain (Classic 1.27b-era install), chosen by wc3path.Classify. Both
// satisfy stockSource, so everything downstream is source-agnostic.
var (
	cascMu      sync.Mutex
	cascStorage stockSource
	cascErr     error
	cascLoaded  bool
)

// wc3InstallPath is the install root CASC mounts. Delegates to the shared
// resolver (env override -> user-saved path -> OS default) so it stays in
// lockstep with the Test-Map launcher.
func wc3InstallPath() string {
	return wc3path.Resolve()
}

func getCASC() (stockSource, error) {
	cascMu.Lock()
	defer cascMu.Unlock()
	if !cascLoaded {
		openCASCLocked()
	}
	return cascStorage, cascErr
}

// reopenCASC closes any open storage and re-opens at the currently-resolved
// install path. Called after the user picks a new WC3 install so stock assets
// resolve immediately, without a restart.
func reopenCASC() (stockSource, error) {
	cascMu.Lock()
	defer cascMu.Unlock()
	if cascStorage != nil {
		_ = cascStorage.Close()
		cascStorage = nil
	}
	openCASCLocked()
	// Drop every stock-data cache that may have memoized a miss while the old
	// (missing/invalid) install was mounted — the sync.Once loaders otherwise
	// keep returning the cached failure forever, so terrain/cliffs/water/icons
	// stay broken until a restart. See resetStockDataCaches.
	resetStockDataCaches()
	return cascStorage, cascErr
}

// openCASCLocked (re)opens the stock-asset source at the resolved path,
// choosing CASC vs the MPQ patch chain by the install kind. Caller holds
// cascMu.
//
// On any failure we leave cascStorage as a nil INTERFACE (not a typed-nil
// pointer wrapped in an interface), so the `c != nil` guards at the call
// sites correctly treat "no source" as absent. Assigning a typed nil pointer
// to the interface would make it non-nil and crash those callers.
func openCASCLocked() {
	cascLoaded = true
	cascStorage = nil
	cascErr = nil

	p := wc3InstallPath()
	switch wc3path.Classify(p) {
	case wc3path.KindClassic:
		log.Printf("stock: opening Classic MPQ chain at %q", p)
		chain, err := mpqchain.Open(p)
		if err != nil {
			cascErr = err
			log.Printf("stock: MPQ chain open failed: %v", err)
			return
		}
		cascStorage = chain
		log.Printf("stock: MPQ chain open")
	default:
		// KindCASC or KindNone: try CASC. On a KindNone path CASC will fail
		// to open and we degrade gracefully (folder maps still work), exactly
		// as before this change.
		log.Printf("CASC: opening storage at %q", p)
		store, err := casc.Open(p)
		if err != nil {
			cascErr = err
			log.Printf("CASC: open failed: %v", err)
			return
		}
		cascStorage = store
		log.Printf("CASC: storage open")
	}
}

func (h *assetHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = "/asset/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	requested := r.URL.Path[len(prefix):]
	if requested == "" {
		http.Error(w, "missing asset path", http.StatusBadRequest)
		return
	}
	defer assetRecordResolveTime(time.Now()) // diagnostics: slow-resolve + max
	// Normalize: lowercase + forward-slash. WC3 asset paths are
	// conventionally case-insensitive; archives and CASC are stored
	// lowercase. Backslash <-> forward-slash interchangeable.
	requested = strings.ToLower(path.Clean(strings.ReplaceAll(requested, "\\", "/")))

	// Build the candidate path list:
	//   1. The requested path itself.
	//   2. If it ends in .blp/.dds/.mdx/.mdl, ALSO try the sibling extension.
	// Reforged CASC ships textures as DDS only (no BLP), and many custom
	// maps reference imported models with the "wrong" extension. mdx-m3-viewer's
	// handlers auto-detect format by magic bytes, so serving DDS through a
	// .blp URL (or MDX through a .mdl URL) works — the lib picks the right
	// resource class from the data.
	//
	// In HD mode we flip the preference when the requested texture is .blp:
	// HD models tend to reference DDS directly, and the SD .blp variant may
	// not even exist in a Reforged install. So under HD + a requested .blp,
	// try .dds FIRST. For requested .dds (the HD case) we keep that first.
	// SD mode always tries the requested extension first. Model extensions
	// (MDX↔MDL) are never flipped.
	candidates := []string{requested}
	if alt := siblingExtension(requested); alt != "" {
		ext := strings.ToLower(path.Ext(requested))
		if ReforgedMode() && ext == ".blp" {
			candidates = []string{alt, requested}
		} else {
			candidates = append(candidates, alt)
		}
	}

	// Try ALL candidates against the map source FIRST, before falling back
	// to CASC. This matters when a map imports a stock-path texture via
	// sibling extension (e.g. ships terrainart/cityscape/City_GrassTrim.blp
	// to override the stock City_GrassTrim.dds). With the old per-candidate
	// interleaving, the requested .dds would resolve from CASC before we'd
	// ever try the map's .blp, hiding the override. Map wins for ANY
	// extension match before CASC ever runs.
	for _, candidate := range candidates {
		data, ok, err := forge.Current.ReadFile(candidate)
		if err != nil {
			assetRecordReadError()
			http.Error(w, "read error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if ok {
			assetRecordHit(false, candidate != requested)
			via := "map source"
			if candidate != requested {
				via = "map source (sibling-ext)"
			}
			log.Printf("asset: %s -> %s as %s (%d bytes)", requested, via, candidate, len(data))
			serveBytes(w, candidate, data)
			return
		}
	}

	// CASC fallback for stock WC3 assets — only after exhausting the map.
	if c, err := getCASC(); err == nil && c != nil {
		applyMapTileset(c)
		for _, candidate := range candidates {
			data, ok, err := c.ReadFile(candidate)
			if err != nil {
				assetRecordReadError()
				log.Printf("asset: %s -> CASC error: %v", candidate, err)
				http.Error(w, "casc read error: "+err.Error(), http.StatusInternalServerError)
				return
			}
			if ok {
				assetRecordHit(true, candidate != requested)
				via := "CASC"
				if candidate != requested {
					via = "CASC (sibling-ext)"
				}
				log.Printf("asset: %s -> %s as %s (%d bytes)", requested, via, candidate, len(data))
				serveBytes(w, candidate, data)
				return
			}
		}
	}

	assetRecord404(requested)
	log.Printf("asset: %s -> 404", requested)
	http.NotFound(w, r)
}

// siblingExtension returns the same path with the format-pair extension
// swapped (BLP↔DDS for textures, MDX↔MDL for models, .tif→.dds for the
// Reforged HD PBR-texture references). Returns "" if the extension isn't
// one of those pairs.
//
// Reforged HD MDX files reference their PBR textures as `.tif` paths
// (e.g. `units/orc/rokhan/orc_rokhan_main_diffuse.tif`) but CASC actually
// ships the bytes as `.dds`. Without this swap, every HD-only unit
// (Shadow Hunter, Beastmaster, several Reforged-era heroes) renders
// invisible in any preview / scene that loads its MDX — the geometry is
// there but every texture binding is null. mdx-m3-viewer's DDS handler
// claims the bytes by magic regardless of URL extension, so serving DDS
// through a .tif URL works. The same swap covers stock sky models like
// IcecrownGlacierSky.mdx that also reference .tif. One-way map: .dds
// doesn't fall back to .tif because nothing in the wild stores it the
// other way.
func siblingExtension(p string) string {
	ext := path.Ext(p)
	stem := p[:len(p)-len(ext)]
	switch strings.ToLower(ext) {
	case ".blp":
		return stem + ".dds"
	case ".dds":
		return stem + ".blp"
	case ".tif":
		return stem + ".dds"
	case ".mdx":
		return stem + ".mdl"
	case ".mdl":
		return stem + ".mdx"
	}
	return ""
}

func serveBytes(w http.ResponseWriter, name string, data []byte) {
	if ct := contentTypeFor(name); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "no-cache") // map changes mean stale bytes; no caching while we iterate
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func contentTypeFor(name string) string {
	switch path.Ext(name) {
	case ".mdx", ".mdl", ".blp", ".dds", ".tga":
		return "application/octet-stream"
	case ".txt", ".ini", ".slk":
		return "text/plain; charset=utf-8"
	case ".json":
		return "application/json"
	}
	return "application/octet-stream"
}
