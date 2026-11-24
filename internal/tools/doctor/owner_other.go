//go:build !unix

package doctor

import (
	"errors"
	"io/fs"
)

func ownerOf(fs.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }

func nofileLimit() (uint64, error) { return 0, errors.New("本平台不支持") }
