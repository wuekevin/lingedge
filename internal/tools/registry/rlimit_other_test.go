//go:build !linux && !darwin

package registry

import "errors"

func signalIgnoreXFSZ()          {}
func setFsizeLimit(uint64) error { return errors.New("unsupported") }
