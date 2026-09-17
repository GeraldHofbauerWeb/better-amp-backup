package format

import "testing"

func TestBytes(t *testing.T) {
	cases := []struct {
		name string
		in   int64
		want string
	}{
		{"zero", 0, "0 B"},
		{"just under a kibibyte", 1023, "1023 B"},
		{"exactly a kibibyte", 1024, "1.0 KiB"},
		{"a mebibyte", 1024 * 1024, "1.0 MiB"},
		{"a production snapshot", 2255798272, "2.1 GiB"},
		{"an AMP backup directory", 84508000000, "78.7 GiB"},
		// The unit runs out at PiB on purpose: a repository that large is not
		// this tool's problem, and an exabyte suffix nobody recognises helps
		// no one read the number.
		{"beyond the largest unit", 4 << 60, "4096.0 PiB"},
		// Negative counts only arise from a subtraction that went the wrong
		// way. Rendering one plainly is more useful than hiding it.
		{"negative", -5, "-5 B"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Bytes(c.in); got != c.want {
				t.Errorf("Bytes(%d) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
