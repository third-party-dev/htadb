//go:build !nomirrorjar

package servers

import (
	"embed"
	"io/fs"
)

// The embedded directory holds the newest jar of each profile, placed there
// by `make fetchdeps` (tools/fetchdeps). Build with -tags nomirrorjar to
// leave them out.
//
//go:embed embedded
var embeddedJars embed.FS

// Embedded returns the embedded jars (may be empty when fetchdeps has not
// run).
func Embedded() fs.FS {
	sub, _ := fs.Sub(embeddedJars, "embedded")
	return sub
}
