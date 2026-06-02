package forge

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/StephenSHorton/wc3-forge/internal/bridge"
	"github.com/StephenSHorton/wc3-forge/internal/formats/wesstrings"
	"github.com/StephenSHorton/wc3-forge/internal/formats/wts"
)

// MCP handlers for the Object Editor. Phase 2a: refactored into kind-agnostic
// factories. RegisterAll wires every registered KindConfig through
// registerObjectKind below, which stamps out six handlers per kind
// ("objects.<kind>.list", "objects.<kind>.get", "objects.<kind>.fields_meta",
// "objects.<kind>.set_field", "objects.<kind>.create_custom",
// "objects.<kind>.delete_custom"). Wire-format choices remain Phase-1b:
//   - One handler per kind (not a polymorphic single endpoint) so per-kind
//     specifics (race column, item-class, ability levels) don't bleed into
//     a union shape.
//   - List endpoint returns flat rows + per-row category/race tags so the
//     frontend can build the tree client-side.

// Lazy WES-strings table — Reforged ships UI/WorldEditStrings.txt and
// UI/WorldEditGameStrings.txt; merged into a single in-memory table.
var (
	wesOnce sync.Once
	wesTab  *wesstrings.Table
)

func loadWES() *wesstrings.Table {
	wesOnce.Do(func() {
		t := wesstrings.New()
		for _, name := range []string{"UI/WorldEditStrings.txt", "UI/WorldEditGameStrings.txt"} {
			data, ok, err := readBaseAsset(name)
			if err != nil || !ok {
				log.Printf("handlers_objects: wes skip %s: ok=%v err=%v", name, ok, err)
				continue
			}
			if err := t.Load(data); err != nil {
				log.Printf("handlers_objects: wes parse %s: %v", name, err)
			}
		}
		wesTab = t
	})
	return wesTab
}

// resolveDisplay turns a raw SLK/INI cell value into something a UI can show.
// Dereferences TRIGSTR_<n> (map-local strings) and WESTRING_FOO (WC3 stock
// strings), strips WC3 color codes (|cAARRGGBB ... |r), and trims. A no-op
// on plain text. Identical contract to package main's resolveDisplay but
// keeps the forge package self-contained (no cross-package import needed).
func resolveDisplay(raw string, mapStrings wts.Strings) string {
	if raw == "" {
		return ""
	}
	return strings.TrimSpace(wts.StripColorCodes(resolveDisplayKeepColors(raw, mapStrings)))
}

// resolveDisplayKeepColors performs the same TRIGSTR_*/WESTRING_* deref
// resolveDisplay does, but leaves inline WC3 color codes (|cAARRGGBB ... |r)
// intact. The frontend's wc3color.ts parses those into colored segments for
// inline rendering. Trims surrounding whitespace but otherwise keeps the
// resolved string verbatim.
//
// Used by the Object Editor's field-table `display_raw` to give the UI the
// information it needs to color tooltip text — units like the Orc Shadow
// Hunter have names like "|cFFFFCC00Shadow|r Hunter" that should render with
// the leading "Shadow" word in yellow.
func resolveDisplayKeepColors(raw string, mapStrings wts.Strings) string {
	if raw == "" {
		return ""
	}
	v := raw
	if strings.HasPrefix(v, "TRIGSTR_") {
		v = mapStrings.Resolve(v)
	} else if strings.HasPrefix(v, "WESTRING_") {
		v = loadWES().Resolve(v)
	}
	return strings.TrimSpace(v)
}

// titleRace turns the lowercase race tag in SLK ("human", "nightelf",
// "creeps") into a UI-friendly label. Duplicates package main's table —
// small and stable enough that the duplication beats a cross-package call.
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

