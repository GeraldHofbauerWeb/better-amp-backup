package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

//go:embed assets
var assetsFS embed.FS

// instanceIDPlaceholder is what the loader carries until it is served. The
// loader has to know which instance this daemon backs up before it decides
// whether to register a tab on the page it finds itself in, and it is a static
// file -- so the daemon fills it in on the way out.
const instanceIDPlaceholder = "@AMPBB_INSTANCE_ID@"

// assetHandler serves the plugin's own files.
//
// They are revalidated rather than cached outright: AMP's panel caches hard,
// and a stale Plugin.js is a bug that looks like a broken feature and takes an
// hour to recognise. An ETag makes the revalidation free once it is warm.
func assetHandler(instanceID string) http.Handler {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		panic("web: embedded assets are missing: " + err.Error())
	}

	// Rendered once, at start-up: the instance a daemon serves is fixed for
	// the life of the process, and a file the loader gets wrong is a tab in
	// somebody else's server.
	rendered := map[string][]byte{}
	if raw, err := fs.ReadFile(sub, "Loader.js"); err == nil {
		rendered["Loader.js"] = []byte(strings.ReplaceAll(
			string(raw), instanceIDPlaceholder, jsString(instanceID)))
	}

	var (
		mu    sync.Mutex
		etags = map[string]string{}
	)
	etagFor := func(name string) string {
		mu.Lock()
		defer mu.Unlock()
		if tag, ok := etags[name]; ok {
			return tag
		}
		raw, ok := rendered[name]
		if !ok {
			var err error
			if raw, err = fs.ReadFile(sub, name); err != nil {
				return ""
			}
		}
		sum := sha256.Sum256(raw)
		tag := `"` + hex.EncodeToString(sum[:8]) + `"`
		etags[name] = tag
		return tag
	}

	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" || name == "." {
			http.NotFound(w, r)
			return
		}
		if tag := etagFor(name); tag != "" {
			w.Header().Set("ETag", tag)
		}
		w.Header().Set("Cache-Control", "no-cache")
		// The tab is rendered inside AMP's page and must not be framed by
		// anyone else's.
		w.Header().Set("X-Content-Type-Options", "nosniff")

		if body, ok := rendered[name]; ok {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
			return
		}
		files.ServeHTTP(w, r)
	})
}

// jsString keeps whatever arrives here inside the string literal it is
// substituted into. An instance id is a GUID from a command line, not from a
// request -- but a value that reaches a served script deserves the escaping
// anyway, and the cost of it is nothing.
func jsString(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`, `'`, `\'`, `"`, `\"`, "\n", `\n`, "\r", `\r`,
		"<", `\x3c`, ">", `\x3e`, "&", `\x26`,
	)
	return replacer.Replace(s)
}
