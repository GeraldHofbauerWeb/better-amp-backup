package exclude

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestParseAMPExcludeFileAnchorsToItsDirectory(t *testing.T) {
	// This is the file that was written to the production instance.
	body := `survival_world_old
survival_world_pre_v5_backup
survival_world.zip
modpack_v5.zip
mods_wo_sable
mods_v4
core.*
hs_err_pid*.log
`
	got, err := ParseAMPExcludeFile(strings.NewReader(body), "Minecraft")
	if err != nil {
		t.Fatalf("ParseAMPExcludeFile: %v", err)
	}
	want := []string{
		"Minecraft/survival_world_old",
		"Minecraft/survival_world_pre_v5_backup",
		"Minecraft/survival_world.zip",
		"Minecraft/modpack_v5.zip",
		"Minecraft/mods_wo_sable",
		"Minecraft/mods_v4",
		"Minecraft/core.*",
		"Minecraft/hs_err_pid*.log",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}

	// And those patterns must actually do the job once compiled.
	s, err := Compile(got)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	for _, p := range []string{
		"Minecraft/core.104",
		"Minecraft/survival_world_old/region/r.0.0.mca",
		"Minecraft/modpack_v5.zip",
		"Minecraft/hs_err_pid104.log",
	} {
		if !s.Match(p) {
			t.Errorf("%s should be excluded", p)
		}
	}
	for _, p := range []string{
		"Minecraft/survival_world/region/r.0.0.mca",
		"Minecraft/modpack_v5/mods/a.jar",
		"Minecraft/mods/a.jar",
		"Minecraft/server.properties",
	} {
		if s.Match(p) {
			t.Errorf("%s must NOT be excluded", p)
		}
	}
}

func TestParseAMPExcludeFileAtRoot(t *testing.T) {
	got, err := ParseAMPExcludeFile(strings.NewReader("Backups\nlogs\n"), "")
	if err != nil {
		t.Fatalf("ParseAMPExcludeFile: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"Backups", "logs"}) {
		t.Errorf("got %q", got)
	}
}

func TestEmptyExcludeFileExcludesItsDirectory(t *testing.T) {
	got, err := ParseAMPExcludeFile(strings.NewReader("\n  \n# just a comment\n"), "Minecraft/bluemap/web")
	if err != nil {
		t.Fatalf("ParseAMPExcludeFile: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"Minecraft/bluemap/web"}) {
		t.Errorf("got %q, want the directory itself", got)
	}
}

func TestEmptyExcludeFileAtRootIsRefused(t *testing.T) {
	// Obeying this would silently back up nothing at all.
	if _, err := ParseAMPExcludeFile(strings.NewReader(""), ""); err == nil {
		t.Error("an empty .backupExclude at the root should be refused, not obeyed")
	}
}

func TestSubPathRuleIsRejected(t *testing.T) {
	_, err := ParseAMPExcludeFile(strings.NewReader("bluemap/web\n"), "Minecraft")
	if err == nil {
		t.Fatal("a rule containing a separator should be rejected")
	}
	if !strings.Contains(err.Error(), "path separator") {
		t.Errorf("error should explain the AMP limitation, got: %v", err)
	}
}

func TestCollectAMPExcludes(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Mirrors the layout written to the production instance.
	mustWrite("Minecraft/"+AMPExcludeFile, "survival_world_old\ncore.*\n")
	mustWrite("Minecraft/bluemap/"+AMPExcludeFile, "web\n")
	mustWrite("Minecraft/mods/a.jar", "not an exclude file")

	got, err := CollectAMPExcludes(root)
	if err != nil {
		t.Fatalf("CollectAMPExcludes: %v", err)
	}
	sort.Strings(got)
	want := []string{"Minecraft/bluemap/web", "Minecraft/core.*", "Minecraft/survival_world_old"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}

	s, err := Compile(got)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !s.Match("Minecraft/bluemap/web/maps/world/tiles/0/x1/z2.png") {
		t.Error("the bluemap tile tree should be excluded")
	}
	if s.Match("Minecraft/bluemap/pluginState.json") {
		t.Error("bluemap's own config must survive; only web/ was excluded")
	}
}

func TestCollectAMPExcludesOnCleanTree(t *testing.T) {
	got, err := CollectAMPExcludes(t.TempDir())
	if err != nil {
		t.Fatalf("CollectAMPExcludes: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %q, want none", got)
	}
}

func TestRenderAMPExcludeFilesRoundTrips(t *testing.T) {
	patterns := []string{
		"Minecraft/survival_world_old",
		"Minecraft/core.*",
		"Minecraft/bluemap/web",
		"Backups",
	}
	files, unsupported := RenderAMPExcludeFiles(patterns)
	if len(unsupported) != 0 {
		t.Errorf("unexpected unsupported patterns: %q", unsupported)
	}

	want := map[string][]string{
		"Minecraft":         {"survival_world_old", "core.*"},
		"Minecraft/bluemap": {"web"},
		"":                  {"Backups"},
	}
	if !reflect.DeepEqual(files, want) {
		t.Errorf("got %#v\nwant %#v", files, want)
	}

	// Feeding the rendered files back through the parser must reproduce the
	// original patterns, which is what makes --sync-to-amp trustworthy.
	var back []string
	for dir, rules := range files {
		got, err := ParseAMPExcludeFile(strings.NewReader(strings.Join(rules, "\n")+"\n"), dir)
		if err != nil {
			t.Fatalf("ParseAMPExcludeFile(%q): %v", dir, err)
		}
		back = append(back, got...)
	}
	sort.Strings(back)
	sorted := append([]string(nil), patterns...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(back, sorted) {
		t.Errorf("round trip lost patterns:\n got %q\nwant %q", back, sorted)
	}
}

func TestRenderAMPExcludeFilesReportsWhatAMPCannotDo(t *testing.T) {
	_, unsupported := RenderAMPExcludeFiles([]string{
		"logs",
		"!logs/keep.log",
		"**/cache",
	})
	sort.Strings(unsupported)
	want := []string{"!logs/keep.log", "**/cache"}
	if !reflect.DeepEqual(unsupported, want) {
		t.Errorf("got %q, want %q", unsupported, want)
	}
}
