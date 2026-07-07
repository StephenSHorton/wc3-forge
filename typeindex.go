package main

import (
	"log"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/StephenSHorton/wc3-forge/internal/forge"
	"github.com/StephenSHorton/wc3-forge/internal/formats/slk"
	"github.com/StephenSHorton/wc3-forge/internal/formats/w3objmod"
	"github.com/StephenSHorton/wc3-forge/internal/formats/wesstrings"
	"github.com/StephenSHorton/wc3-forge/internal/formats/wts"
)

// TypeIndex resolves a FourCC type ID ("Hpal", "ATtr", …) to the data the
// renderer needs: MDX path, vertex tint, model scale, variation count, etc.
// Backed by the WC3 base SLK files (UnitData, unitUI, ItemData, Doodads).
//
// One unit/doodad-shaped lookup per Wails session. Built lazily on first use
// so cold start stays cheap (~100ms saved when running headless). Cached for
// the process lifetime — base SLKs don't change between map opens.

// UnitTypeInfo carries everything the JS scene needs to instantiate a unit
// or item MDX. Fields mirror the SLK column names from War3MapViewer's
// handlers/w3x/unit.js so the JS side can mostly forget about SLK semantics.
type UnitTypeInfo struct {
	File       string  `json:"file"`        // MDX path stem (no .mdx suffix); empty if absent
	ModelScale float64 `json:"model_scale"` // baseline scale multiplier; default 1
	MoveHeight float64 `json:"move_height"` // Z offset above terrain
	Red        int     `json:"red"`         // 0..255 vertex tint
	Green      int     `json:"green"`       // 0..255 vertex tint
	Blue       int     `json:"blue"`        // 0..255 vertex tint
	Name       string  `json:"name"`        // resolved display name (e.g. "Paladin"); empty if unknown
	Category   string  `json:"category"`    // human-readable group (e.g. "Human Hero", "Item")
	// IconArt is the command-button portrait path (e.g.
	// "ReplaceableTextures/CommandButtons/BTNFootman.blp"). For units it
	// comes from Units/unitSkin.txt `Art=`; for items from Units/ItemFunc.txt
	// `Art=`. The original SLK columns (UnitData.slk, ItemData.slk) don't
	// carry an icon column — Reforged moved per-asset paths into the *Skin/
	// *Func INI files. About 60% of stock units and 98% of stock items have
	// this populated; the rest fall back to a generic placeholder in JS.
	IconArt string `json:"icon_art"`
}

// DoodadTypeInfo carries everything the JS scene needs to instantiate a
// doodad or destructible MDX. NumVar selects which fileN.mdx variant.
//
// MaxPitch / MaxRoll come from the Doodads.slk / DestructableData.slk
// `maxPitch` / `maxRoll` columns (radians, HiveWE convention). Per HiveWE's
// src/base/doodad.ixx::update:
//   - negative value: user-set fixed rotation (apply as-is); the magnitude
//     is the pitch / roll angle, in radians, that the doodad should be tilted
//     at regardless of the underlying terrain. This is what custom maps use
//     to author flame-on-sword and similar effects whose source MDX has the
//     particle emitters laid out along +X / +Y in model-local space and
//     needs a fixed rotation to point them upright.
//   - positive value: terrain-following — sample neighbouring terrain heights
//     and pitch / roll to follow the slope, clamped by ±magnitude. We don't
//     implement the terrain-follow branch yet (no caller has needed it on
//     the maps we've shipped against), so a positive value is currently
//     a no-op. The fixed-negative path is what custom D-type doodads in
//     the wild actually rely on (e.g. Enfo FFB's D002 Fire Sword Effect
//     uses maxPitch=-4.71 to rotate the embers from +X to +Z).
type DoodadTypeInfo struct {
	File       string  `json:"file"`        // MDX path stem (no .mdx suffix)
	NumVar     int     `json:"num_var"`     // variation count; 1 means no suffix
	FixedRot   float64 `json:"fixed_rot"`   // terrain-doodad rotation override (degrees)
	ModelScale float64 `json:"model_scale"` // baseline scale; default 1
	MaxPitch   float64 `json:"max_pitch"`   // negative = fixed pitch in radians (rotated about model-local Y)
	MaxRoll    float64 `json:"max_roll"`    // negative = fixed roll in radians (rotated about model-local X)
	Name       string  `json:"name"`        // resolved display name (e.g. "Summer Tree Wall"); empty if unknown
	Category   string  `json:"category"`    // human-readable group (e.g. "Trees/Destructibles")
	// IconArt is the category icon path (e.g.
	// "ReplaceableTextures/WorldEditUI/Doodad-Tree.dds"). Doodads + destruct-
	// ables have NO per-row art column in their SLKs — HiveWE's source-of-
	// truth maps the single-letter `category` column through UI/WorldEditData.txt
	// sections [DoodadCategories] / [DestructibleCategories] to a shared icon
	// per category. So every "Trees/Destructibles" row gets the same tree
	// icon; structures get the structures icon; etc. That's the same scheme
	// HiveWE's asset palette uses.
	IconArt string `json:"icon_art"`
}

