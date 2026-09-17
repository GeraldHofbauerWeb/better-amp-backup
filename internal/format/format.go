// Package format renders numbers the way an operator reads them.
//
// It exists because the same byte formatter had been written twice already --
// once for the CLI's output and once for a disk-space error deep inside the
// backup -- and a third copy was about to be written for the web interface.
// Three renderings of the same gigabyte is how a tool starts contradicting
// itself in its own logs.
package format

import "fmt"

// Bytes renders a byte count in binary units. Backup output is full of sizes,
// and raw digits make the interesting comparisons hard to see.
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
