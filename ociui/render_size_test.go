package ociui

import "testing"

func TestRenderSize(t *testing.T) {
	for _, tc := range []struct {
		size int64
		want string
	}{
		{0, ""}, // Unspecified descriptor size stays absent from the header.
		{1, "1 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
	} {
		if got := renderSize(tc.size); got != tc.want {
			t.Errorf("renderSize(%d) = %q, want %q", tc.size, got, tc.want)
		}
	}
}