var (
	indexOnce sync.Once
	indexErr  error
	unitIndex map[string]UnitTypeInfo
	doodIndex map[string]DoodadTypeInfo

	wesOnce sync.Once
	wesTab  *wesstrings.Table

	doodadIconOnce sync.Once
	// doodadCategoryIcons maps a single-letter doodad/destructable category
	// (e.g. "D" for Trees/Destructibles) to its icon path stem (no extension).
	// Populated lazily from UI/WorldEditData.txt sections [DoodadCategories]
	// and [DestructibleCategories]. Same mapping HiveWE's tree models use.
	doodadCategoryIcons map[string]string
)

func loadSLKs(m *slk.Mapped, names []string) {
	for _, name := range names {
		data, ok, err := readBaseAsset(name)
		if err != nil || !ok {
			log.Printf("typeindex: skip %s: ok=%v err=%v", name, ok, err)
			continue
		}
		if err := m.Load(data); err != nil {
			log.Printf("typeindex: parse %s: %v", name, err)
		}
	}
}

func loadINIs(m *slk.Mapped, names []string) {
	for _, name := range names {
		data, ok, err := readBaseAsset(name)
		if err != nil || !ok {
			log.Printf("typeindex: skip %s: ok=%v err=%v", name, ok, err)
			continue
		}
		if err := m.LoadINI(data); err != nil {
			log.Printf("typeindex: parse %s: %v", name, err)
		}
	}
}

// readBaseAsset is the same map-first-then-CASC lookup the assetHandler uses,
// minus the HTTP plumbing. We need it here so SLK lookups work regardless of
// whether a map is currently loaded.
func readBaseAsset(name string) ([]byte, bool, error) {
	clean := strings.ToLower(path.Clean(strings.ReplaceAll(name, "\\", "/")))

	if data, ok, err := forge.Current.ReadFile(clean); err != nil {
		return nil, false, err
	} else if ok {
		return data, true, nil
	}
	c, err := getCASC()
	if err != nil || c == nil {
		return nil, false, err
	}
	return c.ReadFile(clean)
}

// wesStrings lazily loads the WC3 stock string table (UI/WorldEditStrings.txt
// + WorldEditGameStrings.txt). Empty Table if CASC is unavailable.
func wesStrings() *wesstrings.Table {
	wesOnce.Do(func() {
		t := wesstrings.New()
		for _, name := range []string{
			"UI/WorldEditStrings.txt",
			"UI/WorldEditGameStrings.txt",
		} {
			data, ok, err := readBaseAsset(name)
			if err != nil || !ok {
				log.Printf("typeindex: wes load skip %s: ok=%v err=%v", name, ok, err)
				continue
			}
			if err := t.Load(data); err != nil {
				log.Printf("typeindex: wes parse %s: %v", name, err)
			}
		}
		wesTab = t
	})
	return wesTab
}

