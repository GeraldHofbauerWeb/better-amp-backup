package exclude

import "testing"

func TestMatchAnchoring(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		path     string
		want     bool
	}{
		// A bare name matches at any depth, like .gitignore.
		{"bare name at root", []string{"logs"}, "logs", true},
		{"bare name nested", []string{"logs"}, "Minecraft/logs", true},
		{"bare name subtree", []string{"logs"}, "Minecraft/logs/latest.log", true},
		{"bare name is not a substring", []string{"logs"}, "Minecraft/logsfile", false},
		{"bare name is not a suffix", []string{"logs"}, "Minecraft/oldlogs", false},

		// A pattern with a separator is anchored at the snapshot root.
		{"anchored hit", []string{"bluemap/web"}, "bluemap/web", true},
		{"anchored subtree", []string{"bluemap/web"}, "bluemap/web/maps/x.png", true},
		{"anchored does not float", []string{"bluemap/web"}, "Minecraft/bluemap/web", false},
		{"anchored sibling untouched", []string{"bluemap/web"}, "bluemap/logs", false},

		// The real exclusions from the production instance.
		{"core dump", []string{"core.*"}, "core.104", true},
		{"hs_err", []string{"hs_err_pid*.log"}, "hs_err_pid104.log", true},
		{"world backup dir", []string{"survival_world_old"}, "survival_world_old/region/r.0.0.mca", true},
		{"live world survives", []string{"survival_world_old"}, "survival_world/region/r.0.0.mca", false},
		{"zip duplicate", []string{"modpack_v5.zip"}, "modpack_v5.zip", true},
		{"zip does not catch the dir", []string{"modpack_v5.zip"}, "modpack_v5/mods/a.jar", false},

		// Wildcards stay inside one segment unless doubled.
		{"star stops at slash", []string{"a/*"}, "a/b", true},
		{"star covers subtree of match", []string{"a/*"}, "a/b/c", true},
		{"star does not skip a level", []string{"a/*/c"}, "a/b/d/c", false},
		{"doublestar crosses levels", []string{"a/**/c"}, "a/b/d/c", true},
		{"doublestar matches zero levels", []string{"**/cache"}, "cache", true},
		{"doublestar nested", []string{"**/cache"}, "mods/x/cache", true},
		{"question mark", []string{"r.?.0.mca"}, "r.1.0.mca", true},
		{"question mark is one char", []string{"r.?.0.mca"}, "r.10.0.mca", false},

		// Trailing slash is accepted and means the same thing.
		{"trailing slash", []string{"logs/"}, "logs/latest.log", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Compile(tc.patterns)
			if err != nil {
				t.Fatalf("Compile(%v): %v", tc.patterns, err)
			}
			if got := s.Match(tc.path); got != tc.want {
				t.Errorf("Match(%q) with %v = %v, want %v", tc.path, tc.patterns, got, tc.want)
			}
		})
	}
}

func TestNegationReIncludes(t *testing.T) {
	s, err := Compile([]string{"config", "!config/important.toml"})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !s.Match("config/other.toml") {
		t.Error("config/other.toml should be excluded")
	}
	if s.Match("config/important.toml") {
		t.Error("negation did not re-include config/important.toml")
	}
}

func TestLastRuleWins(t *testing.T) {
	// Order matters: the same pair in the other order must give the opposite
	// answer, otherwise precedence is not really being applied.
	reinclude, err := Compile([]string{"logs", "!logs/keep.log"})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if reinclude.Match("logs/keep.log") {
		t.Error("expected the later negation to win")
	}

	reexclude, err := Compile([]string{"!logs/keep.log", "logs"})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !reexclude.Match("logs/keep.log") {
		t.Error("expected the later exclusion to win")
	}
}

func TestCommentsAndBlanksIgnored(t *testing.T) {
	s, err := Compile([]string{"", "  ", "# a comment", "logs", "\t"})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1", s.Len())
	}
	if !s.Match("logs") {
		t.Error("the one real rule did not apply")
	}
}

func TestLeadingSlashIsTolerated(t *testing.T) {
	s, err := Compile([]string{"logs"})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !s.Match("/logs/latest.log") {
		t.Error("a leading slash on the candidate path should be tolerated")
	}
}

