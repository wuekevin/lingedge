//go:build windows

package setup

import "os"

func openFromFile(string) (*os.File, error) { return nil, ErrFromFilePermission }
