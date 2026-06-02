package forge

import (
	"testing"

	"github.com/StephenSHorton/wc3-forge/internal/formats/slk"
	"github.com/StephenSHorton/wc3-forge/internal/formats/w3objmod"
)

// Spell Book field-collapse regression (Fix 1). The base Spell Book ability
// (Aspb) carries five DISTINCT data FourCCs — spb1 (Spell List), spb2 (Shared
// Spell Cooldown), spb3 (Min Spells), spb4 (Max Spells), spb5 (Base Order ID)
// — that ALL share the SLK column name "Data". The merged read view used to
// key by column name, so setting spb1/spb3/spb4 collapsed them onto one slot
// (last write wins) and a read-back showed every data field equal to the last
// value. The fix keys SHARED columns by FourCC so the five fields stay distinct
// in the merged/get view while every single-data column (name, etc.) is
// byte-identical to before.

// sampleSpellBookMeta is a Spell-Book-shaped AbilityMetaData.slk: five DISTINCT
// FourCCs (spb1..spb5) all on Field="Data", plus a normal `name`/anam field on
// its own column. Sourced through ParseObjectMetadata the same way production
// is, so SharedCols ("data") is detected exactly as it would be live. The SLK
// has no `useAbility` column (matches real Reforged), exercising the same
// applies-relaxation path the other ability tests use.
const sampleSpellBookMeta = `ID;PWXL;N;E
B;X5;Y6;D0
C;Y1
C;X1;K"id"
C;X2;K"field"
C;X3;K"category"
C;X4;K"displayName"
C;X5;K"type"
C;Y2
C;X1;K"anam"
C;X2;K"Name"
C;X3;K"text"
C;X4;K"WESTRING_AE_NAME"
C;X5;K"string"
C;Y3
C;X1;K"spb1"
C;X2;K"Data"
C;X3;K"data"
C;X4;K"WESTRING_AEVAL_SPB1"
C;X5;K"abilityList"
C;Y4
C;X1;K"spb2"
C;X2;K"Data"
C;X3;K"data"
C;X4;K"WESTRING_AEVAL_SPB2"
C;X5;K"bool"
C;Y5
C;X1;K"spb3"
C;X2;K"Data"
C;X3;K"data"
C;X4;K"WESTRING_AEVAL_SPB3"
C;X5;K"int"
C;Y6
C;X1;K"spb4"
C;X2;K"Data"
C;X3;K"data"
C;X4;K"WESTRING_AEVAL_SPB4"
C;X5;K"int"
E
`

// TestParseObjectMetadata_DetectsSharedColumns is the unit-level guard: the
// shared column ("data", claimed by spb1..spb4) must be flagged in SharedCols
// while the single-owner column ("name") must NOT be. This is the one-time
// detection that the merge + read paths consult.
func TestParseObjectMetadata_DetectsSharedColumns(t *testing.T) {
	meta, err := ParseObjectMetadata([]byte(sampleSpellBookMeta))
	if err != nil {
		t.Fatalf("ParseObjectMetadata: %v", err)
	}
	if !meta.SharedCols["data"] {
		t.Error(`"data" should be flagged shared (spb1..spb4 all key it)`)
	}
	if meta.SharedCols["name"] {
		t.Error(`"name" is owned by a single FourCC; must NOT be shared`)
	}
	// storeCol must route shared fields to the FourCC and leave others alone.
	if got := meta.storeCol("spb3", "data"); got != "spb3" {
		t.Errorf(`storeCol("spb3","data") = %q, want "spb3"`, got)
	}
	if got := meta.storeCol("anam", "name"); got != "name" {
		t.Errorf(`storeCol("anam","name") = %q, want "name" (single-owner unchanged)`, got)
	}
}

// stubSpellBookBase installs a synthetic abilities base with one stock Spell
// Book row ("Aspb") and the Spell-Book-shaped metadata above, sourced through
// ParseObjectMetadata so SharedCols is populated exactly as in production.
func stubSpellBookBase(t *testing.T) *KindConfig {
	t.Helper()
	resetObjectBaseCacheForTest("abilities")
	prevReader := baseAssetReader
	baseAssetReader = nil
	t.Cleanup(func() {
		baseAssetReader = prevReader
		resetObjectBaseCacheForTest("abilities")
	})
	stubBase := slk.New()
	stubBase.Rows = map[string]slk.MappedRow{
		"Aspb": {"name": "Spell Book", "race": "other"},
	}
	meta, err := ParseObjectMetadata([]byte(sampleSpellBookMeta))
	if err != nil {
		t.Fatalf("parse stub spell-book meta: %v", err)
	}
	setObjectBaseForTest("abilities", stubBase, meta)
	return AbilitiesConfig()
}

