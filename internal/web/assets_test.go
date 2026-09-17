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
