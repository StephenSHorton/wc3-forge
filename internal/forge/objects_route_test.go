package forge

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/StephenSHorton/wc3-forge/internal/formats/slk"
	"github.com/StephenSHorton/wc3-forge/internal/formats/w3objmod"
)

// stubUnitsBaseNetSafe installs a stock "hpea" plus metadata whose art fields
// (unam Name, umdl Model File) are marked netsafe — i.e. Reforged writes them
// into the war3mapSkin.w3u companion — while the gameplay field (uhpm HP) is
// not. This is the fixture the edit-routing tests assert against: the same
// object, two destination tables split by NetSafe.
func stubUnitsBaseNetSafe(t *testing.T) {
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
			{ID: "unam", Field: "Name", Category: "text", Type: "string", NetSafe: true, UseUnit: true, UseHero: true, UseBuilding: true},
			{ID: "umdl", Field: "file", Category: "art", Type: "model", NetSafe: true, UseUnit: true, UseHero: true, UseBuilding: true},
			{ID: "uhpm", Field: "HP", Category: "stats", Type: "int", NetSafe: false, UseUnit: true, UseHero: true, UseBuilding: true},
		},
		ByID: map[string]*UnitFieldMeta{},
	}
	for i := range meta.Fields {
		meta.ByID[meta.Fields[i].ID] = &meta.Fields[i]
	}
	setObjectBaseForTest("units", stubBase, meta)
}

// TestSetObjectField_Routing_ByNetSafe is the core contract: a brand-new
// art/skin (netsafe) field edit lands in the war3mapSkin.w3u companion, while a
// gameplay field edit lands in the plain war3map.w3u — each returning the right
// `skin` flag so the caller flips the matching dirty bit.
func TestSetObjectField_Routing_ByNetSafe(t *testing.T) {
	stubUnitsBaseNetSafe(t)
	cfg := UnitsConfig()
	s := &Session{loaded: true}

	// Gameplay field → primary.
	_, _, skin, err := setObjectField(s, cfg, "hpea", "uhpm", "240")
	if err != nil {
		t.Fatalf("set uhpm: %v", err)
	}
	if skin {
		t.Fatal("gameplay field uhpm routed to skin table")
	}
	if s.unitMods == nil || len(s.unitMods.OriginalEdits) != 1 ||
		s.unitMods.OriginalEdits[0].Overrides["uhpm"] != "240" {
		t.Fatalf("uhpm not in primary war3map.w3u: %+v", s.unitMods)
	}
	if got := s.objectSkinMods["units"]; got != nil {
		t.Fatalf("gameplay edit leaked into skin table: %+v", got)
	}

	// Art/skin field → skin companion.
	_, _, skin, err = setObjectField(s, cfg, "hpea", "unam", "Sylvanas")
	if err != nil {
		t.Fatalf("set unam: %v", err)
	}
	if !skin {
		t.Fatal("netsafe field unam did NOT route to skin table")
	}
	sk := s.objectSkinMods["units"]
	if sk == nil || len(sk.OriginalEdits) != 1 || sk.OriginalEdits[0].Overrides["unam"] != "Sylvanas" {
		t.Fatalf("unam not in war3mapSkin.w3u: %+v", sk)
	}
	// The gameplay edit must still be the ONLY thing in the primary table.
	if _, has := s.unitMods.OriginalEdits[0].Overrides["unam"]; has {
		t.Error("unam leaked into the primary table")
	}
}

