package web

import "embed"

//go:embed *.html *.js *.css *.svg *.png *.txt *.webmanifest fonts notosanssc-v41
var Assets embed.FS