// resolveDisplay normalizes a raw SLK/INI cell value into something a UI can
// show: dereferences TRIGSTR_<n> (map-local strings) and WESTRING_FOO
// (WC3 stock strings), strips WC3 color codes (|cAARRGGBB ... |r), trims.
// A no-op on plain text inputs.
func resolveDisplay(raw string, mapStrings wts.Strings) string {
	if raw == "" {
		return ""
	}
	v := raw
	if strings.HasPrefix(v, "TRIGSTR_") {
		v = mapStrings.Display(v)
	} else if strings.HasPrefix(v, "WESTRING_") {
		v = wesStrings().Resolve(v)
	}
	return strings.TrimSpace(wts.StripColorCodes(v))
}

// buildTypeIndex parses every base SLK we care about. Failures on individual
// files log a warning but don't abort — partial coverage beats no coverage,
// and the asset handler will 404 missing models cleanly.
//
// **The skin .txt files are NOT optional.** Reforged CASC stripped per-asset
// paths out of the base SLKs (Doodads.slk, UnitData.slk, …) and moved them
// into companion `*Skin.txt` files. Without merging these, every doodad/
// unit lookup returns a row with no `file` column → nothing renders. The
// lib only loads them when isReforged=true; we have to load them regardless,
// because our CASC source IS a Reforged install.
//
// The race-specific *UnitStrings.txt / *UnitFunc.txt files are also merged
// here so each row picks up its proper `Name=Paladin` (the stock SLK only
// has internal lowercase identifiers like "paladin"). Same with the Doodad/
// Destructable SkinStrings files for display names.
func buildTypeIndex() {
	units := slk.New()
	loadSLKs(units, []string{
		"Units/UnitData.slk",
		"Units/unitUI.slk",
		"Units/ItemData.slk",
	})
	loadINIs(units, []string{
		"Units/unitSkin.txt",
		"Units/itemSkin.txt",
		"Units/HumanUnitStrings.txt",
		"Units/OrcUnitStrings.txt",
		"Units/UndeadUnitStrings.txt",
		"Units/NightElfUnitStrings.txt",
		"Units/NeutralUnitStrings.txt",
		"Units/CampaignUnitStrings.txt",
		"Units/ItemStrings.txt",
		// ItemFunc.txt is where item command-button icons live (Art=...);
		// the SLK side and the *Skin.txt variants both lack an icon column.
		"Units/ItemFunc.txt",
	})

	doodads := slk.New()
	loadSLKs(doodads, []string{
		"Doodads/Doodads.slk",
		"Units/DestructableData.slk",
	})
	loadINIs(doodads, []string{
		"Doodads/doodadSkins.txt",
		"Units/destructableSkin.txt",
	})

	wes := wesStrings()

	unitIndex = make(map[string]UnitTypeInfo, len(units.Rows))
	for id, row := range units.Rows {
		info := UnitTypeInfo{
			File:       row.String("file"),
			ModelScale: row.Number("modelScale"),
			MoveHeight: row.Number("moveHeight"),
			Red:        int(row.Number("red")),
			Green:      int(row.Number("green")),
			Blue:       int(row.Number("blue")),
			Name:       resolveDisplay(row.String("name"), nil),
			Category:   unitCategory(row, wes),
			IconArt:    unitIconArt(row),
		}
		// SLKs default to scale 1 when the column is absent; Number returns
		// 0 in that case which would render the model invisibly small.
		if info.ModelScale == 0 {
			info.ModelScale = 1
		}
		unitIndex[id] = info
	}

	doodIndex = make(map[string]DoodadTypeInfo, len(doodads.Rows))
	for id, row := range doodads.Rows {
		info := DoodadTypeInfo{
			File:     row.String("file"),
			NumVar:   int(row.Number("numVar")),
			FixedRot: row.Number("fixedRot"),
			// Doodads use defScale; modelScale is the unit-side name. The
			// Reforged skin .txt also carries defScale:hd for HD-only sizing
			// which we ignore (SD-only rendering for now).
			ModelScale: firstNonZero(row.Number("defScale"), row.Number("modelScale")),
			// SLK Number returns 0 for missing columns, which our renderer
			// treats as "no rotation override" (zero pitch / roll). That
			// matches HiveWE's behaviour for the stock rows that don't carry
			// maxPitch / maxRoll at all.
			MaxPitch: row.Number("maxPitch"),
			MaxRoll:  row.Number("maxRoll"),
			Name:     resolveDisplay(row.String("name"), nil),
			Category: doodadCategory(row.String("category"), wes),
			IconArt:  doodadIconArt(row),
		}
		if info.NumVar <= 0 {
			info.NumVar = 1
		}
		if info.ModelScale == 0 {
			info.ModelScale = 1
		}
		doodIndex[id] = info
	}

	log.Printf("typeindex: %d units/items, %d doodads/destructibles indexed",
		len(unitIndex), len(doodIndex))
}

