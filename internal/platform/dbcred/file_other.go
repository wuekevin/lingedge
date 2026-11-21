//go:build windows

package dbcred

import (
	"errors"
	"io/fs"
	"os"
)

// Windows 不是 Server 的运行平台（仅 lingedge-reporter 发行 Windows 构建）；此处只保证编译，且一律拒绝读写。
func openNoFollow(string) (*os.File, error) { return nil, errors.New("dbcred: 不支持该平台") }
func checkOwner(fs.FileInfo) error          { return errors.New("dbcred: 不支持该平台") }
func writeAtomic(string, *File) error       { return errors.New("dbcred: 不支持该平台") }
