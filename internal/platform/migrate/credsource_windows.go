//go:build windows

package migrate

import (
	"io/fs"
	"os"
)

func openNoFollow(path string) (*os.File, error) { return os.Open(path) }
func checkOwnerRootOrSelf(fs.FileInfo) error     { return nil }
func checkRootOwnedConfig(string) error          { return nil }