// TestSetObjectField_Routing_ExistingFieldStaysPut locks in the precedence of
// on-disk presence over netsafe: an art field that a map already carries in the
// PRIMARY table (the older all-in-one layout) is edited in place, NOT migrated
// to the skin companion. Migrating tables under the user would churn the diff
// and risk a double-write.
func TestSetObjectField_Routing_ExistingFieldStaysPut(t *testing.T) {
	stubUnitsBaseNetSafe(t)
	cfg := UnitsConfig()
	// Seed unam already present in the PRIMARY table for hpea.
	s := &Session{
		loaded: true,
		unitMods: &w3objmod.File{Version: 3, OriginalEdits: []w3objmod.OriginalEdit{
			{BaseID: "hpea", Overrides: w3objmod.Overrides{"unam": "OldName"}},
		}},
	}
	_, had, skin, err := setObjectField(s, cfg, "hpea", "unam", "NewName")
	if err != nil {
		t.Fatalf("set unam: %v", err)
	}
	if skin {
		t.Fatal("re-edit migrated an existing primary field into the skin table")
	}
	if !had {
		t.Fatal("expected had=true for a pre-existing override")
	}
	if s.unitMods.OriginalEdits[0].Overrides["unam"] != "NewName" {
		t.Errorf("primary unam not updated: %+v", s.unitMods.OriginalEdits[0].Overrides)
	}
	if s.objectSkinMods["units"] != nil {
		t.Errorf("skin table should stay empty: %+v", s.objectSkinMods["units"])
	}
}

// TestSetObjectField_Routing_CustomSkinRowCreated verifies a custom object's
// art edit materializes a matching Custom row (id + baseID) in the skin
// companion — mirroring how Reforged writes a custom across both tables — while
// its gameplay edit stays on the primary custom row.
func TestSetObjectField_Routing_CustomSkinRowCreated(t *testing.T) {
	stubUnitsBaseNetSafe(t)
	cfg := UnitsConfig()
	// Custom h000/hpea already exists in the primary table (AddCustomObject
	// puts it there) with no fields yet.
	s := &Session{
		loaded: true,
		unitMods: &w3objmod.File{Version: 3, Customs: []w3objmod.CustomObject{
			{ID: "h000", BaseID: "hpea", Overrides: w3objmod.Overrides{}},
		}},
	}

	// Gameplay edit → primary custom row.
	if _, _, skin, err := setObjectField(s, cfg, "h000", "uhpm", "300"); err != nil || skin {
		t.Fatalf("uhpm on custom: skin=%v err=%v", skin, err)
	}
	if s.unitMods.Customs[0].Overrides["uhpm"] != "300" {
		t.Fatalf("gameplay edit not on primary custom: %+v", s.unitMods.Customs[0])
	}

	// Art edit → new custom row in the skin companion, same identity.
	if _, _, skin, err := setObjectField(s, cfg, "h000", "umdl", `war3mapImported\Sylvanas`); err != nil || !skin {
		t.Fatalf("umdl on custom: skin=%v err=%v", skin, err)
	}
	sk := s.objectSkinMods["units"]
	if sk == nil || len(sk.Customs) != 1 {
		t.Fatalf("skin custom row not created: %+v", sk)
	}
	if sk.Customs[0].ID != "h000" || sk.Customs[0].BaseID != "hpea" {
		t.Errorf("skin custom identity wrong: %+v", sk.Customs[0])
	}
	if sk.Customs[0].Overrides["umdl"] != `war3mapImported\Sylvanas` {
		t.Errorf("skin custom model override wrong: %+v", sk.Customs[0].Overrides)
	}
	// The primary custom must NOT have picked up the art field.
	if _, has := s.unitMods.Customs[0].Overrides["umdl"]; has {
		t.Error("art field leaked onto the primary custom row")
	}
}

// TestClearObjectField_Routing_FromSkin verifies an undo of a skin-routed edit
// clears the override from the skin companion (returning skin=true) and drops
// the now-empty OriginalEdit row so a save doesn't ship a no-op.
func TestClearObjectField_Routing_FromSkin(t *testing.T) {
	stubUnitsBaseNetSafe(t)
	cfg := UnitsConfig()
	s := &Session{loaded: true}

	if _, _, _, err := setObjectField(s, cfg, "hpea", "unam", "Sylvanas"); err != nil {
		t.Fatalf("seed unam: %v", err)
	}
	skin, err := clearObjectField(s, cfg, "hpea", "unam")
	if err != nil {
		t.Fatalf("clear unam: %v", err)
	}
	if !skin {
		t.Fatal("clear reported the wrong table (want skin)")
	}
	if got := s.objectSkinMods["units"]; got == nil || len(got.OriginalEdits) != 0 {
		t.Fatalf("empty skin edit row not dropped: %+v", got)
	}
}

