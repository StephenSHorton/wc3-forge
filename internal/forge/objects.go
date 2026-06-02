package forge

import (
	"log"
	"strings"
	"sync"

	"github.com/StephenSHorton/wc3-forge/internal/formats/slk"
	"github.com/StephenSHorton/wc3-forge/internal/formats/w3objmod"
)

// Object Editor data layer — kind-agnostic generic that hosts every object
// kind (units, items, abilities, buffs, destructables, doodads, upgrades)
// under one shared code path.
//
// Phase 2a ships the refactor with units-only as the lone registered kind;
// Phase 2b adds the other six by stamping out additional KindConfig instances
// and pointing them at the right SLK/INI families + per-kind metadata file.
//
// The original Phase-1b "units only" globals (unitsBaseOnce / unitsBase /
// unitsMeta) are replaced with a per-kind cache keyed by KindConfig.Kind.
// Each kind has its own sync.Once + cache slot, so loading items doesn't
// block units and a failing kind doesn't poison the others.

// KindConfig captures everything that varies per object kind. One instance
// per kind lives at package level (UnitsConfig, ItemsConfig in 2b, ...).
// Handlers + the Session mutators take a *KindConfig and dispatch through it.
//
// Closure-based session accessors (GetMods, SetMods, GetDirty, SetDirty)
// avoid the awkward "pointer-to-pointer struct field" pattern: the per-kind
// shadow lives in a Session field directly (s.unitMods, s.itemMods, ...);
// the closure plumbs the read/write so generic code doesn't need to know
// which field. Captures the *Session at call time (not config time) so the
// same config can serve any Session — important for tests that build their
// own Session rather than touching the global Current.
type KindConfig struct {
	// Kind is the wire-namespace tag: "units", "items", "abilities", "buffs",
	// "destructables", "doodads", "upgrades". MCP routes are
	// "objects." + Kind + ".(list|get|...)" — keep it lowercase + URL-safe.
	Kind string

	// MetaDataFile is the per-kind metadata SLK ("Units/UnitMetaData.slk").
	// Drives field display names, types, applicability flags, FourCC bindings.
	MetaDataFile string

	// BaseSLKs is the ordered list of SLK tables that contribute to the base
	// row set. Later loads overwrite earlier ones on collision (HiveWE merge
	// order). For units: UnitData → UnitBalance → UnitWeapons → UnitAbilities → unitUI.
	BaseSLKs []string

	// BaseINIs is the per-race / per-skin INI overlays. Reforged moved display
	// names and icon paths out of SLKs into these companion .txt files; without
	// them the `name`/`art` columns are empty.
	BaseINIs []string

	// ShadowFile is the per-map war3map.w3* file ("war3map.w3u" for units).
	// Open reads it; Save writes it.
	ShadowFile string

	// ShadowOpt is the `opt` flag for w3objmod.Parse/Encode — true for w3d and
	// w3a (variation/level/dataPointer fields per modification), false for the
	// rest (w3b, w3u, w3t).
	ShadowOpt bool

	// AppliesFn filters metadata rows so a field shared across kinds (the
	// units+items metadata table for instance) only surfaces in the editor
	// that should host it.
	AppliesFn func(ObjectFieldMeta) bool

	// ClassifyFn returns the leaf tree-bucket for a row (e.g. "hero" |
	// "building" | "special" | "unit" for units; item class for items;
	// category for destructables/doodads; "hero"/"item"/"unit" for
	// abilities). Wire-stable strings — the JS Object Editor switches on
	// them. May be nil for kinds that flat-list everything.
	ClassifyFn func(id string, fields map[string]string) string

	// FilterListRow returns true if a row should appear in the list response.
	// Units use "row has a `race` column" (skips item rows that share the
	// metadata family). When nil, every row from the merged base ships.
	FilterListRow func(id string, fields map[string]string) bool

	// CategoryFn composes the per-row "race + kind" label that appears in the
	// list response. For units it's `titleRace(race) + " " + kindSuffix`; for
	// items it'd be the item-class label. Returns the display string.
	CategoryFn func(race, kind string) string

	// IconArtFn resolves the per-row icon path the editor tree shows. nil =
	// "no icon" (the tree renders a placeholder square). Units, items, and
	// abilities all have unitSkin-style `art:hd`/`art:sd` variants; doodads
	// and destructables use a single `art` column.
	IconArtFn func(fields map[string]string) string

	// ModelPathFn resolves the primary .mdx + fallback .mdl paths for the
	// kind's 3D preview pane. Returns ("", nil) when the row has no model;
	// the preview pane stays empty. Units use the `file` column; doodads
	// also use `file`; abilities/upgrades/buffs have no model.
	ModelPathFn func(fields map[string]string) (string, []string)

	// GetMods returns the *w3objmod.File for this kind on the given session.
	// Returns nil when the session has no shadow for this kind (fresh map,
	// no edits yet). Caller MUST hold s.mu (read or write).
	GetMods func(s *Session) *w3objmod.File

	// SetMods replaces the shadow pointer on the session. Used by the
	// ensure-allocated helper and by tests that stub a fresh File in place.
	// Caller MUST hold s.mu (write lock).
	SetMods func(s *Session, f *w3objmod.File)

	// GetDirty returns the per-kind dirty flag value on the session.
	// Caller MUST hold s.mu (read or write).
	GetDirty func(s *Session) bool

	// SetDirty sets the per-kind dirty flag. Caller MUST hold s.mu (write).
	SetDirty func(s *Session, v bool)
}

