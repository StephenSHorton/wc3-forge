package forge

import (
	"testing"

	"github.com/StephenSHorton/wc3-forge/internal/formats/slk"
	"github.com/StephenSHorton/wc3-forge/internal/formats/w3objmod"
)

// stubUnitsBaseWithModel installs a stock "hpea" carrying both a name and a
// model-file column, with unam/umdl/uhpm metadata bound so the merge can route
// each FourCC to its column. Mirrors stubUnitsBase but adds the `file` column
// the skin-overlay tests assert on.
func stubUnitsBaseWithModel(t *testing.T) {
	t.Helper()
	resetObjectBaseCacheForTest("units")
	prevReader := baseAssetReader
	baseAssetReader = nil
	t.Cleanup(func() {
		baseAssetReader = prevReader
		resetObjectBaseCacheForTest("units")
	})
	stubBase := slk.New()
	stubBase.Rows = map[string]slk.MappedRow{
		"hpea": {"name": "Peasant", "file": `units\Human\Peasant\Peasant`, "race": "human"},
	}
	meta := &UnitMetadata{
		Fields: []UnitFieldMeta{
			{ID: "unam", Field: "Name", Category: "text", Type: "string", UseUnit: true, UseHero: true, UseBuilding: true},
			{ID: "umdl", Field: "file", Category: "art", Type: "model", UseUnit: true, UseHero: true, UseBuilding: true},
			{ID: "uhpm", Field: "HP", Category: "stats", Type: "int", UseUnit: true, UseHero: true, UseBuilding: true},
		},
		ByID: map[string]*UnitFieldMeta{},
	}
	for i := range meta.Fields {
		meta.ByID[meta.Fields[i].ID] = &meta.Fields[i]
	}
	setObjectBaseForTest("units", stubBase, meta)
}

// TestMergedObjects_SkinOverlay_FillsNameAndModel reproduces the Reforged split
// that made every custom unit show as its base type: a custom unit's gameplay
// edit lives in war3map.w3u while its Name + Model File live only in
// war3mapSkin.w3u. The merge must surface the skin's name/model on top of the
// base while preserving the primary table's gameplay edit.
func TestMergedObjects_SkinOverlay_FillsNameAndModel(t *testing.T) {
	stubUnitsBaseWithModel(t)

	prevCurrent := Current
	t.Cleanup(func() { Current = prevCurrent })

	// Primary war3map.w3u: custom h000 (based on Peasant) carries only a
	// gameplay edit (HP) — exactly how Reforged authors the split.
	primary := &w3objmod.File{
		Version: 3,
		Customs: []w3objmod.CustomObject{
			{ID: "h000", BaseID: "hpea", Overrides: w3objmod.Overrides{"uhpm": "240"}},
		},
	}
	// war3mapSkin.w3u: the SAME custom carries the name + model overrides that
	// wc3-forge previously ignored.
	skin := &w3objmod.File{
		Version: 3,
		Customs: []w3objmod.CustomObject{
			{ID: "h000", BaseID: "hpea", Overrides: w3objmod.Overrides{
				"unam": "Sylvanas",
				"umdl": `units\Human\Sylvanas\Sylvanas`,
			}},
		},
	}
	Current = &Session{
		unitMods:       primary,
		objectSkinMods: map[string]*w3objmod.File{"units": skin},
	}

	out, _, err := MergedObjects(UnitsConfig())
	if err != nil {
		t.Fatalf("MergedObjects: %v", err)
	}
	u, ok := out["h000"]
	if !ok {
		t.Fatal("custom h000 missing from merge")
	}
	if !u.IsCustom || u.BaseID != "hpea" {
		t.Fatalf("identity wrong: IsCustom=%v BaseID=%q (want true/hpea)", u.IsCustom, u.BaseID)
	}
	if u.Fields["name"] != "Sylvanas" {
		t.Errorf("skin name not applied: got %q want %q", u.Fields["name"], "Sylvanas")
	}
	if got := u.Fields["file"]; got != `units\Human\Sylvanas\Sylvanas` {
		t.Errorf("skin model not applied: got %q", got)
	}
	if u.Fields["hp"] != "240" {
		t.Errorf("primary gameplay edit lost: got %q want 240", u.Fields["hp"])
	}
}

// TestMergedObjects_PrimaryWinsOverSkin locks in the precedence contract: when
// the same field is set in BOTH tables, the primary war3map.w3u wins and the
// skin only fills what the primary left unset. This keeps edits made in
// wc3-forge (which route to the primary table) from being masked by the skin.
func TestMergedObjects_PrimaryWinsOverSkin(t *testing.T) {
	stubUnitsBaseWithModel(t)

	prevCurrent := Current
	t.Cleanup(func() { Current = prevCurrent })

	primary := &w3objmod.File{
		Version: 3,
		Customs: []w3objmod.CustomObject{
			{ID: "h001", BaseID: "hpea", Overrides: w3objmod.Overrides{"unam": "PrimaryName"}},
		},
	}
	skin := &w3objmod.File{
		Version: 3,
		Customs: []w3objmod.CustomObject{
			{ID: "h001", BaseID: "hpea", Overrides: w3objmod.Overrides{
				"unam": "SkinName",
				"umdl": `units\Human\Sylvanas\Sylvanas`,
			}},
		},
	}
	Current = &Session{
		unitMods:       primary,
		objectSkinMods: map[string]*w3objmod.File{"units": skin},
	}

	out, _, err := MergedObjects(UnitsConfig())
	if err != nil {
		t.Fatalf("MergedObjects: %v", err)
	}
	u := out["h001"]
	if u == nil {
		t.Fatal("custom h001 missing from merge")
	}
	if u.Fields["name"] != "PrimaryName" {
		t.Errorf("primary should win on a shared field: got %q want PrimaryName", u.Fields["name"])
	}
	// The skin still fills the model the primary didn't set.
	if got := u.Fields["file"]; got != `units\Human\Sylvanas\Sylvanas` {
		t.Errorf("skin should fill the unset model: got %q", got)
	}
}