// unitCategory derives a human-readable bucket label from the unit row.
// Heroes use the race + "Hero". Items have a `class` column ("Artifact",
// "Permanent", "Charged"). Buildings have `isbldg=1`. Everything else
// falls back to a titlecased `race` column.
func unitCategory(row slk.MappedRow, wes *wesstrings.Table) string {
	unitclass := row.String("unitclass")
	race := strings.TrimSpace(row.String("race"))
	class := strings.TrimSpace(row.String("class"))
	isBldg := row.String("isbldg") == "1"

	if class != "" {
		// Items like ratf carry a class ("Artifact", "Charged", …). Item rows
		// have no race; class doubles as their category.
		return "Item · " + class
	}
	if strings.Contains(unitclass, "Hero") {
		return titleRace(race) + " Hero"
	}
	if isBldg {
		return titleRace(race) + " Building"
	}
	if race != "" {
		return titleRace(race)
	}
	return ""
}

// titleRace turns the lowercase race tag in the SLK ("human", "nightelf",
// "creeps") into a UI-friendly label. WC3 stock races are a small fixed
// set so we hardcode the pretty names rather than calling into WESTRINGS.
func titleRace(r string) string {
	switch strings.ToLower(r) {
	case "human":
		return "Human"
	case "orc":
		return "Orc"
	case "undead":
		return "Undead"
	case "nightelf":
		return "Night Elf"
	case "naga":
		return "Naga"
	case "creeps":
		return "Creep"
	case "critters":
		return "Critter"
	case "commoner":
		return "Commoner"
	case "demon":
		return "Demon"
	case "other":
		return "Special"
	case "":
		return ""
	default:
		return strings.Title(r)
	}
}

// doodadCategoryKeys maps the single-letter `category` column in Doodads.slk
// to the WESTRING_DTYPE_* key in UI/WorldEditGameStrings.txt. Letters come
// from Blizzard's stock data — verified by enumerating Doodads.slk.
var doodadCategoryKeys = map[string]string{
	"B": "WESTRING_DTYPE_BRIDGE",       // Bridges/Ramps
	"C": "WESTRING_DTYPE_CLIFF",        // Cliff/Terrain
	"D": "WESTRING_DTYPE_DESTRUCTABLE", // Trees/Destructibles
	"E": "WESTRING_DTYPE_ENVIRONMENT",  // Environment
	"O": "WESTRING_DTYPE_PROPS",        // Props
	"P": "WESTRING_DTYPE_PATHING",      // Pathing Blockers
	"S": "WESTRING_DTYPE_STRUCTURES",   // Structures
	"T": "WESTRING_DTYPE_TERRAIN",      // Terrain
	"W": "WESTRING_DTYPE_WATER",        // Water
	"Z": "WESTRING_DTYPE_CINEMATIC",    // Cinematic
}

func doodadCategory(letter string, wes *wesstrings.Table) string {
	letter = strings.ToUpper(strings.TrimSpace(letter))
	if letter == "" {
		return ""
	}
	if key, ok := doodadCategoryKeys[letter]; ok {
		if v := wes.Lookup(key); v != "" {
			return v
		}
	}
	return letter
}

