package repo

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The web interface serialises these straight out of the library, so a missing
// tag would put a Go field name in an API response and freeze it there.
func TestReportsMarshalWithSnakeCaseNames(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  []string
	}{
		{"CheckReport", CheckReport{Snapshots: 1, Objects: 2, Rehashed: 3},
			[]string{`"snapshots":1`, `"objects":2`, `"rehashed":3`}},
		{"PruneReport", PruneReport{DeletedObjects: 4, FreedBytes: 5, SparedRecent: 6},
			[]string{`"deleted_objects":4`, `"freed_bytes":5`, `"spared_recent":6`}},
		{"SpaceInfo", SpaceInfo{AvailableBytes: 7, TotalBytes: 8},
			[]string{`"available_bytes":7`, `"total_bytes":8`}},
		{"Decision", Decision{Keep: true, Reasons: []string{"last"}},
			[]string{`"keep":true`, `"reasons":["last"]`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := json.Marshal(c.value)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			got := string(raw)
			for _, want := range c.want {
				if !strings.Contains(got, want) {
					t.Errorf("missing %s in %s", want, got)
				}
			}
			// A capital letter in a key means some field kept its Go name.
			for _, key := range jsonKeys(t, raw) {
				if strings.ToLower(key) != key {
					t.Errorf("key %q is not tagged", key)
				}
			}
		})
	}
}

// Policy has no tags on purpose. If someone adds them without also fixing the
// duration, this says why that is not the fix.
func TestPolicyDurationWouldMarshalAsNanoseconds(t *testing.T) {
	raw, err := json.Marshal(Policy{KeepWithin: 48 * time.Hour})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(raw), "172800000000000") {
		t.Fatalf("expected a raw nanosecond count, got %s", raw)
	}
}

func jsonKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}
	return keys
}