// kindConfigs is the lookup map used by undo/redo commands to dispatch back
// into the right config from a stored Kind string. Populated by package init
// calling registerKindConfig for every kind we ship.
//
// Read-only after init — no mutex needed. Tests that want to stub a kind
// can use the test-only override by calling registerKindConfig from a Test*
// function before the test body runs (each test ends with the same call
// restoring the production config).
var kindConfigs = map[string]*KindConfig{}

// registerKindConfig wires a KindConfig into the lookup table. Called once
// per kind during package init (see UnitsConfig below). Idempotent — calling
// twice with the same Kind overwrites the prior entry (test-friendly).
func registerKindConfig(cfg *KindConfig) {
	kindConfigs[cfg.Kind] = cfg
}

// kindConfigFor returns the KindConfig with the given Kind, or panics if
// none is registered. Internal use only — undo/redo commands resolve their
// stored Kind string this way, and a missing entry means somebody minted a
// command for an unregistered kind (a programmer error, not a runtime
// condition that should silently no-op).
func kindConfigFor(kind string) *KindConfig {
	cfg, ok := kindConfigs[kind]
	if !ok {
		panic("forge: no KindConfig registered for kind " + kind)
	}
	return cfg
}

// objectBaseCache is the per-kind lazy-load cache. One slot per kind, each
// guarded by its own sync.Once so kinds load independently. Indexed by
// KindConfig.Kind.
type objectBaseCache struct {
	once sync.Once
	base *slk.Mapped
	meta *ObjectMetadata
	err  error
}

var (
	objectBaseMu  sync.Mutex
	objectBaseMap = map[string]*objectBaseCache{}
)

// getObjectBaseCache returns (creating if needed) the per-kind cache slot.
// The cache instance survives across calls so the sync.Once stays load-bearing.
func getObjectBaseCache(kind string) *objectBaseCache {
	objectBaseMu.Lock()
	defer objectBaseMu.Unlock()
	if c, ok := objectBaseMap[kind]; ok {
		return c
	}
	c := &objectBaseCache{}
	objectBaseMap[kind] = c
	return c
}

// loadObjectBase performs the lazy one-time load of stock object data for
// the given kind. Replaces Phase-1b's loadUnitsBase — same shape, but the
// SLK/INI/metadata file list comes from cfg, and the cache is per-kind.
//
// Missing files log a warning but don't abort; a partial mount still yields
// a usable (if incomplete) editor. UnitMetaData missing collapses to an
// empty metadata table so the editor renders an empty field list rather
// than crashing.
func loadObjectBase(cfg *KindConfig) (*slk.Mapped, *ObjectMetadata, error) {
	cache := getObjectBaseCache(cfg.Kind)
	cache.once.Do(func() {
		base := slk.New()
		for _, name := range cfg.BaseSLKs {
			data, ok, err := readBaseAsset(name)
			if err != nil || !ok {
				log.Printf("objects[%s]: skip SLK %s: ok=%v err=%v", cfg.Kind, name, ok, err)
				continue
			}
			if err := base.Load(data); err != nil {
				log.Printf("objects[%s]: parse SLK %s: %v", cfg.Kind, name, err)
			}
		}
		for _, name := range cfg.BaseINIs {
			data, ok, err := readBaseAsset(name)
			if err != nil || !ok {
				log.Printf("objects[%s]: skip INI %s: ok=%v err=%v", cfg.Kind, name, ok, err)
				continue
			}
			if err := base.LoadINI(data); err != nil {
				log.Printf("objects[%s]: parse INI %s: %v", cfg.Kind, name, err)
			}
		}
		cache.base = base

		metaData, ok, err := readBaseAsset(cfg.MetaDataFile)
		if err != nil || !ok {
			log.Printf("objects[%s]: %s missing: ok=%v err=%v", cfg.Kind, cfg.MetaDataFile, ok, err)
			cache.meta = &ObjectMetadata{ByID: map[string]*ObjectFieldMeta{}}
			return
		}
		meta, err := ParseObjectMetadata(metaData)
		if err != nil {
			log.Printf("objects[%s]: parse %s: %v", cfg.Kind, cfg.MetaDataFile, err)
			cache.err = err
			cache.meta = &ObjectMetadata{ByID: map[string]*ObjectFieldMeta{}}
			return
		}
		cache.meta = meta
		log.Printf("objects[%s]: base loaded — %d rows, %d field metas",
			cfg.Kind, len(cache.base.Rows), len(cache.meta.Fields))
	})
	return cache.base, cache.meta, cache.err
}

