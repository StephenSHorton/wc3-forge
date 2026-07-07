package forge

import (
	"fmt"

	"github.com/StephenSHorton/wc3-forge/internal/formats/w3objmod"
)

// Level-aware write path for leveled object fields (abilities w3a, upgrades
// w3q, doodad variations w3d). The base/level-0 slot still flows through
// setObjectField + the flat Overrides map (unchanged behavior). A non-zero
// level targets the ordered Levels list on the custom / original-edit row so
// two distinct per-level values for the SAME field don't collapse into one.
//
// All of these mirror their non-leveled counterparts in objects_mods.go and
// follow the same locking contract (caller MUST hold s.mu write lock for the
// mutators; the public Session.SetObjectFieldLevel acquires it).

// findLevelOverride returns the index of the (fourCC, level) entry within a
// Levels slice, or -1 when absent. Matching is on both FourCC and Level so a
// field with values at several levels keeps each one addressable.
func findLevelOverride(levels []w3objmod.LevelOverride, fourCC string, level uint32) int {
	for i := range levels {
		if levels[i].FourCC == fourCC && levels[i].Level == level {
			return i
		}
	}
	return -1
}

// upsertLevelOverride inserts or updates a (fourCC, level) entry in the slice,
// preserving the supplied data_pointer. Returns the new slice plus the prior
// value + had-entry flag (for undo).
func upsertLevelOverride(levels []w3objmod.LevelOverride, fourCC string, level, dp uint32, value string) (out []w3objmod.LevelOverride, prev string, had bool) {
	if i := findLevelOverride(levels, fourCC, level); i >= 0 {
		prev, had = levels[i].Value, true
		levels[i].Value = value
		levels[i].DataPointer = dp
		return levels, prev, had
	}
	return append(levels, w3objmod.LevelOverride{
		FourCC: fourCC, Level: level, DataPointer: dp, Value: value,
	}), "", false
}

// removeLevelOverride drops the (fourCC, level) entry if present, returning the
// shortened slice. Idempotent — a miss is a silent no-op.
func removeLevelOverride(levels []w3objmod.LevelOverride, fourCC string, level uint32) []w3objmod.LevelOverride {
	if i := findLevelOverride(levels, fourCC, level); i >= 0 {
		return append(levels[:i], levels[i+1:]...)
	}
	return levels
}

// setObjectFieldLevel is the lock-held mutator for editing a single field at a
// specific level on a stock or custom object. level==0 routes to the base slot
// (identical to setObjectField); level>0 targets the Levels list.
//
// Caller MUST hold s.mu (write lock). Returns prior value + had-override flag
// for undo. Errors if id isn't a known object or the field doesn't resolve.
//
// Leveled writes are only meaningful for opt-format kinds (abilities/upgrades/
// doodads); for a non-opt kind a level>0 write still records but won't survive
// Encode (no level slot on the wire), so callers should reject level>0 on a
// non-opt kind upstream.
func setObjectFieldLevel(s *Session, cfg *KindConfig, id, field string, level uint32, value string) (prevVal string, hadOverride bool, skin bool, err error) {
	if level == 0 {
		return setObjectField(s, cfg, id, field, value)
	}
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
	prev, had := putLevelOverride(file, id, baseID, fourCC, level, 0, value, isCustom)
	return prev, had, skin, nil
}

// clearObjectFieldLevel removes a leveled override (used by Revert). It clears
// from whichever table holds the field and returns which one so the caller
// flips the matching dirty flag. Drops a resulting empty OriginalEdit row so a
// saved file doesn't list a no-op. Caller MUST hold s.mu (write lock). level==0
// delegates to clearObjectField.
func clearObjectFieldLevel(s *Session, cfg *KindConfig, id, field string, level uint32) (skin bool, err error) {
	if level == 0 {
		return clearObjectField(s, cfg, id, field)
	}
	_, meta, _ := loadObjectBase(cfg)
	fourCC := fieldKeyForMods(meta, field)
	if fourCC == "" {
		return false, fmt.Errorf("unknown field %q", field)
	}
	// pruneEmptyCustom set only for the skin companion — see clearObjectField.
	if clearLevelOverrideFromFile(cfg.GetMods(s), id, fourCC, level, false) {
		return false, nil
	}
	if clearLevelOverrideFromFile(s.objectSkinMods[cfg.Kind], id, fourCC, level, true) {
		return true, nil
	}
	return false, nil
}