// loadDoodadCategoryIcons parses UI/WorldEditData.txt sections [DoodadCategories]
// and [DestructibleCategories]. Each line looks like:
//
//	D=WESTRING_DTYPE_DESTRUCTABLE,ReplaceableTextures\WorldEditUI\Doodad-Destructible
//
// The second comma-separated field is the icon path stem (no extension).
// We don't need a full INI parser here — the format is simple enough to
// inline-scan, and we avoid pulling another loader into this file.
//
// All paths get normalized to forward-slash + lowercased; ".blp" appended
// where the source omits it. JS-side <img> requests resolve via /asset/<path>
// which already handles BLP↔DDS sibling swap.
func loadDoodadCategoryIcons() map[string]string {
	doodadIconOnce.Do(func() {
		doodadCategoryIcons = map[string]string{}
		data, ok, err := readBaseAsset("UI/WorldEditData.txt")
		if err != nil || !ok {
			log.Printf("typeindex: WorldEditData.txt load skip: ok=%v err=%v", ok, err)
			return
		}
		// Strip UTF-8 BOM.
		if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
			data = data[3:]
		}
		section := ""
		for _, raw := range strings.Split(string(data), "\n") {
			line := strings.TrimSpace(raw)
			if line == "" || strings.HasPrefix(line, "//") || strings.HasPrefix(line, ";") {
				continue
			}
			if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
				section = line[1 : len(line)-1]
				continue
			}
			if section != "DoodadCategories" && section != "DestructibleCategories" {
				continue
			}
			eq := strings.IndexByte(line, '=')
			if eq < 0 {
				continue
			}
			key := strings.ToUpper(strings.TrimSpace(line[:eq]))
			val := strings.TrimSpace(line[eq+1:])
			// Format: WESTRING_XYZ,ReplaceableTextures\WorldEditUI\Foo
			// Take the second comma-separated field.
			parts := strings.SplitN(val, ",", 2)
			if len(parts) < 2 {
				continue
			}
			icon := strings.TrimSpace(parts[1])
			if icon == "" {
				continue
			}
			doodadCategoryIcons[key] = normalizeIconPath(icon)
		}
		log.Printf("typeindex: loaded %d doodad/destructable category icons", len(doodadCategoryIcons))
	})
	return doodadCategoryIcons
}

// normalizeIconPath converts an SLK/INI icon-path cell into the form the
// asset HTTP handler expects: forward slashes, lowercased, with a .blp
// extension when none is declared (the handler's BLP↔DDS swap makes the
// extension choice mostly cosmetic). Returns "" for empty input.
func normalizeIconPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.ToLower(p)
	// Append .blp if no recognized extension. Reforged CASC actually ships
	// these as .dds; the asset handler swaps siblings on miss so either
	// extension routes to the live file.
	ext := strings.ToLower(path.Ext(p))
	if ext != ".blp" && ext != ".dds" && ext != ".tga" {
		p += ".blp"
	}
	return p
}

// unitIconArt extracts the per-unit/item icon path. The Reforged data layout
// moves icon paths into the *Skin.txt / *Func.txt INI files (unitSkin.txt's
// "Art=" column and ItemFunc.txt's "Art=" column), both merged into the
// units MappedRow alongside the SLK columns at load time. Returns "" when
// the row has no icon set — caller falls back gracefully.
func unitIconArt(row slk.MappedRow) string {
	return normalizeIconPath(row.String("art"))
}

// doodadIconArt picks the per-row icon path for a doodad/destructable.
// Both kinds use the category icon (HiveWE's behaviour) since their SLKs
// don't carry a per-row art column. Returns "" when the row's category
// letter doesn't map to a known WorldEditData entry.
func doodadIconArt(row slk.MappedRow) string {
	letter := strings.ToUpper(strings.TrimSpace(row.String("category")))
	if letter == "" {
		return ""
	}
	icons := loadDoodadCategoryIcons()
	if v, ok := icons[letter]; ok {
		return v
	}
	return ""
}

func ensureTypeIndex() error {
	indexOnce.Do(buildTypeIndex)
	return indexErr
}

