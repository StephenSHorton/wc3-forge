package forge

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/StephenSHorton/wc3-forge/internal/formats/unitsdoo"
	"github.com/StephenSHorton/wc3-forge/internal/formats/w3i"
)

// TestMoveUnit_Save_RoundTrip is the end-to-end smoke test for the save slice:
// open a folder-backed map, mutate one unit's position via MoveUnit, save,
// reopen the map, and confirm the new position survived. Also verifies dirty
// listeners fire on the right transitions.
func TestMoveUnit_Save_RoundTrip(t *testing.T) {
	// Copy the committed extracted-map fixture to a tempdir so we don't disturb
	// the source.
	tmp := copyCommittedMapFixture(t)

	// Open via a fresh Session (avoid the global to keep this test isolated).
	s := &Session{}
	var dirtyHistory []bool
	s.OnDirtyChanged(func(d bool) { dirtyHistory = append(dirtyHistory, d) })
	if err := s.Open(tmp); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.IsDirty() {
		t.Errorf("freshly-opened session is dirty")
	}

	// Capture original first unit's creation_number + position.
	units := s.Units()
	if units == nil || len(units.Entities) == 0 {
		t.Fatalf("expected entities in fixture, got none")
	}
	first := units.Entities[0]
	cn := first.CreationNumber
	origPos := first.Position

	// Move by +100 on X.
	newX := origPos[0] + 100
	if err := s.MoveUnit(cn, newX, origPos[1], origPos[2]); err != nil {
		t.Fatalf("MoveUnit: %v", err)
	}
	if !s.IsDirty() {
		t.Errorf("expected dirty after MoveUnit")
	}

	// Save and verify clean.
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if s.IsDirty() {
		t.Errorf("expected clean after Save")
	}

	// Dirty history should include a true transition then a false transition.
	if len(dirtyHistory) < 2 {
		t.Errorf("expected at least 2 dirty events, got %d: %v", len(dirtyHistory), dirtyHistory)
	} else {
		if !dirtyHistory[0] {
			t.Errorf("first dirty event should be true, got %v", dirtyHistory[0])
		}
		if dirtyHistory[len(dirtyHistory)-1] {
			t.Errorf("last dirty event should be false, got %v", dirtyHistory[len(dirtyHistory)-1])
		}
	}

	// Now reopen and confirm position survived on disk.
	s2 := &Session{}
	if err := s2.Open(tmp); err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	var got *unitsdoo.Entity
	for i := range s2.Units().Entities {
		if s2.Units().Entities[i].CreationNumber == cn {
			got = &s2.Units().Entities[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("entity %d disappeared on re-open", cn)
	}
	if got.Position[0] != newX || got.Position[1] != origPos[1] || got.Position[2] != origPos[2] {
		t.Errorf("position after reopen = %v, want (%v, %v, %v)",
			got.Position, newX, origPos[1], origPos[2])
	}
}

// TestMoveUnit_NoOpDoesNotDirty asserts that MoveUnit to the unit's current
// position is a no-op — no dirty flip, no listener fire. The Properties panel
// commits position edits on blur/Enter even when the value didn't actually
// change (e.g. user clicked into the field and Escape-blurred without typing),
// so without this short-circuit the Save pill flips to amber for no real edit.
func TestMoveUnit_NoOpDoesNotDirty(t *testing.T) {
	tmp := copyCommittedMapFixture(t)
	s := &Session{}
	var dirtyHistory []bool
	s.OnDirtyChanged(func(d bool) { dirtyHistory = append(dirtyHistory, d) })
	if err := s.Open(tmp); err != nil {
		t.Fatalf("Open: %v", err)
	}
	units := s.Units()
	if units == nil || len(units.Entities) == 0 {
		t.Fatalf("expected entities in fixture, got none")
	}
	first := units.Entities[0]
	cn := first.CreationNumber
	pos := first.Position

	// Same-position move should not flip dirty.
	if err := s.MoveUnit(cn, pos[0], pos[1], pos[2]); err != nil {
		t.Fatalf("MoveUnit (no-op): %v", err)
	}
	if s.IsDirty() {
		t.Errorf("same-position MoveUnit flipped dirty")
	}
	if len(dirtyHistory) != 0 {
		t.Errorf("same-position MoveUnit fired %d dirty events, want 0: %v",
			len(dirtyHistory), dirtyHistory)
	}

	// Sanity: a real move still flips dirty.
	if err := s.MoveUnit(cn, pos[0]+1, pos[1], pos[2]); err != nil {
		t.Fatalf("MoveUnit (real): %v", err)
	}
	if !s.IsDirty() {
		t.Errorf("real MoveUnit failed to flip dirty")
	}
	if len(dirtyHistory) != 1 || !dirtyHistory[0] {
		t.Errorf("real MoveUnit dirty history = %v, want [true]", dirtyHistory)
	}
}

// TestMoveUnit_UnknownCN asserts MoveUnit surfaces a clear error for a
// creation_number that doesn't exist (and doesn't mark the session dirty).
func TestMoveUnit_UnknownCN(t *testing.T) {
	tmp := copyCommittedMapFixture(t)
	s := &Session{}
	if err := s.Open(tmp); err != nil {
		t.Fatalf("Open: %v", err)
	}
	err := s.MoveUnit(99999, 0, 0, 0)
	if err == nil {
		t.Fatal("expected error for unknown creation_number")
	}
	if s.IsDirty() {
		t.Errorf("session went dirty on failed MoveUnit")
	}
}

// TestMoveUnit_FiresEntityChanged asserts MoveUnit notifies OnEntityChanged
// subscribers with a "position" Field and the new coordinates baked into the
// payload. Bridge-driven and UI-driven move paths both flow through this
// event so the Properties panel + 3D scene can repaint without polling.
//
// Also verifies the no-op short-circuit does NOT fire entity-changed (no
// real mutation, no need to repaint).
func TestMoveUnit_FiresEntityChanged(t *testing.T) {
	tmp := copyCommittedMapFixture(t)
	s := &Session{}
	var changes []EntityChange
	s.OnEntityChanged(func(c EntityChange) { changes = append(changes, c) })
	if err := s.Open(tmp); err != nil {
		t.Fatalf("Open: %v", err)
	}
	units := s.Units()
	if units == nil || len(units.Entities) == 0 {
		t.Fatalf("expected entities in fixture, got none")
	}
	first := units.Entities[0]
	cn := first.CreationNumber
	pos := first.Position

	// Real move → expect exactly one entity-changed event with the new
	// position and the right kind/id/field tags.
	newPos := [3]float32{pos[0] + 50, pos[1] - 25, pos[2]}
	if err := s.MoveUnit(cn, newPos[0], newPos[1], newPos[2]); err != nil {
		t.Fatalf("MoveUnit: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("expected 1 entity-change event, got %d: %+v", len(changes), changes)
	}
	got := changes[0]
	if got.Kind != "unit" {
		t.Errorf("Kind = %q, want %q", got.Kind, "unit")
	}
	if got.ID != cn {
		t.Errorf("ID = %d, want %d", got.ID, cn)
	}
	if got.Field != "position" {
		t.Errorf("Field = %q, want %q", got.Field, "position")
	}
	if got.Position != newPos {
		t.Errorf("Position = %v, want %v", got.Position, newPos)
	}

	// Same-position move (no-op short-circuit) → no additional event.
	if err := s.MoveUnit(cn, newPos[0], newPos[1], newPos[2]); err != nil {
		t.Fatalf("MoveUnit (no-op): %v", err)
	}
	if len(changes) != 1 {
		t.Errorf("no-op MoveUnit fired extra entity-change event(s): %+v", changes)
	}

	// Different move → second event.
	if err := s.MoveUnit(cn, newPos[0]+10, newPos[1], newPos[2]); err != nil {
		t.Fatalf("MoveUnit (second real): %v", err)
	}
	if len(changes) != 2 {
		t.Errorf("expected 2 entity-change events after second real move, got %d", len(changes))
	}
}

// TestMutateInfo_Save_RoundTrip is the end-to-end smoke test for the w3i
// save slice: open a folder-backed map, mutate Info.Name via MutateInfo,
// save, reopen, confirm the new name survived. Mirrors the MoveUnit save
// round-trip shape — the dirtyInfo flag, MutateInfo path, and Save's
// w3i.Encode dispatch all light up here.
func TestMutateInfo_Save_RoundTrip(t *testing.T) {
	tmp := copyCommittedMapFixture(t)

	s := &Session{}
	var dirtyHistory []bool
	s.OnDirtyChanged(func(d bool) { dirtyHistory = append(dirtyHistory, d) })
	if err := s.Open(tmp); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.IsDirty() {
		t.Errorf("freshly-opened session is dirty")
	}

	const newName = "___wc3-forge-test-name___"
	if err := s.MutateInfo(func(info *w3i.Info) {
		info.Name = newName
	}); err != nil {
		t.Fatalf("MutateInfo: %v", err)
	}
	if !s.IsDirty() {
		t.Errorf("expected dirty after MutateInfo")
	}

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if s.IsDirty() {
		t.Errorf("expected clean after Save")
	}

	// Dirty history should have at least the rising + falling transitions.
	if len(dirtyHistory) < 2 || !dirtyHistory[0] || dirtyHistory[len(dirtyHistory)-1] {
		t.Errorf("expected dirty history starting true and ending false, got %v", dirtyHistory)
	}

	// Re-open and confirm the new name survived on disk.
	s2 := &Session{}
	if err := s2.Open(tmp); err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	if got := s2.Info().Name; got != newName {
		t.Errorf("post-reopen Info.Name = %q, want %q", got, newName)
	}
}

// TestMutateInfo_NoMapLoaded asserts MutateInfo surfaces a clear error
// when no map is loaded (rather than panicking on the nil Info pointer).
func TestMutateInfo_NoMapLoaded(t *testing.T) {
	s := &Session{}
	err := s.MutateInfo(func(info *w3i.Info) {
		info.Name = "should not be called"
	})
	if err == nil {
		t.Fatal("expected error from MutateInfo on unloaded session")
	}
}

// TestApplyInfoUpdates_KnownKeys asserts the shared ApplyInfoUpdates walker
// applies each documented key with the right type, ignores unknowns, and
// returns the correct changed-field count for the MCP map.info_set response.
func TestApplyInfoUpdates_KnownKeys(t *testing.T) {
	info := &w3i.Info{}
	updates := map[string]any{
		"name":             "Test Name",
		"author":           "Test Author",
		"description":      "Test Description",
		"suggestedPlayers": "2v2",
		"lua":              true,
		"unknown_field":    "ignored",
		"name_typo_ish":    "also ignored",
	}
	changed := ApplyInfoUpdates(info, updates)
	if changed != 5 {
		t.Errorf("changed = %d, want 5 (5 known keys; 2 unknowns ignored)", changed)
	}
	if info.Name != "Test Name" {
		t.Errorf("Name = %q", info.Name)
	}
	if info.Author != "Test Author" {
		t.Errorf("Author = %q", info.Author)
	}
	if info.Description != "Test Description" {
		t.Errorf("Description = %q", info.Description)
	}
	if info.SuggestedPlayers != "2v2" {
		t.Errorf("SuggestedPlayers = %q", info.SuggestedPlayers)
	}
	if !info.Lua {
		t.Errorf("Lua = %v, want true", info.Lua)
	}
}

// TestApplyInfoUpdates_TypeMismatch asserts wrong-type values for known keys
// are silently skipped (not counted in changed, not assigned). Matches the
// permissive "make progress" contract documented on ApplyInfoUpdates.
func TestApplyInfoUpdates_TypeMismatch(t *testing.T) {
	info := &w3i.Info{Name: "original"}
	updates := map[string]any{
		"name": 42,     // wrong type — number for string field
		"lua":  "true", // wrong type — string for bool field
	}
	changed := ApplyInfoUpdates(info, updates)
	if changed != 0 {
		t.Errorf("changed = %d, want 0 (both type-mismatched)", changed)
	}
	if info.Name != "original" {
		t.Errorf("Name mutated under type mismatch: %q", info.Name)
	}
}

// TestApplyInfoUpdates_OptionFlagBits asserts the option-tab checkboxes flip
// the right bits of Flags.Raw AND that the named-bool mirror gets re-derived.
// Encoders read Raw only, but Info consumers reading the bool view (e.g. the
// JS infoToPending mapping) would otherwise see a stale value.
func TestApplyInfoUpdates_OptionFlagBits(t *testing.T) {
	info := &w3i.Info{}
	updates := map[string]any{
		"meleeMap":                   true,
		"hideMinimapPreview":         true,
		"maskedAreaPartiallyVisible": true,
		"cliffShoreWaves":            true,
		"rollingShoreWaves":          true,
		"itemClassification":         true,
		"waterTinting":               true,
		"forceDefaultZoom":           true,
		"forceMaxZoom":               true,
		"forceMinZoom":               true,
	}
	changed := ApplyInfoUpdates(info, updates)
	if changed != len(updates) {
		t.Errorf("changed = %d, want %d", changed, len(updates))
	}
	// Spot-check Raw bits.
	wantBits := uint32(0x000004 | 0x000001 | 0x000010 | 0x000800 | 0x001000 |
		0x008000 | 0x010000 | 0x100000 | 0x200000 | 0x400000)
	if info.Flags.Raw != wantBits {
		t.Errorf("Flags.Raw = %#x, want %#x", info.Flags.Raw, wantBits)
	}
	// Named mirror must match — proves the DecodeFlags re-derivation fired.
	if !info.Flags.MeleeMap || !info.Flags.ForceMinZoom || !info.Flags.WaterTinting {
		t.Errorf("named-bool mirror not re-derived from Raw: %+v", info.Flags)
	}
	// Flipping off the bit should clear both Raw and the mirror.
	off := ApplyInfoUpdates(info, map[string]any{"meleeMap": false})
	if off != 1 {
		t.Errorf("clear changed = %d, want 1", off)
	}
	if info.Flags.Raw&0x000004 != 0 {
		t.Errorf("Flags.Raw still has melee bit set: %#x", info.Flags.Raw)
	}
	if info.Flags.MeleeMap {
		t.Errorf("Flags.MeleeMap still true after clearing")
	}
}

// TestApplyInfoUpdates_FogAndWater asserts the float/int/color wire shapes
// are coerced correctly. JSON-decoded numbers come in as float64; the walker
// must accept that without forcing every UI to send a typed value.
func TestApplyInfoUpdates_FogAndWater(t *testing.T) {
	info := &w3i.Info{Fog: w3i.Fog{Color: w3i.Color{A: 200}}}
	updates := map[string]any{
		"fogStyle":   float64(2),
		"fogStartZ":  float64(1500),
		"fogEndZ":    float64(8000.5),
		"fogDensity": float64(0.42),
		"fogColor":   map[string]any{"r": float64(255), "g": float64(64), "b": float64(0)},
		"waterColor": map[string]any{"r": float64(10), "g": float64(20), "b": float64(30), "a": float64(180)},
	}
	changed := ApplyInfoUpdates(info, updates)
	if changed != 6 {
		t.Errorf("changed = %d, want 6", changed)
	}
	if info.Fog.Style != 2 {
		t.Errorf("Fog.Style = %d, want 2", info.Fog.Style)
	}
	if info.Fog.StartZ != 1500 {
		t.Errorf("Fog.StartZ = %v, want 1500", info.Fog.StartZ)
	}
	if info.Fog.EndZ != 8000.5 {
		t.Errorf("Fog.EndZ = %v, want 8000.5", info.Fog.EndZ)
	}
	if info.Fog.Density != 0.42 {
		t.Errorf("Fog.Density = %v, want 0.42", info.Fog.Density)
	}
	// Fog color: alpha not supplied in updates, must preserve prior value (200).
	if info.Fog.Color != (w3i.Color{R: 255, G: 64, B: 0, A: 200}) {
		t.Errorf("Fog.Color = %+v", info.Fog.Color)
	}
	// Water color: explicit alpha=180 wins.
	if info.WaterColor != (w3i.Color{R: 10, G: 20, B: 30, A: 180}) {
		t.Errorf("WaterColor = %+v", info.WaterColor)
	}
}

// TestApplyInfoUpdates_WeatherIDPacking asserts FourCC strings round-trip to
// the packed int32 the on-disk format uses, matching HiveWE's reinterpret_cast
// trick. Empty string clears the field (= "no weather override").
func TestApplyInfoUpdates_WeatherIDPacking(t *testing.T) {
	info := &w3i.Info{}
	ApplyInfoUpdates(info, map[string]any{"weatherID": "RAhr"})
	wantInt := int32(uint32('R') | uint32('A')<<8 | uint32('h')<<16 | uint32('r')<<24)
	if info.WeatherID != wantInt {
		t.Errorf("WeatherID = %d, want %d", info.WeatherID, wantInt)
	}
	if Int32ToFourCC(info.WeatherID) != "RAhr" {
		t.Errorf("Int32ToFourCC round-trip = %q, want %q", Int32ToFourCC(info.WeatherID), "RAhr")
	}
	// Clear.
	ApplyInfoUpdates(info, map[string]any{"weatherID": ""})
	if info.WeatherID != 0 {
		t.Errorf("WeatherID after clear = %d, want 0", info.WeatherID)
	}
	if Int32ToFourCC(0) != "" {
		t.Errorf("Int32ToFourCC(0) = %q, want empty", Int32ToFourCC(0))
	}
}

// TestApplyInfoUpdates_CameraComplements asserts the {left,right,bottom,top}
// wire shape maps to IVec4{A,B,C,D} exactly the way the parser stored them.
func TestApplyInfoUpdates_CameraComplements(t *testing.T) {
	info := &w3i.Info{}
	updates := map[string]any{
		"cameraComplements": map[string]any{
			"left":   float64(7),
			"right":  float64(8),
			"bottom": float64(5),
			"top":    float64(9),
		},
	}
	changed := ApplyInfoUpdates(info, updates)
	if changed != 1 {
		t.Errorf("changed = %d, want 1", changed)
	}
	want := w3i.IVec4{A: 7, B: 8, C: 5, D: 9}
	if info.CameraComplements != want {
		t.Errorf("CameraComplements = %+v, want %+v", info.CameraComplements, want)
	}
}

// TestApplyInfoUpdates_LoadingScreen asserts the three loading-screen modes
// (default/campaign/imported) round-trip through the wire-key contract.
func TestApplyInfoUpdates_LoadingScreen(t *testing.T) {
	info := &w3i.Info{}
	// Mode = campaign.
	ApplyInfoUpdates(info, map[string]any{
		"loadingScreenNumber":   float64(3),
		"loadingScreenModel":    "",
		"loadingScreenTitle":    "My Map",
		"loadingScreenSubtitle": "A subtitle",
		"loadingScreenText":     "Lorem ipsum.",
	})
	if info.LoadingScreen.Number != 3 {
		t.Errorf("Number = %d, want 3", info.LoadingScreen.Number)
	}
	if info.LoadingScreen.Model != "" {
		t.Errorf("Model = %q, want empty (campaign mode)", info.LoadingScreen.Model)
	}
	if info.LoadingScreen.Title != "My Map" {
		t.Errorf("Title = %q", info.LoadingScreen.Title)
	}
	// Switch to imported mode.
	ApplyInfoUpdates(info, map[string]any{
		"loadingScreenNumber": float64(-1),
		"loadingScreenModel":  "LoadingScreen\\Custom.mdx",
	})
	if info.LoadingScreen.Number != -1 {
		t.Errorf("Number = %d, want -1", info.LoadingScreen.Number)
	}
	if info.LoadingScreen.Model != "LoadingScreen\\Custom.mdx" {
		t.Errorf("Model = %q", info.LoadingScreen.Model)
	}
}

// TestApplyInfoUpdates_AdvancedFields asserts the Reforged-tail Advanced-tab
// fields (camera distances, supported modes, game data version) coerce
// uint32-shaped JSON values.
func TestApplyInfoUpdates_AdvancedFields(t *testing.T) {
	info := &w3i.Info{}
	updates := map[string]any{
		"gameDataSet":        float64(2),
		"supportedModes":     float64(3), // SD + HD bits
		"gameDataVersion":    float64(10000),
		"camDistanceDefault": float64(1650),
		"camDistanceMax":     float64(3000),
		"camDistanceMin":     float64(800),
	}
	changed := ApplyInfoUpdates(info, updates)
	if changed != len(updates) {
		t.Errorf("changed = %d, want %d", changed, len(updates))
	}
	if info.GameDataSet != 2 || info.SupportedModes != 3 || info.GameDataVersion != 10000 {
		t.Errorf("Advanced int fields = %d/%d/%d", info.GameDataSet, info.SupportedModes, info.GameDataVersion)
	}
	if info.CamDistance != (w3i.CamDistance{Default: 1650, Max: 3000, Min: 800}) {
		t.Errorf("CamDistance = %+v", info.CamDistance)
	}
}

// TestApplyInfoUpdates_CustomLightTileset asserts the single-char tileset
// code wire form ("L", "A", "") matches the on-disk byte field.
func TestApplyInfoUpdates_CustomLightTileset(t *testing.T) {
	info := &w3i.Info{CustomLightTileset: 'X'}
	ApplyInfoUpdates(info, map[string]any{"customLightTileset": "L"})
	if info.CustomLightTileset != 'L' {
		t.Errorf("CustomLightTileset = %q, want 'L'", string(info.CustomLightTileset))
	}
	ApplyInfoUpdates(info, map[string]any{"customLightTileset": ""})
	if info.CustomLightTileset != 0 {
		t.Errorf("CustomLightTileset after clear = %d, want 0", info.CustomLightTileset)
	}
}

// TestRotateUnit_Save_RoundTrip mirrors TestMoveUnit_Save_RoundTrip but for
// rotation. Open, mutate Rotation, save, reopen, assert the new angle survived.
func TestRotateUnit_Save_RoundTrip(t *testing.T) {
	tmp := copyCommittedMapFixture(t)

	s := &Session{}
	var dirtyHistory []bool
	s.OnDirtyChanged(func(d bool) { dirtyHistory = append(dirtyHistory, d) })
	if err := s.Open(tmp); err != nil {
		t.Fatalf("Open: %v", err)
	}

	units := s.Units()
	if units == nil || len(units.Entities) == 0 {
		t.Fatalf("expected entities in fixture, got none")
	}
	first := units.Entities[0]
	cn := first.CreationNumber
	origRot := first.Rotation

	newRot := origRot + 1.5707963 // +90° in radians
	if err := s.RotateUnit(cn, newRot); err != nil {
		t.Fatalf("RotateUnit: %v", err)
	}
	if !s.IsDirty() {
		t.Errorf("expected dirty after RotateUnit")
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if s.IsDirty() {
		t.Errorf("expected clean after Save")
	}
	if len(dirtyHistory) < 2 || !dirtyHistory[0] || dirtyHistory[len(dirtyHistory)-1] {
		t.Errorf("dirty history = %v, want [true, ... false]", dirtyHistory)
	}

	s2 := &Session{}
	if err := s2.Open(tmp); err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	var got *unitsdoo.Entity
	for i := range s2.Units().Entities {
		if s2.Units().Entities[i].CreationNumber == cn {
			got = &s2.Units().Entities[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("entity %d disappeared on re-open", cn)
	}
	if got.Rotation != newRot {
		t.Errorf("Rotation after reopen = %v, want %v", got.Rotation, newRot)
	}
}

// TestScaleUnit_Save_RoundTrip mirrors TestMoveUnit_Save_RoundTrip but for
// scale. The key assertion is that scaleRaw invalidation works end-to-end:
// the re-opened entity must carry the NEW scale, not the old on-disk bits.
func TestScaleUnit_Save_RoundTrip(t *testing.T) {
	tmp := copyCommittedMapFixture(t)

	s := &Session{}
	var dirtyHistory []bool
	s.OnDirtyChanged(func(d bool) { dirtyHistory = append(dirtyHistory, d) })
	if err := s.Open(tmp); err != nil {
		t.Fatalf("Open: %v", err)
	}

	units := s.Units()
	if units == nil || len(units.Entities) == 0 {
		t.Fatalf("expected entities in fixture, got none")
	}
	first := units.Entities[0]
	cn := first.CreationNumber

	newScale := [3]float32{2.0, 2.0, 2.0}
	if err := s.ScaleUnit(cn, 2.0, 2.0, 2.0); err != nil {
		t.Fatalf("ScaleUnit: %v", err)
	}
	if !s.IsDirty() {
		t.Errorf("expected dirty after ScaleUnit")
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if s.IsDirty() {
		t.Errorf("expected clean after Save")
	}
	if len(dirtyHistory) < 2 || !dirtyHistory[0] || dirtyHistory[len(dirtyHistory)-1] {
		t.Errorf("dirty history = %v, want [true, ... false]", dirtyHistory)
	}

	// Re-open — the critical assertion for the scaleRaw gotcha.
	s2 := &Session{}
	if err := s2.Open(tmp); err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	var got *unitsdoo.Entity
	for i := range s2.Units().Entities {
		if s2.Units().Entities[i].CreationNumber == cn {
			got = &s2.Units().Entities[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("entity %d disappeared on re-open", cn)
	}
	// This is the scaleRaw-invalidation regression assertion: if ClearScaleRaw
	// was NOT called in ScaleUnit, Encode would preserve the old raw bits and
	// the re-parsed Scale would NOT equal newScale.
	if got.Scale != newScale {
		t.Errorf("Scale after reopen = %v, want %v (scaleRaw invalidation may be broken)", got.Scale, newScale)
	}
}

// TestRotateUnit_NoOp_DoesNotDirty asserts same-value RotateUnit is a no-op.
func TestRotateUnit_NoOp_DoesNotDirty(t *testing.T) {
	tmp := copyCommittedMapFixture(t)
	s := &Session{}
	var dirtyHistory []bool
	s.OnDirtyChanged(func(d bool) { dirtyHistory = append(dirtyHistory, d) })
	if err := s.Open(tmp); err != nil {
		t.Fatalf("Open: %v", err)
	}
	units := s.Units()
	if units == nil || len(units.Entities) == 0 {
		t.Fatalf("expected entities")
	}
	cn := units.Entities[0].CreationNumber
	rot := units.Entities[0].Rotation

	if err := s.RotateUnit(cn, rot); err != nil {
		t.Fatalf("RotateUnit (no-op): %v", err)
	}
	if s.IsDirty() {
		t.Errorf("same-rotation RotateUnit flipped dirty")
	}
	if len(dirtyHistory) != 0 {
		t.Errorf("same-rotation RotateUnit fired %d dirty events", len(dirtyHistory))
	}
}

// TestRotateUnit_FiresEntityChanged asserts RotateUnit emits an entity-changed
// event with Field="rotation" and the correct Rotation value in the payload.
func TestRotateUnit_FiresEntityChanged(t *testing.T) {
	tmp := copyCommittedMapFixture(t)
	s := &Session{}
	var changes []EntityChange
	s.OnEntityChanged(func(c EntityChange) { changes = append(changes, c) })
	if err := s.Open(tmp); err != nil {
		t.Fatalf("Open: %v", err)
	}
	units := s.Units()
	if units == nil || len(units.Entities) == 0 {
		t.Fatalf("expected entities")
	}
	cn := units.Entities[0].CreationNumber
	rot := units.Entities[0].Rotation
	newRot := rot + 0.5

	if err := s.RotateUnit(cn, newRot); err != nil {
		t.Fatalf("RotateUnit: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("expected 1 entity-change event, got %d", len(changes))
	}
	got := changes[0]
	if got.Kind != "unit" {
		t.Errorf("Kind = %q, want unit", got.Kind)
	}
	if got.Field != "rotation" {
		t.Errorf("Field = %q, want rotation", got.Field)
	}
	if got.Rotation != newRot {
		t.Errorf("Rotation = %v, want %v", got.Rotation, newRot)
	}
}

// TestScaleUnit_FiresEntityChanged asserts ScaleUnit emits an entity-changed
// event with Field="scale" and the correct Scale values in the payload.
func TestScaleUnit_FiresEntityChanged(t *testing.T) {
	tmp := copyCommittedMapFixture(t)
	s := &Session{}
	var changes []EntityChange
	s.OnEntityChanged(func(c EntityChange) { changes = append(changes, c) })
	if err := s.Open(tmp); err != nil {
		t.Fatalf("Open: %v", err)
	}
	units := s.Units()
	if units == nil || len(units.Entities) == 0 {
		t.Fatalf("expected entities")
	}
	cn := units.Entities[0].CreationNumber
	newScale := [3]float32{1.5, 1.5, 1.5}

	if err := s.ScaleUnit(cn, 1.5, 1.5, 1.5); err != nil {
		t.Fatalf("ScaleUnit: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("expected 1 entity-change event, got %d", len(changes))
	}
	got := changes[0]
	if got.Kind != "unit" {
		t.Errorf("Kind = %q, want unit", got.Kind)
	}
	if got.Field != "scale" {
		t.Errorf("Field = %q, want scale", got.Field)
	}
	if got.Scale != newScale {
		t.Errorf("Scale = %v, want %v", got.Scale, newScale)
	}
}

// TestMPQ_WriteBuffersThenFlushFails asserts the new MPQ write semantics:
// write() now BUFFERS (returns nil) instead of failing eagerly, and the
// failure surfaces only at flush() time when the source can't actually repack
// (no path / no open archive). This documents the boundary where the writer
// degrades gracefully rather than corrupting anything.
func TestMPQ_WriteBuffersThenFlushFails(t *testing.T) {
	src := newMPQSource(nil, "") // no archive, no path

	// write buffers without error.
	if err := src.write("war3mapUnits.doo", []byte("buffered")); err != nil {
		t.Fatalf("write should buffer, got error: %v", err)
	}
	// The buffered bytes are visible to a subsequent read.
	got, ok, err := src.read("war3mapUnits.doo")
	if err != nil || !ok {
		t.Fatalf("read after write: ok=%v err=%v", ok, err)
	}
	if string(got) != "buffered" {
		t.Errorf("read = %q, want %q", got, "buffered")
	}
	// flush can't repack (no path) so it returns an errors.Is-checkable error
	// rather than silently dropping the data.
	if err := src.flush(); !errors.Is(err, ErrMPQRepackFailed) {
		t.Errorf("flush with no path: want ErrMPQRepackFailed, got %v", err)
	}
}

// TestFolderSource_WriteSafety asserts the folderSource path-traversal guard
// rejects names that try to escape the map root.
func TestFolderSource_WriteSafety(t *testing.T) {
	tmp := t.TempDir()
	fs := folderSource{root: tmp}
	cases := []string{
		`..\war3map.w3i`,
		`..`,
		`C:\Windows\System32\evil.dll`,
		`subdir\..\..\evil`,
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			if err := fs.write(name, []byte("x")); err == nil {
				t.Errorf("write: expected error for path %q, got nil", name)
			}
			if _, _, err := fs.read(name); err == nil {
				t.Errorf("read: expected error for path %q, got nil", name)
			}
			if err := fs.delete(name); err == nil {
				t.Errorf("delete: expected error for path %q, got nil", name)
			}
		})
	}
	// Sanity: a normal name works.
	if err := fs.write("war3mapUnits.doo", []byte("x")); err != nil {
		t.Errorf("unexpected error for normal name: %v", err)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(tmp, "war3mapUnits.doo")), []byte("x")) {
		t.Errorf("write did not produce expected contents")
	}
}

// TestFolderSource_BackslashPathRoundTrip is the Darwin CI regression: WC3
// archive paths use backslashes, write() already FromSlash'd them into a real
// subdirectory, but read()/delete() used filepath.Join on the raw name.
// On POSIX that looks for a single file whose name contains `\`, so
// AddImport/ImportModel succeeded and ListImports/Rename/ReadFile reported
// the file missing.
func TestFolderSource_BackslashPathRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	fs := folderSource{root: tmp}
	name := `war3mapImported\foo.txt`
	if err := fs.write(name, []byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	want := filepath.Join(tmp, "war3mapImported", "foo.txt")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected OS path %s after write: %v", want, err)
	}
	got, ok, err := fs.read(name)
	if err != nil || !ok {
		t.Fatalf("read(%q): ok=%v err=%v (POSIX Join-without-FromSlash looks for a literal-backslash filename)", name, ok, err)
	}
	if string(got) != "hi" {
		t.Fatalf("read bytes = %q, want %q", got, "hi")
	}
	if err := fs.delete(name); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("delete left file at %s: %v", want, err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// TestSwapTileset_Save_RoundTrip exercises the tileset-swap save path: load a
// real Reforged map, swap its palettes to a tiny synthetic 2-tile set,
// remapping every old tile to one of the two new slots, save, reopen, and
// confirm both .w3e and .w3i picked up the new tileset letter + palette and
// every Tilepoint.GroundTexture is in the new range.
func TestSwapTileset_Save_RoundTrip(t *testing.T) {
	tmp := copyCommittedMapFixture(t)

	s := &Session{}
	if err := s.Open(tmp); err != nil {
		t.Fatalf("Open: %v", err)
	}

	oldGroundN := len(s.Terrain().GroundTilesets)
	oldCliffN := len(s.Terrain().CliffTilesets)
	if oldGroundN == 0 {
		t.Fatalf("fixture has no ground tilesets")
	}

	// Build a remap that flattens every old ground tile onto slot 0, every old
	// cliff tile onto slot 0. Real UI would use a HiveWE-style picker; we
	// just need a valid request shape here.
	groundFromTo := make([]int, oldGroundN)
	cliffFromTo := make([]int, oldCliffN)

	newCliff := []string{}
	if oldCliffN > 0 {
		newCliff = []string{"CAa1"}
	}
	req := SwapTilesetRequest{
		NewLetter:         'A',
		NewGroundTilesets: []string{"Agrs", "Adrt"},
		NewCliffTilesets:  newCliff,
		GroundFromTo:      groundFromTo,
		CliffFromTo:       cliffFromTo,
	}
	if err := s.SwapTileset(req); err != nil {
		t.Fatalf("SwapTileset: %v", err)
	}
	if !s.IsDirty() {
		t.Errorf("expected dirty after SwapTileset")
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if s.IsDirty() {
		t.Errorf("expected clean after Save")
	}

	// Reopen and assert the swap persisted on both files.
	s2 := &Session{}
	if err := s2.Open(tmp); err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	if got, want := s2.Terrain().Tileset, byte('A'); got != want {
		t.Errorf("Terrain().Tileset = %q, want %q", string(got), string(want))
	}
	if got := s2.Terrain().GroundTilesets; len(got) != 2 || got[0] != "Agrs" || got[1] != "Adrt" {
		t.Errorf("Terrain().GroundTilesets = %v, want [Agrs Adrt]", got)
	}
	if got, want := s2.Info().Tileset, byte('A'); got != want {
		t.Errorf("Info().Tileset = %q, want %q", string(got), string(want))
	}
	for i, tp := range s2.Terrain().Tiles {
		if int(tp.GroundTexture) >= 2 {
			t.Fatalf("Tiles[%d].GroundTexture = %d, want < 2", i, tp.GroundTexture)
		}
	}
}

// TestSwapTileset_UndoRedo exercises the history path for tileset swap: do
// the swap, undo it, verify the original tileset letter + per-tile texture
// indices come back, then redo and confirm the swap re-applies. Critically,
// undo must preserve the per-tile values that were ALREADY using the old
// palette (not "any tile pointing at slot 0 → slot 0" — actual originals).
func TestSwapTileset_UndoRedo(t *testing.T) {
	tmp := copyCommittedMapFixture(t)
	s := &Session{}
	if err := s.Open(tmp); err != nil {
		t.Fatalf("Open: %v", err)
	}

	origLetter := s.Terrain().Tileset
	origGround := append([]string(nil), s.Terrain().GroundTilesets...)
	origCliff := append([]string(nil), s.Terrain().CliffTilesets...)
	origTileG := make([]uint8, len(s.Terrain().Tiles))
	origTileC := make([]uint8, len(s.Terrain().Tiles))
	for i, tp := range s.Terrain().Tiles {
		origTileG[i] = tp.GroundTexture
		origTileC[i] = tp.CliffTexture
	}

	groundFromTo := make([]int, len(origGround))
	cliffFromTo := make([]int, len(origCliff))
	newCliff := []string{}
	if len(origCliff) > 0 {
		newCliff = []string{"CAa1"}
	}
	if err := s.SwapTileset(SwapTilesetRequest{
		NewLetter:         'A',
		NewGroundTilesets: []string{"Agrs", "Adrt"},
		NewCliffTilesets:  newCliff,
		GroundFromTo:      groundFromTo,
		CliffFromTo:       cliffFromTo,
	}); err != nil {
		t.Fatalf("SwapTileset: %v", err)
	}
	if got, want := s.Terrain().Tileset, byte('A'); got != want {
		t.Fatalf("post-swap Tileset = %q, want %q", string(got), string(want))
	}
	if !s.CanUndo() {
		t.Fatalf("CanUndo = false after swap; want true (swap should be in history)")
	}

	if err := s.Undo(); err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if got, want := s.Terrain().Tileset, origLetter; got != want {
		t.Errorf("post-undo Tileset = %q, want %q", string(got), string(want))
	}
	if got := s.Terrain().GroundTilesets; !equalStringsSlice(got, origGround) {
		t.Errorf("post-undo GroundTilesets = %v, want %v", got, origGround)
	}
	if got := s.Info().Tileset; got != origLetter {
		t.Errorf("post-undo Info().Tileset = %q, want %q", string(got), string(origLetter))
	}
	for i, tp := range s.Terrain().Tiles {
		if tp.GroundTexture != origTileG[i] {
			t.Fatalf("post-undo Tiles[%d].GroundTexture = %d, want %d", i, tp.GroundTexture, origTileG[i])
		}
		if tp.CliffTexture != origTileC[i] {
			t.Fatalf("post-undo Tiles[%d].CliffTexture = %d, want %d", i, tp.CliffTexture, origTileC[i])
		}
	}

	if err := s.Redo(); err != nil {
		t.Fatalf("Redo: %v", err)
	}
	if got, want := s.Terrain().Tileset, byte('A'); got != want {
		t.Errorf("post-redo Tileset = %q, want %q", string(got), string(want))
	}
	if got := s.Terrain().GroundTilesets; len(got) != 2 || got[0] != "Agrs" || got[1] != "Adrt" {
		t.Errorf("post-redo GroundTilesets = %v, want [Agrs Adrt]", got)
	}
	for i, tp := range s.Terrain().Tiles {
		if int(tp.GroundTexture) >= 2 {
			t.Fatalf("post-redo Tiles[%d].GroundTexture = %d, want < 2", i, tp.GroundTexture)
		}
	}
}

func equalStringsSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