// clearLevelOverrideFromFile removes the (fourCC, level) entry for `id` in
// `file`, dropping a now-empty OriginalEdit row (always) or a now-empty Custom
// row (only when pruneEmptyCustom — the skin companion; see clearObjectField).
// Returns true if the (fourCC, level) entry existed. Caller MUST hold s.mu.
func clearLevelOverrideFromFile(file *w3objmod.File, id, fourCC string, level uint32, pruneEmptyCustom bool) bool {
	if file == nil {
		return false
	}
	if ci := findCustomIndex(file, id); ci >= 0 {
		if findLevelOverride(file.Customs[ci].Levels, fourCC, level) < 0 {
			return false
		}
		file.Customs[ci].Levels = removeLevelOverride(file.Customs[ci].Levels, fourCC, level)
		if pruneEmptyCustom && len(file.Customs[ci].Overrides) == 0 && len(file.Customs[ci].Levels) == 0 {
			file.Customs = append(file.Customs[:ci], file.Customs[ci+1:]...)
		}
		return true
	}
	if ei := findOriginalEditIndex(file, id); ei >= 0 {
		e := &file.OriginalEdits[ei]
		if findLevelOverride(e.Levels, fourCC, level) < 0 {
			return false
		}
		e.Levels = removeLevelOverride(e.Levels, fourCC, level)
		if len(e.Overrides) == 0 && len(e.Levels) == 0 {
			file.OriginalEdits = append(file.OriginalEdits[:ei], file.OriginalEdits[ei+1:]...)
		}
		return true
	}
	return false
}

// readObjectFieldLevel returns the current value + had-entry flag for a
// (id, field, level) tuple, consulting the primary shadow first and then the
// war3mapSkin.w3* companion (an art/skin field lives in exactly one of them, so
// the first hit is authoritative). level==0 reads the flat Overrides slot.
// Caller MUST hold s.mu (read or write). Used by the public mutator for
// idempotence + undo bookkeeping.
func readObjectFieldLevel(s *Session, cfg *KindConfig, id, fourCC string, level uint32) (string, bool) {
	if v, ok := readFileFieldLevel(cfg.GetMods(s), id, fourCC, level); ok {
		return v, true
	}
	return readFileFieldLevel(s.objectSkinMods[cfg.Kind], id, fourCC, level)
}

// readFileFieldLevel reads the (id, fourCC, level) value + presence from a
// single File. level==0 reads the flat Overrides slot; level>0 the Levels list.
// Caller MUST hold s.mu (read or write).
func readFileFieldLevel(file *w3objmod.File, id, fourCC string, level uint32) (string, bool) {
	if file == nil {
		return "", false
	}
	if ci := findCustomIndex(file, id); ci >= 0 {
		c := &file.Customs[ci]
		if level == 0 {
			v, ok := c.Overrides[fourCC]
			return v, ok
		}
		if i := findLevelOverride(c.Levels, fourCC, level); i >= 0 {
			return c.Levels[i].Value, true
		}
		return "", false
	}
	if ei := findOriginalEditIndex(file, id); ei >= 0 {
		e := &file.OriginalEdits[ei]
		if level == 0 {
			v, ok := e.Overrides[fourCC]
			return v, ok
		}
		if i := findLevelOverride(e.Levels, fourCC, level); i >= 0 {
			return e.Levels[i].Value, true
		}
		return "", false
	}
	return "", false
}

// setObjectFieldLevelCmd is the undo/redo command for a leveled field edit.
// Mirrors setObjectFieldCmd but carries the level so Apply/Revert target the
// right slot. level==0 commands behave identically to setObjectFieldCmd; the
// public mutator records the base-slot variant through setObjectFieldCmd, so
// this type only ships for level>0 edits.
type setObjectFieldLevelCmd struct {
	kind        string
	id          string
	column      string
	level       uint32
	oldVal      string
	newVal      string
	hadOverride bool
}

func (c *setObjectFieldLevelCmd) Label() string { return "Edit " + c.kind + " field (level)" }

func (c *setObjectFieldLevelCmd) Apply(s *Session) error {
	cfg := kindConfigFor(c.kind)
	_, _, skin, err := setObjectFieldLevel(s, cfg, c.id, c.column, c.level, c.newVal)
	if err != nil {
		return err
	}
	markObjectDirty(s, cfg, skin)
	return nil
}

