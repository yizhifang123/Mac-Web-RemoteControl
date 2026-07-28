// Package play embeds the browser client so a built binary is self-contained —
// it can be copied anywhere and run without the repo's web/ directory beside it.
package play

import (
	"embed"
	"io/fs"
	"os"
)

//go:embed web
var embedded embed.FS

// WebFS returns the embedded client assets rooted at web/ (so "/" serves index.html).
func WebFS() fs.FS {
	sub, err := fs.Sub(embedded, "web")
	if err != nil {
		panic(err) // the embed directive above guarantees web/ exists
	}
	return sub
}

// WebRoot returns dir as a filesystem, or the embedded assets when dir is empty.
// Serving from a directory lets you edit the client without rebuilding.
func WebRoot(dir string) fs.FS {
	if dir == "" {
		return WebFS()
	}
	return os.DirFS(dir)
}
