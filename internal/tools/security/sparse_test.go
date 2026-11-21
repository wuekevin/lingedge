package security

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 稀疏文件（1.28b）：用手工写出的真实 GNU / PAX 稀疏 tar（macOS bsdtar 不会产出稀疏记录，GNU tar 本机不可用，
// 所以直接按格式规范构造头与数据）验证三个读取入口都明确拒绝，并且没有越界读写、没有按「还原后的巨大逻辑大小」分配内存。

func gnuTarHeader(name string, typ byte, size int64, realsize int64, sparseMap [][2]int64, ext bool) []byte {
	h := make([]byte, 512)
	copy(h, name)
	copy(h[100:], "0000644\x00")
	copy(h[108:], "0000000\x00")
	copy(h[116:], "0000000\x00")
	copy(h[124:], fmt.Sprintf("%011o\x00", size))
	copy(h[136:], "00000000000\x00")
	h[156] = typ
	copy(h[257:], "ustar  \x00") // GNU magic
	for i, e := range sparseMap {
		if i >= 4 {
			break
		}
		copy(h[386+24*i:], fmt.Sprintf("%011o\x00", e[0]))
		copy(h[386+24*i+12:], fmt.Sprintf("%011o\x00", e[1]))
	}
	if ext {
		h[482] = 1
	}
	copy(h[483:], fmt.Sprintf("%011o\x00", realsize))
	copy(h[148:], "        ")
	sum := 0
	for _, c := range h {
		sum += int(c)
	}
	copy(h[148:], fmt.Sprintf("%06o\x00 ", sum))
	return h
}

func paxHeaderBlock(name, recs string) []byte {
	var b bytes.Buffer
	b.Write(ustarHeader("PaxHeaders/"+name, 'x', int64(len(recs))))
	b.Write(pad512([]byte(recs)))
	return b.Bytes()
}

type sparseCase struct {
	name string
	raw  []byte
	// content 是还原后的文件内容（供构造「哈希完全正确」的 SHA256SUMS，证明拒绝来自稀疏检测而不是哈希不符）。
	content []byte
	// wellFormed 为 true 时 Go 的 tar.Reader 能解析该条目（IndexTar/ReadMembers 必须以 ErrUnsupportedType 拒绝）。
	wellFormed bool
}

