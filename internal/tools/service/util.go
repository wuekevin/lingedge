package service

import (
	"io/fs"
	"strconv"
)

func itoa(n int) string { return strconv.Itoa(n) }

func fmtMode(m fs.FileMode) string { return "0" + strconv.FormatUint(uint64(m.Perm()), 8) }
