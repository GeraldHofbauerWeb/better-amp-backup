package web

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func asset(t *testing.T, name string) string {
	t.Helper()
	raw, err := fs.ReadFile(assetsFS, "assets/"+name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(raw)
}

// The script and the markup are two files that have to agree about every
// element id. Renaming one and forgetting the other produces a tab that looks
// fine and silently does nothing -- there is no compiler between them, so this
// is the compiler.
func TestEveryIdTheScriptUsesExistsInTheMarkup(t *testing.T) {
	script := asset(t, "Plugin.js")
	markup := asset(t, "tab.html")

	present := map[string]bool{}
	for _, m := range regexp.MustCompile(`id="([^"]+)"`).FindAllStringSubmatch(markup, -1) {
		present[m[1]] = true
	}

	var missing []string
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`el\('([^']+)'\)`).FindAllStringSubmatch(script, -1) {
		id := m[1]
		if seen[id] || present[id] {
			continue
		}
		seen[id] = true
		missing = append(missing, id)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("Plugin.js reaches for ids that tab.html does not define: %v", missing)
	}
}

// The same in reverse for the ids the markup declares: an orphan is either a
// leftover or a control nobody wired up, and both are worth noticing.
func TestEveryIdInTheMarkupIsUsed(t *testing.T) {
	script := asset(t, "Plugin.js")
	markup := asset(t, "tab.html")

	var unused []string
	for _, m := range regexp.MustCompile(`id="([^"]+)"`).FindAllStringSubmatch(markup, -1) {
		id := m[1]
		// Ids are also reached through querySelectorAll and label for=, so a
		// plain substring search is the honest test here.
		if strings.Contains(script, "'"+id+"'") || strings.Contains(script, "#"+id) {
			continue
		}
		unused = append(unused, id)
	}
	if len(unused) > 0 {
		t.Errorf("tab.html declares ids nothing uses: %v", unused)
	}
}

// AMP's loader reads module.tabs and module.plugin off the object it builds
// from this file. Getting the shape wrong means no tab and a modal apologising
// for our plugin in somebody else's panel.
func TestThePluginDeclaresWhatAMPsLoaderReads(t *testing.T) {
	script := asset(t, "Plugin.js")
	for _, needed := range []string{
		"this.tabs = [",
		"this.plugin = {",
		"this.stylesheet =",
		"ShortName:",
		"File: 'tab.html'",
		"PostInit:",
	} {
		if !strings.Contains(script, needed) {
			t.Errorf("Plugin.js no longer contains %q, which AMP's loader depends on", needed)
		}
	}
}

// The stylesheet has to undo AMP's own button styling for the controls that
// are links in disguise, padding included -- leaving that out is what made the
// file browser render its names as grey pills.
func TestTheStylesheetResetsAMPsButtonStyling(t *testing.T) {
	css := asset(t, "amp-bb.css")
	block := css[strings.Index(css, ".ampbb .ampbb-name"):]
	block = block[:strings.Index(block, "}")]
	for _, property := range []string{"padding", "min-width", "border", "background"} {
		if !strings.Contains(block, property) {
			t.Errorf("the button reset does not neutralise %q", property)
		}
	}
}

func TestTheLoaderIsTinyAndDefensive(t *testing.T) {
	loader := asset(t, "Loader.js")
	if len(loader) > 4096 {
		t.Errorf("Loader.js is %d bytes; it runs inside somebody else's panel and should stay small", len(loader))
	}
	if !strings.Contains(loader, "catch") {
		t.Error("Loader.js does not catch anything, so a failure here would surface as AMP's")
	}
	// The panel may re-run its start-up, and the injected tag is in every
	// page. Loading the plugin twice would register the tab twice.
	if !strings.Contains(loader, "PluginIsLoaded") {
		t.Error("Loader.js does not guard against loading the plugin twice")
	}
}

