package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// runCLI 运行 CLI（进程内），返回退出码与 stderr/stdout 文本。
func runCLI(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var so, se bytes.Buffer
	code := run(args, strings.NewReader(stdin), &so, &se)
	return code, so.String(), se.String()
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

const fixtureSums = "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03  payload/hello.txt\n"

func fixtureManifest(url, extra string) []byte {
	return []byte(`{"schema_version":1,"channel":"stable","sequence":7,"generated_at":"2026-11-21T00:00:00Z","valid_until":"2027-02-19T00:00:00Z",` +
		extra + `"components":{"node":{"status":"available","versions":[{"version":"0.1.0","variants":{"default":{"artifacts":{"linux-amd64":{"url":"` + url +
		`","sha256":"` + strings.Repeat("ab", 32) + `","size":123}}}}}]}}}` + "\n")
}

// 构造一轮待签目录：第 1 轮（包内 SHA256SUMS）。
func makeRound1(t *testing.T) (dir, list string) {
	t.Helper()
	dir = t.TempDir()
	writeFile(t, filepath.Join(dir, "stage/node/META/SHA256SUMS"), []byte(fixtureSums))
	ts := ToSign{Schema: toSignSchema, Release: "0.1.0", Commit: strings.Repeat("c", 40), Round: 1, Items: []Item{{
		ID: "node-0.1.0-linux-amd64", Kind: kindPackageSums, File: "stage/node/META/SHA256SUMS", SHA256: sha([]byte(fixtureSums)),
		Sig: "signatures/node-0.1.0-linux-amd64.sig", Component: "node", Version: "0.1.0", Platform: "linux-amd64", FilesCount: 1, UnpackedSz: 6}}}
	return dir, writeList(t, dir, "to-sign-1.json", ts)
}

func makeRound2(t *testing.T, manifest []byte) (dir, list string) {
	t.Helper()
	dir = t.TempDir()
	writeFile(t, filepath.Join(dir, "manifest.json"), manifest)
	ts := ToSign{Schema: toSignSchema, Release: "0.1.0", Commit: strings.Repeat("c", 40), Round: 2, Items: []Item{{
		ID: "manifest", Kind: kindManifest, File: "manifest.json", SHA256: sha(manifest), Sig: "manifest.json.sig",
		Channel: "stable", Sequence: 7, GeneratedAt: "2026-11-21T00:00:00Z", ValidUntil: "2027-02-19T00:00:00Z"}}}
	return dir, writeList(t, dir, "to-sign-2.json", ts)
}

func writeList(t *testing.T, dir, name string, ts ToSign) string {
	t.Helper()
	b, err := json.MarshalIndent(ts, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	writeFile(t, p, b)
	return p
}

func newKey(t *testing.T, id string) (keyPath, keysJSON string) {
	t.Helper()
	d := t.TempDir()
	keyPath = filepath.Join(d, "release.key.json")
	keysJSON = filepath.Join(d, "keys.json")
	if code, _, se := runCLI(t, "", "keygen", "-key-id", id, "-key-out", keyPath, "-pub-out", keysJSON); code != 0 {
		t.Fatalf("keygen: %d %s", code, se)
	}
	return
}