// resetObjectBaseCacheForTest clears the lazy-load globals for one kind so
// a test can re-run loadObjectBase under different conditions (different
// asset reader, missing CASC, etc.). Production code never calls this.
//
// Pass "" to clear ALL kinds — useful for tests that touch the global state
// without binding to a specific kind.
func resetObjectBaseCacheForTest(kind string) {
	objectBaseMu.Lock()
	defer objectBaseMu.Unlock()
	if kind == "" {
		objectBaseMap = map[string]*objectBaseCache{}
		return
	}
	delete(objectBaseMap, kind)
}

// setObjectBaseForTest installs a pre-built base + metadata into the cache
// for the given kind, bypassing the sync.Once-guarded loader. Test-only
// helper used by stubUnitsBase + friends in objects_mods_test.go so tests
// don't have to drive the real CASC mount.
func setObjectBaseForTest(kind string, base *slk.Mapped, meta *ObjectMetadata) {
	cache := getObjectBaseCache(kind)
	// Consume the sync.Once with a no-op load so the public loadObjectBase
	// path returns our stub on every subsequent call (and so subsequent test
	// resets land in a known state).
	cache.once.Do(func() {})
	cache.base = base
	cache.meta = meta
	cache.err = nil
}

// MergedObject is one object after stacking base SLK + INI overlays + the
// loaded map's per-kind shadow. Kind-agnostic; the same struct serves units,
// items, doodads, etc. Phase 1b's MergedUnit type is now an alias of this
// so existing references compile unchanged.
type MergedObject struct {
	ID         string
	BaseID     string
	IsCustom   bool
	IsEdited   bool
	Fields     map[string]string
	Overridden map[string]bool
	// LevelFields holds per-level override values for leveled (opt-format)
	// kinds: column-name → (level → value). Empty for non-leveled objects.
	// The base/level-0 value stays in Fields; LevelFields carries level>0.
	LevelFields map[string]map[uint32]string
}

// MergedUnit is the legacy alias kept for backward compatibility with code
// that imports the type by name. Going forward, prefer MergedObject.
type MergedUnit = MergedObject

// MergedObjects returns the merged base+shadow view for every object of
// this kind. Generic over kind — pass UnitsConfig() to recover the old
// MergedUnits behavior bit-for-bit.
//
// The returned map is freshly built; callers can mutate it without affecting
// the cache.
func MergedObjects(cfg *KindConfig) (map[string]*MergedObject, *ObjectMetadata, error) {
	base, meta, err := loadObjectBase(cfg)
	if err != nil {
		return nil, meta, err
	}
	out := make(map[string]*MergedObject, len(base.Rows))

	// Layer 1: every stock row from the base.
	for id, row := range base.Rows {
		fields := make(map[string]string, len(row))
		for col, val := range row {
			fields[col] = val
		}
		out[id] = &MergedObject{
			ID:         id,
			Fields:     fields,
			Overridden: map[string]bool{},
		}
	}

	// Layer 2: per-map shadow from the kind's war3map.w3* file.
	Current.mu.RLock()
	mods := cfg.GetMods(Current)
	Current.mu.RUnlock()
	if mods != nil {
		for _, edit := range mods.OriginalEdits {
			u, ok := out[edit.BaseID]
			if !ok {
				// Original-table entry pointing at a base ID we don't know.
				// Fabricate a stub so the override is still visible.
				u = &MergedObject{
					ID:         edit.BaseID,
					Fields:     map[string]string{},
					Overridden: map[string]bool{},
				}
				out[edit.BaseID] = u
			}
			applyOverrides(u, edit.Overrides, meta)
			applyLevelOverrides(u, edit.Levels, meta)
		}
		for _, c := range mods.Customs {
			u := &MergedObject{
				ID:         c.ID,
				BaseID:     c.BaseID,
				IsCustom:   true,
				Fields:     map[string]string{},
				Overridden: map[string]bool{},
			}
			if baseRow := base.Rows[c.BaseID]; baseRow != nil {
				for col, val := range baseRow {
					u.Fields[col] = val
				}
			}
			applyOverrides(u, c.Overrides, meta)
			applyLevelOverrides(u, c.Levels, meta)
			out[c.ID] = u
		}
	}
	return out, meta, nil
}

