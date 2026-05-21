package local

import _ "embed"

// indexHTML is the local-mode viewer shell, served by the daemon at GET /.
//
//go:embed web/index.html
var indexHTML []byte

//go:embed web/app.css
var appCSS []byte

//go:embed web/app.jsx
var appJSX []byte
