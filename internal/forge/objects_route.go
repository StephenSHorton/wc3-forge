package forge

import (
	"github.com/StephenSHorton/wc3-forge/internal/formats/w3objmod"
)

// Edit routing for the Reforged two-table object-data split. Reforged's World
// Editor writes each object's overrides across TWO per-kind files: war3map.w3*
// (the "primary" shadow — gameplay/logic fields) and war3mapSkin.w3* (the
// "skin" companion — art fields: Name unam, Model File umdl, Icon uico, Tooltip
// utip, vertex tint, …). The two files carry the SAME object ids: a stock edit
// is an OriginalEdit row in whichever file(s) it touches, and a custom object
// is a Custom row (id + baseID) mirrored into each file that carries a field
// for it.
//
// v1.0.5 loaded the skin companion read-only and merged it under the primary
// for display. This file adds the WRITE half: a field edit lands in the table
// the World Editor would use, so a saved map round-trips through the vanilla
// editor without art fields collapsing into the plain table (which some tools
// tolerate but the WE treats as a distinct, non-canonical layout).
//
// The decision is netsafe-driven (see ObjectFieldMeta.NetSafe), but a field
// already present on disk stays where it lives so we never migrate a map's
// existing layout (e.g. an old all-in-one map that keeps unam in war3map.w3u).

// fileFieldPresent reports whether `file` carries any override (base slot OR
// any level slot) for `fourCC` on object `id`. Used by routing to keep an
// already-materialized field in the table it lives in. Caller MUST hold s.mu.
func fileFieldPresent(file *w3objmod.File, id, fourCC string) bool {
	if file == nil {
		return false
	}
	if ci := findCustomIndex(file, id); ci >= 0 {
		c := &file.Customs[ci]
		if _, ok := c.Overrides[fourCC]; ok {
			return true
		}
		for i := range c.Levels {
			if c.Levels[i].FourCC == fourCC {
				return true
			}
		}
		return false
	}
	if ei := findOriginalEditIndex(file, id); ei >= 0 {
		e := &file.OriginalEdits[ei]
		if _, ok := e.Overrides[fourCC]; ok {
			return true
		}
		for i := range e.Levels {
			if e.Levels[i].FourCC == fourCC {
				return true
			}
		}
	}
	return false
}

// objectIdentity classifies `id` for the given kind: whether it's a custom
// object (and its baseID) or a stock row, using the PRIMARY shadow + base SLK
// as authoritative (that's where AddCustomObject records the definition). A
// custom that somehow exists only in the skin companion is still recognized so
// a skin-only import doesn't wedge. ok=false means the id is neither a known
// custom nor a stock row (caller should error). Caller MUST hold s.mu.
func objectIdentity(s *Session, cfg *KindConfig, id string) (isCustom bool, baseID string, ok bool) {
	if primary := cfg.GetMods(s); primary != nil {
		if ci := findCustomIndex(primary, id); ci >= 0 {
			return true, primary.Customs[ci].BaseID, true
		}
	}
	if skin := s.objectSkinMods[cfg.Kind]; skin != nil {
		if ci := findCustomIndex(skin, id); ci >= 0 {
			return true, skin.Customs[ci].BaseID, true
		}
	}
	if base, _, _ := loadObjectBase(cfg); base != nil && base.Rows[id] != nil {
		return false, "", true
	}
	return false, "", false
}

// routeSkinTable decides whether an edit of `fourCC` on `id` lands in the
// war3mapSkin.w3* companion (true) or the plain war3map.w3* table (false),
// non-allocating. Rule, in order:
//  1. field already present in the primary shadow for id → primary.
//  2. field already present in the skin companion for id → skin.
//  3. brand-new override → skin iff the field is netsafe (art/skin), else main.
//
// Presence wins over netsafe so a map's existing on-disk layout is never
// migrated between tables by a re-edit. Caller MUST hold s.mu.
func routeSkinTable(s *Session, cfg *KindConfig, id, fourCC string, meta *ObjectMetadata) bool {
	if fileFieldPresent(cfg.GetMods(s), id, fourCC) {
		return false
	}
	if fileFieldPresent(s.objectSkinMods[cfg.Kind], id, fourCC) {
		return true
	}
	if meta != nil {
		if m, ok := meta.ByID[fourCC]; ok && m.NetSafe {
			return true
		}
	}
	return false
}

// targetModFile returns the ensured (allocated-if-nil) destination File for a
// routed edit: the skin companion when skin, else the primary shadow. Caller
// MUST hold s.mu (write lock).
func targetModFile(s *Session, cfg *KindConfig, skin bool) *w3objmod.File {
	if skin {
		return s.ensureSkinModsLocked(cfg.Kind)
	}
	return ensureObjectMods(s, cfg)
}

