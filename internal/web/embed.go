// Package web embeds the dashboard's static assets — vanilla HTML/CSS/JS, no
// build step, no node_modules, no CDN. A build toolchain or a fetched script
// would break the single-binary `go build` deploy story, and a CDN would make
// the dashboard reach the network from a tool whose whole premise is that the
// user's traffic stays local.
//
// Files is mounted by internal/api.New, so the served binary never reads these
// off disk at runtime.
package web

import "embed"

//go:embed index.html app.js style.css
var Files embed.FS
