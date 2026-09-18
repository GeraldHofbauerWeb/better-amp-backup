package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
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
	// display and justify-content belong in that list: AMP draws a button as an
	// inline-flex box that centres its own contents, so a file name was not
	// text being aligned but a flex item being centred, and text-align alone
	// could do nothing about it.
	for _, property := range []string{
		"padding", "min-width", "border", "background",
		"display", "justify-content", "text-align",
	} {
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
		m := regexp.MustCompile(`class="checkbox ampbb-rule-toggle"[^>]*><input type="checkbox" id="([^"]+)"`).
			FindStringSubmatch(row)
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

// A settings row lays its three parts out in a grid. As a wrapping flex line
// the explanation took the full width of the section and ran underneath the
// input box beside it, and rows with an explanation sat further apart than
// rows without, so the switches down the left edge were at an uneven pitch.
func TestASettingsRowKeepsItsNoteOutFromUnderTheInput(t *testing.T) {
	css := asset(t, "amp-bb.css")

	block := css[strings.Index(css, ".ampbb-row-setting {"):]
	block = block[:strings.Index(block, "}")]
	if !strings.Contains(block, "display: grid") {
		t.Error("the settings row is not a grid, so its note has nothing to stay inside of")
	}

	// Column 2 is the label's. Letting the note reach column 3, or span to the
	// end, puts it back under the input.
	if !strings.Contains(css, ".ampbb-row-setting > .ampbb-row-note { grid-column: 2; grid-row: 2; }") {
		t.Error("the note is no longer pinned to the label's column")
	}
	if strings.Contains(css, ".ampbb-row-note { flex-basis: 100%") {
		t.Error("the note still claims the full width of the row")
	}
}

// The rules are switches rather than tick boxes, and the switch is AMP's own:
// <label class="checkbox"><input type="checkbox"><span></span></label>. The
// empty span is not decoration -- it *is* the track AMP's stylesheet draws.
// Dropping it leaves a checkbox that AMP hides, and nothing visible at all.
func TestTheRuleSwitchesUseAMPsOwnSwitch(t *testing.T) {
	markup := asset(t, "tab.html")

	switches := regexp.MustCompile(
		`<label class="checkbox[^"]*"[^>]*><input type="checkbox" id="([^"]+)"><span></span></label>`,
	).FindAllStringSubmatch(markup, -1)
	if len(switches) < 13 {
		t.Errorf("found %d switches; every retention rule and every toggle block should have one", len(switches))
	}

	// And no settings checkbox may be left bare, or it renders as a tick box
	// beside the switches.
	for _, m := range regexp.MustCompile(`(.{90})<input type="checkbox" id="ampbb-[^"]+">`).
		FindAllStringSubmatch(markup, -1) {
		if !strings.Contains(m[1], `<label class="checkbox`) {
			t.Errorf("a checkbox is not wrapped in AMP's switch:\n%s", strings.TrimSpace(m[0]))
		}
	}
}

// AMP styles *any* span directly following a checkbox as a switch track, and
// the rule is not scoped to its own label. A file row puts the type icon right
// after the tick box, so every folder in the browser was drawn as a broken
// switch with the real tick box beside it.
func TestAMPsSwitchStylingDoesNotSwallowTheFileIcon(t *testing.T) {
	css := asset(t, "amp-bb.css")
	if !strings.Contains(css, `.ampbb input[type="checkbox"] + .mat-icon {`) {
		t.Fatal("nothing undoes AMP's switch styling on the icon that follows a tick box")
	}
	block := css[strings.Index(css, `.ampbb input[type="checkbox"] + .mat-icon {`):]
	block = block[:strings.Index(block, "}")]
	for _, property := range []string{"background", "box-shadow", "border-radius", "width"} {
		if !strings.Contains(block, property) {
			t.Errorf("the icon rescue does not undo %q, which AMP's track sets", property)
		}
	}
	if !strings.Contains(css, `.ampbb input[type="checkbox"] + .mat-icon::after { content: none; }`) {
		t.Error("the knob AMP draws in ::after is still there")
	}
	// It has to stay narrow: undoing it for every span would take AMP's own
	// switch with it, and those are what the rules are switched by.
	if strings.Contains(css, `.ampbb input[type="checkbox"] + span {`) {
		t.Error("the reset is wide enough to destroy the rule switches as well")
	}
}

// The icon and the tick box in a file row sit next to a name that grows to
// fill the line. A flex item shrinks unless told not to, so a long file name
// used to squeeze them out of shape.
func TestTheFileRowIconCannotBeSqueezed(t *testing.T) {
	css := asset(t, "amp-bb.css")
	block := css[strings.Index(css, ".ampbb-row .mat-icon {"):]
	block = block[:strings.Index(block, "}")]
	if !strings.Contains(block, "flex: 0 0 auto") {
		t.Error("the row icon may still be shrunk by a long name")
	}
	if !strings.Contains(css, ".ampbb-row > input[type=\"checkbox\"] { flex: 0 0 auto; }") {
		t.Error("the row's tick box may still be shrunk by a long name")
	}
}

// nginx injects the loader into every page the panel serves, the controller's
// instance list included. The tab belongs to one instance, so the loader has
// to recognise where it is before it registers anything: on the controller the
// session is a controller session that no instance accepts, and on another
// instance it would show one server's snapshots to somebody looking at
// another's, with a restore button underneath them.
func TestTheLoaderOnlyRegistersInItsOwnInstanceView(t *testing.T) {
	loader := asset(t, "Loader.js")

	if !strings.Contains(loader, instanceIDPlaceholder) {
		t.Fatalf("Loader.js no longer carries %s, so the daemon cannot tell it which instance is its own",
			instanceIDPlaceholder)
	}
	// Mirrors AMP's checkADSLogin: an instance view is /remote/<id>/...,
	// /instance/<id>/..., or ?remote= / ?instance=.
	for _, needed := range []string{"'remote'", "'instance'", "location.pathname", "location.search"} {
		if !strings.Contains(loader, needed) {
			t.Errorf("Loader.js does not look for %s, which is how AMP names an instance view", needed)
		}
	}
	if !strings.Contains(loader, "if (!belongsOnThisPage()) { return; }") {
		t.Error("the loader never acts on what it worked out")
	}
	// The gate has to come before the hook, or the plugin loads anyway.
	if strings.Index(loader, "belongsOnThisPage()) { return; }") > strings.Index(loader, "if (hook())") {
		t.Error("the loader hooks AMP's start-up before deciding whether it belongs here")
	}
}

// The placeholder is filled in on the way out, and what arrives has to stay
// inside the string literal it lands in.
func TestTheServedLoaderCarriesTheInstanceID(t *testing.T) {
	rec := httptest.NewRecorder()
	assetHandler("6b85d8b3-eaff-4722-ac97-52e4f4441948").ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, "/Loader.js", nil))

	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(body, instanceIDPlaceholder) {
		t.Error("the placeholder was served as it stands")
	}
	if !strings.Contains(body, "'6b85d8b3-eaff-4722-ac97-52e4f4441948'") {
		t.Error("the served loader does not know which instance it belongs to")
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("the substituted file has no ETag, so the panel would cache the embedded one")
	}
}