// TestSetObjectFieldLevel_Routing_NetSafe covers the leveled write path: a
// netsafe field edited at a level lands in the skin companion's Levels list.
// (Exercised at the low level so it doesn't need an opt-format kind wired
// through the public, ShadowOpt-gated entry point.)
func TestSetObjectFieldLevel_Routing_NetSafe(t *testing.T) {
	stubUnitsBaseNetSafe(t)
	cfg := UnitsConfig()
	s := &Session{loaded: true}

	_, _, skin, err := setObjectFieldLevel(s, cfg, "hpea", "unam", 2, "L2Name")
	if err != nil {
		t.Fatalf("set leveled unam: %v", err)
	}
	if !skin {
		t.Fatal("leveled netsafe field did not route to skin")
	}
	sk := s.objectSkinMods["units"]
	if sk == nil || len(sk.OriginalEdits) != 1 {
		t.Fatalf("skin leveled edit missing: %+v", sk)
	}
	if i := findLevelOverride(sk.OriginalEdits[0].Levels, "unam", 2); i < 0 ||
		sk.OriginalEdits[0].Levels[i].Value != "L2Name" {
		t.Fatalf("leveled override not in skin Levels: %+v", sk.OriginalEdits[0].Levels)
	}
	// A read must find it in the skin table (routing-agnostic read).
	if v, ok := readObjectFieldLevel(s, cfg, "hpea", "unam", 2); !ok || v != "L2Name" {
		t.Fatalf("readObjectFieldLevel missed the skin value: %q ok=%v", v, ok)
	}
}

// TestRemoveCustomObject_SkinMirrorCleanup verifies deleting a custom drops its
// mirror in the skin companion (so no orphaned art rows) and hands back the
// snapshot so a Revert restores BOTH tables.
func TestRemoveCustomObject_SkinMirrorCleanup(t *testing.T) {
	stubUnitsBaseNetSafe(t)
	cfg := UnitsConfig()
	s := &Session{
		loaded: true,
		unitMods: &w3objmod.File{Version: 3, Customs: []w3objmod.CustomObject{
			{ID: "h000", BaseID: "hpea", Overrides: w3objmod.Overrides{"uhpm": "300"}},
		}},
		objectSkinMods: map[string]*w3objmod.File{"units": {Version: 3, Customs: []w3objmod.CustomObject{
			{ID: "h000", BaseID: "hpea", Overrides: w3objmod.Overrides{"unam": "Sylvanas"}},
		}}},
	}

	saved, savedSkin, ok := removeCustomObject(s, cfg, "h000")
	if !ok {
		t.Fatal("removeCustomObject reported not-found")
	}
	if len(s.unitMods.Customs) != 0 {
		t.Errorf("primary custom not removed: %+v", s.unitMods.Customs)
	}
	if len(s.objectSkinMods["units"].Customs) != 0 {
		t.Errorf("skin mirror not removed: %+v", s.objectSkinMods["units"].Customs)
	}
	if savedSkin == nil || savedSkin.Overrides["unam"] != "Sylvanas" {
		t.Fatalf("skin snapshot not returned for undo: %+v", savedSkin)
	}

	// Revert restores both tables verbatim.
	reinsertCustomObject(s, cfg, saved, savedSkin)
	if len(s.unitMods.Customs) != 1 || s.unitMods.Customs[0].Overrides["uhpm"] != "300" {
		t.Errorf("primary custom not restored: %+v", s.unitMods.Customs)
	}
	if len(s.objectSkinMods["units"].Customs) != 1 ||
		s.objectSkinMods["units"].Customs[0].Overrides["unam"] != "Sylvanas" {
		t.Errorf("skin mirror not restored: %+v", s.objectSkinMods["units"].Customs)
	}
}

