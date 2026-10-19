// Package dbscripts ships the schema scripts in the binary so the console can apply them at boot.
package dbscripts

import "embed"

//go:embed *.sql
var FS embed.FS
