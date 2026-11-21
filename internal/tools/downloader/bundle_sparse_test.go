package downloader_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/downloader"
)

func tarHdr(name string, typ byte, size int) []byte {
	h := make([]byte, 512)
	copy(h, name)
	copy(h[100:], "0000644\x00")
	copy(h[124:], fmt.Sprintf("%011o\x00", size))
	h[156] = typ
	copy(h[257:], "ustar\x0000")
	copy(h[148:], "        ")
	sum := 0
	for _, c := range h {
		sum += int(c)
	}
	copy(h[148:], fmt.Sprintf("%06o\x00 ", sum))
	return h
}

func pad(b []byte) []byte {
	if r := len(b) % 512; r != 0 {
		b = append(b, make([]byte, 512-r)...)
	}
	return b
}

func paxRec(k, v string) string {
	n := len(k) + len(v) + 3
	for {
		s := fmt.Sprintf("%d %s=%s\n", n, k, v)
		if len(s) == n {
			return s
		}
		n = len(s)
	}
}

// 离线 bundle 里的包条目是（PAX 1.0）稀疏文件：明确拒绝（KindBundle / 退出码 5），staging 不变。
func TestBundleRejectsSparsePackageEntry(t *testing.T) {
	w := newWorld(t)
	base := okBundle(t, w)
	head := bundleBytes(t, base[0], base[1])
	head = head[:len(head)-1024] // 去掉结束块，接上手写的稀疏条目
	recs := paxRec("GNU.sparse.major", "1") + paxRec("GNU.sparse.minor", "0") + paxRec("GNU.sparse.name", "packages/"+pkgFile) + paxRec("GNU.sparse.realsize", "10")
	body := append(pad([]byte("1\n0\n3\n")), []byte("abc")...)
	var b bytes.Buffer
	b.Write(head)
	b.Write(tarHdr("PaxHeaders/x", 'x', len(recs)))
	b.Write(pad([]byte(recs)))
	b.Write(tarHdr("packages/"+pkgFile, '0', len(body)))
	b.Write(pad(body))
	b.Write(make([]byte, 1024))
	before := tree(t, w.staging)
	err := acquireBundle(t, w, bundleFile(t, b.Bytes()))
	wantKind(t, err, downloader.KindBundle)
	if exitOf(err) != 5 {
		t.Fatalf("退出码 = %d", exitOf(err))
	}
	equalTree(t, "staging", before, tree(t, w.staging))
}