func (c *setObjectFieldLevelCmd) Revert(s *Session) error {
	cfg := kindConfigFor(c.kind)
	if !c.hadOverride {
		skin, err := clearObjectFieldLevel(s, cfg, c.id, c.column, c.level)
		if err != nil {
			return err
		}
		markObjectDirty(s, cfg, skin)
	} else {
		_, _, skin, err := setObjectFieldLevel(s, cfg, c.id, c.column, c.level, c.oldVal)
		if err != nil {
			return err
		}
		markObjectDirty(s, cfg, skin)
	}
	return nil
}

func (c *setObjectFieldLevelCmd) Affected(s *Session) []EntityChange {
	return []EntityChange{{Kind: c.kind + "_mod", ID: 0, Field: c.column}}
}

// SetObjectFieldLevel writes `value` to `field` at the given `level` on the
// object `id` of kind `cfg`. level==0 is the base/level-0 slot (identical to
// SetObjectField — same flat-override behavior + same command type). level>0
// targets the per-level Levels list and is only meaningful for opt-format
// kinds (abilities/upgrades/doodads); a level>0 write to a non-opt kind is
// rejected so the value can't silently vanish on Save.
//
// Idempotence + history + event semantics mirror SetObjectField exactly.
func (s *Session) SetObjectFieldLevel(cfg *KindConfig, id, field string, level uint32, value string) error {
	if level == 0 {
		return s.SetObjectField(cfg, id, field, value)
	}
	if !cfg.ShadowOpt {
		return fmt.Errorf("kind %q has no per-level fields (level must be 0)", cfg.Kind)
	}
	// LOCK-ORDERING HAZARD — warm the per-kind base cache BEFORE s.mu. The
	// level>0 path below (and setObjectFieldLevel itself) call loadObjectBase
	// while holding the write lock; the first-ever load runs readBaseAsset,
	// whose reader takes Current.mu.RLock(). A write-holder re-acquiring a
	// read lock on the same non-reentrant RWMutex deadlocks forever. Warming
	// here (no lock held) turns the in-lock loads into cache hits. See
	// SetObjectField for the full rationale; do NOT remove this.
	loadObjectBase(cfg)
	s.mu.Lock()
	if !s.loaded {
		s.mu.Unlock()
		return fmt.Errorf("no map loaded")
	}
	_, meta, _ := loadObjectBase(cfg)
	fourCC := fieldKeyForMods(meta, field)
	if fourCC == "" {
		s.mu.Unlock()
		return fmt.Errorf("unknown field %q (not in %s)", field, cfg.MetaDataFile)
	}
	// Read the prior value from the SAME table the edit will route to (mirrors
	// SetObjectField). Reading per-(fourCC,level) across both tables could pick
	// a different table than routeSkinTable resolves per-fourCC, desyncing the
	// no-op check + oldVal capture from where the write lands.
	skinTable := routeSkinTable(s, cfg, id, fourCC, meta)
	prev, had := readFileFieldLevel(s.routedReadFile(cfg, skinTable), id, fourCC, level)
	if had && prev == value {
		s.mu.Unlock()
		return nil // no-op
	}
	// Validate id before mutating (mirrors SetObjectField — don't leave an
	// empty shadow for a bad id).
	if _, _, ok := objectIdentity(s, cfg, id); !ok {
		s.mu.Unlock()
		return fmt.Errorf("no %s object with id %q", cfg.Kind, id)
	}
	_, _, skin, err := setObjectFieldLevel(s, cfg, id, field, level, value)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	wasDirty := s.anyDirtyLocked()
	markObjectDirty(s, cfg, skin)
	s.recordCommand(&setObjectFieldLevelCmd{
		kind: cfg.Kind, id: id, column: field, level: level,
		oldVal: prev, newVal: value, hadOverride: had,
	})
	historyChanged := s.groupDepth == 0
	s.mu.Unlock()
	if !wasDirty {
		s.notifyDirty(true)
	}
	s.notifyEntityChanged(EntityChange{Kind: cfg.Kind + "_mod", ID: 0, Field: field})
	if historyChanged {
		s.notifyHistoryChanged()
	}
	return nil
}
