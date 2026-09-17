package restore

import (
	"strings"
	"testing"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/repo"
)

func file(p string) repo.Entry { return repo.Entry{Path: p, Type: repo.TypeFile} }
func dir(p string) repo.Entry  { return repo.Entry{Path: p, Type: repo.TypeDir} }

func TestSelectionKeepsTheNamedPathAndWhatIsUnderIt(t *testing.T) {
	sel, err := newSelection([]string{"world", "mods/[1.21.1] Some Mod.jar"})
	if err != nil {
		t.Fatalf("newSelection: %v", err)
	}

	cases := []struct {
		entry repo.Entry
		want  bool
		why   string
	}{
		{dir("world"), true, "the selection itself"},
		{file("world/level.dat"), true, "below a selected directory"},
		{file("world/region/r.0.0.mca"), true, "further below it"},
		{file("mods/[1.21.1] Some Mod.jar"), true, "an exact file, metacharacters and all"},
		{dir("mods"), true, "carried for its metadata, as an ancestor"},
		{file("mods/Other Mod.jar"), false, "a sibling of the selected file"},
		{dir("config"), false, "an unrelated directory"},
		// The glob engine would match this for a pattern of "world", because
		// a pattern without a slash matches at any depth. A path does not.
		{file("config/world/foo.cfg"), false, "the same name at another depth"},
		// Prefix matching must respect the separator, or "world2" comes along
		// for the ride with "world".
		{file("world2/level.dat"), false, "a directory whose name merely starts the same"},
	}
	for _, c := range cases {
		if got := sel.keep(c.entry); got != c.want {
			t.Errorf("keep(%q) = %v, want %v -- %s", c.entry.Path, got, c.want, c.why)
		}
	}
}

// An ancestor directory is restored for its mode and time, but it is not what
// the caller asked for, so it must not make a selection below it look present.
func TestSelectionAncestorsAreNotEvidence(t *testing.T) {
	sel, err := newSelection([]string{"world/level.dat"})
	if err != nil {
		t.Fatalf("newSelection: %v", err)
	}
	if !sel.keep(dir("world")) {
		t.Error("the parent directory should be restored")
	}
	if got := sel.unmatched(); len(got) != 1 || got[0] != "world/level.dat" {
		t.Errorf("unmatched() = %v, want the file itself -- the parent is not proof it exists", got)
	}
	if !sel.keep(file("world/level.dat")) {
		t.Fatal("the selected file should be kept")
	}
	if got := sel.unmatched(); len(got) != 0 {
		t.Errorf("unmatched() = %v, want none once the file was seen", got)
	}
}

// A file browser sends a parent and, depending on how the boxes were ticked,
// some of its children too. Collapsing them keeps the lookup a single probe.
func TestSelectionCollapsesNestedPaths(t *testing.T) {
	sel, err := newSelection([]string{"world/region", "world", "world/level.dat", "mods"})
	if err != nil {
		t.Fatalf("newSelection: %v", err)
	}
	want := []string{"mods", "world"}
	if len(sel.paths) != len(want) {
		t.Fatalf("paths = %v, want %v", sel.paths, want)
	}
	for i := range want {
		if sel.paths[i] != want[i] {
			t.Fatalf("paths = %v, want %v", sel.paths, want)
		}
	}
	// And the collapsed children must not then be reported as unmatched.
	sel.keep(dir("world"))
	sel.keep(dir("mods"))
	if got := sel.unmatched(); len(got) != 0 {
		t.Errorf("unmatched() = %v, want none", got)
	}
}

func TestSelectionRejectsUnsafePaths(t *testing.T) {
	for _, p := range []string{"", "   ", "/etc/passwd", "../outside", "world/../../etc", "world//region", "./world"} {
		if _, err := newSelection([]string{p}); err == nil {
			t.Errorf("newSelection(%q) was accepted; it must not be", p)
		}
	}
}

func TestSelectionReportsWhatItNeverFound(t *testing.T) {
	sel, err := newSelection([]string{"world", "mods/ghost.jar"})
	if err != nil {
		t.Fatalf("newSelection: %v", err)
	}
	sel.keep(dir("world"))
	got := sel.unmatched()
	if len(got) != 1 || !strings.Contains(got[0], "ghost.jar") {
		t.Errorf("unmatched() = %v, want the path that is not in the snapshot", got)
	}
}

// No selection means no restriction, which has to stay distinguishable from an
// empty one -- the latter would restore nothing at all.
func TestEmptySelectionIsNoSelection(t *testing.T) {
	sel, err := newSelection(nil)
	if err != nil {
		t.Fatalf("newSelection(nil): %v", err)
	}
	if sel != nil {
		t.Errorf("newSelection(nil) = %v, want nil", sel)
	}
}
