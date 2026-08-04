package local

import (
	"embed"
	_ "embed"
)

// indexHTML is the local-mode viewer shell, served by the daemon at GET /.
//
//go:embed web/index.html
var indexHTML []byte

//go:embed web/app.css
var appCSS []byte

//go:embed web/app.jsx
var appJSX []byte

// webStatic holds the third-party assets the viewer used to pull from unpkg
// and Google Fonts: React, ReactDOM, Babel, and the two variable font subsets.
// They ship in the binary because the viewer renders private session data, so
// opening it must not tell a CDN that you did, and because a local-first tool
// has to work with no network at all.
//
// Refresh them with `mise run local:vendor-assets`, which pins the same
// versions the task file records.
//
//go:embed web/vendor/*.js web/fonts/*.woff2
var webStatic embed.FS
