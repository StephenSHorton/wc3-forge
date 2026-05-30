package forge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"

	// stdlib image decoders — registered for image.Decode side effects so the
	// texture-baking path can read PNG/JPEG/GIF source images. golang.org/x/image
	// (TGA/BMP/etc.) is deliberately NOT pulled in: an unresolved/undecodable
	// source texture falls back to a placeholder BLP + a warning rather than
	// adding a new module dependency (see go.mod policy in the task brief).
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/StephenSHorton/wc3-forge/internal/blp"
	"github.com/StephenSHorton/wc3-forge/internal/mdx"
	"github.com/StephenSHorton/wc3-forge/internal/mesh3d"

	"github.com/StephenSHorton/wc3-forge/internal/bridge"
)

// importBaseDir is the in-archive directory every imported model + texture
// lands under, matching the World Editor's Import Manager default and the
// war3mapImported\ convention war3map.imp registers.
const importBaseDir = `war3mapImported`

// parseMesh dispatches to the right mesh3d parser by file extension, sniffing
// content where the extension is ambiguous or missing. baseDir lets the OBJ /
// glTF parsers locate sibling material/texture files relative to the source.
func parseMesh(path string, data []byte, baseDir string) (*mesh3d.Mesh, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".obj":
		return mesh3d.ParseOBJ(data, baseDir)
	case ".gltf", ".glb":
		return mesh3d.ParseGLTF(data, baseDir)
	case ".stl":
		return mesh3d.ParseSTL(data)
	}
	// Unknown/empty extension: sniff the magic. GLB starts with "glTF";
	// ASCII STL starts with "solid"; otherwise assume binary glTF vs STL is
	// indistinguishable enough that we try glTF (covers .glb mislabeled),
	// then fall back to OBJ as the text default.
	if len(data) >= 4 && string(data[:4]) == "glTF" {
		return mesh3d.ParseGLTF(data, baseDir)
	}
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("solid")) {
		return mesh3d.ParseSTL(data)
	}
	return nil, fmt.Errorf("unsupported model format %q (expected .obj/.gltf/.glb/.stl)", ext)
}

