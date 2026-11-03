package render

import (
	"math"
	"testing"
)

func TestByteSize(t *testing.T) {
	for _, tc := range []struct {
		in   uint64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{9, "9 B"},
		{10, "10 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1500, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{2*1024*1024 + 512*1024, "2.5 MiB"},
		{1024 * 1024 * 1024, "1.0 GiB"},
		{1024 * 1024 * 1024 * 1024, "1.0 TiB"},
		{1024 * 1024 * 1024 * 1024 * 1024, "1.0 PiB"},
		{1024 * 1024 * 1024 * 1024 * 1024 * 1024, "1.0 EiB"},
		{10 * 1024, "10 KiB"},
		{10*1024 - 1, "10 KiB"},
		{math.MaxInt64, "8.0 EiB"},
		{math.MaxUint64, "16 EiB"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			if got := ByteSize(tc.in); got != tc.want {
				t.Errorf("ByteSize(%d) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
