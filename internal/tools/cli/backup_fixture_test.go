package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/backup"
)

// backupFixtureWithDest 手工拼出一个结构与哈希都正确、但清单把恢复目标指向 dest 的备份文件（WriteArchive 会拒绝写出非法清单，
// 所以这里绕过它——这正是「被改写的备份」的样子）。仅测试使用（测试文件可直接用 archive/tar，G-4 只约束非测试代码）。
func backupFixtureWithDest(t *testing.T, dest string) (string, error) {
	t.Helper()
	body := []byte("pwned: true\n")
	h := sha256.Sum256(body)
	m := backup.Manifest{FormatVersion: backup.FormatVersion, Purpose: backup.PurposeManual, Component: "node", Version: "0.1.0", Host: "edge-host-1",
		CreatedAt: time.Now().UTC().Format(time.RFC3339), ToolsVersion: "t", KEKFingerprints: map[string]string{}, KEKIDsRequired: []string{},
		Files: []backup.FileEntry{{Archive: "files/000001", Dest: dest, Kind: backup.KindConfig, Size: int64(len(body)), SHA256: hex.EncodeToString(h[:]), Mode: 0o644}}}
	mb, err := backup.MarshalManifest(m)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range []struct {
		name string
		b    []byte
	}{{backup.ManifestName, mb}, {"files/000001", body}} {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: e.name, Size: int64(len(e.b)), Mode: 0o600}); err != nil {
			return "", err
		}
		if _, err := tw.Write(e.b); err != nil {
			return "", err
		}
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	out := filepath.Join(t.TempDir(), "lingedge-backup-evil-node-20260101T000000Z.tar.gz")
	return out, os.WriteFile(out, buf.Bytes(), 0o600)
}
