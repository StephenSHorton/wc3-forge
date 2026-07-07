package forge

import (
	"fmt"
	"strings"

	"github.com/StephenSHorton/wc3-forge/internal/formats/w3objmod"
)

// Write-side helpers for the per-kind object-modification table
// (war3map.w3u for units, war3map.w3t for items, ...). Kind-agnostic
// generic — every per-kind handler funnels here through a *KindConfig.
//
// Convention: Overrides keys on the kind's shadow File are FOURCC strings
// (Parse is called with nil FieldMap at Open time so the keys stay raw).
// Public callers may pass either FourCC or column-name; setObjectField
// normalizes via the kind's metadata before writing. This lets the wire
// surface accept either form (HiveWE compatibility) without forcing the
// JS caller to know the binding.

// fieldKeyForMods resolves caller-supplied field identifier (FourCC OR
// column-name) to the FourCC the on-disk shadow expects. Returns "" when
// the identifier doesn't resolve to a known field (caller should error out).
//
// Lookup order:
//  1. Direct match in ByID (caller passed a FourCC like "unam").
//  2. Lowercase column-name match in the metadata's Field column
//     (caller passed "name").
//
// Lowercased input on the column-match path — slk.Mapped columns are
// lowercase-keyed and metadata.Field is stored as-typed; we normalize
// both sides to lowercase for the comparison.
func fieldKeyForMods(meta *UnitMetadata, ident string) string {
	if meta == nil {
		return ""
	}
	if _, ok := meta.ByID[ident]; ok {
		return ident
	}
	lc := strings.ToLower(ident)
	for i := range meta.Fields {
		if strings.ToLower(meta.Fields[i].Field) == lc {
			return meta.Fields[i].ID
		}
	}
	return ""
}

// findCustomIndex returns the index of the custom with the given ID in the
// mods table, or -1 if not present. Caller MUST hold s.mu (read or write).
func findCustomIndex(mods *w3objmod.File, id string) int {
	if mods == nil {
		return -1
	}
	for i := range mods.Customs {
		if mods.Customs[i].ID == id {
			return i
		}
	}
	return -1
}

// findOriginalEditIndex returns the index of the original-edit row for the
// given base ID, or -1 if not present.
func findOriginalEditIndex(mods *w3objmod.File, baseID string) int {
	if mods == nil {
		return -1
	}
	for i := range mods.OriginalEdits {
		if mods.OriginalEdits[i].BaseID == baseID {
			return i
		}
	}
	return -1
}

// ensureObjectMods returns the kind's shadow File, allocating a fresh empty
// File when nil. Caller MUST hold s.mu (write lock).
func ensureObjectMods(s *Session, cfg *KindConfig) *w3objmod.File {
	mods := cfg.GetMods(s)
	if mods == nil {
		mods = &w3objmod.File{Version: 3}
		cfg.SetMods(s, mods)
	}
	return mods
}

// setObjectField is the lock-held mutator for "edit a single field on a stock
// or custom object". Used by the public Set*Field AND by the command's
// Apply/Revert so undo/redo replays the same code path.
//
// Caller MUST hold s.mu (write lock). Returns the prior override value +
// "had override" flag (so the caller can build the undo command) plus which
// table the edit landed in — `skin` true means the war3mapSkin.w3* companion
// (an art/skin field), so the caller flips the skin dirty flag rather than the
// primary one. See routeSkinTable for the primary-vs-skin decision.
//
// For STOCK rows (id matches a SLK row), the override lands in the
// OriginalEdits table — creates a new edit-row entry if none exists yet.
// For CUSTOM rows (id matches a custom row), the override lands on a Custom
// row in the target table (created there if the skin companion didn't already
// mirror the custom).
//
// Returns an error if id isn't a known object (neither stock nor custom) or
// the field key doesn't translate to a metadata-known FourCC.
func setObjectField(s *Session, cfg *KindConfig, id, field, value string) (prevVal string, hadOverride bool, skin bool, err error) {
	_, meta, _ := loadObjectBase(cfg)
	fourCC := fieldKeyForMods(meta, field)
	if fourCC == "" {
		return "", false, false, fmt.Errorf("unknown field %q (not in %s)", field, cfg.MetaDataFile)
	}
	isCustom, baseID, ok := objectIdentity(s, cfg, id)
	if !ok {
		return "", false, false, fmt.Errorf("no %s object with id %q", cfg.Kind, id)
	}
	skin = routeSkinTable(s, cfg, id, fourCC, meta)
	file := targetModFile(s, cfg, skin)
	prev, had := putBaseOverride(file, id, baseID, fourCC, value, isCustom)
	return prev, had, skin, nil
}