// TestClearObjectField_Routing_DropsEmptySkinCustomRow guards the review's
// Bug 1: undoing an art edit on a CUSTOM object must not leave a phantom empty
// Custom row in the skin companion (which would bump the file off the lossless
// copy-through). A PRIMARY custom row, by contrast, is the object definition
// and must survive an empty-override state.
func TestClearObjectField_Routing_DropsEmptySkinCustomRow(t *testing.T) {
	stubUnitsBaseNetSafe(t)
	cfg := UnitsConfig()
	// Custom h000 exists only in the primary table (AddCustomObject).
	s := &Session{
		loaded: true,
		unitMods: &w3objmod.File{Version: 3, Customs: []w3objmod.CustomObject{
			{ID: "h000", BaseID: "hpea", Overrides: w3objmod.Overrides{}},
		}},
	}
	// Set a netsafe art field → creates a skin custom mirror row.
	if _, _, skin, err := setObjectField(s, cfg, "h000", "unam", "Sylvanas"); err != nil || !skin {
		t.Fatalf("seed skin custom: skin=%v err=%v", skin, err)
	}
	if len(s.objectSkinMods["units"].Customs) != 1 {
		t.Fatalf("expected 1 skin custom, got %+v", s.objectSkinMods["units"])
	}
	// Undo (clear, hadOverride was false) → the emptied skin custom row must go.
	if _, err := clearObjectField(s, cfg, "h000", "unam"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := len(s.objectSkinMods["units"].Customs); got != 0 {
		t.Errorf("phantom empty skin custom row not dropped: %d remain (%+v)",
			got, s.objectSkinMods["units"].Customs)
	}
	// The primary custom (the object's definition) must NOT have been dropped
	// even though its overrides are empty.
	if len(s.unitMods.Customs) != 1 || s.unitMods.Customs[0].ID != "h000" {
		t.Errorf("primary custom definition wrongly dropped: %+v", s.unitMods.Customs)
	}
}

// TestSetObjectFieldLevel_NoopRead_MatchesWriteTable guards the review's Bug 2:
// the leveled no-op read must consult the same table the write routes to. We
// construct the pathological split — the same fourCC leveled in BOTH tables at
// different levels — and confirm a level-3 edit is NOT wrongly suppressed as a
// no-op by reading level 3 out of the wrong table.
func TestSetObjectFieldLevel_NoopRead_MatchesWriteTable(t *testing.T) {
	stubUnitsBaseNetSafe(t)
	cfg := UnitsConfig()
	// primary holds unam@2; skin holds unam@3 (a legacy/multi-tool split).
	s := &Session{
		loaded: true,
		unitMods: &w3objmod.File{Version: 3, OriginalEdits: []w3objmod.OriginalEdit{
			{BaseID: "hpea", Levels: []w3objmod.LevelOverride{{FourCC: "unam", Level: 2, Value: "P2"}}},
		}},
		objectSkinMods: map[string]*w3objmod.File{"units": {Version: 3, OriginalEdits: []w3objmod.OriginalEdit{
			{BaseID: "hpea", Levels: []w3objmod.LevelOverride{{FourCC: "unam", Level: 3, Value: "S3"}}},
		}}},
	}
	// routeSkinTable is per-fourCC: unam is present in primary (via @2) → routes
	// to primary. The no-op read must therefore look at primary's level 3 (absent
	// → not a no-op), NOT skin's "S3".
	skin := routeSkinTable(s, cfg, "hpea", "unam", mustMeta(t, cfg))
	if skin {
		t.Fatal("routeSkinTable should route unam to primary (present there via level 2)")
	}
	prev, had := readFileFieldLevel(s.routedReadFile(cfg, skin), "hpea", "unam", 3)
	if had {
		t.Errorf("no-op read consulted the wrong table: found level-3 %q in the routed (primary) table", prev)
	}
}

// mustMeta fetches the (stubbed) metadata for a kind or fails the test.
func mustMeta(t *testing.T, cfg *KindConfig) *ObjectMetadata {
	t.Helper()
	_, meta, err := loadObjectBase(cfg)
	if err != nil || meta == nil {
		t.Fatalf("loadObjectBase(%s): meta=%v err=%v", cfg.Kind, meta, err)
	}
	return meta
}

// TestSetUnitField_NetSafe_SaveRoundTrip is the end-to-end public-API contract:
// editing a netsafe field dirties ONLY the skin companion, Save writes
// war3mapSkin.w3u to the map, and a reopen surfaces the override from the skin
// table (never the primary). Fixture-gated on the shared w3i like the sibling
// round-trip test.
func TestSetUnitField_NetSafe_SaveRoundTrip(t *testing.T) {
	stubUnitsBaseNetSafe(t)
	tmp := minimalFolderMap(t)

	s := &Session{}
	if err := s.Open(tmp); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.IsDirty() {
		t.Fatal("freshly-opened session dirty")
	}

	if err := s.SetUnitField("hpea", "unam", "Sylvanas"); err != nil {
		t.Fatalf("SetUnitField: %v", err)
	}
	if !s.IsDirty() {
		t.Fatal("expected dirty after netsafe edit")
	}
	// Only the skin table should be dirty; the primary must stay clean so its
	// lossless copy-through is untouched.
	if s.dirtyUnitMods {
		t.Error("primary war3map.w3u wrongly marked dirty by a netsafe edit")
	}
	if !s.skinDirtyLocked("units") {
		t.Error("war3mapSkin.w3u not marked dirty by a netsafe edit")
	}
	if s.UnitMods() != nil {
		t.Errorf("netsafe edit allocated a primary shadow: %+v", s.UnitMods())
	}

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "war3mapSkin.w3u")); err != nil {
		t.Fatalf("war3mapSkin.w3u not written: %v", err)
	}
	if s.IsDirty() {
		t.Fatal("expected clean after Save")
	}

	s2 := &Session{}
	if err := s2.Open(tmp); err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	sk := s2.UnitSkinMods()
	if sk == nil || len(sk.OriginalEdits) != 1 || sk.OriginalEdits[0].Overrides["unam"] != "Sylvanas" {
		t.Fatalf("skin override didn't persist: %+v", sk)
	}
	if s2.UnitMods() != nil && len(s2.UnitMods().OriginalEdits) != 0 {
		t.Errorf("netsafe edit polluted the primary table: %+v", s2.UnitMods())
	}
}