// AMP derives a sidebar entry's URL from its display name: spaces and anything
// in brackets are stripped and the rest lower-cased (UI.js, SideMenuEntryVM).
// "Backups (amp-bb)" therefore becomes /backups -- the path AMP's own Backups
// tab already owns -- and its popstate handler resolves a path by taking the
// first entry with that name, which would always be AMP's.
//
// So the plugin overrides shortName after registering. This checks that the
// collision is still real and that the override is still there: if AMP ever
// changes the derivation, the first half of this fails and says so.
func TestTheTabDoesNotStealAMPsBackupsURL(t *testing.T) {
	script := asset(t, "Plugin.js")

	name := regexp.MustCompile(`Name: '([^']+)'`).FindStringSubmatch(script)
	if name == nil {
		t.Fatal("no tab name found in Plugin.js")
	}
	if derived := ampShortName(name[1]); derived != "backups" {
		t.Errorf("the display name %q now derives to %q; if it no longer collides with AMP's own"+
			" Backups tab, the override below can go", name[1], derived)
	}

	if !strings.Contains(script, "vm.shortName = 'ampbb'") {
		t.Error("the tab no longer claims a URL of its own, so it would share AMP's /backups")
	}
	if !strings.Contains(script, "claimOurOwnURL()") {
		t.Error("the override is never called")
	}
}

// ampShortName mirrors UI.js: displayName.replaceAll(/ and .+$|[\s'!?]|\(.+?\)/g, ”).toLowerCase()
func ampShortName(display string) string {
	out := regexp.MustCompile(` and .+$`).ReplaceAllString(display, "")
	out = regexp.MustCompile(`\(.+?\)`).ReplaceAllString(out, "")
	out = regexp.MustCompile(`[\s'!?]`).ReplaceAllString(out, "")
	return strings.ToLower(out)
}

// A view that sets its own display outranks the browser's own rule for the
// hidden attribute, so every tab would show every panel at once. That is
// exactly what happened once the settings became a grid.
func TestHiddenViewsStayHidden(t *testing.T) {
	css := asset(t, "amp-bb.css")
	if !strings.Contains(css, "[hidden]") {
		t.Fatal("nothing in the stylesheet makes a hidden view hidden")
	}
	// And the rule has to come from a selector that outranks a bare class.
	if !strings.Contains(css, ".ampbb .ampbb-view[hidden]") {
		t.Error("the rule is not specific enough to beat .ampbb-settings-grid")
	}
}

// Every rule in "how many backups to keep" can be switched off on its own, and
// switching one off means storing a zero. A row whose tick box the script does
// not know about would look switchable and silently keep applying -- a
// retention rule that ignores its own switch deletes snapshots somebody
// believed they had protected.
func TestEveryRetentionRuleHasASwitchTheScriptKnowsAbout(t *testing.T) {
	markup := asset(t, "tab.html")
	script := asset(t, "Plugin.js")

	section := markup[strings.Index(markup, `data-area="retention"`):]
	section = section[:strings.Index(section, `data-area="exclusions"`)]

	rows := strings.Split(section, `class="ampbb-row-setting"`)[1:]
	if len(rows) < 8 {
		t.Fatalf("found %d retention rows; the section has lost most of itself", len(rows))
	}

	table := script[strings.Index(script, "const RETENTION_RULES = ["):]
	table = table[:strings.Index(table, "];")]

	for _, row := range rows {
		row = row[:strings.Index(row, "</div>")]
		m := regexp.MustCompile(`id="([^"]+)" class="ampbb-rule-toggle"`).FindStringSubmatch(row)
		if m == nil {
			t.Errorf("a retention row has no switch:\n%s", strings.TrimSpace(row))
			continue
		}
		if !strings.Contains(table, "'"+m[1]+"'") {
			t.Errorf("the switch %q is not in RETENTION_RULES, so nothing reads it", m[1])
		}
	}
}

// Off has to reach the server as zero rather than as the number still showing
// in the greyed-out box, which is the whole difference between a rule that is
// switched off and one that is switched off on screen only.
func TestSwitchedOffRulesCollectAsZero(t *testing.T) {
	script := asset(t, "Plugin.js")
	collect := script[strings.Index(script, "function collectRetention()"):]
	collect = collect[:strings.Index(collect, "\n}")]

	for _, needed := range []string{"ruleIsOn(toggle) ? number(id) : 0", "ruleIsOn('ampbb-keep-tags-on')"} {
		if !strings.Contains(collect, needed) {
			t.Errorf("collectRetention no longer contains %q", needed)
		}
	}
	if !strings.Contains(script, "if (!ruleIsOn('ampbb-within-on')) { return '0s'; }") {
		t.Error("the keep-everything-within rule cannot be switched off")
	}
}