// clearObjectField removes a field override (used by Revert of setObjectFieldCmd
// when the prior state had no override). It clears from whichever table holds
// the field — primary first, then the skin companion — and returns which one
// so the caller flips the matching dirty flag. If the resulting OriginalEdit
// row has no remaining overrides (base or level), the row itself is dropped so
// a Save after a full undo doesn't ship empty edit rows.
//
// Caller MUST hold s.mu (write lock). Idempotent — clearing a field that
// isn't present in either table is a silent no-op (skin=false).
func clearObjectField(s *Session, cfg *KindConfig, id, field string) (skin bool, err error) {
	_, meta, _ := loadObjectBase(cfg)
	fourCC := fieldKeyForMods(meta, field)
	if fourCC == "" {
		return false, fmt.Errorf("unknown field %q", field)
	}
	// Clear from the table that actually holds the base override. A field is
	// only ever in one table for a given id (routing keeps it there), so the
	// first hit is authoritative. pruneEmptyCustom is set ONLY for the skin
	// companion: a skin Custom row is a pure art mirror, so once its last
	// override is gone the row is a phantom and must be dropped (else an
	// edit→undo→save leaves an inert {id,baseID} row and bumps the file off the
	// lossless copy-through). A PRIMARY custom row is the object's definition —
	// an empty one is a legitimate "inherit everything" custom, never dropped.
	if clearBaseOverrideFromFile(cfg.GetMods(s), id, fourCC, false) {
		return false, nil
	}
	if clearBaseOverrideFromFile(s.objectSkinMods[cfg.Kind], id, fourCC, true) {
		return true, nil
	}
	return false, nil
}

// clearBaseOverrideFromFile deletes the base-slot override for (id, fourCC) in
// `file`, dropping a now-empty OriginalEdit row (always) or a now-empty Custom
// row (only when pruneEmptyCustom — the skin companion; see clearObjectField).
// Returns true if it removed anything. Caller MUST hold s.mu (write lock).
func clearBaseOverrideFromFile(file *w3objmod.File, id, fourCC string, pruneEmptyCustom bool) bool {
	if file == nil {
		return false
	}
	if ci := findCustomIndex(file, id); ci >= 0 {
		if _, had := file.Customs[ci].Overrides[fourCC]; !had {
			return false
		}
		delete(file.Customs[ci].Overrides, fourCC)
		if pruneEmptyCustom && len(file.Customs[ci].Overrides) == 0 && len(file.Customs[ci].Levels) == 0 {
			file.Customs = append(file.Customs[:ci], file.Customs[ci+1:]...)
		}
		return true
	}
	if ei := findOriginalEditIndex(file, id); ei >= 0 {
		e := &file.OriginalEdits[ei]
		if _, had := e.Overrides[fourCC]; !had {
			return false
		}
		delete(e.Overrides, fourCC)
		if len(e.Overrides) == 0 && len(e.Levels) == 0 {
			// Drop the empty edit row so the saved file doesn't list a no-op.
			file.OriginalEdits = append(
				file.OriginalEdits[:ei],
				file.OriginalEdits[ei+1:]...,
			)
		}
		return true
	}
	return false
}

// addCustomObject appends a new custom row with empty Overrides. Caller MUST
// hold s.mu (write lock). Returns error if newID already collides with another
// custom OR with a stock row (which would shadow it).
func addCustomObject(s *Session, cfg *KindConfig, newID, baseID string) error {
	if len(newID) != 4 {
		return fmt.Errorf("custom id %q must be 4 chars", newID)
	}
	if len(baseID) != 4 {
		return fmt.Errorf("base id %q must be 4 chars", baseID)
	}
	mods := ensureObjectMods(s, cfg)
	if findCustomIndex(mods, newID) >= 0 {
		return fmt.Errorf("custom %q already exists", newID)
	}
	base, _, _ := loadObjectBase(cfg)
	if base != nil && base.Rows[newID] != nil {
		// Shadowing a stock row is technically valid in the wire format but
		// is almost always a mistake — the user loses access to the stock
		// object. The allocator already skips occupied IDs.
		return fmt.Errorf("custom id %q would shadow a stock %s", newID, cfg.Kind)
	}
	mods.Customs = append(mods.Customs, w3objmod.CustomObject{
		ID:        newID,
		BaseID:    baseID,
		Overrides: w3objmod.Overrides{},
	})
	return nil
}

