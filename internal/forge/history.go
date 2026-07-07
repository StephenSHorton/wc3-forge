package forge

import (
	"fmt"

	"github.com/StephenSHorton/wc3-forge/internal/formats/unitsdoo"
	"github.com/StephenSHorton/wc3-forge/internal/formats/w3objmod"
)

// Command is the unit of undo/redo. Each mutator method on Session (MoveUnit,
// RotateDoodad, ScaleUnit, etc.) builds one of these after capturing the
// pre-mutation state, then records it via recordCommand. Undo pops the top
// of the history stack and calls Revert; Redo pops the top of the redo stack
// and calls Apply.
//
// Apply / Revert are called with s.mu held. Both must update any per-file
// dirty flags they need (the public dirty event fires post-unlock from the
// outer caller). Neither method should fire notifications directly — the
// caller (mutator, Undo, Redo) collects Affected() and fires entity-changed
// events after releasing the lock.
//
// A new mutator wires itself into history in 3 steps:
//   1. Define a *xxxCmd type with the channels it changes.
//   2. The mutator method captures old state under the lock, builds the cmd,
//      mutates in-place, then calls s.recordCommand(cmd).
//   3. Add the cmd's Apply/Revert/Affected/Label methods.
type Command interface {
	// Apply re-executes the command (used by Redo). Called with s.mu held.
	Apply(s *Session) error
	// Revert undoes the command (used by Undo). Called with s.mu held.
	Revert(s *Session) error
	// Affected returns the entity-changed events that should fire after the
	// most recent Apply or Revert. Read from current session state (not from
	// stored "new" values) so it works for both directions.
	Affected(s *Session) []EntityChange
	// Label is a short human-readable label for the UI (e.g. "Move Footman").
	Label() string
}

// HistoryEntry is the serializable view returned by HistoryList for the UI.
// Index is position from oldest (0) to newest (len-1).
type HistoryEntry struct {
	Index  int    `json:"index"`
	Label  string `json:"label"`
	Active bool   `json:"active"` // true if this is the next entry that Undo would revert
}

// HistoryState describes both stacks in one snapshot — used by the Wails
// HistoryList method so the UI can render the full undo+redo state in a
// single round-trip rather than two.
type HistoryState struct {
	Undo []HistoryEntry `json:"undo"` // oldest-first
	Redo []HistoryEntry `json:"redo"` // oldest-first
	// OpenGroupDepth is the current undo-group nesting depth (0 = no group
	// open). When > 0, Undo/Redo are blocked until the group is closed
	// (EndUndoGroup) or discarded (AbortUndoGroup). An agent that began a
	// group and then aborted/crashed without ending it leaves this > 0,
	// wedging undo/redo for the whole session — surfacing it here lets a
	// caller detect the dangling group and recover via history.abort_group.
	OpenGroupDepth int `json:"open_group_depth"`
}

// ---------------------------------------------------------------------------
// Group command — wraps an ordered list of sub-commands into one undo unit.
// Used by gizmo drag-end: a single drag may commit N per-entity mutations
// (move + rotate for orbit, scale + move for centroid-scale); the user
// expects one Ctrl+Z to undo the whole drag, not N undos.
// ---------------------------------------------------------------------------

type groupCmd struct {
	label string
	cmds  []Command
}

func (g *groupCmd) Label() string { return g.label }

func (g *groupCmd) Apply(s *Session) error {
	for _, c := range g.cmds {
		if err := c.Apply(s); err != nil {
			return err
		}
	}
	return nil
}

func (g *groupCmd) Revert(s *Session) error {
	// Reverse order — the last applied is the first reverted, so dependent
	// edits in a group unwind in the inverse order they were laid down.
	for i := len(g.cmds) - 1; i >= 0; i-- {
		if err := g.cmds[i].Revert(s); err != nil {
			return err
		}
	}
	return nil
}

func (g *groupCmd) Affected(s *Session) []EntityChange {
	// Aggregate sub-command notifications. Duplicates are tolerable (the JS
	// scene handler is idempotent — applying the same position twice is fine).
	var out []EntityChange
	for _, c := range g.cmds {
		out = append(out, c.Affected(s)...)
	}
	return out
}

// ---------------------------------------------------------------------------
// Per-mutator commands. Each captures the minimum state needed to round-trip
// the mutation. Scale commands also capture the unexported scaleRaw on units
// because Scale is canonicalized on Parse (divide by 128) but the preservation
// path stores the raw on-disk bytes for byte-faithful round-trip; Revert must
// restore both.
// ---------------------------------------------------------------------------