// MergedUnits is the Phase-1b read entry point, preserved verbatim as a thin
// wrapper around the generic. Existing callers (the units-API + handlers)
// keep working.
func MergedUnits() (map[string]*MergedObject, *ObjectMetadata, error) {
	return MergedObjects(UnitsConfig())
}

// applyOverrides walks the FourCC-keyed Overrides map from w3objmod and
// writes the values into the merged object's column-name-keyed Fields map.
// Identical to Phase 1b — kept on the generic since every kind merges the
// same way (FourCC → metadata column lookup → write into Fields).
func applyOverrides(u *MergedObject, ov w3objmod.Overrides, meta *ObjectMetadata) {
	if len(ov) == 0 {
		return
	}
	u.IsEdited = true
	for fourCC, val := range ov {
		col := fourCC
		if m, ok := meta.ByID[fourCC]; ok {
			// Shared columns (Spell Book spb1..spb5 all on "Data") key by
			// FourCC so they don't collapse onto one slot; everything else
			// keys by the lowercased column name as before.
			col = meta.storeCol(fourCC, strings.ToLower(m.Field))
		} else if !looksLikeFourCC(fourCC) {
			col = strings.ToLower(fourCC)
		}
		u.Fields[col] = val
		u.Overridden[col] = true
	}
}

// applyLevelOverrides folds the shadow's per-level entries into the merged
// object's LevelFields (column-name → level → value). The base/level-0 slot is
// handled by applyOverrides; this only carries level>0 entries so the Object
// Editor can render and round-trip distinct per-level values for the same
// field. Marks the object edited.
func applyLevelOverrides(u *MergedObject, levels []w3objmod.LevelOverride, meta *ObjectMetadata) {
	if len(levels) == 0 {
		return
	}
	u.IsEdited = true
	if u.LevelFields == nil {
		u.LevelFields = map[string]map[uint32]string{}
	}
	for _, lo := range levels {
		col := lo.FourCC
		if m, ok := meta.ByID[lo.FourCC]; ok {
			// Same shared-column rule as applyOverrides so leveled fields that
			// share a column (rare, but symmetric) don't collapse either.
			col = meta.storeCol(lo.FourCC, strings.ToLower(m.Field))
		} else if !looksLikeFourCC(lo.FourCC) {
			col = strings.ToLower(lo.FourCC)
		}
		if u.LevelFields[col] == nil {
			u.LevelFields[col] = map[uint32]string{}
		}
		u.LevelFields[col][lo.Level] = lo.Value
	}
}

