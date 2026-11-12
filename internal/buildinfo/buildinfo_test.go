package buildinfo

import "testing"

func TestBelow(t *testing.T) {
	cases := []struct {
		version, minimum string
		below, ok        bool
	}{
		{"0.1.4", "0.2.0", true, true},
		{"v0.2.0", "0.2.0", false, true},
		{"0.2.0-rc.1", "0.2.0", true, true},
		{"0.2.0+build.5", "0.2.0", false, true},
		{"1.0.0", "0.9.9", false, true},
		{"0.0.0-dev", "0.1.0", true, true},
		{"0.2.0-rc.1", "0.2.0-rc.2", true, true},
		{"garbage", "0.2.0", false, false},
		{"0.2.0", "", false, false},
	}

	for _, testCase := range cases {
		below, ok := Below(testCase.version, testCase.minimum)
		if below != testCase.below || ok != testCase.ok {
			t.Errorf("Below(%q, %q) = %v, %v; want %v, %v", testCase.version, testCase.minimum, below, ok, testCase.below, testCase.ok)
		}
	}
}
