package pps

import "testing"

func TestParseEdge(t *testing.T) {
	for _, tc := range []struct {
		name string
		want Edge
	}{
		{"assert", Assert},
		{"clear", Clear},
	} {
		got, err := ParseEdge(tc.name)
		if err != nil || got != tc.want || got.String() != tc.name {
			t.Errorf("ParseEdge(%q) = %v, %v", tc.name, got, err)
		}
	}
	if _, err := ParseEdge("rising"); err == nil {
		t.Fatal("bad edge accepted")
	}
}
