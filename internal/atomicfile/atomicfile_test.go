package atomicfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteJSONInstallsTheFileWithTheGivenMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "settings.json")
	if err := WriteJSON(path, map[string]int{"keep_last": 6}, 0o640); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("mode = %04o, want 0640 -- CreateTemp makes 0600 and the group would get nothing", got)
	}

	var back map[string]int
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("what was written is not JSON: %v", err)
	}
	if back["keep_last"] != 6 {
		t.Errorf("read back %v", back)
	}
	if raw[len(raw)-1] != '\n' {
		t.Error("the file does not end with a newline")
	}
}

// The temporary file has to share the directory: a rename is only atomic
// within one filesystem, and leaving one behind would accumulate.
func TestNothingIsLeftBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	for range 3 {
		if err := Write(path, []byte("{}\n"), 0o640); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only state.json", names)
	}
}

// A failed write must leave the previous contents intact -- that is the whole
// point of the exercise.
func TestAFailedWriteLeavesTheOldFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := Write(path, []byte("original\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	// A channel cannot be marshalled, so this fails after the file exists and
	// before anything is written.
	if err := WriteJSON(path, make(chan int), 0o640); err == nil {
		t.Fatal("marshalling a channel should have failed")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "original\n" {
		t.Errorf("the old contents were disturbed: %q", raw)
	}
}