func firstNonZero(vs ...float64) float64 {
	for _, v := range vs {
		if v != 0 {
			return v
		}
	}
	return 0
}

// --- Per-map overlay (custom + modified types) ---
//
// war3map.w3{d,b,u,t} carry the per-map object-modification tables. They
// hold two kinds of edits:
//   - "Original" edits: column overrides applied to a stock type (e.g.,
//     "ATtr" with maxScale changed to 1.5 for this map).
//   - "Custom" derived types: new FourCCs ("D006") that inherit a base
//     type's data, then apply their own overrides.
//
// We merge these on top of the cached stock indices when callers ask for
// the index. Stock cache stays immutable; the merge is per-call. At the
// scale we operate on (a few hundred customs at most) this is fine.
//
// Field-FourCC ↔ column-name mapping comes from {Doodad,Destructable,…}
// MetaData.slk; for the rendering-critical handful we hardcode it here so
// we don't have to ship another lazy SLK loader. Adding a column to the
// hardcoded set is a one-line change.

// doodadFieldMap maps the field-FourCCs used in war3map.w3d (and w3b for
// destructibles where they overlap) to their SLK column names. Add fields
// here as the renderer grows to consume more.
var doodadFieldMap = map[string]string{
	// Doodads (DoodadMetaData.slk)
	"dfil": "file",
	"dvar": "numVar",
	"ddes": "defScale",
	"dmis": "minScale",
	"dmas": "maxScale",
	"dfxr": "fixedRot",
	"dmap": "maxPitch", // per-doodad pitch override (radians; negative = fixed)
	"dmar": "maxRoll",  // per-doodad roll override (radians; negative = fixed)
	"dvr1": "red",
	"dvg1": "green",
	"dvb1": "blue",
	"dnam": "name",
	"dcat": "category",
	// Destructibles (DestructableMetaData.slk)
	"bfil": "file",
	"bvar": "numVar",
	"bmis": "minScale",
	"bmas": "maxScale",
	"bfxr": "fixedRot",
	"bmap": "maxPitch",
	"bmar": "maxRoll",
	"bnam": "name",
	"bcat": "category",
}

// unitFieldMap maps the field-FourCCs used in war3map.w3u / war3map.w3t to
// their SLK column names. We only consume display-name overrides for now;
// expand as the renderer surfaces more fields.
var unitFieldMap = map[string]string{
	"unam":      "name",       // unit name override (w3u)
	"umdl":      "file",       // unit model-file override (w3u / war3mapSkin.w3u) — custom skins
	"usca":      "modelscale", // unit scaling value (Art - Scaling Value)
	"uclr":      "red",        // Art - Tinting Color 1 (Red)
	"uclg":      "green",      // Art - Tinting Color 2 (Green)
	"uclb":      "blue",       // Art - Tinting Color 3 (Blue)
	"unsf":      "editorsuffix",
	"utub":      "tilesets",
	"urac":      "race",
	"ucla":      "unitclass",
	"unam2":     "name", // item name (w3t variant)
	"unam_item": "name",
	"unin":      "description",
	"inam":      "name", // item name (w3t)
	"uico":      "art",  // unit command-button icon override (w3u)
	"iico":      "art",  // item icon override (w3t)
}

func resolveColumn(modID string, fields map[string]string) string {
	if c, ok := fields[modID]; ok {
		return c
	}
	return modID
}

