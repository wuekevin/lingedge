package manifest

import (
	"embed"
	"errors"
	"io/fs"
)

// builtinFS 嵌入 builtin/ 目录。发行流水线在构建前把真实 keys.json 放入该目录（不入库，见 builtin/README.txt）；
// 目录里只有 README.txt 的构建没有内置公钥：BuiltinKeys 返回 KindNoBuiltinKeys，任何验签都失败关闭。
// 生产构建「缺失或含测试公钥即构建失败」由 gates release-keys（make build RELEASE=1）保证，
// 而不是靠这里的运行期错误——运行期错误只是最后一道失败关闭。
//
//go:embed all:builtin
var builtinFS embed.FS

// BuiltinKeys 返回内置 keys.json 的原始字节。
func BuiltinKeys() ([]byte, error) {
	b, err := builtinFS.ReadFile("builtin/keys.json")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, newErr(KindNoBuiltinKeys, "本构建未注入发行公钥 keys.json（发行流水线注入，UD-3 待发行签名人确认真实公钥）")
	}
	if err != nil {
		return nil, &Error{Kind: KindNoBuiltinKeys, Msg: "读取内置 keys.json 失败", Cause: err}
	}
	return b, nil
}
