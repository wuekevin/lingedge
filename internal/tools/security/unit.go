package security

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// maxUnitValueLen 限制单个值的长度（systemd 行长度远小于此，防滥用）。
const maxUnitValueLen = 4096

// ValidateUnitValue 校验将写入 systemd unit 等生成文件的值（tools-design §15、§18.2）。
// 规则：非空；仅允许 [A-Za-z0-9_./:@-]（因此空白、换行、引号、`$`、`%`（systemd specifier）、`;`、反引号等一律拒绝）；
// 不得含 `..`（连续两个点，比「路径段为 ..」更严）；含 `/` 的值按路径处理——必须是绝对路径且已是 Clean 形态。
func ValidateUnitValue(v string) error {
	if v == "" {
		return errors.New("值为空")
	}
	if len(v) > maxUnitValueLen {
		return fmt.Errorf("值过长（%d > %d）", len(v), maxUnitValueLen)
	}
	for i := 0; i < len(v); i++ {
		if !unitByteOK(v[i]) {
			return fmt.Errorf("含不允许的字符（偏移 %d，仅允许 [A-Za-z0-9_./:@-]）", i)
		}
	}
	if strings.Contains(v, "..") {
		return errors.New("值含连续点号（..）")
	}
	if strings.Contains(v, "/") {
		return ValidateUnitPath(v)
	}
	return nil
}

// ValidateUnitPath 要求 v 是通过 ValidateUnitValue 字符集的绝对路径，且 path.Clean 后不变（无 `//`、`/./`、尾部 `/`）。
func ValidateUnitPath(v string) error {
	if v == "" {
		return errors.New("路径为空")
	}
	if len(v) > maxUnitValueLen {
		return fmt.Errorf("路径过长（%d > %d）", len(v), maxUnitValueLen)
	}
	for i := 0; i < len(v); i++ {
		if !unitByteOK(v[i]) {
			return fmt.Errorf("路径含不允许的字符（偏移 %d，仅允许 [A-Za-z0-9_./:@-]）", i)
		}
	}
	if !strings.HasPrefix(v, "/") {
		return errors.New("路径必须是绝对路径")
	}
	if strings.Contains(v, "..") {
		return errors.New("路径含连续点号（..）")
	}
	if path.Clean(v) != v {
		return errors.New("路径必须是规范形态（无重复斜杠、./ 或尾部斜杠）")
	}
	return nil
}

func unitByteOK(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '_', '.', '/', ':', '@', '-':
		return true
	}
	return false
}