func applyDoodadOverrides(base DoodadTypeInfo, ov w3objmod.Overrides, mapStrings wts.Strings) DoodadTypeInfo {
	out := base
	for rawCol, val := range ov {
		col := resolveColumn(rawCol, doodadFieldMap)
		switch strings.ToLower(col) {
		case "file":
			out.File = val
		case "numvar":
			out.NumVar = parseInt(val, base.NumVar)
		case "defscale", "modelscale":
			out.ModelScale = parseFloat(val, base.ModelScale)
		case "fixedrot":
			out.FixedRot = parseFloat(val, base.FixedRot)
		case "maxpitch":
			out.MaxPitch = parseFloat(val, base.MaxPitch)
		case "maxroll":
			out.MaxRoll = parseFloat(val, base.MaxRoll)
		case "name":
			out.Name = resolveDisplay(val, mapStrings)
		case "category":
			out.Category = doodadCategory(val, wesStrings())
			// Re-derive the icon when the per-map override changes the
			// category letter — custom doodads that switch from Trees to
			// Structures get the structures icon automatically.
			if icons := loadDoodadCategoryIcons(); icons != nil {
				if v, ok := icons[strings.ToUpper(strings.TrimSpace(val))]; ok {
					out.IconArt = v
				}
			}
		}
	}
	return out
}

func applyUnitOverrides(base UnitTypeInfo, ov w3objmod.Overrides, mapStrings wts.Strings) UnitTypeInfo {
	out := base
	for rawCol, val := range ov {
		col := resolveColumn(rawCol, unitFieldMap)
		switch strings.ToLower(col) {
		case "name":
			out.Name = resolveDisplay(val, mapStrings)
		case "file":
			// Custom units can point at a different model — a per-map skin swap
			// (e.g. a Peasant-based unit using the Sylvanas model). Without this
			// the viewport falls back to the base type's model, the exact
			// mismatch users see vs the World Editor. UnitTypeInfo.File is a stem
			// (placeUnit re-appends ".mdx" via mdxPath), so strip any extension
			// the override carries. Empty override keeps the base model.
			if s := trimModelExt(strings.TrimSpace(val)); s != "" {
				out.File = s
			}
		case "modelscale":
			// Art - Scaling Value (usca). Only applies when the override is
			// present; absent leaves the stock scale (default 1).
			out.ModelScale = parseFloat(val, base.ModelScale)
		case "red":
			// Art - Tinting Color (uclr/uclg/uclb), 0..255 per channel. Only
			// applied when the override is present; the renderer treats an
			// all-zero tint as "no tint" (scene-instances leaves the instance
			// default-white), so an untinted unit is never darkened.
			out.Red = parseInt(val, base.Red)
		case "green":
			out.Green = parseInt(val, base.Green)
		case "blue":
			out.Blue = parseInt(val, base.Blue)
		case "art":
			// Per-map custom units may declare their own command-button icon
			// (war3map.w3u uart field). Lets a map override the Footman
			// icon for a Hero Footman variant, etc.
			out.IconArt = normalizeIconPath(val)
		}
	}
	return out
}

// trimModelExt strips a trailing .mdx/.mdl (case-insensitive) so a model-file
// override collapses to the extension-less stem UnitTypeInfo.File holds
// (placeUnit re-appends ".mdx"). Leaves a stem without extension untouched.
func trimModelExt(s string) string {
	if i := strings.LastIndexByte(s, '.'); i > 0 {
		switch strings.ToLower(s[i:]) {
		case ".mdx", ".mdl":
			return s[:i]
		}
	}
	return s
}

func parseInt(s string, fallback int) int {
	// w3objmod serialises float modifications as "1.5" even when the SLK
	// column is conceptually an int; ParseFloat-then-truncate covers both.
	if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
		return int(v)
	}
	return fallback
}

func parseFloat(s string, fallback float64) float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
		return v
	}
	return fallback
}

// mergeDoodadIndex returns a copy of the stock doodad index with the per-map
// w3d (doodads) + w3b (destructibles) modifications applied.
//
//   - OriginalEdits modify the row for the stock FourCC in place.
//   - Customs add a new row keyed by the derived FourCC, starting from the
//     base type's data and applying the custom's overrides.
//
// If the base type is missing from the stock index, the custom skips
// (logged). That can happen for very old maps referencing pre-Reforged
// types that have been retired.
func mergeDoodadIndex() map[string]DoodadTypeInfo {
	out := make(map[string]DoodadTypeInfo, len(doodIndex))
	for k, v := range doodIndex {
		out[k] = v
	}
	mapStrings := forge.Current.Strings()
	for _, mods := range []*w3objmod.File{forge.Current.DoodadMods(), forge.Current.DestructibleMods()} {
		if mods == nil {
			continue
		}
		for _, edit := range mods.OriginalEdits {
			base, ok := out[edit.BaseID]
			if !ok {
				continue
			}
			out[edit.BaseID] = applyDoodadOverrides(base, edit.Overrides, mapStrings)
		}
		for _, c := range mods.Customs {
			base, ok := out[c.BaseID]
			if !ok {
				log.Printf("typeindex: custom %s missing base %s; skipping", c.ID, c.BaseID)
				continue
			}
			out[c.ID] = applyDoodadOverrides(base, c.Overrides, mapStrings)
		}
	}
	return out
}

