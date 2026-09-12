package migration

import "testing"

// Versions were compared as strings, which is correct only while every
// component stays a single digit. "1.10.0" < "1.5.1" is true lexicographically,
// so the first release after 1.9 would have replayed to1_5_1 against every
// database -- stripping the explicit CA from every TLS client config that had
// one.
func TestCompareVersions(t *testing.T) {
	testCases := []struct {
		a, b string
		want int
	}{
		// The case the string compare got wrong.
		{"1.10.0", "1.5.1", 1},
		{"1.5.1", "1.10.0", -1},
		{"1.9.0", "1.10.0", -1},

		// Ordinary ordering.
		{"1.5.0", "1.5.1", -1},
		{"1.5.2", "1.5.1", 1},
		{"1.6.0", "1.5.2", 1},
		{"2.0.0", "1.99.99", 1},

		// Equality, including a missing component.
		{"1.5.1", "1.5.1", 0},
		{"1.2", "1.2.0", 0},
		{"1", "1.0.0", 0},

		// An unset version is the oldest thing there is, so every migration
		// still runs against a database that has never recorded one.
		{"", "1.0.0", -1},
		{"", "", 0},

		// Shapes a release tag can take.
		{"v1.6.0", "1.6.0", 0},
		{"1.6.0-rc1", "1.6.0", 0},

		// Garbage must sort oldest rather than panic.
		{"not-a-version", "1.0.0", -1},
		{"1.x.3", "1.0.0", 0},
	}

	for _, tc := range testCases {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// The 1.3 step was gated on dbVersion[0:3] == "1.2", which panics outright on
// any stored value shorter than three characters.
func TestMajorMinorHandlesShortAndEmptyVersions(t *testing.T) {
	testCases := []struct {
		version          string
		wantMaj, wantMin int
	}{
		{"1.2", 1, 2},
		{"1.2.9", 1, 2},
		{"1.10.0", 1, 10},
		{"", 0, 0},
		{"1", 1, 0},
		{"x", 0, 0},
	}

	for _, tc := range testCases {
		maj, min := majorMinor(tc.version)
		if maj != tc.wantMaj || min != tc.wantMin {
			t.Errorf("majorMinor(%q) = %d.%d, want %d.%d", tc.version, maj, min, tc.wantMaj, tc.wantMin)
		}
	}
}

// The 1.2-series gate must match the whole series and nothing outside it. A
// prefix comparison on "1.2" also matched "1.20".
func TestOneTwoSeriesGate(t *testing.T) {
	inSeries := func(v string) bool {
		major, minor := majorMinor(v)
		return major == 1 && minor == 2
	}

	for _, v := range []string{"1.2", "1.2.0", "1.2.7"} {
		if !inSeries(v) {
			t.Errorf("%q should be treated as the 1.2 series", v)
		}
	}
	for _, v := range []string{"1.20.0", "1.3.0", "1.1.9", "2.2.0", ""} {
		if inSeries(v) {
			t.Errorf("%q should not be treated as the 1.2 series", v)
		}
	}
}
