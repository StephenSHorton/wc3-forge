package main

import (
	"testing"

	"github.com/StephenSHorton/wc3-forge/internal/formats/w3objmod"
	"github.com/StephenSHorton/wc3-forge/internal/formats/wts"
)

// TestApplyUnitOverrides_ArtOverrides verifies the per-unit render overrides
// added for custom skins: model file (umdl, extension-stripped to the stem),
// model scale (usca), and vertex tint (uclr/uclg/uclb). An override that
// doesn't touch tint must leave the base tint untouched (the renderer treats an
// all-zero tint as "no tint", so a stock 0,0,0 must stay 0,0,0 rather than
// being forced anywhere).
func TestApplyUnitOverrides_ArtOverrides(t *testing.T) {
	var ms wts.Strings // nil is fine: no TRIGSTR/WESTRING resolution needed here

	base := UnitTypeInfo{File: `units\Human\Peasant\Peasant`, ModelScale: 1}
	out := applyUnitOverrides(base, w3objmod.Overrides{
		"umdl": `war3mapImported\Sylvanas.mdl`,
		"usca": "1.5",
		"uclr": "200",
		"uclg": "150",
		"uclb": "255",
	}, ms)

	if out.File != `war3mapImported\Sylvanas` {
		t.Errorf("model override not applied/stripped: got %q", out.File)
	}
	if out.ModelScale != 1.5 {
		t.Errorf("scale override not applied: got %v", out.ModelScale)
	}
	if out.Red != 200 || out.Green != 150 || out.Blue != 255 {
		t.Errorf("tint override not applied: got R=%d G=%d B=%d", out.Red, out.Green, out.Blue)
	}

	// A scale-only override must not disturb the (stock) zero tint.
	stock := UnitTypeInfo{File: `units\Human\Peasant\Peasant`, ModelScale: 1}
	out2 := applyUnitOverrides(stock, w3objmod.Overrides{"usca": "2"}, ms)
	if out2.Red != 0 || out2.Green != 0 || out2.Blue != 0 {
		t.Errorf("unrelated override disturbed tint: got R=%d G=%d B=%d", out2.Red, out2.Green, out2.Blue)
	}
}