// setSkyModelCmd represents one picker-driven sky change. oldPath and newPath
// are the pending-override pointer values BEFORE and AFTER the mutation:
//   - nil      → no override (script's call wins, or gradient-only if absent)
//   - non-nil  → explicit pending value (will commit to script on next Save)
// Apply re-applies newPath; Revert restores oldPath. Both call into the lib's
// non-recording setter so undo/redo doesn't pile new history entries on top
// of each other.
type setSkyModelCmd struct {
	oldPath, newPath *string
}

func (c *setSkyModelCmd) Label() string { return "Change sky" }
func (c *setSkyModelCmd) Apply(s *Session) error {
	s.pendingSkyModel = clonePtr(c.newPath)
	return nil
}
func (c *setSkyModelCmd) Revert(s *Session) error {
	s.pendingSkyModel = clonePtr(c.oldPath)
	return nil
}
func (c *setSkyModelCmd) Affected(s *Session) []EntityChange {
	return []EntityChange{{Kind: "info", ID: 0, Field: "sky_model"}}
}

func clonePtr(p *string) *string {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

type moveUnitCmd struct {
	cn             uint32
	oldPos, newPos [3]float32
}

func (c *moveUnitCmd) Label() string                   { return "Move unit" }
func (c *moveUnitCmd) Apply(s *Session) error          { return setUnitPos(s, c.cn, c.newPos) }
func (c *moveUnitCmd) Revert(s *Session) error         { return setUnitPos(s, c.cn, c.oldPos) }
func (c *moveUnitCmd) Affected(s *Session) []EntityChange {
	pos, ok := getUnitPos(s, c.cn)
	if !ok {
		return nil
	}
	return []EntityChange{{Kind: "unit", ID: c.cn, Field: "position", Position: pos}}
}

type moveDoodadCmd struct {
	cn             uint32
	oldPos, newPos [3]float32
}

func (c *moveDoodadCmd) Label() string           { return "Move doodad" }
func (c *moveDoodadCmd) Apply(s *Session) error  { return setDoodadPos(s, c.cn, c.newPos) }
func (c *moveDoodadCmd) Revert(s *Session) error { return setDoodadPos(s, c.cn, c.oldPos) }
func (c *moveDoodadCmd) Affected(s *Session) []EntityChange {
	pos, ok := getDoodadPos(s, c.cn)
	if !ok {
		return nil
	}
	return []EntityChange{{Kind: "doodad", ID: c.cn, Field: "position", Position: pos}}
}

type rotateUnitCmd struct {
	cn             uint32
	oldRot, newRot float32
}

func (c *rotateUnitCmd) Label() string           { return "Rotate unit" }
func (c *rotateUnitCmd) Apply(s *Session) error  { return setUnitRot(s, c.cn, c.newRot) }
func (c *rotateUnitCmd) Revert(s *Session) error { return setUnitRot(s, c.cn, c.oldRot) }
func (c *rotateUnitCmd) Affected(s *Session) []EntityChange {
	rot, ok := getUnitRot(s, c.cn)
	if !ok {
		return nil
	}
	return []EntityChange{{Kind: "unit", ID: c.cn, Field: "rotation", Rotation: rot}}
}

type rotateDoodadCmd struct {
	cn             uint32
	oldRot, newRot float32
}

func (c *rotateDoodadCmd) Label() string           { return "Rotate doodad" }
func (c *rotateDoodadCmd) Apply(s *Session) error  { return setDoodadRot(s, c.cn, c.newRot) }
func (c *rotateDoodadCmd) Revert(s *Session) error { return setDoodadRot(s, c.cn, c.oldRot) }
func (c *rotateDoodadCmd) Affected(s *Session) []EntityChange {
	rot, ok := getDoodadRot(s, c.cn)
	if !ok {
		return nil
	}
	return []EntityChange{{Kind: "doodad", ID: c.cn, Field: "rotation", Rotation: rot}}
}

type scaleUnitCmd struct {
	cn          uint32
	oldScale    [3]float32
	newScale    [3]float32
	oldScaleRaw [3]float32 // captured PRE-mutation; Revert restores both fields
}

func (c *scaleUnitCmd) Label() string { return "Scale unit" }
func (c *scaleUnitCmd) Apply(s *Session) error {
	// Mirror Session.ScaleUnit's behavior: set Scale + clear scaleRaw so the
	// encoder uses Scale*128 rather than the stale preserved bytes.
	if err := setUnitScale(s, c.cn, c.newScale); err != nil {
		return err
	}
	return clearUnitScaleRaw(s, c.cn)
}
func (c *scaleUnitCmd) Revert(s *Session) error {
	// Restore both the public Scale field AND the preservation raw bytes so
	// a save after undo emits the original on-disk bit pattern.
	if err := setUnitScale(s, c.cn, c.oldScale); err != nil {
		return err
	}
	return setUnitScaleRaw(s, c.cn, c.oldScaleRaw)
}
func (c *scaleUnitCmd) Affected(s *Session) []EntityChange {
	sc, ok := getUnitScale(s, c.cn)
	if !ok {
		return nil
	}
	return []EntityChange{{Kind: "unit", ID: c.cn, Field: "scale", Scale: sc}}
}

type scaleDoodadCmd struct {
	cn       uint32
	oldScale [3]float32
	newScale [3]float32
}

func (c *scaleDoodadCmd) Label() string           { return "Scale doodad" }
func (c *scaleDoodadCmd) Apply(s *Session) error  { return setDoodadScale(s, c.cn, c.newScale) }
func (c *scaleDoodadCmd) Revert(s *Session) error { return setDoodadScale(s, c.cn, c.oldScale) }
func (c *scaleDoodadCmd) Affected(s *Session) []EntityChange {
	sc, ok := getDoodadScale(s, c.cn)
	if !ok {
		return nil
	}
	return []EntityChange{{Kind: "doodad", ID: c.cn, Field: "scale", Scale: sc}}
}

// swapTilesetCmd captures both directions of a tileset swap in full. The
// per-tile arrays are required because the SwapTileset remap is lossy
// (multiple old palette slots can collapse onto a single new slot, so
// inverting the from_to table wouldn't restore the original GroundTexture
// values). Storing full snapshots costs ~2 bytes × map vertex count per
// undo step — even a 480×480 map is under 500 KB, which is fine for an
// editor where each tileset swap is a deliberate user action.
type swapTilesetCmd struct {
	oldLetter byte
	oldGround []string
	oldCliff  []string
	oldTileG  []uint8
	oldTileC  []uint8
	newLetter byte
	newGround []string
	newCliff  []string
	newTileG  []uint8
	newTileC  []uint8
}

func (c *swapTilesetCmd) Label() string { return "Swap tileset" }
func (c *swapTilesetCmd) Apply(s *Session) error {
	if s.terrain == nil || s.info == nil {
		return fmt.Errorf("no map loaded")
	}
	applyTilesetSnapshot(s, c.newLetter, c.newGround, c.newCliff, c.newTileG, c.newTileC)
	s.dirtyTerrain = true
	s.dirtyInfo = true
	return nil
}
func (c *swapTilesetCmd) Revert(s *Session) error {
	if s.terrain == nil || s.info == nil {
		return fmt.Errorf("no map loaded")
	}
	applyTilesetSnapshot(s, c.oldLetter, c.oldGround, c.oldCliff, c.oldTileG, c.oldTileC)
	s.dirtyTerrain = true
	s.dirtyInfo = true
	return nil
}
func (c *swapTilesetCmd) Affected(s *Session) []EntityChange {
	return []EntityChange{{Kind: "terrain", ID: 0, Field: "tileset"}}
}

// ---------------------------------------------------------------------------
// Object Editor write commands. These operate on the per-kind shadow files
// (s.unitMods / s.itemMods / ...), not on placed entities. Each command
// carries a `kind` discriminator so Apply/Revert can dispatch into the right
// KindConfig via kindConfigFor. Affected events use Kind="<kind>_mod" with
// ID=0 — the tree refresh in the Object Editor is coarse (re-list, re-detail)
// rather than per-row; the JS side ignores ID and rebuilds based on Field.
// ---------------------------------------------------------------------------

// setObjectFieldCmd represents one field-edit on a stock or custom object of
// the given kind. hadOverride distinguishes "the field was previously a
// default" (Revert deletes the override entirely) from "the field had a
// previous override value" (Revert restores oldVal). Without this flag,
// Revert would always write oldVal back — including when oldVal was the
// empty string from a non-existent override, which would create a spurious
// "" override on Undo.
type setObjectFieldCmd struct {
	kind        string
	id          string
	column      string // FourCC or column-name; setObjectField normalizes
	oldVal      string
	newVal      string
	hadOverride bool
}

func (c *setObjectFieldCmd) Label() string { return "Edit " + c.kind + " field" }

func (c *setObjectFieldCmd) Apply(s *Session) error {
	cfg := kindConfigFor(c.kind)
	_, _, skin, err := setObjectField(s, cfg, c.id, c.column, c.newVal)
	if err != nil {
		return err
	}
	markObjectDirty(s, cfg, skin)
	return nil
}

func (c *setObjectFieldCmd) Revert(s *Session) error {
	cfg := kindConfigFor(c.kind)
	if !c.hadOverride {
		skin, err := clearObjectField(s, cfg, c.id, c.column)
		if err != nil {
			return err
		}
		markObjectDirty(s, cfg, skin)
	} else {
		_, _, skin, err := setObjectField(s, cfg, c.id, c.column, c.oldVal)
		if err != nil {
			return err
		}
		markObjectDirty(s, cfg, skin)
	}
	return nil
}

func (c *setObjectFieldCmd) Affected(s *Session) []EntityChange {
	return []EntityChange{{Kind: c.kind + "_mod", ID: 0, Field: c.column}}
}

// addCustomObjectCmd represents creating a new custom row in the kind's
// shadow. Apply appends a custom with empty overrides; Revert removes it.
// The "shadow stock id" check lives in addCustomObject so Apply's error
// path is the same whether the cmd ran from the mutator or from Redo.
type addCustomObjectCmd struct {
	kind   string
	newID  string
	baseID string
}

func (c *addCustomObjectCmd) Label() string { return "Add custom " + c.kind }

func (c *addCustomObjectCmd) Apply(s *Session) error {
	cfg := kindConfigFor(c.kind)
	if err := addCustomObject(s, cfg, c.newID, c.baseID); err != nil {
		return err
	}
	cfg.SetDirty(s, true)
	return nil
}

func (c *addCustomObjectCmd) Revert(s *Session) error {
	cfg := kindConfigFor(c.kind)
	_, savedSkin, _ := removeCustomObject(s, cfg, c.newID)
	cfg.SetDirty(s, true)
	if savedSkin != nil {
		s.setSkinDirtyLocked(cfg.Kind, true)
	}
	return nil
}

func (c *addCustomObjectCmd) Affected(s *Session) []EntityChange {
	return []EntityChange{{Kind: c.kind + "_mod", ID: 0, Field: "customs"}}
}

// deleteCustomObjectCmd removes a custom row from the shadow. We snapshot the
// full CustomObject (id, base id, overrides) — plus its war3mapSkin.w3* mirror
// (savedSkin, nil when there is none) — so Revert restores both tables verbatim.
// Without the snapshot, Undo would re-create the shell but lose any per-field
// edits (gameplay OR art/skin) the user had committed.
type deleteCustomObjectCmd struct {
	kind      string
	saved     w3objmod.CustomObject
	savedSkin *w3objmod.CustomObject
}

func (c *deleteCustomObjectCmd) Label() string { return "Delete custom " + c.kind }

func (c *deleteCustomObjectCmd) Apply(s *Session) error {
	cfg := kindConfigFor(c.kind)
	_, savedSkin, ok := removeCustomObject(s, cfg, c.saved.ID)
	if !ok {
		return fmt.Errorf("custom %q not found", c.saved.ID)
	}
	// Keep the snapshot current with what Apply actually removed so a
	// later Revert restores the right skin mirror even across redo.
	c.savedSkin = savedSkin
	cfg.SetDirty(s, true)
	if savedSkin != nil {
		s.setSkinDirtyLocked(cfg.Kind, true)
	}
	return nil
}

func (c *deleteCustomObjectCmd) Revert(s *Session) error {
	cfg := kindConfigFor(c.kind)
	reinsertCustomObject(s, cfg, c.saved, c.savedSkin)
	cfg.SetDirty(s, true)
	if c.savedSkin != nil {
		s.setSkinDirtyLocked(cfg.Kind, true)
	}
	return nil
}

func (c *deleteCustomObjectCmd) Affected(s *Session) []EntityChange {
	return []EntityChange{{Kind: c.kind + "_mod", ID: 0, Field: "customs"}}
}

// ---------------------------------------------------------------------------
// Low-level entity accessors. All called with s.mu held. They handle the
// dirty-flag flip for the affected file. Public ScaleUnit / RotateUnit / etc.
// methods use these via their Command type so Apply (Redo) and Revert use
// the same code path the original mutator did.
// ---------------------------------------------------------------------------

func setUnitPos(s *Session, cn uint32, pos [3]float32) error {
	if s.units == nil {
		return fmt.Errorf("no map loaded")
	}
	for i := range s.units.Entities {
		if s.units.Entities[i].CreationNumber == cn {
			s.units.Entities[i].Position = pos
			s.dirtyUnits = true
			return nil
		}
	}
	return fmt.Errorf("no unit with creation_number %d", cn)
}

func getUnitPos(s *Session, cn uint32) ([3]float32, bool) {
	if s.units == nil {
		return [3]float32{}, false
	}
	for i := range s.units.Entities {
		if s.units.Entities[i].CreationNumber == cn {
			return s.units.Entities[i].Position, true
		}
	}
	return [3]float32{}, false
}

func setDoodadPos(s *Session, cn uint32, pos [3]float32) error {
	if s.doodads == nil {
		return fmt.Errorf("no map loaded")
	}
	for i := range s.doodads.Doodads {
		if s.doodads.Doodads[i].CreationNumber == cn {
			s.doodads.Doodads[i].Position = pos
			s.dirtyDoodads = true
			return nil
		}
	}
	return fmt.Errorf("no doodad with creation_number %d", cn)
}

func getDoodadPos(s *Session, cn uint32) ([3]float32, bool) {
	if s.doodads == nil {
		return [3]float32{}, false
	}
	for i := range s.doodads.Doodads {
		if s.doodads.Doodads[i].CreationNumber == cn {
			return s.doodads.Doodads[i].Position, true
		}
	}
	return [3]float32{}, false
}

func setUnitRot(s *Session, cn uint32, rot float32) error {
	if s.units == nil {
		return fmt.Errorf("no map loaded")
	}
	for i := range s.units.Entities {
		if s.units.Entities[i].CreationNumber == cn {
			s.units.Entities[i].Rotation = rot
			s.dirtyUnits = true
			return nil
		}
	}
	return fmt.Errorf("no unit with creation_number %d", cn)
}

func getUnitRot(s *Session, cn uint32) (float32, bool) {
	if s.units == nil {
		return 0, false
	}
	for i := range s.units.Entities {
		if s.units.Entities[i].CreationNumber == cn {
			return s.units.Entities[i].Rotation, true
		}
	}
	return 0, false
}

func setDoodadRot(s *Session, cn uint32, rot float32) error {
	if s.doodads == nil {
		return fmt.Errorf("no map loaded")
	}
	for i := range s.doodads.Doodads {
		if s.doodads.Doodads[i].CreationNumber == cn {
			s.doodads.Doodads[i].Rotation = rot
			s.dirtyDoodads = true
			return nil
		}
	}
	return fmt.Errorf("no doodad with creation_number %d", cn)
}

func getDoodadRot(s *Session, cn uint32) (float32, bool) {
	if s.doodads == nil {
		return 0, false
	}
	for i := range s.doodads.Doodads {
		if s.doodads.Doodads[i].CreationNumber == cn {
			return s.doodads.Doodads[i].Rotation, true
		}
	}
	return 0, false
}

func setUnitScale(s *Session, cn uint32, sc [3]float32) error {
	if s.units == nil {
		return fmt.Errorf("no map loaded")
	}
	for i := range s.units.Entities {
		if s.units.Entities[i].CreationNumber == cn {
			s.units.Entities[i].Scale = sc
			s.dirtyUnits = true
			return nil
		}
	}
	return fmt.Errorf("no unit with creation_number %d", cn)
}

func getUnitScale(s *Session, cn uint32) ([3]float32, bool) {
	if s.units == nil {
		return [3]float32{}, false
	}
	for i := range s.units.Entities {
		if s.units.Entities[i].CreationNumber == cn {
			return s.units.Entities[i].Scale, true
		}
	}
	return [3]float32{}, false
}

func clearUnitScaleRaw(s *Session, cn uint32) error {
	if s.units == nil {
		return fmt.Errorf("no map loaded")
	}
	for i := range s.units.Entities {
		if s.units.Entities[i].CreationNumber == cn {
			unitsdoo.ClearScaleRaw(&s.units.Entities[i])
			return nil
		}
	}
	return fmt.Errorf("no unit with creation_number %d", cn)
}

func setUnitScaleRaw(s *Session, cn uint32, raw [3]float32) error {
	if s.units == nil {
		return fmt.Errorf("no map loaded")
	}
	for i := range s.units.Entities {
		if s.units.Entities[i].CreationNumber == cn {
			unitsdoo.SetScaleRaw(&s.units.Entities[i], raw)
			return nil
		}
	}
	return fmt.Errorf("no unit with creation_number %d", cn)
}

func getUnitScaleRaw(s *Session, cn uint32) [3]float32 {
	if s.units == nil {
		return [3]float32{}
	}
	for i := range s.units.Entities {
		if s.units.Entities[i].CreationNumber == cn {
			return unitsdoo.ScaleRaw(&s.units.Entities[i])
		}
	}
	return [3]float32{}
}

func setDoodadScale(s *Session, cn uint32, sc [3]float32) error {
	if s.doodads == nil {
		return fmt.Errorf("no map loaded")
	}
	for i := range s.doodads.Doodads {
		if s.doodads.Doodads[i].CreationNumber == cn {
			s.doodads.Doodads[i].Scale = sc
			s.dirtyDoodads = true
			return nil
		}
	}
	return fmt.Errorf("no doodad with creation_number %d", cn)
}

func getDoodadScale(s *Session, cn uint32) ([3]float32, bool) {
	if s.doodads == nil {
		return [3]float32{}, false
	}
	for i := range s.doodads.Doodads {
		if s.doodads.Doodads[i].CreationNumber == cn {
			return s.doodads.Doodads[i].Scale, true
		}
	}
	return [3]float32{}, false
}

// ---------------------------------------------------------------------------
// Session-level history machinery.
// ---------------------------------------------------------------------------

// recordCommand pushes a command onto the active group (if any) or directly
// onto the history stack. Called from mutators with s.mu held, AFTER the
// mutation has succeeded. Caller is responsible for firing notifications
// post-unlock — recordCommand only touches in-memory bookkeeping.
//
// Any new command invalidates the redo stack: if the user undoes a few
// steps and then makes a fresh edit, the previously-redoable commands
// are dropped (standard editor behavior).
func (s *Session) recordCommand(cmd Command) {
	if s.groupDepth > 0 {
		s.pendingGroup.cmds = append(s.pendingGroup.cmds, cmd)
		return
	}
	s.history = append(s.history, cmd)
	s.redoStack = nil
}

// BeginUndoGroup starts a transaction. All subsequent mutations until
// EndUndoGroup land in a single undo step labeled `label`. Nestable —
// only the outermost begin/end pair publishes to history. If `label`
// is empty, the group inherits the first inner command's label.
//
// The gizmo's drag-end commit loop wraps its N per-entity mutations
// in Begin/End so the entire drag becomes one Ctrl+Z step.
func (s *Session) BeginUndoGroup(label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.groupDepth == 0 {
		s.pendingGroup = &groupCmd{label: label}
	}
	s.groupDepth++
}

// EndUndoGroup closes the outermost group and pushes it to the history
// stack (if non-empty — empty groups are silently dropped so a "begin,
// no mutations, end" pair doesn't pollute the stack). Returns an error
// if called without a matching BeginUndoGroup.
func (s *Session) EndUndoGroup() error {
	s.mu.Lock()
	if s.groupDepth == 0 {
		s.mu.Unlock()
		return fmt.Errorf("EndUndoGroup without BeginUndoGroup")
	}
	s.groupDepth--
	if s.groupDepth > 0 {
		s.mu.Unlock()
		return nil
	}
	grp := s.pendingGroup
	s.pendingGroup = nil
	historyChanged := false
	if grp != nil && len(grp.cmds) > 0 {
		if grp.label == "" {
			grp.label = grp.cmds[0].Label()
		}
		// Multi-entity grouped commits read nicer with a count tag.
		if len(grp.cmds) > 1 && grp.label == grp.cmds[0].Label() {
			grp.label = fmt.Sprintf("%s ×%d", grp.label, len(grp.cmds))
		}
		s.history = append(s.history, grp)
		s.redoStack = nil
		historyChanged = true
	}
	s.mu.Unlock()
	if historyChanged {
		s.notifyHistoryChanged()
	}
	return nil
}

// Undo pops the top of the history stack and reverts it. No-op when the
// stack is empty or a group is currently open (preventing nested-state
// corruption — the user must close the group first).
func (s *Session) Undo() error {
	s.mu.Lock()
	if len(s.history) == 0 {
		s.mu.Unlock()
		return nil
	}
	if s.groupDepth > 0 {
		s.mu.Unlock()
		return fmt.Errorf("cannot undo while a group is open")
	}
	cmd := s.history[len(s.history)-1]
	s.history = s.history[:len(s.history)-1]
	if err := cmd.Revert(s); err != nil {
		// Push back so the user can retry / inspect.
		s.history = append(s.history, cmd)
		s.mu.Unlock()
		return err
	}
	s.redoStack = append(s.redoStack, cmd)
	notifs := cmd.Affected(s)
	// Mark dirty defensively — the in-memory state changed, so it now
	// differs from on-disk. We don't try to detect "undo brought state
	// back to the save baseline"; the user can Ctrl+S to clear dirty.
	prevDirty := s.dirtyUnits || s.dirtyDoodads || s.dirtyInfo
	s.dirtyUnits = true
	s.dirtyDoodads = true
	s.mu.Unlock()
	for _, n := range notifs {
		s.notifyEntityChanged(n)
	}
	if !prevDirty {
		s.notifyDirty(true)
	}
	s.notifyHistoryChanged()
	return nil
}

// Redo pops the top of the redo stack and re-applies it. No-op on empty
// stack or open group. The redo stack is wiped whenever a new mutation
// is recorded (standard editor behavior).
func (s *Session) Redo() error {
	s.mu.Lock()
	if len(s.redoStack) == 0 {
		s.mu.Unlock()
		return nil
	}
	if s.groupDepth > 0 {
		s.mu.Unlock()
		return fmt.Errorf("cannot redo while a group is open")
	}
	cmd := s.redoStack[len(s.redoStack)-1]
	s.redoStack = s.redoStack[:len(s.redoStack)-1]
	if err := cmd.Apply(s); err != nil {
		s.redoStack = append(s.redoStack, cmd)
		s.mu.Unlock()
		return err
	}
	s.history = append(s.history, cmd)
	notifs := cmd.Affected(s)
	prevDirty := s.dirtyUnits || s.dirtyDoodads || s.dirtyInfo
	s.dirtyUnits = true
	s.dirtyDoodads = true
	s.mu.Unlock()
	for _, n := range notifs {
		s.notifyEntityChanged(n)
	}
	if !prevDirty {
		s.notifyDirty(true)
	}
	s.notifyHistoryChanged()
	return nil
}

// ClearHistory drops both stacks. Called on Open/Close so a fresh map
// starts with no undoable history (and the previous map's history
// doesn't leak across).
func (s *Session) ClearHistory() {
	s.mu.Lock()
	hadHistory := len(s.history) > 0 || len(s.redoStack) > 0
	s.history = nil
	s.redoStack = nil
	s.groupDepth = 0
	s.pendingGroup = nil
	s.mu.Unlock()
	if hadHistory {
		s.notifyHistoryChanged()
	}
}

// AbortUndoGroup discards an open undo group WITHOUT publishing it to the
// history stack, collapsing any nesting straight to depth 0 and unblocking
// Undo/Redo. Returns an error if no group is open (mirrors EndUndoGroup).
//
// Recovery, not rollback: the commands recorded into the open group have
// ALREADY mutated session state and fired entity-changed events to both
// surfaces. Aborting only drops the group WRAPPER — the mutations stay in
// place (un-grouped, but never recorded onto the undo stack since a pending
// group's commands live only in pendingGroup until EndUndoGroup publishes
// them). Reverting them here would desync the frontend, so we don't; callers
// wanting true rollback should snapshot HistoryUndoCount() at begin-time and
// UndoTo() it instead. The purpose is to rescue a session whose undo/redo is
// wedged by a dangling group (e.g. an agent that began a group then crashed
// or cancelled before EndUndoGroup).
func (s *Session) AbortUndoGroup() error {
	s.mu.Lock()
	if s.groupDepth == 0 {
		s.mu.Unlock()
		return fmt.Errorf("AbortUndoGroup without BeginUndoGroup")
	}
	s.groupDepth = 0
	s.pendingGroup = nil
	s.mu.Unlock()
	// The undo/redo availability changed (it was blocked while the group was
	// open); notify so the UI re-enables its Edit menu / toolbar.
	s.notifyHistoryChanged()
	return nil
}

// CanUndo / CanRedo report whether the corresponding action is available.
// For UI enable/disable state (Edit menu, toolbar buttons).
func (s *Session) CanUndo() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.history) > 0 && s.groupDepth == 0
}