// routedReadFile returns the (possibly nil) File a routed read should consult
// for `skin`, WITHOUT allocating — used for no-op detection before a mutation.
// Caller MUST hold s.mu.
func (s *Session) routedReadFile(cfg *KindConfig, skin bool) *w3objmod.File {
	if skin {
		return s.objectSkinMods[cfg.Kind]
	}
	return cfg.GetMods(s)
}

// readBaseOverride returns the base-slot (level 0) override value + presence
// for `fourCC` on `id` within `file`. Caller MUST hold s.mu.
func readBaseOverride(file *w3objmod.File, id, fourCC string) (string, bool) {
	if file == nil {
		return "", false
	}
	if ci := findCustomIndex(file, id); ci >= 0 {
		v, ok := file.Customs[ci].Overrides[fourCC]
		return v, ok
	}
	if ei := findOriginalEditIndex(file, id); ei >= 0 {
		v, ok := file.OriginalEdits[ei].Overrides[fourCC]
		return v, ok
	}
	return "", false
}

// putBaseOverride writes `value` to `fourCC` in the base slot of `file` for
// object `id`, creating the row it belongs in: a Custom row (id + baseID) when
// isCustom, otherwise an OriginalEdit row. Mirrors setObjectField's original
// stock/custom branching but on an explicit File so a skin edit can create the
// matching skin-table row. Returns the prior value + had-override flag for
// undo. Caller MUST hold s.mu (write lock).
func putBaseOverride(file *w3objmod.File, id, baseID, fourCC, value string, isCustom bool) (prev string, had bool) {
	if isCustom {
		ci := findCustomIndex(file, id)
		if ci < 0 {
			file.Customs = append(file.Customs, w3objmod.CustomObject{
				ID: id, BaseID: baseID, Overrides: w3objmod.Overrides{},
			})
			ci = len(file.Customs) - 1
		}
		c := &file.Customs[ci]
		if c.Overrides == nil {
			c.Overrides = w3objmod.Overrides{}
		}
		prev, had = c.Overrides[fourCC]
		c.Overrides[fourCC] = value
		return prev, had
	}
	ei := findOriginalEditIndex(file, id)
	if ei < 0 {
		file.OriginalEdits = append(file.OriginalEdits, w3objmod.OriginalEdit{
			BaseID: id, Overrides: w3objmod.Overrides{},
		})
		ei = len(file.OriginalEdits) - 1
	}
	e := &file.OriginalEdits[ei]
	if e.Overrides == nil {
		e.Overrides = w3objmod.Overrides{}
	}
	prev, had = e.Overrides[fourCC]
	e.Overrides[fourCC] = value
	return prev, had
}

// putLevelOverride writes `value` to (`fourCC`, `level`) in the Levels list of
// `file` for object `id`, creating the Custom / OriginalEdit row as needed
// (mirrors putBaseOverride for the leveled slot). Returns the prior value +
// had-override flag for undo. Caller MUST hold s.mu (write lock).
func putLevelOverride(file *w3objmod.File, id, baseID, fourCC string, level, dp uint32, value string, isCustom bool) (prev string, had bool) {
	if isCustom {
		ci := findCustomIndex(file, id)
		if ci < 0 {
			file.Customs = append(file.Customs, w3objmod.CustomObject{ID: id, BaseID: baseID})
			ci = len(file.Customs) - 1
		}
		c := &file.Customs[ci]
		c.Levels, prev, had = upsertLevelOverride(c.Levels, fourCC, level, dp, value)
		return prev, had
	}
	ei := findOriginalEditIndex(file, id)
	if ei < 0 {
		file.OriginalEdits = append(file.OriginalEdits, w3objmod.OriginalEdit{BaseID: id})
		ei = len(file.OriginalEdits) - 1
	}
	e := &file.OriginalEdits[ei]
	e.Levels, prev, had = upsertLevelOverride(e.Levels, fourCC, level, dp, value)
	return prev, had
}

// markObjectDirty flips the correct per-kind dirty flag for a mutation that
// landed in either table: the skin companion when skin, else the primary
// shadow. Every object-edit command + public mutator funnels through this so
// Save re-encodes exactly the file that changed. Caller MUST hold s.mu (write).
func markObjectDirty(s *Session, cfg *KindConfig, skin bool) {
	if skin {
		s.setSkinDirtyLocked(cfg.Kind, true)
		return
	}
	cfg.SetDirty(s, true)
}
