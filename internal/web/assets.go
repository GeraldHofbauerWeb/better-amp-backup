package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
)

//go:embed assets
var assetsFS embed.FS

// assetHandler serves the plugin's own files.
//
// They are revalidated rather than cached outright: AMP's panel caches hard,
// and a stale Plugin.js is a bug that looks like a broken feature and takes an
// hour to recognise. An ETag makes the revalidation free once it is warm.
func assetHandler() http.Handler {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		panic("web: embedded assets are missing: " + err.Error())
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
		raw, err := fs.ReadFile(sub, name)
		if err != nil {
			return ""
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
		files.ServeHTTP(w, r)
	})
}
