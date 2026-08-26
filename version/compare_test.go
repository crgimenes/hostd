package version

import "testing"

func TestReleasesAreOrderedTheWayTheirTagsAre(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.0.3", "v0.0.4", -1},
		{"v0.0.4", "v0.0.3", 1},
		{"v0.0.4", "v0.0.4", 0},
		{"v0.9.0", "v0.10.0", -1},
		{"v1.0.0", "v0.99.99", 1},
		{"0.0.4", "v0.0.4", 0},
	}
	for _, c := range cases {
		got, comparable := Compare(c.a, c.b)
		if !comparable {
			t.Errorf("%s vs %s could not be compared", c.a, c.b)
			continue
		}
		if got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// Ten is not three, and a version scheme that read it as three would rank
// v0.10.0 below v0.9.0 — which is the ordering mistake that walks a fleet
// backwards while every line on the screen says upgrade.
func TestTenIsNewerThanNine(t *testing.T) {
	order, comparable := Compare("v0.10.0", "v0.9.0")
	if !comparable || order <= 0 {
		t.Fatalf("Compare(v0.10.0, v0.9.0) = %d, %v", order, comparable)
	}
}

// "Cannot tell" is an answer, and it is the one that stops rather than acts.
// Every case here would otherwise have to be guessed, and a wrong guess is a
// silent downgrade.
func TestWhatIsNotAReleaseVersionIsNotRanked(t *testing.T) {
	for _, c := range [][2]string{
		{"dev", "v0.0.4"},
		{"v0.0.4", "dev"},
		{"", "v0.0.4"},
		{"v0.0", "v0.0.4"},
		{"v0.0.4.1", "v0.0.4"},
		{"v0.0.4-rc1", "v0.0.4"},
		{"v0.0.x", "v0.0.4"},
		{"v0.0.+4", "v0.0.4"},
		{"hostd v0.0.4 (protocol 1, schema 1)", "v0.0.4"},
	} {
		_, comparable := Compare(c[0], c[1])
		if comparable {
			t.Errorf("%q and %q were ranked against each other", c[0], c[1])
		}
	}
}
