package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"sync"
)

// DEPLOY-04: assets are embedded, so the binary runs with no files beside it.
//
//go:embed static
var staticFS embed.FS

var (
	assetOnce sync.Once
	assetTag  string
)

// AssetVersion is a short digest of every embedded asset, computed once.
//
// It exists because a long cache lifetime and a stable URL are a bad pair. The
// reasoning here used to be that "the binary is replaced wholesale on upgrade,
// so a long max-age is safe" -- which is wrong, and wrong in a way that cost an
// afternoon: replacing the binary does nothing to a stylesheet a browser has
// already cached for an hour. A shipped CSS change simply did not appear, and
// the feature that depended on it looked broken rather than stale.
//
// Hashing the contents rather than using the version constant matters during
// development, where the version does not change between builds but the
// stylesheet does. Content is the only thing that reliably tracks content.
func AssetVersion() string {
	assetOnce.Do(func() {
		sum := sha256.New()
		var names []string
		_ = fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			names = append(names, p)
			return nil
		})
		// Walk order is already lexical; sorting makes the digest independent of
		// that guarantee.
		sort.Strings(names)
		for _, name := range names {
			f, err := staticFS.Open(name)
			if err != nil {
				continue
			}
			_, _ = io.WriteString(sum, name)
			_, _ = io.Copy(sum, f)
			_ = f.Close()
		}
		assetTag = hex.EncodeToString(sum.Sum(nil))[:12]
	})
	return assetTag
}

// AssetURL returns the cache-busting URL for an embedded asset, for example
// "/static/app.css?v=1a2b3c4d5e6f". Templates must use this rather than writing
// the path directly, or the asset gets cached straight past the change that
// mattered.
func AssetURL(name string) string {
	return "/static/" + path.Clean(name) + "?v=" + AssetVersion()
}

// staticHandler serves the embedded assets.
//
// A request carrying the current content digest is immutable by construction --
// if the content changes, so does the URL -- and can be cached indefinitely.
// One without it, such as a bookmark or markup this missed, gets a short
// lifetime instead, so a stale copy heals in a minute rather than an hour.
func staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("web: static assets missing from binary: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") == AssetVersion() {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=60")
		}
		fileServer.ServeHTTP(w, r)
	})
}
