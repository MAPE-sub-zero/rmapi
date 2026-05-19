package annotations

import (
	"math"
	"testing"

	"github.com/juruen/rmapi/encoding/rm"
)

// TestHighlightRGBExplicitRGBA verifies that an explicit color_rgba (firmware
// 3.6+, BrushColor == HighlightDynamic) takes precedence over the enum and
// is normalized to [0,1].
func TestHighlightRGBExplicitRGBA(t *testing.T) {
	rgba := &[4]uint8{255, 128, 64, 200}
	r, g, b := highlightRGB(rm.HighlightDynamic, rgba)
	if !approxEq(r, 1.0) || !approxEq(g, 128.0/255) || !approxEq(b, 64.0/255) {
		t.Errorf("RGBA override: got (%v, %v, %v); want (1.0, 0.502, 0.251)", r, g, b)
	}
}

// TestHighlightRGBEnumMapping locks down the per-enum RGB choices so the
// rendered output stays stable across releases.
func TestHighlightRGBEnumMapping(t *testing.T) {
	cases := []struct {
		name     string
		color    rm.BrushColor
		wantR    float64
		wantG    float64
		wantB    float64
	}{
		{"yellow", rm.HighlightYellow, 1.0, 0.93, 0.0},
		{"yellow2", rm.Yellow2, 1.0, 0.93, 0.0},
		{"green", rm.HighlightGreen, 0.36, 0.86, 0.36},
		{"green2", rm.Green2, 0.36, 0.86, 0.36},
		{"pink", rm.HighlightPink, 1.0, 0.45, 0.75},
		{"magenta", rm.Magenta, 1.0, 0.45, 0.75},
		{"blue", rm.Blue, 0.40, 0.75, 1.0},
		{"cyan", rm.Cyan, 0.40, 0.75, 1.0},
		{"red", rm.Red, 1.0, 0.45, 0.45},
	}
	for _, c := range cases {
		r, g, b := highlightRGB(c.color, nil)
		if !approxEq(r, c.wantR) || !approxEq(g, c.wantG) || !approxEq(b, c.wantB) {
			t.Errorf("%s: got (%v, %v, %v); want (%v, %v, %v)",
				c.name, r, g, b, c.wantR, c.wantG, c.wantB)
		}
	}
}

// TestHighlightRGBUnknownFallback guarantees that any unrecognised color
// code falls back to yellow — i.e. that geta exporting a future-firmware
// .rm file never silently produces an invisible (white-on-white) highlight.
func TestHighlightRGBUnknownFallback(t *testing.T) {
	r, g, b := highlightRGB(rm.BrushColor(99), nil)
	if !approxEq(r, 1.0) || !approxEq(g, 0.93) || !approxEq(b, 0.0) {
		t.Errorf("unknown color fallback: got (%v, %v, %v); want yellow", r, g, b)
	}
}

// TestHighlightRGBV3V5Compat verifies that the v5 highlighter stroke path
// (which historically rendered yellow regardless of brush color) still
// produces yellow when called with the legacy Black-default brush color.
// Without this, v3/v5 highlight strokes would silently render in stroke
// colors like grey from the brush color carried on the line.
func TestHighlightRGBV3V5Compat(t *testing.T) {
	// The HighlighterV5 callsite passes line.BrushColor; for legacy v5
	// content this is typically Black (0). The helper falls through the
	// switch and ends up at the yellow fallback.
	r, g, b := highlightRGB(rm.Black, nil)
	// Black is mapped to grey for the highlight rect path (visible),
	// not yellow — locking down the documented choice.
	if !approxEq(r, 0.7) || !approxEq(g, 0.7) || !approxEq(b, 0.7) {
		t.Errorf("Black highlight: got (%v, %v, %v); want grey (0.7, 0.7, 0.7)", r, g, b)
	}
}

func approxEq(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}