// convertModel runs the shared mesh -> (MDX bytes + baked BLP textures)
// pipeline. It is the single conversion code path both the Wails App method
// (ConvertAndImportModel) and the MCP handler (models.import) funnel through.
//
//   - picks the parser by extension (sniffing when ambiguous)
//   - mesh3d.Normalize (axis remap, V-flip, normal recompute, weld, scale,
//     >65535-vertex geoset split)
//   - for each unique non-empty TextureRef present in Images: image.Decode +
//     blp.Encode (placeholder BLP + warning when absent/undecodable)
//   - synthesizes collision-free war3mapImported\<base>.{blp,mdx} names against
//     the loaded archive (surfacing a warning on any suffixing)
//   - builds the mdx.Model (mesh3d.Submesh -> mdx.Submesh, TexturePath = the
//     chosen in-archive BLP) and EncodeStatic
//
// The returned mdxData is written under "<base>.mdx" (the first element the
// caller registers); textures carry their own already-resolved in-archive Name.
func convertModel(path string, data []byte, scale float64, upAxis string, flipV bool) (mdxData []byte, textures []ImportTexture, warnings []string, err error) {
	mesh, perr := parseMesh(path, data, filepath.Dir(path))
	if perr != nil {
		return nil, nil, nil, perr
	}

	sc := float32(scale)
	if sc == 0 {
		sc = 1
	}
	// Auto-fit. Raw model units are usually tiny in WC3's coordinate space (a
	// unit/doodad is ~100+ studs across), so a scale-1 import of, say, a
	// ±1-unit cube renders ~2 studs wide — present but invisibly small on the
	// map. Normalize the model's largest source dimension to a doodad-ish
	// target size; the user's Scale then multiplies that. A model already
	// authored at WC3 size just gets a ~1x auto-fit factor. (maxDim is
	// rotation-invariant, so computing it pre-Normalize is correct.)
	const importTargetSize = 160.0
	mn, mx := mesh.ComputeAABB()
	maxDim := float32(0)
	for i := 0; i < 3; i++ {
		if d := mx[i] - mn[i]; d > maxDim {
			maxDim = d
		}
	}
	if maxDim > 0 {
		sc *= importTargetSize / maxDim
	}
	axis := upAxis
	if axis == "" {
		axis = "y"
	}
	normWarnings, nerr := mesh3d.Normalize(mesh, mesh3d.Options{UpAxis: axis, Scale: sc, FlipV: flipV})
	if nerr != nil {
		return nil, nil, nil, fmt.Errorf("normalize: %w", nerr)
	}
	warnings = append(warnings, normWarnings...)

	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if base == "" {
		base = "model"
	}
	base = sanitizeImportName(base)

	// taken tracks the in-archive paths this conversion has already minted so a
	// model + its textures don't collide with each other (and we suffix against
	// the existing archive too).
	taken := map[string]bool{}

	// Bake each unique non-empty TextureRef that resolved to bytes in Images.
	// texPathFor maps a TextureRef -> the chosen in-archive BLP path so multiple
	// submeshes sharing a texture reuse one BLP entry.
	texPathFor := map[string]string{}
	for _, sm := range mesh.Submeshes {
		ref := sm.TextureRef
		if ref == "" {
			continue // untextured submesh (e.g. STL)
		}
		if _, done := texPathFor[ref]; done {
			continue
		}
		texBase := sanitizeImportName(strings.TrimSuffix(ref, filepath.Ext(ref)))
		if texBase == "" {
			texBase = base
		}
		blpPath, suffixed := uniqueImportPath(texBase, ".blp", taken)
		if suffixed {
			warnings = append(warnings, fmt.Sprintf("texture name collided in archive; imported as %s", blpPath))
		}

		var blpData []byte
		if raw, ok := mesh.Images[ref]; ok && len(raw) > 0 {
			img, _, derr := image.Decode(bytes.NewReader(raw))
			if derr != nil {
				blpData = blp.Placeholder(64)
				warnings = append(warnings, fmt.Sprintf("could not decode texture %q (%v); used placeholder", ref, derr))
			} else if enc, eerr := blp.Encode(img); eerr != nil {
				blpData = blp.Placeholder(64)
				warnings = append(warnings, fmt.Sprintf("could not encode texture %q (%v); used placeholder", ref, eerr))
			} else {
				blpData = enc
			}
		} else {
			// TextureRef present but not in Images -> couldn't locate the file.
			blpData = blp.Placeholder(64)
			warnings = append(warnings, fmt.Sprintf("texture %q not found alongside model; used placeholder", ref))
		}
		if blpData == nil {
			return nil, nil, nil, fmt.Errorf("blp.Placeholder failed for texture %q", ref)
		}

		texPathFor[ref] = blpPath
		textures = append(textures, ImportTexture{Name: blpPath, Data: blpData})
	}

	// Build the mdx.Model. Every submesh needs a TexturePath; untextured
	// submeshes (STL, or a model whose only material had no diffuse) get a
	// shared placeholder BLP so the MDX layer always references a valid TEXS.
	var placeholderPath string
	model := &mdx.Model{
		Name:      base,
		Positions: mesh.Positions,
		Normals:   mesh.Normals,
		UVs:       mesh.UVs,
	}
	for _, sm := range mesh.Submeshes {
		texPath := texPathFor[sm.TextureRef]
		if texPath == "" {
			if placeholderPath == "" {
				p, suffixed := uniqueImportPath(base+"_tex", ".blp", taken)
				if suffixed {
					warnings = append(warnings, fmt.Sprintf("placeholder texture name collided in archive; imported as %s", p))
				}
				ph := blp.Placeholder(64)
				if ph == nil {
					return nil, nil, nil, fmt.Errorf("blp.Placeholder failed for untextured submesh")
				}
				placeholderPath = p
				textures = append(textures, ImportTexture{Name: p, Data: ph})
				warnings = append(warnings, "model has untextured geometry; used placeholder texture")
			}
			texPath = placeholderPath
		}
		model.Submeshes = append(model.Submeshes, mdx.Submesh{
			Indices:     sm.Indices,
			TexturePath: texPath,
		})
	}

	// The mdx name is chosen by the caller (ConvertAndImport) against the live
	// archive + the texture names just minted, so convertModel stays a pure
	// mesh -> bytes transform and the single Current.ImportModel call owns all
	// the in-archive paths.
	mdxData, eerr := mdx.EncodeStatic(model)
	if eerr != nil {
		return nil, nil, nil, fmt.Errorf("encode mdx: %w", eerr)
	}
	return mdxData, textures, warnings, nil
}

