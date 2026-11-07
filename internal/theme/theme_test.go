package theme

import "testing"

func TestDetectDark(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	yes := func() bool { return true }
	no := func() bool { return false }
	cases := []struct {
		env      map[string]string
		fallback func() bool
		want     bool
	}{
		{nil, nil, true},
		{nil, no, false},
		{map[string]string{"SENDIO_THEME": "light"}, yes, false},
		{map[string]string{"SENDIO_THEME": "dark", "COLORFGBG": "0;15"}, no, true},
		{map[string]string{"COLORFGBG": "15;0"}, no, true},
		{map[string]string{"COLORFGBG": "0;15"}, yes, false},
		{map[string]string{"COLORFGBG": "0;default;7"}, yes, false},
	}
	for _, c := range cases {
		if got := DetectDark(env(c.env), c.fallback); got != c.want {
			t.Errorf("%v: got %v, want %v", c.env, got, c.want)
		}
	}
	if !TrueColor(env(map[string]string{"COLORTERM": "truecolor"})) || TrueColor(env(nil)) {
		t.Error("TrueColor")
	}
}

func TestPaletteKeepsBrand(t *testing.T) {
	for _, dark := range []bool{true, false} {
		p := For(dark)
		if p.Block != SignalOrange || p.OnBlock != TerminalBlack || p.Text != "" {
			t.Errorf("dark=%v: %+v", dark, p)
		}
		if p.Error == p.Accent || p.Warn == p.Accent {
			t.Errorf("dark=%v: error/warn must differ from orange", dark)
		}
	}
	if Huh(For(true)) == nil {
		t.Fatal("nil huh theme")
	}
}
