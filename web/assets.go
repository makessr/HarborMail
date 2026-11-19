package web

import "embed"

//go:embed *.html *.js *.css *.svg *.png *.txt fonts notosanssc-v41
var Assets embed.FS