// looksLikeFourCC reports whether s is exactly 4 bytes of printable ASCII —
// a best-effort heuristic to distinguish raw modification-IDs from already-
// translated column names.
func looksLikeFourCC(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[i]
		if c < 0x21 || c > 0x7E {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Units kind — the Phase-1b behavior, expressed as a KindConfig.
// ---------------------------------------------------------------------------

var (
	unitsConfigOnce sync.Once
	unitsConfigCfg  *KindConfig
)

// UnitsConfig returns the KindConfig for units. Lazy-built singleton so the
// closures only allocate once. Phase 1b's hard-coded UnitData/unitSkin/etc.
// paths live inside this builder; the rest of the generic doesn't know
// they're units-specific.
func UnitsConfig() *KindConfig {
	unitsConfigOnce.Do(func() {
		unitsConfigCfg = &KindConfig{
			Kind:         "units",
			MetaDataFile: "Units/UnitMetaData.slk",
			BaseSLKs: []string{
				"Units/UnitData.slk",
				"Units/UnitBalance.slk",
				"Units/UnitWeapons.slk",
				"Units/UnitAbilities.slk",
				"Units/unitUI.slk",
			},
			BaseINIs: []string{
				// Reforged splits per-asset paths + display names into companion
				// .txt files. Without these, the `name` and `file` columns are
				// blank for every stock unit.
				"Units/unitSkin.txt",
				"Units/HumanUnitFunc.txt",
				"Units/HumanUnitStrings.txt",
				"Units/OrcUnitFunc.txt",
				"Units/OrcUnitStrings.txt",
				"Units/UndeadUnitFunc.txt",
				"Units/UndeadUnitStrings.txt",
				"Units/NightElfUnitFunc.txt",
				"Units/NightElfUnitStrings.txt",
				"Units/NeutralUnitFunc.txt",
				"Units/NeutralUnitStrings.txt",
				"Units/CampaignUnitFunc.txt",
				"Units/CampaignUnitStrings.txt",
			},
			ShadowFile: "war3map.w3u",
			ShadowOpt:  false,
			AppliesFn: func(f ObjectFieldMeta) bool {
				return f.AppliesToUnits()
			},
			ClassifyFn: classifyUnitKind,
			FilterListRow: func(id string, fields map[string]string) bool {
				// HiveWE's filter for the unit editor: row must have a `race`
				// column. Items share the metadata family but have no race,
				// so this cheaply separates them.
				return strings.TrimSpace(fields["race"]) != ""
			},
			CategoryFn: func(race, kind string) string {
				cat := titleRace(race)
				switch kind {
				case "hero":
					cat += " Hero"
				case "building":
					cat += " Building"
				case "special":
					cat += " Special"
				}
				return cat
			},
			IconArtFn:   unitIconArt,
			ModelPathFn: unitModelPath,
			GetMods:     func(s *Session) *w3objmod.File { return s.unitMods },
			SetMods:     func(s *Session, f *w3objmod.File) { s.unitMods = f },
			GetDirty:    func(s *Session) bool { return s.dirtyUnitMods },
			SetDirty:    func(s *Session, v bool) { s.dirtyUnitMods = v },
		}
		registerKindConfig(unitsConfigCfg)
	})
	return unitsConfigCfg
}

// classifyUnitKind mirrors HiveWE's UnitTreeModel::getFolderParent — order
// is special > building > hero > unit. Hero detection uses the WC3 FourCC
// convention: heroes have an uppercase first character (Hpal Paladin, Orkn
// Shadow Hunter, E000 custom hero), non-heroes start lowercase (hpea Peasant).
func classifyUnitKind(id string, fields map[string]string) string {
	if fields["special"] == "1" {
		return "special"
	}
	if fields["isbldg"] == "1" {
		return "building"
	}
	if id != "" && id[0] >= 'A' && id[0] <= 'Z' {
		return "hero"
	}
	return "unit"
}

// init prebuilds every KindConfig + registers it so commands that reference
// Kind="<x>" can resolve without an explicit XConfig() call. Each builder
// is a lazy sync.Once internally, so calling them here is the same as the
// first user access.
func init() {
	_ = UnitsConfig()
	_ = ItemsConfig()
	_ = AbilitiesConfig()
	_ = BuffsConfig()
	_ = DestructablesConfig()
	_ = DoodadsConfig()
	_ = UpgradesConfig()
}

// ---------------------------------------------------------------------------
// Items kind — w3t shadow, opt=false. Items share the units metadata SLK; the
// editor filters with AppliesToItems() (UseItem flag). The tree is grouped by
// item class ("Permanent" | "Charged" | …) from the `class` column.
// ---------------------------------------------------------------------------

var (
	itemsConfigOnce sync.Once
	itemsConfigCfg  *KindConfig
)

func ItemsConfig() *KindConfig {
	itemsConfigOnce.Do(func() {
		itemsConfigCfg = &KindConfig{
			Kind: "items",
			// Items have NO dedicated metadata SLK in WC3 — their editable
			// fields live in UnitMetaData.slk (shared with units), flagged by
			// the `useItem` column and selected via AppliesToItems() below.
			// Pointing this at a nonexistent "Units/ItemMetaData.slk" loaded
			// EMPTY metadata, so item field edits failed with "unknown field"
			// (listing still worked — that reads ItemData.slk). See AppliesFn.
			MetaDataFile: "Units/UnitMetaData.slk",
			BaseSLKs:     []string{"Units/ItemData.slk"},
			BaseINIs: []string{
				"Units/ItemFunc.txt",
				"Units/ItemStrings.txt",
				"Units/itemSkin.txt",
			},
			ShadowFile: "war3map.w3t",
			ShadowOpt:  false,
			AppliesFn: func(f ObjectFieldMeta) bool {
				return f.AppliesToItems()
			},
			ClassifyFn: func(id string, fields map[string]string) string {
				// HiveWE buckets the item editor by the `class` column: one of
				// Permanent / Charged / Campaign / Artifact / PowerUp /
				// Purchasable / Miscellaneous. We pass it through as the kind
				// tag; the JS layer titlecases for display.
				c := strings.TrimSpace(fields["class"])
				if c == "" {
					return "misc"
				}
				return strings.ToLower(c)
			},
			CategoryFn: func(race, kind string) string {
				if kind == "" {
					return "Miscellaneous"
				}
				return strings.Title(kind)
			},
			// Items have no `race` — drop FilterListRow (nil = include all).
			IconArtFn: unitIconArt,
			// Items don't have a 3D model preview (they're icons in the world);
			// leave ModelPathFn nil so the preview pane stays empty.
			GetMods:  func(s *Session) *w3objmod.File { return s.itemMods },
			SetMods:  func(s *Session, f *w3objmod.File) { s.itemMods = f },
			GetDirty: func(s *Session) bool { return s.dirtyItemMods },
			SetDirty: func(s *Session, v bool) { s.dirtyItemMods = v },
		}
		registerKindConfig(itemsConfigCfg)
	})
	return itemsConfigCfg
}

// ---------------------------------------------------------------------------
// Abilities kind — w3a shadow, opt=TRUE (per-modification level/dataPointer
// slots between type and value). Per-race base INIs span every WC3 race plus
// Campaign/Common/Item/Neutral/NightElf/Orc/Undead families.
// ---------------------------------------------------------------------------

var (
	abilitiesConfigOnce sync.Once
	abilitiesConfigCfg  *KindConfig
)

func AbilitiesConfig() *KindConfig {
	abilitiesConfigOnce.Do(func() {
		abilitiesConfigCfg = &KindConfig{
			Kind:         "abilities",
			MetaDataFile: "Units/AbilityMetaData.slk",
			BaseSLKs:     []string{"Units/AbilityData.slk"},
			BaseINIs: []string{
				"Units/HumanAbilityFunc.txt",
				"Units/HumanAbilityStrings.txt",
				"Units/OrcAbilityFunc.txt",
				"Units/OrcAbilityStrings.txt",
				"Units/UndeadAbilityFunc.txt",
				"Units/UndeadAbilityStrings.txt",
				"Units/NightElfAbilityFunc.txt",
				"Units/NightElfAbilityStrings.txt",
				"Units/NeutralAbilityFunc.txt",
				"Units/NeutralAbilityStrings.txt",
				"Units/CampaignAbilityFunc.txt",
				"Units/CampaignAbilityStrings.txt",
				"Units/CommonAbilityFunc.txt",
				"Units/CommonAbilityStrings.txt",
				"Units/ItemAbilityFunc.txt",
				"Units/ItemAbilityStrings.txt",
			},
			ShadowFile: "war3map.w3a",
			ShadowOpt:  true,
			AppliesFn: func(f ObjectFieldMeta) bool {
				// Ability metadata uses useAbility; if that's missing the
				// row never surfaces (silently filtered).
				return f.AppliesToAbilities()
			},
			ClassifyFn: classifyAbilityKind,
			CategoryFn: func(race, kind string) string {
				cat := titleRace(race)
				switch kind {
				case "hero":
					cat += " Hero Ability"
				case "item":
					cat += " Item Ability"
				case "unit":
					cat += " Unit Ability"
				}
				return cat
			},
			IconArtFn: unitIconArt,
			GetMods:   func(s *Session) *w3objmod.File { return s.abilityMods },
			SetMods:   func(s *Session, f *w3objmod.File) { s.abilityMods = f },
			GetDirty:  func(s *Session) bool { return s.dirtyAbilityMods },
			SetDirty:  func(s *Session, v bool) { s.dirtyAbilityMods = v },
		}
		registerKindConfig(abilitiesConfigCfg)
	})
	return abilitiesConfigCfg
}

// classifyAbilityKind buckets ability rows into "hero" | "item" | "unit"
// using per-ability usehero / useitem / useunit columns (NOT the metadata
// flags — those describe FIELD applicability; THESE columns on each ability
// row describe what kind of entity can carry the ability).
//
// Order: hero > item > unit. An ability marked both hero AND unit (rare but
// possible) shows under heroes. Falls back to "unit" when no flag matches.
func classifyAbilityKind(id string, fields map[string]string) string {
	if fields["usehero"] == "1" {
		return "hero"
	}
	if fields["useitem"] == "1" {
		return "item"
	}
	if fields["useunit"] == "1" {
		return "unit"
	}
	// HiveWE also falls back to the FourCC casing heuristic when the
	// columns are unset (custom abilities sometimes omit them) — uppercase
	// first char = hero. Matches the unit classifier.
	if id != "" && id[0] >= 'A' && id[0] <= 'Z' {
		return "hero"
	}
	return "unit"
}

// ---------------------------------------------------------------------------
// Buffs kind — w3h shadow, opt=false. Shares the per-race ability INIs (buffs
// and abilities live in the same .txt files in Reforged); the SLK + metadata
// are separate.
// ---------------------------------------------------------------------------

var (
	buffsConfigOnce sync.Once
	buffsConfigCfg  *KindConfig
)

func BuffsConfig() *KindConfig {
	buffsConfigOnce.Do(func() {
		buffsConfigCfg = &KindConfig{
			Kind:         "buffs",
			MetaDataFile: "Units/AbilityBuffMetaData.slk",
			BaseSLKs:     []string{"Units/AbilityBuffData.slk"},
			BaseINIs: []string{
				"Units/HumanAbilityFunc.txt",
				"Units/HumanAbilityStrings.txt",
				"Units/OrcAbilityFunc.txt",
				"Units/OrcAbilityStrings.txt",
				"Units/UndeadAbilityFunc.txt",
				"Units/UndeadAbilityStrings.txt",
				"Units/NightElfAbilityFunc.txt",
				"Units/NightElfAbilityStrings.txt",
				"Units/NeutralAbilityFunc.txt",
				"Units/NeutralAbilityStrings.txt",
				"Units/CampaignAbilityFunc.txt",
				"Units/CampaignAbilityStrings.txt",
				"Units/CommonAbilityFunc.txt",
				"Units/CommonAbilityStrings.txt",
				"Units/ItemAbilityFunc.txt",
				"Units/ItemAbilityStrings.txt",
			},
			ShadowFile: "war3map.w3h",
			ShadowOpt:  false,
			AppliesFn: func(f ObjectFieldMeta) bool {
				return f.AppliesToBuffs()
			},
			ClassifyFn: func(id string, fields map[string]string) string {
				// Buffs typically have a `race` column for grouping. We keep
				// the classification minimal — race already does the bucket
				// work; everything else collapses to "buff".
				return "buff"
			},
			CategoryFn: func(race, kind string) string {
				return titleRace(race)
			},
			IconArtFn: unitIconArt,
			GetMods:   func(s *Session) *w3objmod.File { return s.buffMods },
			SetMods:   func(s *Session, f *w3objmod.File) { s.buffMods = f },
			GetDirty:  func(s *Session) bool { return s.dirtyBuffMods },
			SetDirty:  func(s *Session, v bool) { s.dirtyBuffMods = v },
		}
		registerKindConfig(buffsConfigCfg)
	})
	return buffsConfigCfg
}

// ---------------------------------------------------------------------------
// Destructables kind — w3b shadow, opt=false. Tree-trunks, walls, bridges,
// chests. Grouped by the `category` column.
// ---------------------------------------------------------------------------

var (
	destructablesConfigOnce sync.Once
	destructablesConfigCfg  *KindConfig
)

func DestructablesConfig() *KindConfig {
	destructablesConfigOnce.Do(func() {
		destructablesConfigCfg = &KindConfig{
			Kind:         "destructables",
			MetaDataFile: "Units/DestructableMetaData.slk",
			BaseSLKs:     []string{"Units/DestructableData.slk"},
			BaseINIs: []string{
				"Units/DestructableSkin.txt",
			},
			ShadowFile: "war3map.w3b",
			ShadowOpt:  false,
			AppliesFn: func(f ObjectFieldMeta) bool {
				return f.AppliesToDestructables()
			},
			ClassifyFn: classifyByCategoryColumn,
			CategoryFn: categoryPassthrough,
			IconArtFn:  destructableIconArt,
			ModelPathFn: doodadModelPath,
			GetMods:    func(s *Session) *w3objmod.File { return s.destructibleMods },
			SetMods:    func(s *Session, f *w3objmod.File) { s.destructibleMods = f },
			GetDirty:   func(s *Session) bool { return s.dirtyDestructibleMods },
			SetDirty:   func(s *Session, v bool) { s.dirtyDestructibleMods = v },
		}
		registerKindConfig(destructablesConfigCfg)
	})
	return destructablesConfigCfg
}

// ---------------------------------------------------------------------------
// Doodads kind — w3d shadow, opt=TRUE (per-modification variation slot for
// random visual variants). Grouped by the `category` column.
// ---------------------------------------------------------------------------

var (
	doodadsConfigOnce sync.Once
	doodadsConfigCfg  *KindConfig
)

func DoodadsConfig() *KindConfig {
	doodadsConfigOnce.Do(func() {
		doodadsConfigCfg = &KindConfig{
			Kind:         "doodads",
			MetaDataFile: "Doodads/DoodadMetaData.slk",
			BaseSLKs:     []string{"Doodads/Doodads.slk"},
			BaseINIs: []string{
				"Doodads/doodadSkins.txt",
			},
			ShadowFile: "war3map.w3d",
			ShadowOpt:  true,
			AppliesFn: func(f ObjectFieldMeta) bool {
				return f.AppliesToDoodads()
			},
			ClassifyFn:  classifyByCategoryColumn,
			CategoryFn:  categoryPassthrough,
			IconArtFn:   destructableIconArt,
			ModelPathFn: doodadModelPath,
			GetMods:     func(s *Session) *w3objmod.File { return s.doodadMods },
			SetMods:     func(s *Session, f *w3objmod.File) { s.doodadMods = f },
			GetDirty:    func(s *Session) bool { return s.dirtyDoodadMods },
			SetDirty:    func(s *Session, v bool) { s.dirtyDoodadMods = v },
		}
		registerKindConfig(doodadsConfigCfg)
	})
	return doodadsConfigCfg
}

// ---------------------------------------------------------------------------
// Upgrades kind — w3q shadow, opt=TRUE (per-level fields). Grouped by race.
// ---------------------------------------------------------------------------

var (
	upgradesConfigOnce sync.Once
	upgradesConfigCfg  *KindConfig
)

func UpgradesConfig() *KindConfig {
	upgradesConfigOnce.Do(func() {
		upgradesConfigCfg = &KindConfig{
			Kind:         "upgrades",
			MetaDataFile: "Units/UpgradeMetaData.slk",
			BaseSLKs:     []string{"Units/UpgradeData.slk"},
			BaseINIs: []string{
				"Units/CampaignUpgradeFunc.txt",
				"Units/CampaignUpgradeStrings.txt",
				"Units/HumanUpgradeFunc.txt",
				"Units/HumanUpgradeStrings.txt",
				"Units/OrcUpgradeFunc.txt",
				"Units/OrcUpgradeStrings.txt",
				"Units/NightElfUpgradeFunc.txt",
				"Units/NightElfUpgradeStrings.txt",
				"Units/UndeadUpgradeFunc.txt",
				"Units/UndeadUpgradeStrings.txt",
				"Units/NeutralUpgradeFunc.txt",
				"Units/NeutralUpgradeStrings.txt",
			},
			ShadowFile: "war3map.w3q",
			ShadowOpt:  true,
			AppliesFn: func(f ObjectFieldMeta) bool {
				return f.AppliesToUpgrades()
			},
			ClassifyFn: func(id string, fields map[string]string) string {
				return "upgrade"
			},
			CategoryFn: func(race, kind string) string {
				return titleRace(race)
			},
			IconArtFn: unitIconArt,
			GetMods:   func(s *Session) *w3objmod.File { return s.upgradeMods },
			SetMods:   func(s *Session, f *w3objmod.File) { s.upgradeMods = f },
			GetDirty:  func(s *Session) bool { return s.dirtyUpgradeMods },
			SetDirty:  func(s *Session, v bool) { s.dirtyUpgradeMods = v },
		}
		registerKindConfig(upgradesConfigCfg)
	})
	return upgradesConfigCfg
}

// classifyByCategoryColumn is the shared ClassifyFn for kinds where the
// `category` SLK column is the tree-bucket label (doodads, destructables).
// Falls back to "misc" when the column is empty.
func classifyByCategoryColumn(id string, fields map[string]string) string {
	c := strings.TrimSpace(fields["category"])
	if c == "" {
		return "misc"
	}
	return c
}

// categoryPassthrough returns the kind tag verbatim (used for the human-
// readable category label when the kind tag IS already a human label).
// Caller passes "" race; the kind comes through as-is.
func categoryPassthrough(race, kind string) string {
	if kind == "" {
		return "Miscellaneous"
	}
	return kind
}

// destructableIconArt picks the doodad/destructable icon path. Same
// fallback chain as unitIconArt — `art` then `art:sd` then `art:hd` —
// because destructables/doodads use the same per-mode variant naming
// convention in their respective skin INIs.
func destructableIconArt(fields map[string]string) string {
	return unitIconArt(fields)
}

// doodadModelPath returns the .mdx + .mdl-fallback paths for doodads /
// destructables. Doodads.slk stores the model in the `file` column, with
// `file:hd`/`file:sd` per-mode variants in doodadSkins.txt.
//
// For doodads with multiple model variations (e.g. tree variants stored as
// "TreeA.mdx,TreeB.mdx" or as "Tree" stem + numbered files), we surface
// only the FIRST variant for the preview — the editor's preview pane is
// a single-model view, not a variant carousel.
func doodadModelPath(fields map[string]string) (string, []string) {
	for _, k := range []string{"file", "file:hd", "file:sd"} {
		v := strings.TrimSpace(fields[k])
		if v == "" {
			continue
		}
		// Doodad `file` columns sometimes carry a comma-separated list of
		// variant stems. Take the first one for the preview.
		if i := strings.IndexByte(v, ','); i >= 0 {
			v = strings.TrimSpace(v[:i])
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