// An id that could close the literal it is substituted into would be a script
// injected into every page of somebody's panel. It comes from a command line
// rather than a request, which is a reason to be sure and not a reason to skip
// it.
func TestAnAwkwardInstanceIDCannotEscapeTheLiteral(t *testing.T) {
	rec := httptest.NewRecorder()
	assetHandler(`'; alert(1); var x='`).ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, "/Loader.js", nil))

	body := rec.Body.String()
	line := regexp.MustCompile(`const INSTANCE = '.*';`).FindString(body)
	if line == "" {
		t.Fatal("no INSTANCE line in the served loader")
	}
	// Every quote inside the literal has to be escaped, or the rest of the
	// line is code rather than a value.
	inside := strings.TrimSuffix(strings.TrimPrefix(line, "const INSTANCE = '"), "';")
	if strings.Contains(strings.ReplaceAll(inside, `\'`, ""), "'") {
		t.Errorf("the id closed its own string literal: %s", line)
	}
	if !strings.Contains(inside, "alert(1)") {
		t.Errorf("the id did not survive the escaping at all: %s", line)
	}
}

// Picking files is not switching a setting on, so those stay tick boxes -- but
// AMP has no styled tick box to borrow, its only checkbox being the switch, and
// the browser's default looks like it wandered in from another page.
func TestTheFilePickerBoxesAreDrawnLikeThePanel(t *testing.T) {
	css := asset(t, "amp-bb.css")
	block := css[strings.Index(css, `.ampbb-row > input[type="checkbox"],`):]
	block = block[:strings.Index(block, "}")]
	for _, property := range []string{"appearance: none", "border-radius: 3px", "width: 1rem"} {
		if !strings.Contains(block, property) {
			t.Errorf("the file picker box is missing %q", property)
		}
	}
	if !strings.Contains(css, `.ampbb-row > input[type="checkbox"]:checked::after,`) {
		t.Error("a ticked box has no tick in it")
	}
}

// The snapshot browser takes whatever is left of the window, which cannot be
// written into the stylesheet: AMP's header, our banners and the tab strip all
// sit above it and none has a height worth assuming. The script measures it;
// the stylesheet keeps the floor for before the first measurement, and for the
// case where it never runs.
func TestTheBrowserFillsTheWindowWithAFloorUnderIt(t *testing.T) {
	css := asset(t, "amp-bb.css")
	script := asset(t, "Plugin.js")

	block := css[strings.Index(css, ".ampbb-split {"):]
	block = block[:strings.Index(block, "}")]
	if !strings.Contains(block, "height: var(--ampbb-split-height, 34rem)") {
		t.Error("the split does not take its height from the measurement")
	}
	if !strings.Contains(block, "min-height") {
		t.Error("the split has no floor, so a short window would collapse it")
	}
	if strings.Contains(block, "align-items: start") {
		t.Error("align-items:start is back, so the two columns are different heights again")
	}

	// A flex item will not shrink below its content without this, so the
	// scrollbar never appears and the box grows past the window instead.
	for _, selector := range []string{"#ampbb-snapshot-list {", ".ampbb-tree {"} {
		rule := css[strings.Index(css, selector):]
		rule = rule[:strings.Index(rule, "}")]
		if !strings.Contains(rule, "min-height: 0") {
			t.Errorf("%s does not allow itself to shrink, so it cannot scroll", selector)
		}
	}

	if !strings.Contains(script, "--ampbb-split-height") {
		t.Error("nothing ever measures the room the browser has")
	}
	for _, when := range []string{"window.addEventListener('resize', sizeTheBrowser)", "sizeTheBrowser();"} {
		if !strings.Contains(script, when) {
			t.Errorf("the measurement is not taken at %q", when)
		}
	}
}

// An element the script has hidden has to be hidden. This tab lives inside a
// stylesheet nobody here wrote, and a rule of ours three classes deep already
// beat the plain version once -- which left the custom-interval box on screen
// beside a preset that was not "custom".
func TestHiddenOutranksEverything(t *testing.T) {
	css := asset(t, "amp-bb.css")
	if !strings.Contains(css, ".ampbb [hidden] { display: none !important; }") {
		t.Error("hidden is not the last word, so any deeper rule can undo it")
	}
}
