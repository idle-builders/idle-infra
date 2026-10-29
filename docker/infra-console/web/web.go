// Package web serves the console UI: one self-contained HTML file embedded in
// the binary, so the image needs no build step and no Node toolchain.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed index.html
var assets embed.FS

// sessionCookie mirrors auth.SessionCookie. Duplicated rather than imported so
// serving static assets does not depend on the auth package.
const sessionCookie = "__Host-infra-console"

// Handler serves the UI. Non-API paths fall through to index.html so the
// console can own client-side routing without a server change.
//
// signInPath, when set, redirects a visitor with no session straight to
// sign-in. Without it an unauthenticated visitor gets the shell and watches it
// fail its own API calls, which reads as a broken console rather than as "you
// are not signed in". Empty disables the redirect, for the local development
// mode where there is no sign-in to send anyone to.
func Handler(signInPath string) http.Handler {
	sub, err := fs.Sub(assets, ".")
	if err != nil {
		panic("embed console assets: " + err.Error())
	}
	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/mcp") {
			http.NotFound(w, r)
			return
		}
		// No session cookie and sign-in is available: send them to it. This
		// only checks presence — an expired or invalid cookie still reaches the
		// shell, whose first API call 401s and redirects from the client side.
		if signInPath != "" {
			if _, err := r.Cookie(sessionCookie); err != nil {
				http.Redirect(w, r, signInPath, http.StatusFound)
				return
			}
		}
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			// Single-page: unknown paths render the shell rather than 404.
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		// Revalidate: a stale shell must not talk to a newer API.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}
