//go:build !unix

package security

import "os"

const noFollowFlag = 0

func withUmask(_ int, fn func() error) error { return fn() }

func linkCount(os.FileInfo) (uint64, bool) { return 0, false }
