//go:build nomirrorjar

package servers

import (
	"io/fs"
	"testing/fstest"
)

// Embedded returns an empty FS in nomirrorjar builds.
func Embedded() fs.FS { return fstest.MapFS{} }