func (s *Session) CanRedo() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.redoStack) > 0 && s.groupDepth == 0
}

// HistoryUndoCount returns the current undo-stack depth. Used by modal
// dialogs (Map Info Sky picker) to snapshot the depth at open-time and then
// UndoTo that depth on Cancel — reverting any commands the dialog session
// recorded without inventing a per-dialog group abstraction.
func (s *Session) HistoryUndoCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.history)
}

// UndoTo undoes commands until the undo stack length is at most `target`.
// No-op when the stack is already at or below target. Returns the first
// error encountered (if any); successfully-undone commands stay reverted.
//
// Intended for modal "cancel reverts" flows where the dialog opened at
// `target` and may have recorded N commands since. NOT a public undo API —
// it can undo any commands recorded during the window, not just those from
// one user surface; callers should ensure the only mutator during the
// window is the one they're cancelling.
func (s *Session) UndoTo(target int) error {
	for {
		s.mu.RLock()
		n := len(s.history)
		s.mu.RUnlock()
		if n <= target {
			return nil
		}
		if err := s.Undo(); err != nil {
			return err
		}
	}
}

// DiscardTo reverts commands above `target` AND removes them from history
// entirely — neither the undo stack nor the redo stack retains them. Use
// for "cancel reverts" semantics where the user expects the dialog session
// to vanish, not just be reversible. Mirrors UndoTo's loop but pops without
// pushing to redo.
//
// Fires entity-changed events for each reverted command so the UI repaints,
// and a single history-changed event at the end. Any sub-command Revert
// failure stops the loop and returns the error; the partially-reverted
// state is what the user sees.
func (s *Session) DiscardTo(target int) error {
	var anyReverted bool
	for {
		s.mu.Lock()
		if len(s.history) <= target {
			s.mu.Unlock()
			if anyReverted {
				s.notifyHistoryChanged()
			}
			return nil
		}
		cmd := s.history[len(s.history)-1]
		s.history = s.history[:len(s.history)-1]
		if err := cmd.Revert(s); err != nil {
			// Restore stack ordering and bail. The cmd's state is partially
			// reverted; that's the same compromise Undo() makes on Revert
			// failure (it pushes back too).
			s.history = append(s.history, cmd)
			s.mu.Unlock()
			if anyReverted {
				s.notifyHistoryChanged()
			}
			return err
		}
		notifs := cmd.Affected(s)
		// Defensive dirty mark — state changed in memory, may differ from disk.
		// Save will recompute what to write.
		s.dirtyUnits = true
		s.dirtyDoodads = true
		s.mu.Unlock()
		for _, n := range notifs {
			s.notifyEntityChanged(n)
		}
		anyReverted = true
	}
}