// normalizeIconPath converts an SLK/INI icon-path cell into the form the
// asset HTTP handler expects: forward slashes, lowercased, with .blp
// appended when no recognized texture extension is declared. Duplicated
// from typeindex.go's same-named helper — same contract, kept local to
// avoid pulling package main into internal/forge.
func normalizeIconPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.ToLower(p)
	ext := strings.ToLower(path.Ext(p))
	if ext != ".blp" && ext != ".dds" && ext != ".tga" {
		p += ".blp"
	}
	return p
}

// unitModelPath resolves the per-unit MDX path from the merged row's
// `file` column. Reforged splits some entries into `file:hd` / `file:sd`
// per-mode variants the same way the icon path does (see unitIconArt);
// the same fallback chain applies. Returns (primary, fallbacks) where
// primary is the .mdx form and the fallbacks list carries the .mdl
// sibling — the asset handler's mdx↔mdl swap already covers most cases,
// but the explicit fallback gives mdx-m3-viewer a second attempt before
// giving up on the load.
//
// Custom maps frequently ship .mdl exports of imported models (e.g.
// "war3mapImported/foo.mdl") so the .mdl fallback is load-bearing for
// custom-content maps even when the asset handler's sibling swap fires.
func unitModelPath(fields map[string]string) (string, []string) {
	for _, k := range []string{"file", "file:hd", "file:sd"} {
		v := strings.TrimSpace(fields[k])
		if v == "" {
			continue
		}
		stem := v
		if i := strings.LastIndexByte(v, '.'); i > 0 {
			ext := strings.ToLower(v[i:])
			if ext == ".mdx" || ext == ".mdl" {
				stem = v[:i]
			}
		}
		return stem + ".mdx", []string{stem + ".mdl"}
	}
	return "", nil
}