func TestMergePutsOtherLast(t *testing.T) {
	base := MustCompile([]string{"logs"})
	override := MustCompile([]string{"!logs/keep.log"})

	merged := base.Merge(override)
	if merged.Match("logs/keep.log") {
		t.Error("merged set should let the later source re-include the file")
	}
	// Merging must not mutate either input.
	if !base.Match("logs/keep.log") {
		t.Error("Merge mutated the base set")
	}
	if merged.Len() != 2 {
		t.Errorf("merged Len = %d, want 2", merged.Len())
	}
	if base.Merge(nil).Len() != 1 {
		t.Error("Merge(nil) should return the receiver unchanged")
	}
}

func TestCompileRejectsBadPatterns(t *testing.T) {
	for _, bad := range []string{"!", "!  ", "/"} {
		if _, err := Compile([]string{bad}); err == nil {
			t.Errorf("Compile accepted %q", bad)
		}
	}
}

func TestEmptySetMatchesNothing(t *testing.T) {
	s, err := Compile(nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if s.Match("anything/at/all") {
		t.Error("an empty set must not exclude anything")
	}
}

// TestDefaultProfileExcludesAMPsOwnBackups guards the mistake that filled a
// production disk: without this rule a first run stores AMP's own full-ZIP
// backups, which on a busy instance is tens of gigabytes of redundant data.
func TestDefaultProfileExcludesAMPsOwnBackups(t *testing.T) {
	s := DefaultProfile()

	mustExclude := []string{
		"Backups",
		"Backups/20260907-170000-68b8a324853a4ef98f350c2b20146bda.zip",
		"Backups/Backups.json",
		"AMP_Logs/00-00-00.log",
		"Minecraft/logs/latest.log",
		"Minecraft/crash-reports/crash-2026-07-19.txt",
		"Minecraft/hs_err_pid104.log",
		"Minecraft/core.104",
		"Minecraft/survival_world/session.lock",
		"Minecraft/bluemap/web/maps/world/tiles/0/x1/z2.png",
		"Minecraft/.trash/survival_world.zip",
		"Minecraft/.trash/mods/freecam-neoforge-1.3.0+mc1.21.jar",
	}
	for _, p := range mustExclude {
		if !s.Match(p) {
			t.Errorf("%s should be excluded by the default profile", p)
		}
	}

	// Nothing a restored instance needs to boot may be caught by the defaults.
	mustKeep := []string{
		"Minecraft/survival_world/level.dat",
		"Minecraft/survival_world/region/r.0.0.mca",
		"Minecraft/survival_world/playerdata/uuid.dat",
		"Minecraft/mods/vanillaplus.jar",
		"Minecraft/config/bluemap/core.conf",
		"Minecraft/bluemap/pluginState.json",
		"Minecraft/server.properties",
		"Minecraft/run.sh",
		"Minecraft/libraries/net/neoforged/neoforge/21.1.248/unix_args.txt",
		"MinecraftModule.kvp",
		"AMPConfig.conf",
	}
	for _, p := range mustKeep {
		if s.Match(p) {
			t.Errorf("%s must NOT be excluded: a restore needs it", p)
		}
	}
}

func TestDefaultProfileCanBeOverriddenByTheUser(t *testing.T) {
	// A user who really does want their logs backed up must be able to say so.
	s := DefaultProfile().Merge(MustCompile([]string{"!Minecraft/logs/latest.log"}))
	if s.Match("Minecraft/logs/latest.log") {
		t.Error("a user negation should win over the built-in profile")
	}
	if !s.Match("Backups/x.zip") {
		t.Error("unrelated defaults should still apply")
	}
}

func TestCharacterClasses(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		// The distinction that matters: a JVM core dump versus a config file.
		{"core.[0-9]*", "core.104", true},
		{"core.[0-9]*", "Minecraft/core.104", true},
		{"core.[0-9]*", "Minecraft/config/bluemap/core.conf", false},
		{"core.[0-9]*", "core.json", false},
		{"r.[0-9].[0-9].mca", "r.1.0.mca", true},
		{"r.[0-9].[0-9].mca", "r.a.0.mca", false},
		{"[!x]bc", "abc", true},
		{"[!x]bc", "xbc", false},
	}
	for _, tc := range cases {
		s, err := Compile([]string{tc.pattern})
		if err != nil {
			t.Fatalf("Compile(%q): %v", tc.pattern, err)
		}
		if got := s.Match(tc.path); got != tc.want {
			t.Errorf("Match(%q) with %q = %v, want %v", tc.path, tc.pattern, got, tc.want)
		}
	}
}