func sparseCases() []sparseCase {
	end := make([]byte, 1024)
	real10 := append([]byte("abc"), make([]byte, 7)...)
	var cs []sparseCase
	// 旧 GNU 稀疏（typeflag 'S'）：map 在头里，数据流只含 "abc"，还原后 10 字节。
	{
		var b bytes.Buffer
		b.Write(gnuTarHeader("s", 'S', 3, 10, [][2]int64{{0, 3}}, false))
		b.Write(pad512([]byte("abc")))
		b.Write(end)
		cs = append(cs, sparseCase{"GNU 旧式(S)", b.Bytes(), real10, true})
	}
	// 旧 GNU 稀疏，还原后巨大（1 TiB）而数据只有 3 字节：不得按逻辑大小分配/读取。
	{
		var b bytes.Buffer
		b.Write(gnuTarHeader("s", 'S', 3, 1<<40, [][2]int64{{(1 << 40) - 3, 3}}, false))
		b.Write(pad512([]byte("abc")))
		b.Write(end)
		cs = append(cs, sparseCase{"GNU 旧式(S) 逻辑大小 1TiB", b.Bytes(), nil, false})
	}
	// PAX 0.0：GNU.sparse.numblocks/offset/numbytes 记录。
	{
		recs := paxRecord("GNU.sparse.size", "10") + paxRecord("GNU.sparse.numblocks", "1") + paxRecord("GNU.sparse.offset", "0") + paxRecord("GNU.sparse.numbytes", "3")
		var b bytes.Buffer
		b.Write(paxHeaderBlock("s", recs))
		b.Write(ustarHeader("s", '0', 3))
		b.Write(pad512([]byte("abc")))
		b.Write(end)
		cs = append(cs, sparseCase{"PAX 0.0", b.Bytes(), real10, true})
	}
	// PAX 0.1：GNU.sparse.map。
	{
		recs := paxRecord("GNU.sparse.size", "10") + paxRecord("GNU.sparse.numblocks", "1") + paxRecord("GNU.sparse.map", "0,3")
		var b bytes.Buffer
		b.Write(paxHeaderBlock("s", recs))
		b.Write(ustarHeader("s", '0', 3))
		b.Write(pad512([]byte("abc")))
		b.Write(end)
		cs = append(cs, sparseCase{"PAX 0.1", b.Bytes(), real10, true})
	}
	// PAX 1.0：map 在数据流开头（数据块 = "1\n0\n3\n" 填充 + 数据）。
	{
		recs := paxRecord("GNU.sparse.major", "1") + paxRecord("GNU.sparse.minor", "0") + paxRecord("GNU.sparse.name", "s") + paxRecord("GNU.sparse.realsize", "10")
		body := append(pad512([]byte("1\n0\n3\n")), []byte("abc")...)
		var b bytes.Buffer
		b.Write(paxHeaderBlock("s", recs))
		b.Write(ustarHeader("s", '0', int64(len(body))))
		b.Write(pad512(body))
		b.Write(end)
		cs = append(cs, sparseCase{"PAX 1.0", b.Bytes(), real10, true})
	}
	// PAX 1.0，逻辑大小 1 TiB。
	{
		recs := paxRecord("GNU.sparse.major", "1") + paxRecord("GNU.sparse.minor", "0") + paxRecord("GNU.sparse.name", "s") + paxRecord("GNU.sparse.realsize", fmt.Sprint(int64(1)<<40))
		body := append(pad512([]byte(fmt.Sprintf("1\n%d\n3\n", (int64(1)<<40)-3))), []byte("abc")...)
		var b bytes.Buffer
		b.Write(paxHeaderBlock("s", recs))
		b.Write(ustarHeader("s", '0', int64(len(body))))
		b.Write(pad512(body))
		b.Write(end)
		cs = append(cs, sparseCase{"PAX 1.0 逻辑大小 1TiB", b.Bytes(), nil, false})
	}
	// 畸形：数据区被截断 / map 条目越界 / numbytes 超过实际数据——必须报错而不是 panic 或越界。
	{
		recs := paxRecord("GNU.sparse.major", "1") + paxRecord("GNU.sparse.minor", "0") + paxRecord("GNU.sparse.name", "s") + paxRecord("GNU.sparse.realsize", "10")
		body := pad512([]byte("1\n100\n3\n")) // 偏移 100 > realsize 10
		var b bytes.Buffer
		b.Write(paxHeaderBlock("s", recs))
		b.Write(ustarHeader("s", '0', int64(len(body)+512)))
		b.Write(body) // 声明比实际多一块：截断
		cs = append(cs, sparseCase{name: "PAX 1.0 畸形/截断", raw: b.Bytes()})
	}
	{
		var b bytes.Buffer
		b.Write(gnuTarHeader("s", 'S', 3, 10, [][2]int64{{8, 100}}, false)) // 块越出 realsize
		b.Write(pad512([]byte("abc")))
		cs = append(cs, sparseCase{name: "GNU 旧式 map 越界", raw: b.Bytes()})
	}
	{
		var b bytes.Buffer
		b.Write(gnuTarHeader("s", 'S', 3, 10, [][2]int64{{0, 3}}, true)) // 声明有扩展 map 块但没有
		cs = append(cs, sparseCase{name: "GNU 旧式 声明扩展块但缺失", raw: b.Bytes()})
	}
	return cs
}

func allocated() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.TotalAlloc
}

