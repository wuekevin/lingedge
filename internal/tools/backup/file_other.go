//go:build !unix

package backup

import (
	"errors"
	"os"
)

// 非 unix 平台（仅开发机）：Tools 的生产目标是 Linux；这里 fail-closed。
func openNoFollow(string) (*os.File, error) { return nil, errors.New("backup: 仅支持 unix 平台") }

func ownerIDs(os.FileInfo) (uint32, uint32, bool) { return 0, 0, false }