// mergeUnitIndex applies per-map w3u (units) + w3t (items) modifications,
// including their Reforged war3mapSkin.w3* companions. Display name, model
// file (umdl), model scale (usca), and icon (uico) overrides flow through so
// custom-skin units render with their real model instead of the base type's.
func mergeUnitIndex() map[string]UnitTypeInfo {
	out := make(map[string]UnitTypeInfo, len(unitIndex))
	for k, v := range unitIndex {
		out[k] = v
	}
	mapStrings := forge.Current.Strings()
	// Apply the war3mapSkin.w3* companion first (lower precedence), then the
	// primary war3map.w3* on top — a field set in both resolves to the primary
	// value, while art/skin fields present only in the skin table (the common
	// Reforged case: custom name/model/icon) still flow through. Mirrors the
	// primary-wins merge in forge.MergedObjects.
	for _, mods := range []*w3objmod.File{forge.Current.UnitSkinMods(), forge.Current.ItemSkinMods()} {
		applyUnitModsLayer(out, mods, mapStrings)
	}
	for _, mods := range []*w3objmod.File{forge.Current.UnitMods(), forge.Current.ItemMods()} {
		applyUnitModsLayer(out, mods, mapStrings)
	}
	return out
}

// applyUnitModsLayer folds one w3objmod shadow (a primary war3map.w3* table or
// its war3mapSkin.w3* companion) into the unit type index. Customs apply onto
// the entry a prior layer already minted (so skin-then-primary accumulates)
// or, on first sight, onto a fresh copy of their base type.
func applyUnitModsLayer(out map[string]UnitTypeInfo, mods *w3objmod.File, mapStrings wts.Strings) {
	if mods == nil {
		return
	}
	for _, edit := range mods.OriginalEdits {
		base, ok := out[edit.BaseID]
		if !ok {
			continue
		}
		out[edit.BaseID] = applyUnitOverrides(base, edit.Overrides, mapStrings)
	}
	for _, c := range mods.Customs {
		cur, ok := out[c.ID]
		if !ok {
			base, bok := out[c.BaseID]
			if !bok {
				log.Printf("typeindex: custom unit %s missing base %s; skipping", c.ID, c.BaseID)
				continue
			}
			cur = base
		}
		out[c.ID] = applyUnitOverrides(cur, c.Overrides, mapStrings)
	}
}

// GetUnitTypeIndex returns the full FourCC → UnitTypeInfo map. Per-map
// w3u/w3t modifications layer on top of the stock SLK so display names
// reflect per-map overrides — JS-side caches this per-map, not per-process.
func (a *App) GetUnitTypeIndex() (map[string]UnitTypeInfo, error) {
	if err := ensureTypeIndex(); err != nil {
		return nil, err
	}
	return mergeUnitIndex(), nil
}

// GetDoodadTypeIndex returns the full FourCC → DoodadTypeInfo map, with
// per-map w3d (doodads) + w3b (destructibles) modifications layered on
// top of the stock SLK. Must be re-fetched after each map open since the
// overlay changes — JS-side caches this per-map, not per-process.
func (a *App) GetDoodadTypeIndex() (map[string]DoodadTypeInfo, error) {
	if err := ensureTypeIndex(); err != nil {
		return nil, err
	}
	return mergeDoodadIndex(), nil
}
