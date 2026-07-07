package forge

import (
	"strconv"
	"strings"

	"github.com/StephenSHorton/wc3-forge/internal/formats/slk"
	"github.com/StephenSHorton/wc3-forge/internal/formats/w3objmod"
)

// ObjectFieldMeta is one row of a per-kind MetaData.slk — the description of
// a single editable field on units, items, doodads, destructables, abilities,
// buffs, or upgrades. Each kind's MetaData.slk is laid out the same way (the
// SLK schema is identical across kinds); the use* columns determine which
// kinds the field applies to.
//
// Phase 2b: a single MetaData family (the units+items shared one) is no
// longer the only consumer — Doodad/Destructable/Ability/Buff/Upgrade
// metadata each have their own use* columns. We carry all of them on this
// struct; ParseObjectMetadata fills whatever's present in the SLK and zero-
// defaults the rest. Filter via the Applies* methods or directly through
// KindConfig.AppliesFn.
type ObjectFieldMeta struct {
	ID          string // FourCC of the field (the w3{u,t,b,d,h,a,q} modification_id)
	Field       string // SLK column name; matches slk.Mapped column-key after lowercasing
	SLK         string // source SLK family (Profile / UnitData / ItemData / DoodadData / DestructableData / AbilityData / BuffData / UpgradeData)
	Index       int    // display order within its category; -1 means "unindexed" (variant subfields)
	Category    string // "abil" | "art" | "combat" | "editor" | "move" | "path" | "sound" | "stats" | "tech" | "text"
	DisplayName string // WESTRING_* key; resolved at render time, not parse time
	Type        string // value type: "int" | "real" | "bool" | "string" | "abilityList" | "icon" | "model" | ...
	Data        int    // variant repeat count; >0 means the field is split into N synthetic variants (e.g. missileart→missileart,missileart2)
	MinVal      string // numeric constraints kept as strings — consumer parses on demand (and the column is often blank)
	MaxVal      string

	// NetSafe marks a field that Reforged's World Editor writes into the
	// war3mapSkin.w3* companion table instead of the plain war3map.w3* table.
	// It's the exact mechanism the vanilla editor uses to decide the split:
	// the MetaData.slk `netsafe` column is non-empty and not "0" for art/skin
	// fields (Name unam, Model File umdl, Icon uico, Tooltip utip, tint, …) and
	// "0"/blank for gameplay/logic fields. NOT the same as Category — e.g.
	// `unam` is Category "text" but netsafe (→ skin), while `ucbs` (combat
	// tinting-ignore) is Category "art" but netsafe 0 (→ main). Edit routing
	// keys on THIS, not Category. See routeSkinTable.
	NetSafe bool

	// Per-kind applicability flags. A single field row may carry several
	// (e.g. a font-color field could be shared across units AND items).
	// Each Applies* method ORs the relevant set so a kind's filter is one
	// boolean lookup.
	UseUnit        bool
	UseHero        bool
	UseBuilding    bool
	UseItem        bool
	UseAbility     bool
	UseBuff        bool
	UseDestructable bool
	UseDoodad      bool
	UseUpgrade     bool
}

// AppliesToUnits reports whether this field is editable on any unit-family
// kind (regular units, heroes, buildings). The units editor filters with this.
func (f ObjectFieldMeta) AppliesToUnits() bool {
	return f.UseUnit || f.UseHero || f.UseBuilding
}

// AppliesToItems reports whether this field is editable on items. Distinct
// from AppliesToUnits because the same field-metadata SLK feeds both, and the
// item editor wants only `UseItem == true` rows.
func (f ObjectFieldMeta) AppliesToItems() bool { return f.UseItem }

// AppliesToAbilities reports whether the field belongs in the ability editor.
func (f ObjectFieldMeta) AppliesToAbilities() bool { return f.UseAbility }

// AppliesToBuffs reports whether the field belongs in the buff editor.
func (f ObjectFieldMeta) AppliesToBuffs() bool { return f.UseBuff }

// AppliesToDestructables reports whether the field belongs in the
// destructable editor.
func (f ObjectFieldMeta) AppliesToDestructables() bool { return f.UseDestructable }

// AppliesToDoodads reports whether the field belongs in the doodad editor.
func (f ObjectFieldMeta) AppliesToDoodads() bool { return f.UseDoodad }