// TestSpellBook_DataFieldsDoNotCollapse is the Fix-1 end-to-end regression. It
// drives the SAME code path objects_abilities_set_field uses (Current.
// SetObjectField) to set spb1/spb2/spb3/spb4 to four DISTINCT values, then reads
// them back through getObject (the merged/get path) and asserts all four come
// back DISTINCT — not collapsed onto the last write. It also asserts a normal
// single-data field (name/anam) round-trips unchanged.
func TestSpellBook_DataFieldsDoNotCollapse(t *testing.T) {
	cfg := stubSpellBookBase(t)
	tmp := minimalFolderMap(t)
	prevCurrent := Current
	Current = &Session{}
	t.Cleanup(func() {
		Current.Close()
		Current = prevCurrent
	})
	if err := Current.Open(tmp); err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Distinct values for the four shared "Data" fields + a normal name field.
	// Pass FourCCs explicitly (the wire surface accepts FourCC or column name);
	// the shared column name "Data" would be ambiguous on the write side, so
	// real callers address shared fields by FourCC — mirror that here.
	writes := []struct{ fourCC, value string }{
		{"spb1", "AHbz,AHwe"}, // Spell List
		{"spb2", "1"},         // Shared Spell Cooldown
		{"spb3", "2"},         // Min Spells
		{"spb4", "5"},         // Max Spells
		{"anam", "My Book"},   // normal single-data field
	}
	for _, w := range writes {
		if err := Current.SetObjectField(cfg, "Aspb", w.fourCC, w.value); err != nil {
			t.Fatalf("SetObjectField %s=%q: %v", w.fourCC, w.value, err)
		}
	}

	// Read back through getObject — the merged/get path that collapsed before.
	got, err := getObject(cfg, "Aspb")
	if err != nil {
		t.Fatalf("getObject: %v", err)
	}
	byID := map[string]*objectsUnitsField{}
	for i := range got.Fields {
		byID[got.Fields[i].ID] = &got.Fields[i]
	}

	// All four shared fields must read back as their OWN distinct value.
	want := map[string]string{
		"spb1": "AHbz,AHwe",
		"spb2": "1",
		"spb3": "2",
		"spb4": "5",
	}
	for _, id := range []string{"spb1", "spb2", "spb3", "spb4"} {
		row := byID[id]
		if row == nil {
			t.Fatalf("field %s missing from getObject result", id)
		}
		if row.Value != want[id] {
			t.Errorf("field %s = %q, want %q (data fields collapsed?)", id, row.Value, want[id])
		}
		if !row.Overridden {
			t.Errorf("field %s should be marked overridden", id)
		}
	}
	// Guard against the original symptom directly: no two of the four may share
	// a value (they're all distinct above, so a collapse would equalize them).
	if byID["spb1"].Value == byID["spb4"].Value {
		t.Error("spb1 and spb4 collapsed to the same value (Fix 1 regressed)")
	}

	// The normal single-data field must round-trip unchanged.
	if byID["anam"] == nil || byID["anam"].Value != "My Book" {
		t.Errorf("name field = %+v, want value \"My Book\"", byID["anam"])
	}

	// Store-layer assertion: the on-disk shadow keeps all five overrides as
	// DISTINCT FourCC entries (the shadow was always correct; this guards that
	// the read fix didn't perturb the write path).
	mods := cfg.GetMods(Current)
	if mods == nil || len(mods.OriginalEdits) != 1 {
		t.Fatalf("expected 1 OriginalEdit on the shadow, got %+v", mods)
	}
	ov := mods.OriginalEdits[0].Overrides
	for id, v := range want {
		if ov[id] != v {
			t.Errorf("shadow override %s = %q, want %q", id, ov[id], v)
		}
	}
}

// TestApplyOverrides_SharedColumnKeyedByFourCC is the direct unit-level proof
// that applyOverrides routes shared columns to FourCC slots while keeping a
// unique-column field on its column name. Uses a hand-built ObjectMetadata
// (two FourCCs sharing one Field + one unique-Field FourCC) so it doesn't need
// a session or CASC mount.
func TestApplyOverrides_SharedColumnKeyedByFourCC(t *testing.T) {
	meta := &ObjectMetadata{
		Fields: []ObjectFieldMeta{
			{ID: "spb1", Field: "Data"},
			{ID: "spb3", Field: "Data"},
			{ID: "anam", Field: "Name"},
		},
		ByID:       map[string]*ObjectFieldMeta{},
		SharedCols: map[string]bool{"data": true},
	}
	for i := range meta.Fields {
		meta.ByID[meta.Fields[i].ID] = &meta.Fields[i]
	}

	u := &MergedObject{
		ID:         "Aspb",
		Fields:     map[string]string{},
		Overridden: map[string]bool{},
	}
	applyOverrides(u, w3objmod.Overrides{
		"spb1": "list",
		"spb3": "3",
		"anam": "Book",
	}, meta)

	// Shared fields land in distinct FourCC slots, not one "data" slot.
	if u.Fields["spb1"] != "list" {
		t.Errorf(`u.Fields["spb1"] = %q, want "list"`, u.Fields["spb1"])
	}
	if u.Fields["spb3"] != "3" {
		t.Errorf(`u.Fields["spb3"] = %q, want "3"`, u.Fields["spb3"])
	}
	if _, collapsed := u.Fields["data"]; collapsed {
		t.Error(`shared fields collapsed into a single "data" slot`)
	}
	// The unique-column field stays keyed by its column name.
	if u.Fields["name"] != "Book" {
		t.Errorf(`u.Fields["name"] = %q, want "Book" (unique column must stay column-keyed)`, u.Fields["name"])
	}
}
