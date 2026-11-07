package cli

import (
	"testing"
	"time"
)

func TestClockParsesAPITimes(t *testing.T) {
	_, a, _ := appFor(t, "")
	want := time.Date(2026, 10, 8, 13, 41, 46, 0, time.UTC).Local().Format("2006-01-02 15:04:05")
	for _, ts := range []string{"2026-10-08 13:41:46.029999+00", "2026-10-08T13:41:46Z", "2026-10-08T13:41:46.5+00:00"} {
		if got := a.clock(ts); got != want {
			t.Errorf("clock(%q) = %s, want %s", ts, got, want)
		}
	}
}
