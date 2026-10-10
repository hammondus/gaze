package threshold

import "testing"

func TestOf(t *testing.T) {
	th := T{Warn: 80, Crit: 90}
	for _, c := range []struct {
		v    float64
		want Level
	}{
		{0, OK}, {79.9, OK}, {80, Warn}, {89.9, Warn}, {90, Crit}, {100, Crit},
	} {
		if got := th.Of(c.v); got != c.want {
			t.Errorf("Of(%v) = %v, want %v", c.v, got, c.want)
		}
	}
}

// TestOrdered catches a table edit that puts a warning above its critical
// point, which would skip amber entirely.
func TestOrdered(t *testing.T) {
	for name, th := range map[string]T{"cpu": CPU, "memory": Memory, "swap": Swap, "disk": Disk} {
		if th.Warn >= th.Crit {
			t.Errorf("%s: warn %v is not below crit %v", name, th.Warn, th.Crit)
		}
	}
}