func TestMalformedCharacterClassIsRejected(t *testing.T) {
	for _, bad := range []string{"core.[0-9", "core.[]", "a[b/c]d"} {
		if _, err := Compile([]string{bad}); err == nil {
			t.Errorf("Compile accepted malformed pattern %q", bad)
		}
	}
}

// A pattern holding a non-ASCII character used to compile into a regexp that
// could never match the name it was written for. The compiler walked bytes and
// turned each one into a string, which re-encodes any byte above 0x7F as a
// different character -- so the rule was silently inert, and the file it named
// went into every backup.
func TestPatternsWithNonASCIICharacters(t *testing.T) {
	cases := []struct{ pattern, path string }{
		{"größe-täst.toml", "config/größe-täst.toml"},
		{"config/größe-täst.toml", "config/größe-täst.toml"},
		{"**/Mödpäck/**", "instances/Mödpäck/mods/a.jar"},
		{"世界", "saves/世界/level.dat"},
		{"täst?.cfg", "täst1.cfg"},
		{"[äö]test.cfg", "ätest.cfg"},
	}
	for _, c := range cases {
		set, err := Compile([]string{c.pattern})
		if err != nil {
			t.Errorf("Compile(%q): %v", c.pattern, err)
			continue
		}
		if !set.Match(c.path) {
			t.Errorf("pattern %q does not match %q", c.pattern, c.path)
		}
	}
}

// The names this exists for: a modpack jar whose brackets would otherwise open
// a character class, and the handful of characters that mean something to the
// compiler.
func TestQuoteGlobMakesARealPathLiteral(t *testing.T) {
	names := []string{
		"[1.21.1] Some Mod.jar",
		"weird*name?.txt",
		"!not-a-negation",
		"#not-a-comment",
		"back\\slash.cfg",
		"plain.txt",
		"a/b/c.txt",
		"größe-täst.toml",
	}
	for _, name := range names {
		set, err := Compile([]string{QuoteGlob(name)})
		if err != nil {
			t.Errorf("Compile(QuoteGlob(%q)): %v", name, err)
			continue
		}
		if !set.Match(name) {
			t.Errorf("QuoteGlob(%q) does not match its own input", name)
		}
	}
}

// Escaping has to actually narrow the match, or it is decoration.
func TestQuoteGlobStopsMetacharactersMatchingOtherFiles(t *testing.T) {
	set := MustCompile([]string{QuoteGlob("mods/weird*name.jar")})
	if !set.Match("mods/weird*name.jar") {
		t.Error("the literal name should match")
	}
	if set.Match("mods/weirdOTHERname.jar") {
		t.Error("an escaped '*' must not behave like a wildcard")
	}

	set = MustCompile([]string{QuoteGlob("mods/[1.21.1] Mod.jar")})
	if !set.Match("mods/[1.21.1] Mod.jar") {
		t.Error("the bracketed name should match")
	}
	if set.Match("mods/1 Mod.jar") {
		t.Error("escaped brackets must not behave like a character class")
	}
}

// A negation is a leading '!', so a filename starting with one has to survive.
func TestQuoteGlobDefusesALeadingBang(t *testing.T) {
	set := MustCompile([]string{"logs", QuoteGlob("!keep-me.txt")})
	if !set.Match("!keep-me.txt") {
		t.Error("a file named like a negation should be matched, not re-include something")
	}
	if !set.Match("logs/server.log") {
		t.Error("the earlier rule was undone by what should have been a literal")
	}
}

func TestBackslashEscapeEdges(t *testing.T) {
	if _, err := Compile([]string{`trailing\`}); err == nil {
		t.Error("a dangling backslash should be an error, not a silent no-op")
	}
	if _, err := Compile([]string{`a\/b`}); err == nil {
		t.Error("escaping the path separator should be refused")
	}
	// A doubled backslash is one literal backslash.
	set := MustCompile([]string{`back\\slash.cfg`})
	if !set.Match(`back\slash.cfg`) {
		t.Error(`\\ should match a single backslash`)
	}
}
