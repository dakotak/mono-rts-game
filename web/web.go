// Package web embeds the browser client.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var files embed.FS

// Static returns the client's files rooted at the static directory.
func Static() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
