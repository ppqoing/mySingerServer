package gui

import (
	"embed"
	"io/fs"
)

//go:embed web
var webContent embed.FS

func webFS() fs.FS {
	sub, err := fs.Sub(webContent, "web")
	if err != nil {
		panic(err)
	}
	return sub
}