// HistoryList returns a snapshot of both stacks for the UI to render.
// Active=true marks the next entry Undo would revert (the top of the
// undo stack). Both slices are oldest-first.
func (s *Session) HistoryList() HistoryState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := HistoryState{
		Undo:           make([]HistoryEntry, len(s.history)),
		Redo:           make([]HistoryEntry, len(s.redoStack)),
		OpenGroupDepth: s.groupDepth,
	}
	for i, c := range s.history {
		out.Undo[i] = HistoryEntry{Index: i, Label: c.Label(), Active: i == len(s.history)-1}
	}
	for i, c := range s.redoStack {
		out.Redo[i] = HistoryEntry{Index: i, Label: c.Label()}
	}
	return out
}

// OnHistoryChanged subscribes to history-stack mutations. Fired after any
// recordCommand, BeginUndoGroup commit, Undo, Redo, or ClearHistory call
// (whichever changed the visible stack state). Same listener-slice pattern
// as the other Session buses.
func (s *Session) OnHistoryChanged(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.historyListeners = append(s.historyListeners, fn)
}

func (s *Session) notifyHistoryChanged() {
	s.mu.RLock()
	listeners := make([]func(), len(s.historyListeners))
	copy(listeners, s.historyListeners)
	s.mu.RUnlock()
	for _, fn := range listeners {
		fn()
	}
}