// AppliesToUpgrades reports whether the field belongs in the upgrade editor.
func (f ObjectFieldMeta) AppliesToUpgrades() bool { return f.UseUpgrade }

// UnitFieldMeta is the Phase-1b name retained as an alias of ObjectFieldMeta.
// Old call sites that reference the type by name keep compiling unchanged.
// New code should target ObjectFieldMeta directly.
type UnitFieldMeta = ObjectFieldMeta

// ObjectMetadata is a parsed per-kind MetaData.slk: every editable field for
// that kind, indexed for both ordered display (Fields, sorted by category
// then Index) and FourCC lookup (ByID).
type ObjectMetadata struct {
	Fields []ObjectFieldMeta
	ByID   map[string]*ObjectFieldMeta
	// SharedCols is the set of lowercased SLK column names ("field") that are
	// claimed by MORE THAN ONE FourCC in this kind's metadata. Spell Book
	// abilities are the canonical case: spb1..spb5 (Spell List / Shared
	// Cooldown / Min / Max / Base Order ID) are five DISTINCT FourCCs that all
	// share Field="Data". Keying the merged read view by column name collapses
	// them onto one slot (last write wins); the merge + get paths consult this
	// set and key shared columns by FourCC instead so each lands in its own
	// slot. Non-shared columns (name, hp, ...) stay column-name-keyed so the
	// frontend's existing field vocabulary is byte-for-byte unchanged.
	SharedCols map[string]bool
}

// UnitMetadata is the Phase-1b name retained as an alias of ObjectMetadata.
type UnitMetadata = ObjectMetadata

// storeCol resolves the map slot a field's value lives in within a
// MergedObject's column-name-keyed Fields / Overridden / LevelFields maps.
//
// Default keying is the lowercased SLK column ("name", "hp"). For a SHARED
// column — one claimed by multiple FourCCs (Spell Book spb1..spb5 all on
// "Data") — we key by the lowercased FourCC instead so the distinct fields
// don't collapse onto one slot. fourCC is the field's modification ID; col is
// its already-lowercased Field column. Both applyOverrides (merge) and
// getObject (read) call this so set + get + list + fields_meta agree.
func (m *ObjectMetadata) storeCol(fourCC, col string) string {
	if m != nil && m.SharedCols[col] {
		return strings.ToLower(fourCC)
	}
	return col
}

// FieldMap builds the FourCC → column-name map w3objmod.Parse needs to
// translate a war3map.w3* modification ID into SLK column-name space, so
// shadow overrides can be merged column-for-column with the base table.
//
// Lowercased to match slk.Mapped's column convention.
func (m *ObjectMetadata) FieldMap() w3objmod.FieldMap {
	fm := w3objmod.FieldMap{}
	for i := range m.Fields {
		f := &m.Fields[i]
		fm[f.ID] = strings.ToLower(f.Field)
	}
	return fm
}