func TestSparseRejectedByAllReaders(t *testing.T) {
	for _, c := range sparseCases() {
		t.Run(c.name, func(t *testing.T) {
			before := allocated()
			// IndexTar
			es, err := IndexTar(bytes.NewReader(c.raw), IndexLimits{})
			if err == nil {
				t.Fatalf("IndexTar 必须拒绝：%v", es)
			}
			if c.wellFormed && !errors.Is(err, ErrUnsupportedType) {
				t.Fatalf("IndexTar 应以 ErrUnsupportedType 明确拒绝：%v", err)
			}
			// ReadMembers（Want 命名了该条目：若被当成普通文件就会取到内容）
			got, err := ReadMembers(bytes.NewReader(c.raw), ReadMembersOptions{Want: map[string]int64{"s": 1 << 20}})
			if err == nil {
				t.Fatalf("ReadMembers 必须拒绝：%q", got["s"])
			}
			if c.wellFormed && !errors.Is(err, ErrUnsupportedType) {
				t.Fatalf("ReadMembers 应以 ErrUnsupportedType 明确拒绝：%v", err)
			}
			// SafeExtract：提供「完全正确」的哈希，拒绝只能来自稀疏检测；失败后目标与 .partial 都不存在。
			dst := filepath.Join(t.TempDir(), "out")
			sum := map[string]string{"s": hex.EncodeToString(func() []byte { h := sha256.Sum256(c.content); return h[:] }())}
			_, err = SafeExtract(context.Background(), bytes.NewReader(c.raw), dst, ExtractOptions{Checksums: sum})
			if err == nil {
				t.Fatal("SafeExtract 必须拒绝稀疏条目")
			}
			if c.wellFormed && !errors.Is(err, ErrUnsupportedType) {
				t.Fatalf("SafeExtract 应以 ErrUnsupportedType 明确拒绝：%v", err)
			}
			for _, p := range []string{dst, dst + ".partial"} {
				if _, e := os.Lstat(p); e == nil {
					t.Fatalf("拒绝后不应遗留 %s", p)
				}
			}
			// 没有按逻辑大小（最大 1 TiB）分配：三个读取器合计分配远小于 32 MiB。
			if d := allocated() - before; d > 32<<20 {
				t.Fatalf("分配了 %d 字节：疑似按还原后的逻辑大小处理", d)
			}
		})
	}
}

// 头部本身的判定依赖：任何以 GNU.sparse. 开头的 PAX 记录，即使数据看上去是普通文件，也被拒绝。
func TestSparsePAXRecordAloneIsEnough(t *testing.T) {
	recs := paxRecord("GNU.sparse.foo", "1")
	var b bytes.Buffer
	b.Write(paxHeaderBlock("s", recs))
	b.Write(ustarHeader("s", '0', 3))
	b.Write(pad512([]byte("abc")))
	b.Write(make([]byte, 1024))
	if _, err := IndexTar(bytes.NewReader(b.Bytes()), IndexLimits{}); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("IndexTar：%v", err)
	}
	if _, err := ReadMembers(bytes.NewReader(b.Bytes()), ReadMembersOptions{Want: map[string]int64{"s": 10}}); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("ReadMembers：%v", err)
	}
	dst := filepath.Join(t.TempDir(), "o")
	sum := sha256.Sum256([]byte("abc"))
	if _, err := SafeExtract(context.Background(), bytes.NewReader(b.Bytes()), dst, ExtractOptions{Checksums: map[string]string{"s": hex.EncodeToString(sum[:])}}); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("SafeExtract：%v", err)
	}
	// 对照：同样的内容、没有稀疏记录，是普通文件，三者都接受（证明上面的拒绝确实来自稀疏记录）。
	var ok bytes.Buffer
	ok.Write(ustarHeader("s", '0', 3))
	ok.Write(pad512([]byte("abc")))
	ok.Write(make([]byte, 1024))
	if _, err := IndexTar(bytes.NewReader(ok.Bytes()), IndexLimits{}); err != nil {
		t.Fatalf("对照应成功：%v", err)
	}
	if _, err := SafeExtract(context.Background(), bytes.NewReader(ok.Bytes()), dst, ExtractOptions{Checksums: map[string]string{"s": hex.EncodeToString(sum[:])}}); err != nil {
		t.Fatalf("对照应成功：%v", err)
	}
}

// 区分「声明大小超限」这一层（先于读取内容）与「实际流超限」那一层：错误文本不同。
func TestReadMembersDeclaredSizeLayer(t *testing.T) {
	raw := buildTar(t, f("META/a", strings.Repeat("x", 11)))
	_, err := ReadMembers(bytes.NewReader(raw), ReadMembersOptions{Want: map[string]int64{"META/a": 10}})
	if !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "声明大小") {
		t.Fatalf("应在读取内容之前以「声明大小」拒绝：%v", err)
	}
}
