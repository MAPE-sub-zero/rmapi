package rm

import (
	"fmt"
	"os"
	"testing"
)

func testUnmarshalBinary(t *testing.T, fn string, ver Version) *Rm {
	b, err := os.ReadFile(fn)
	if err != nil {
		t.Errorf("can't open %s file", fn)
	}

	rm := New()
	err = rm.UnmarshalBinary(b)
	if err != nil {
		t.Error(err)
	}

	if rm.Version != ver {
		t.Error("wrong version parsed")
	}

	t.Log(rm)

	fmt.Println("unmarshaling complete")

	return rm
}

func TestUnmarshalBinaryV5(t *testing.T) {
	rm := testUnmarshalBinary(t, "test_v5.rm", V5)
	for _, layer := range rm.Layers {
		for _, line := range layer.Lines {
			if line.BrushSize != 2.0 {
				t.Error("Incorrectly parsing BrushSize")
			}
		}
	}
}

func TestUnmarshalBinaryV3(t *testing.T) {
	testUnmarshalBinary(t, "test_v3.rm", V3)
}

// TestV3V5NoHighlights regression-guards the v6 changes: the new
// Rm.Highlights field must stay empty for v3/v5 files (highlights are a
// v6-only concept; v5 highlighting is encoded as a stroke with
// HighlighterV5 brush, not a separate Highlight item).
func TestV3V5NoHighlights(t *testing.T) {
	for _, tc := range []struct {
		file string
		ver  Version
	}{
		{"test_v3.rm", V3},
		{"test_v5.rm", V5},
	} {
		rm := testUnmarshalBinary(t, tc.file, tc.ver)
		if len(rm.Highlights) != 0 {
			t.Errorf("%s: expected no v6 highlights, got %d", tc.file, len(rm.Highlights))
		}
		if len(rm.Layers) == 0 {
			t.Errorf("%s: expected at least one layer", tc.file)
		}
	}
}

// TestBrushColorPenColorEnum locks down the BrushColor integer values to
// match rmscene's PenColor enum so the wire format keeps decoding correctly.
// Reference: https://github.com/ricklupton/rmscene/blob/main/src/rmscene/scene_items.py
func TestBrushColorPenColorEnum(t *testing.T) {
	cases := map[string]struct {
		got  BrushColor
		want uint32
	}{
		"Black":            {Black, 0},
		"Grey":             {Grey, 1},
		"White":            {White, 2},
		"HighlightYellow":  {HighlightYellow, 3},
		"HighlightGreen":   {HighlightGreen, 4},
		"HighlightPink":    {HighlightPink, 5},
		"Blue":             {Blue, 6},
		"Red":              {Red, 7},
		"GreyOverlap":      {GreyOverlap, 8},
		"HighlightDynamic": {HighlightDynamic, 9},
		"Green2":           {Green2, 10},
		"Cyan":             {Cyan, 11},
		"Magenta":          {Magenta, 12},
		"Yellow2":          {Yellow2, 13},
	}
	for name, c := range cases {
		if uint32(c.got) != c.want {
			t.Errorf("%s: got %d, want %d", name, c.got, c.want)
		}
	}
}