// ParseObjectMetadata parses a per-kind MetaData.slk into ObjectMetadata.
// Kind-agnostic: it reads all known use* columns; columns the file doesn't
// have are zero-defaulted (slk.MappedRow returns "" for missing keys, which
// the "1" comparison rejects). This means UnitMetaData.slk (which has
// useUnit/useHero/useBuilding/useItem but NOT useAbility/useBuff/etc.)
// produces UseAbility=false naturally, with no per-kind branching here.
//
// One row per editable field. Row key is the field FourCC (also stored as
// the `id` column). Empty rows + the SLK header are handled by slk.Mapped;
// we just transform rows into ObjectFieldMeta records.
func ParseObjectMetadata(data []byte) (*ObjectMetadata, error) {
	m := slk.New()
	if err := m.Load(data); err != nil {
		return nil, err
	}
	out := &ObjectMetadata{
		Fields: make([]ObjectFieldMeta, 0, len(m.Rows)),
		ByID:   make(map[string]*ObjectFieldMeta, len(m.Rows)),
	}

	// Reforged's per-kind metadata SLKs DON'T all carry every applicability
	// column. AbilityMetaData.slk has no `useAbility`; UpgradeMetaData.slk,
	// AbilityBuffMetaData.slk, DestructableMetaData.slk and DoodadMetaData.slk
	// carry no use* columns at all. The whole SLK IS the field set for that
	// kind in those cases, so gating on a column that doesn't exist filtered
	// out EVERY field (proven live: objects.abilities.fields_meta=0, even
	// `name`/anam hidden). Detect whether each gating column is present
	// anywhere in the table; when it's absent we treat the dimension as
	// "applies to all" rather than over-filtering to nothing.
	hasCol := func(names ...string) bool {
		for _, row := range m.Rows {
			for _, n := range names {
				if _, ok := row[strings.ToLower(n)]; ok {
					return true
				}
			}
		}
		return false
	}
	hasAbility := hasCol("useability")
	hasUpgrade := hasCol("useupgrade", "useupgr")
	hasBuff := hasCol("usespecific", "usebuff")
	hasDestructable := hasCol("usedestructable", "usedest")
	hasDoodad := hasCol("usedoodad", "usedood")

	for key, row := range m.Rows {
		// Some SLK rows carry only an `id` column (no `field`) — skip those
		// as they can't be merged into a base table.
		field := row.String("field")
		if field == "" {
			continue
		}
		idx, _ := strconv.Atoi(strings.TrimSpace(row.String("index")))
		data, _ := strconv.Atoi(strings.TrimSpace(row.String("data")))
		out.Fields = append(out.Fields, ObjectFieldMeta{
			ID:          key,
			Field:       field,
			SLK:         row.String("slk"),
			Index:       idx,
			Category:    row.String("category"),
			DisplayName: row.String("displayname"),
			Type:        row.String("type"),
			Data:        data,
			MinVal:      row.String("minval"),
			MaxVal:      row.String("maxval"),
			// netsafe: non-empty and not "0" → the field lives in the Reforged
			// war3mapSkin.w3* companion. Absent column ⇒ every field false ⇒
			// edits route to the plain table (the pre-Reforged behavior), which
			// is a safe fallback since existing skin-table fields still route by
			// presence (see routeSkinTable).
			NetSafe: isNetSafe(row.String("netsafe")),
			UseUnit:     row.String("useunit") == "1",
			UseHero:     row.String("usehero") == "1",
			UseBuilding: row.String("usebuilding") == "1",
			UseItem:     row.String("useitem") == "1",
			// When the gating column is absent table-wide, the whole metadata
			// SLK is the field set for that kind — every row applies (relax
			// rather than over-filter to nothing). When the column IS present,
			// gate on it as before.
			UseAbility:      !hasAbility || row.String("useability") == "1",
			UseBuff:         !hasBuff || row.String("usespecific") == "1" || row.String("usebuff") == "1",
			UseDestructable: !hasDestructable || row.String("usedestructable") == "1" || row.String("usedest") == "1",
			UseDoodad:       !hasDoodad || row.String("usedoodad") == "1" || row.String("usedood") == "1",
			UseUpgrade:      !hasUpgrade || row.String("useupgrade") == "1" || row.String("useupgr") == "1",
		})
	}
	// Build the FourCC → *ObjectFieldMeta index. Pointers into the slice are
	// stable for the lifetime of this ObjectMetadata (we never mutate Fields
	// after construction).
	for i := range out.Fields {
		out.ByID[out.Fields[i].ID] = &out.Fields[i]
	}
	// Detect the shared columns once: count how many distinct FourCCs claim
	// each lowercased column name; any column claimed by >1 FourCC is shared
	// (e.g. "data" for Spell Book spb1..spb5). The merge + get read paths key
	// these by FourCC instead of column name so the fields don't collapse.
	colCount := make(map[string]int, len(out.Fields))
	for i := range out.Fields {
		colCount[strings.ToLower(out.Fields[i].Field)]++
	}
	out.SharedCols = map[string]bool{}
	for col, n := range colCount {
		if n > 1 {
			out.SharedCols[col] = true
		}
	}
	return out, nil
}

// isNetSafe reports whether a MetaData.slk `netsafe` cell marks a skin-table
// field. Reforged writes "1" for art/skin fields and "0" (or leaves it blank)
// for gameplay fields; any non-"0" non-empty value is treated as netsafe so a
// future "2"-style flag doesn't silently fall through to the plain table.
func isNetSafe(v string) bool {
	v = strings.TrimSpace(v)
	return v != "" && v != "0"
}

// ParseUnitMetadata is the Phase-1b name retained as a thin wrapper over the
// generic ParseObjectMetadata. Same behavior, same return shape — kept so
// existing call sites in handlers/tests keep compiling.
func ParseUnitMetadata(data []byte) (*ObjectMetadata, error) {
	return ParseObjectMetadata(data)
}