// ConvertAndImport is the single shared entrypoint both control surfaces use:
// it runs convertModel and then commits the result through Current.ImportModel
// in one call, so app.go and the MCP handler share ONE code path. path is the
// on-disk source file; data is its bytes (read by the caller, matching the
// path-in / Go-reads-bytes ingress idiom).
func ConvertAndImport(path string, data []byte, scale float64, upAxis string, flipV bool) (ImportModelResult, []string, error) {
	mdxData, textures, warnings, err := convertModel(path, data, scale, upAxis, flipV)
	if err != nil {
		return ImportModelResult{}, warnings, err
	}
	base := sanitizeImportName(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	if base == "" {
		base = "model"
	}
	// Recompute the mdx name against the live archive so it doesn't collide
	// with an existing war3mapImported\<base>.mdx. Textures already carry
	// fully-resolved names from convertModel.
	taken := map[string]bool{}
	for _, t := range textures {
		taken[strings.ToLower(t.Name)] = true
	}
	mdxName, suffixed := uniqueImportPath(base, ".mdx", taken)
	if suffixed {
		warnings = append(warnings, fmt.Sprintf("model name collided in archive; imported as %s", mdxName))
	}
	res, ierr := Current.ImportModel(ImportModelReq{
		ModelName: mdxName,
		MdxData:   mdxData,
		Textures:  textures,
	})
	if ierr != nil {
		return ImportModelResult{}, warnings, ierr
	}
	return res, warnings, nil
}

// sanitizeImportName strips path separators + characters that don't belong in
// an in-archive filename, leaving a bare base name. Keeps the result non-empty
// callers' responsibility.
func sanitizeImportName(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, `\`, "_")
	s = strings.ReplaceAll(s, " ", "_")
	return strings.TrimSpace(s)
}

// uniqueImportPath builds an in-archive war3mapImported\<base><ext> path that
// collides neither with the already-loaded map's files nor with names minted
// earlier in this same conversion (tracked in taken). On collision it appends
// _1, _2, … and reports suffixed=true so the caller can surface a warning.
// taken is updated with the chosen (lowercased) path.
func uniqueImportPath(base, ext string, taken map[string]bool) (path string, suffixed bool) {
	// In-archive paths use the WC3 backslash convention regardless of host OS
	// (filepath.Join would emit "/" on macOS/Linux). imp.Add + folderSource.write
	// both re-normalize, but the path we hand back to the frontend / object
	// "file" field must be the backslash form.
	candidate := func(n int) string {
		if n == 0 {
			return importBaseDir + `\` + base + ext
		}
		return fmt.Sprintf(`%s\%s_%d%s`, importBaseDir, base, n, ext)
	}
	for n := 0; ; n++ {
		cand := candidate(n)
		key := strings.ToLower(cand)
		if taken[key] {
			continue
		}
		if Current.archiveHas(cand) {
			continue
		}
		taken[key] = true
		return cand, n != 0
	}
}

// archiveHas reports whether the loaded map's source already contains name.
// Used by import-name collision-suffixing. Returns false when nothing's loaded.
func (s *Session) archiveHas(name string) bool {
	_, ok, err := s.ReadFile(name)
	return err == nil && ok
}

// modelsImportParams is the wire shape for models.import. Field names match the
// Go json tags consumed by mcp/src/tools.ts.
type modelsImportParams struct {
	SourcePath       string  `json:"source_path"`
	Scale            float64 `json:"scale"`
	UpAxis           string  `json:"up_axis"`
	FlipV            bool    `json:"flip_v"`
	TargetObjectKind string  `json:"target_object_kind"`
	TargetObjectID   string  `json:"target_object_id"`
}

type modelsImportResult struct {
	ModelPath    string   `json:"model_path"`
	TexturePaths []string `json:"texture_paths"`
	Warnings     []string `json:"warnings"`
}

// handleModelsImport is the MCP handler for models.import. It reads the source
// file Go-side, runs the shared ConvertAndImport pipeline, and — when a target
// object is named — chains Current.SetObjectField to set the object's "file"
// field to the imported model path (the same mutator the Object Editor uses).
func handleModelsImport(params json.RawMessage) (any, error) {
	var p modelsImportParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}
	if p.SourcePath == "" {
		return nil, fmt.Errorf("source_path is required")
	}
	data, err := os.ReadFile(p.SourcePath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p.SourcePath, err)
	}

	res, warnings, err := ConvertAndImport(p.SourcePath, data, p.Scale, p.UpAxis, p.FlipV)
	if err != nil {
		return nil, err
	}

	// Optional auto-target: assign the imported model to an object's "file"
	// field. Resolve the kind through the same KindConfig lookup the object
	// handlers use, then route through SetObjectField (the undoable mutator).
	if p.TargetObjectKind != "" || p.TargetObjectID != "" {
		if p.TargetObjectKind == "" || p.TargetObjectID == "" {
			return nil, fmt.Errorf("target_object_kind and target_object_id must both be set")
		}
		cfg, kerr := resolveKind(p.TargetObjectKind)
		if kerr != nil {
			return nil, kerr
		}
		// Pre-warm this kind's base SLK/metadata cache BEFORE SetObjectField.
		// SetObjectField calls loadObjectBase while holding s.mu (write lock);
		// the first-ever load runs readBaseAsset, which takes Current.mu.RLock()
		// — the same goroutine then deadlocks on its own write lock. Warming the
		// cache here (no lock held) makes that first load a no-op inside the
		// mutator, so the assignment can't wedge the session.
		loadObjectBase(cfg)
		if serr := Current.SetObjectField(cfg, p.TargetObjectID, "file", res.ModelPath); serr != nil {
			return nil, fmt.Errorf("assign model to %s %q: %w", p.TargetObjectKind, p.TargetObjectID, serr)
		}
	}

	return modelsImportResult{
		ModelPath:    res.ModelPath,
		TexturePaths: res.TexturePaths,
		Warnings:     warnings,
	}, nil
}

// registerModelHandlers wires the model-import MCP route. Called from
// RegisterAll, mirroring registerObjectKind / registerTriggerHandlers.
func registerModelHandlers(reg func(method string, h bridge.Handler)) {
	reg("models.import", handleModelsImport)
}
