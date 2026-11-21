//go:build !unix

package downloader

import (
	"errors"
	"os"
)

const noFollow = 0

func fileOwnerOK(os.FileInfo) bool { return true }
func dirOwnerOK(os.FileInfo) bool  { return true }

// 非 unix 平台没有 flock：不提供跨调用续传（调用方退回到 staging 内的私有 .part，行为与未配置 ResumeDir 相同）。
func lockFile(string) (func(), bool, error) {
	return nil, true, errors.New("该平台不支持续传缓存锁")
}
