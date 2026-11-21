//go:build !linux && !darwin

package service

import "io/fs"

// ownerOf 在没有 Stat_t 的平台上不可用：属主核对被跳过（ok=false），权限位核对照常进行。Tools 仅支持 Linux，此处仅为可编译。
func ownerOf(fs.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