// TestSetUnitField_NetSafe_UndoRedo exercises the full command replay through
// the public API: an art edit routes to skin, undo clears it (skin clean +
// primary still clean), redo restores it to the skin table. Guards against a
// replay that silently migrates the field between tables.
func TestSetUnitField_NetSafe_UndoRedo(t *testing.T) {
	stubUnitsBaseNetSafe(t)
	tmp := minimalFolderMap(t)

	s := &Session{}
	if err := s.Open(tmp); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.SetUnitField("hpea", "unam", "Sylvanas"); err != nil {
		t.Fatalf("SetUnitField: %v", err)
	}

	if err := s.Undo(); err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if v, ok := readBaseOverride(s.UnitSkinMods(), "hpea", "unam"); ok {
		t.Fatalf("undo left the skin override in place: %q", v)
	}
	if s.dirtyUnitMods {
		t.Error("undo wrongly dirtied the primary table")
	}

	if err := s.Redo(); err != nil {
		t.Fatalf("Redo: %v", err)
	}
	sk := s.UnitSkinMods()
	if v, ok := readBaseOverride(sk, "hpea", "unam"); !ok || v != "Sylvanas" {
		t.Fatalf("redo didn't restore the skin override: %q ok=%v", v, ok)
	}
	if s.UnitMods() != nil && len(s.UnitMods().OriginalEdits) != 0 {
		t.Errorf("redo migrated the field into the primary table: %+v", s.UnitMods())
	}
}
