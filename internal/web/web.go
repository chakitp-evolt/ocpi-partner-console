// Package web holds the console pages and their static assets.
package web

import "embed"

//go:embed *.html *.js *.css
var FS embed.FS
