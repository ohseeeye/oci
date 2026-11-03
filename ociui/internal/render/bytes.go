// Adapted from Explorer's copied humanize formatter; see ../../UPSTREAM.md.
//
// Package render provides shared presentation helpers for Explorer and archive
// rendering. It depends only on the standard library.
package render

import (
	"fmt"
	"math"
)

// ByteSize formats a byte count with IEC units, retaining Explorer's existing
// rounding and precision (for example, "1.5 KiB" and "10 MiB").
func ByteSize(size uint64) string {
	if size < 10 {
		return fmt.Sprintf("%d B", size)
	}
	const base = 1024
	units := [...]string{"B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}

	exponent := math.Floor(math.Log(float64(size)) / math.Log(base))
	value := math.Floor(float64(size)/math.Pow(base, exponent)*10+0.5) / 10
	format := "%.0f %s"
	if value < 10 {
		format = "%.1f %s"
	}
	return fmt.Sprintf(format, value, units[int(exponent)])
}
