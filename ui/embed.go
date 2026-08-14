// Package ui embeds the built single-page app into the controller binary.
//
// One binary, no static-file path to configure, no chance of serving a UI that
// disagrees with the API it is talking to. IMPLEMENTATION §11's container is
// FROM scratch, so there is nowhere else for these files to live anyway.
package ui

import (
	"embed"
	"errors"
	"io/fs"
)

// dist is the Vite build output. `all:` is required so the pattern still
// matches when only the committed .gitkeep is present — that is, when someone
// builds the Go binary without having run `npm run build`. Without it, go:embed
// fails the whole build with a pattern error, which is a confusing way to learn
// you skipped a step.
//
//go:embed all:dist
var dist embed.FS

// ErrNotBuilt means the binary was compiled without a UI build. The daemon
// serves an explanation rather than a 404, because a blank page gives an
// operator nothing to act on.
var ErrNotBuilt = errors.New("ui: not built — run `npm --prefix ui run build`")

// FS returns the built app rooted at dist/.
func FS() (fs.FS, error) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, err
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, ErrNotBuilt
	}
	return sub, nil
}