// unitIconArt resolves the per-unit command-button icon path from the
// merged unit's `art` column. Reforged moved per-asset icon paths out of
// the base SLK and into unitSkin.txt's `Art=` line, which the base loader
// merges into the Mapped row alongside the SLK columns. Customs that
// override `art` get their own icon; customs that don't override it
// inherit the base's icon via the standard merge.
//
// Reforged unitSkin.txt also carries `art:hd=` / `art:sd=` per-mode
// variants for many units (e.g. Orkn Shadow Hunter has only the HD/SD
// pair, no plain `art`). Fall back to those when the unsuffixed key is
// missing — `art:sd` first because it's the canonical BLP that ships in
// every install, then `art:hd` (which is sometimes DDS-only and the
// asset handler's BLP↔DDS sibling swap recovers it transparently).
func unitIconArt(fields map[string]string) string {
	for _, k := range []string{"art", "art:sd", "art:hd"} {
		if v := strings.TrimSpace(fields[k]); v != "" {
			return normalizeIconPath(v)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Wire shapes. Phase-1b types are kept under their objectsUnits* names so the
// JSON tags stay byte-identical to what shipped; type aliases (objectsList*)
// give the generic factories a kind-agnostic name to work with internally.
// ---------------------------------------------------------------------------

// objectsUnitsListEntity is one row in the list tree. Frontend groups by
// Race, then Kind, to build HiveWE's three-level tree. Sort order is
// name-asc within each Kind. JSON shape is the Phase-1b wire contract —
// do not rename fields without bumping the wire version.
type objectsUnitsListEntity struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Race      string `json:"race"`        // raw lowercase tag ("human"); frontend titlecases via shared lookup
	RaceLabel string `json:"race_label"`  // pre-titled label ("Human")
	Kind      string `json:"kind"`        // "unit" | "hero" | "building" | "special"
	Category  string `json:"category"`    // human-readable bucket combining race + kind
	IsCustom  bool   `json:"is_custom"`
	IsEdited  bool   `json:"is_edited"`
	BaseID    string `json:"base_id,omitempty"` // only set for customs
	Campaign  bool   `json:"campaign"`            // hides under "Campaign" subtree when true
	IconArt   string `json:"icon_art"`            // command-button icon path stem
}

type objectsUnitsGetParams struct {
	ID string `json:"id"`
}

type objectsUnitsField struct {
	ID          string `json:"id"`           // FourCC of the field
	Field       string `json:"field"`        // SLK column name (lowercased)
	DisplayName string `json:"display_name"` // resolved label
	Category    string `json:"category"`     // "stats" | "combat" | "text" | ...
	Type        string `json:"type"`         // "int" | "string" | "bool" | ...
	Value       string `json:"value"`        // raw cell value
	Display     string `json:"display"`      // value with TRIGSTR/WESTRING resolved AND color codes stripped (legacy)
	// DisplayRaw is the resolved value WITH inline WC3 color codes preserved
	// (|cAARRGGBB ... |r). The client parses these for inline rendering. Always
	// safe to fall back to Display when an old client doesn't know about this
	// field. Empty when Value is empty.
	DisplayRaw string `json:"display_raw"`
	Overridden bool   `json:"overridden"`   // true if this came from the per-map shadow
	// Levels carries per-level override values for leveled (opt-format) kinds:
	// level (1-based) → raw value. Empty/omitted for non-leveled fields and for
	// fields with no per-level overrides. The base/level-0 value stays in Value.
	Levels map[uint32]string `json:"levels,omitempty"`
}

type objectsUnitsGetResult struct {
	ID             string              `json:"id"`
	Name           string              `json:"name"`
	BaseID         string              `json:"base_id,omitempty"`
	IsCustom       bool                `json:"is_custom"`
	IsEdited       bool                `json:"is_edited"`
	Race           string              `json:"race"`
	Kind           string              `json:"kind"`
	IconArt        string              `json:"icon_art"`
	ModelPath      string              `json:"model_path"`      // primary .mdx path for the 3D preview
	ModelFallbacks []string            `json:"model_fallbacks"` // .mdl sibling + variants the previewer tries on load failure
	Fields         []objectsUnitsField `json:"fields"`
}

type objectsUnitsSetFieldParams struct {
	ID     string `json:"id"`
	Column string `json:"column"`
	Value  string `json:"value"`
	// Level is the optional 1-based level (abilities/upgrades) or variation
	// index (doodads) the edit targets. Omitted / 0 == the base slot, which
	// preserves the prior non-leveled behavior for every existing caller.
	Level int `json:"level,omitempty"`
}

type objectsUnitsCreateCustomParams struct {
	BaseID string `json:"base_id"`
	ID     string `json:"id,omitempty"`
}

type objectsUnitsCreateCustomResult struct {
	ID     string                 `json:"id"`
	Detail *objectsUnitsGetResult `json:"detail"`
}

type objectsUnitsDeleteCustomParams struct {
	ID string `json:"id"`
}

type objectsUnitsFieldsMetaItem struct {
	ID          string `json:"id"`
	Field       string `json:"field"`
	DisplayName string `json:"display_name"`
	Category    string `json:"category"`
	Type        string `json:"type"`
	MinVal      string `json:"min_val,omitempty"`
	MaxVal      string `json:"max_val,omitempty"`
}

// ---------------------------------------------------------------------------
// Generic handler factories. Each kind gets six handlers stamped from these.
// ---------------------------------------------------------------------------

// listObjects is the generic implementation behind objects.<kind>.list AND
// the Wails ListUnitObjects/etc. shims. Returns rows sorted by race → kind →
// name (case-insensitive).
func listObjects(cfg *KindConfig) ([]objectsUnitsListEntity, error) {
	merged, _, err := MergedObjects(cfg)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", cfg.Kind, err)
	}
	mapStrings := Current.Strings()
	out := make([]objectsUnitsListEntity, 0, len(merged))
	for id, u := range merged {
		if cfg.FilterListRow != nil && !cfg.FilterListRow(id, u.Fields) {
			continue
		}
		name := resolveDisplay(u.Fields["name"], mapStrings)
		if name == "" {
			// Stock rows without a resolved Name still belong in the tree —
			// fall back to the FourCC so the user can locate them.
			name = id
		}
		kind := ""
		if cfg.ClassifyFn != nil {
			kind = cfg.ClassifyFn(u.ID, u.Fields)
		}
		race := strings.ToLower(strings.TrimSpace(u.Fields["race"]))
		cat := ""
		if cfg.CategoryFn != nil {
			cat = cfg.CategoryFn(race, kind)
		}
		iconArt := ""
		if cfg.IconArtFn != nil {
			iconArt = cfg.IconArtFn(u.Fields)
		}
		out = append(out, objectsUnitsListEntity{
			ID:        id,
			Name:      name,
			Race:      race,
			RaceLabel: titleRace(race),
			Kind:      kind,
			Category:  cat,
			IsCustom:  u.IsCustom,
			IsEdited:  u.IsEdited,
			BaseID:    u.BaseID,
			Campaign:  u.Fields["campaign"] == "1",
			IconArt:   iconArt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Race != out[j].Race {
			return out[i].Race < out[j].Race
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// getObject is the generic implementation behind objects.<kind>.get. Returns
// nil error + nil result when the id isn't known so callers can treat that
// as "empty state" without branching on error vs. missing.
func getObject(cfg *KindConfig, id string) (*objectsUnitsGetResult, error) {
	if id == "" {
		return nil, errors.New("id is required")
	}
	merged, meta, err := MergedObjects(cfg)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", cfg.Kind, err)
	}
	u, ok := merged[id]
	if !ok {
		return nil, fmt.Errorf("no %s object with id %q", cfg.Kind, id)
	}

	mapStrings := Current.Strings()
	rows := make([]objectsUnitsField, 0, len(meta.Fields))
	for i := range meta.Fields {
		f := &meta.Fields[i]
		if cfg.AppliesFn != nil && !cfg.AppliesFn(*f) {
			continue
		}
		// `col` is the display/wire column name (lowercased Field), kept byte-
		// identical for every field so the frontend's vocabulary is unchanged.
		// `storeCol` is where the value actually lives in u.Fields: the same as
		// `col` for normal fields, but the lowercased FourCC for SHARED columns
		// (Spell Book spb1..spb5 all on "Data") so they don't read back collapsed.
		col := strings.ToLower(f.Field)
		storeCol := meta.storeCol(f.ID, col)
		val, has := u.Fields[storeCol]
		// Fallback to the column-name slot when the FourCC slot is absent —
		// shields against any path that stored a shared field by column name.
		if !has && storeCol != col {
			val, has = u.Fields[col]
		}
		if !has {
			val = ""
		}
		var levels map[uint32]string
		lf := u.LevelFields[storeCol]
		if len(lf) == 0 && storeCol != col {
			lf = u.LevelFields[col]
		}
		if len(lf) > 0 {
			levels = make(map[uint32]string, len(lf))
			for lvl, v := range lf {
				levels[lvl] = v
			}
		}
		rows = append(rows, objectsUnitsField{
			ID:          f.ID,
			Field:       col,
			DisplayName: resolveDisplay(f.DisplayName, mapStrings),
			Category:    f.Category,
			Type:        f.Type,
			Value:       val,
			Display:     resolveDisplay(val, mapStrings),
			DisplayRaw:  resolveDisplayKeepColors(val, mapStrings),
			Overridden:  u.Overridden[storeCol] || u.Overridden[col] || levels != nil,
			Levels:      levels,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Category != rows[j].Category {
			return rows[i].Category < rows[j].Category
		}
		fi := meta.ByID[rows[i].ID]
		fj := meta.ByID[rows[j].ID]
		ii, jj := fi.Index, fj.Index
		if ii < 0 {
			ii = 1 << 30
		}
		if jj < 0 {
			jj = 1 << 30
		}
		if ii != jj {
			return ii < jj
		}
		return rows[i].DisplayName < rows[j].DisplayName
	})

	modelPath, modelFallbacks := "", []string(nil)
	if cfg.ModelPathFn != nil {
		modelPath, modelFallbacks = cfg.ModelPathFn(u.Fields)
	}
	kind := ""
	if cfg.ClassifyFn != nil {
		kind = cfg.ClassifyFn(u.ID, u.Fields)
	}
	iconArt := ""
	if cfg.IconArtFn != nil {
		iconArt = cfg.IconArtFn(u.Fields)
	}
	return &objectsUnitsGetResult{
		ID:             u.ID,
		Name:           resolveDisplay(u.Fields["name"], mapStrings),
		BaseID:         u.BaseID,
		IsCustom:       u.IsCustom,
		IsEdited:       u.IsEdited,
		Race:           strings.ToLower(u.Fields["race"]),
		Kind:           kind,
		IconArt:        iconArt,
		ModelPath:      modelPath,
		ModelFallbacks: modelFallbacks,
		Fields:         rows,
	}, nil
}

// fieldsMetaForKind is the generic implementation behind
// objects.<kind>.fields_meta. Static for the process lifetime modulo
// map-strings resolution.
func fieldsMetaForKind(cfg *KindConfig) ([]objectsUnitsFieldsMetaItem, error) {
	_, meta, err := loadObjectBase(cfg)
	if err != nil {
		return nil, fmt.Errorf("load metadata: %w", err)
	}
	mapStrings := Current.Strings()
	out := make([]objectsUnitsFieldsMetaItem, 0, len(meta.Fields))
	for i := range meta.Fields {
		f := &meta.Fields[i]
		if cfg.AppliesFn != nil && !cfg.AppliesFn(*f) {
			continue
		}
		out = append(out, objectsUnitsFieldsMetaItem{
			ID:          f.ID,
			Field:       strings.ToLower(f.Field),
			DisplayName: resolveDisplay(f.DisplayName, mapStrings),
			Category:    f.Category,
			Type:        f.Type,
			MinVal:      f.MinVal,
			MaxVal:      f.MaxVal,
		})
	}
	return out, nil
}

// makeListHandler returns the objects.<kind>.list handler bound to cfg.
func makeListHandler(cfg *KindConfig) bridge.Handler {
	return func(_ json.RawMessage) (any, error) {
		return listObjects(cfg)
	}
}

// makeGetHandler returns the objects.<kind>.get handler bound to cfg.
func makeGetHandler(cfg *KindConfig) bridge.Handler {
	return func(params json.RawMessage) (any, error) {
		var p objectsUnitsGetParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		return getObject(cfg, p.ID)
	}
}

// makeFieldsMetaHandler returns the objects.<kind>.fields_meta handler.
func makeFieldsMetaHandler(cfg *KindConfig) bridge.Handler {
	return func(_ json.RawMessage) (any, error) {
		return fieldsMetaForKind(cfg)
	}
}

// makeSetFieldHandler returns the objects.<kind>.set_field handler. Re-emits
// the post-mutation Get payload so the caller doesn't need a follow-up
// round-trip to refresh its view.
func makeSetFieldHandler(cfg *KindConfig) bridge.Handler {
	return func(params json.RawMessage) (any, error) {
		var p objectsUnitsSetFieldParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if p.ID == "" {
			return nil, errors.New("id is required")
		}
		if p.Column == "" {
			return nil, errors.New("column is required")
		}
		if p.Level < 0 {
			return nil, errors.New("level must be >= 0")
		}
		if err := Current.SetObjectFieldLevel(cfg, p.ID, p.Column, uint32(p.Level), p.Value); err != nil {
			return nil, err
		}
		return getObject(cfg, p.ID)
	}
}

// makeCreateCustomHandler returns the objects.<kind>.create_custom handler.
func makeCreateCustomHandler(cfg *KindConfig) bridge.Handler {
	return func(params json.RawMessage) (any, error) {
		var p objectsUnitsCreateCustomParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if p.BaseID == "" {
			return nil, errors.New("base_id is required")
		}
		id, err := Current.AddCustomObject(cfg, p.ID, p.BaseID)
		if err != nil {
			return nil, err
		}
		detail, err := getObject(cfg, id)
		if err != nil {
			return nil, fmt.Errorf("get newly-created %q: %w", id, err)
		}
		return objectsUnitsCreateCustomResult{ID: id, Detail: detail}, nil
	}
}

// makeDeleteCustomHandler returns the objects.<kind>.delete_custom handler.
func makeDeleteCustomHandler(cfg *KindConfig) bridge.Handler {
	return func(params json.RawMessage) (any, error) {
		var p objectsUnitsDeleteCustomParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if p.ID == "" {
			return nil, errors.New("id is required")
		}
		if err := Current.DeleteCustomObject(cfg, p.ID); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	}
}

// objectsConvertParams is the wire shape for objects.convert. Kind names are
// the same wire tags KindConfig.Kind uses; today only doodads↔destructables
// is accepted.
type objectsConvertParams struct {
	SrcKind string `json:"src_kind"`
	SrcID   string `json:"src_id"`
	DstKind string `json:"dst_kind"`
}

type objectsConvertResult struct {
	ID     string                 `json:"id"`
	Detail *objectsUnitsGetResult `json:"detail"`
}

// handleObjectsConvert is the MCP handler for objects.convert. Wraps
// Session.ConvertObject. Returns the new destination custom's id + its full
// detail payload so the JS caller doesn't need a follow-up Get round-trip.
func handleObjectsConvert(params json.RawMessage) (any, error) {
	var p objectsConvertParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}
	if p.SrcKind == "" || p.SrcID == "" || p.DstKind == "" {
		return nil, errors.New("src_kind, src_id, dst_kind are required")
	}
	id, err := Current.ConvertObject(p.SrcKind, p.SrcID, p.DstKind)
	if err != nil {
		return nil, err
	}
	dstCfg, _ := resolveKind(p.DstKind)
	detail, err := getObject(dstCfg, id)
	if err != nil {
		return objectsConvertResult{ID: id}, nil
	}
	return objectsConvertResult{ID: id, Detail: detail}, nil
}

// registerObjectKind wires the six MCP handlers for one kind into the
// bridge. RegisterAll calls this once per kind (today: units; Phase 2b adds
// the other six). `reg` is RegisterAll's local closure that goes through the
// `instrumented` wrapper so Agent Console subscribers see every dispatch.
func registerObjectKind(reg func(method string, h bridge.Handler), cfg *KindConfig) {
	reg("objects."+cfg.Kind+".list", makeListHandler(cfg))
	reg("objects."+cfg.Kind+".get", makeGetHandler(cfg))
	reg("objects."+cfg.Kind+".fields_meta", makeFieldsMetaHandler(cfg))
	reg("objects."+cfg.Kind+".set_field", makeSetFieldHandler(cfg))
	reg("objects."+cfg.Kind+".create_custom", makeCreateCustomHandler(cfg))
	reg("objects."+cfg.Kind+".delete_custom", makeDeleteCustomHandler(cfg))
}

// ---------------------------------------------------------------------------
// Phase-1b-named wrappers retained for the test harness. The tests reference
// `handleObjectsUnitsList` / `handleObjectsUnitsGet` directly; those names
// still resolve to the units variant of the generic factory output. New
// tests should prefer `listObjects(UnitsConfig())` etc. directly.
// ---------------------------------------------------------------------------

func handleObjectsUnitsList(_ json.RawMessage) (any, error) {
	return listObjects(UnitsConfig())
}

func handleObjectsUnitsGet(params json.RawMessage) (any, error) {
	var p objectsUnitsGetParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}
	return getObject(UnitsConfig(), p.ID)
}
