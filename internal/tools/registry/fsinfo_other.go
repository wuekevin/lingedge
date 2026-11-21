//go:build !linux && !darwin

package registry

import "errors"

func fsKind(string) (FSInfo, error) {
	return FSInfo{}, errors.New("本平台不支持文件系统类型识别")
}