// removeCustomObject deletes the custom with the given ID from the primary
// shadow AND, if present, its mirror in the war3mapSkin.w3* companion (so a
// deleted object doesn't orphan its art/skin overrides). Caller MUST hold s.mu
// (write lock). Returns the deleted primary CustomObject, the deleted skin
// CustomObject (nil when the companion had no mirror), and a flag indicating
// whether the primary deletion happened (false = not found). The caller flips
// the skin dirty flag when savedSkin != nil.
func removeCustomObject(s *Session, cfg *KindConfig, id string) (saved w3objmod.CustomObject, savedSkin *w3objmod.CustomObject, ok bool) {
	mods := cfg.GetMods(s)
	if mods == nil {
		return w3objmod.CustomObject{}, nil, false
	}
	ci := findCustomIndex(mods, id)
	if ci < 0 {
		return w3objmod.CustomObject{}, nil, false
	}
	saved = mods.Customs[ci]
	mods.Customs = append(mods.Customs[:ci], mods.Customs[ci+1:]...)

	if skin := s.objectSkinMods[cfg.Kind]; skin != nil {
		if si := findCustomIndex(skin, id); si >= 0 {
			sc := skin.Customs[si]
			savedSkin = &sc
			skin.Customs = append(skin.Customs[:si], skin.Customs[si+1:]...)
		}
	}
	return saved, savedSkin, true
}

// reinsertCustomObject re-appends a previously-deleted custom to the primary
// shadow and, when savedSkin != nil, its mirror to the war3mapSkin.w3*
// companion (used by Revert of deleteCustomObjectCmd). Caller MUST hold s.mu
// (write lock). The original slice position isn't preserved — Customs is an
// unordered set in practice (HiveWE writes them in iteration order), so
// appending is fine.
func reinsertCustomObject(s *Session, cfg *KindConfig, saved w3objmod.CustomObject, savedSkin *w3objmod.CustomObject) {
	mods := ensureObjectMods(s, cfg)
	mods.Customs = append(mods.Customs, saved)
	if savedSkin != nil {
		skin := s.ensureSkinModsLocked(cfg.Kind)
		skin.Customs = append(skin.Customs, *savedSkin)
	}
}

// allocateCustomID picks the next-free custom FourCC, modeled on HiveWE's
// allocator: take the base FourCC's first character, then scan
// "<char>000".."<char>FFF" using hex-style 3-character suffixes ("000", "001",
// …, "00A", …) and return the first unused slot.
//
// Skips IDs that collide with stock FourCCs (shadowing a stock object would
// lose access to it in the editor). Also skips existing customs of course.
//
// Returns "" if no free slot exists in the prefix space (~4096 customs
// sharing a first letter — practically unreachable).
func allocateCustomID(s *Session, cfg *KindConfig, baseID string) string {
	if baseID == "" {
		return ""
	}
	prefix := byte(baseID[0])
	base, _, _ := loadObjectBase(cfg)
	mods := cfg.GetMods(s)

	occupied := map[string]bool{}
	if mods != nil {
		for _, c := range mods.Customs {
			if c.ID != "" && c.ID[0] == prefix {
				occupied[c.ID] = true
			}
		}
	}
	if base != nil {
		for id := range base.Rows {
			if len(id) == 4 && id[0] == prefix {
				occupied[id] = true
			}
		}
	}

	const hex = "0123456789ABCDEF"
	for a := 0; a < 16; a++ {
		for b := 0; b < 16; b++ {
			for c := 0; c < 16; c++ {
				id := string([]byte{prefix, hex[a], hex[b], hex[c]})
				if !occupied[id] {
					return id
				}
			}
		}
	}
	return ""
}
