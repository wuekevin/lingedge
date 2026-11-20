package service

import (
	"context"
	"os"
	"strconv"
	"testing"
)

// BenchmarkArgon2idHash 是口令哈希参数的基准方法（ADR-018 §4：参数以基准测定并写入配置）。
//
// 用法（单次哈希耗时即 ns/op；目标约 50–250 ms，且在并发登录峰值下内存可承受：并发哈希数 × m KiB）：
//
//	go test -run '^$' -bench Argon2idHash -benchtime 20x ./internal/auth/internal/service/
//	LE_ARGON2_M=65536 LE_ARGON2_T=3 LE_ARGON2_P=4 go test -run '^$' -bench Argon2idHash -benchtime 20x ./internal/auth/internal/service/
//
// 生产机必须复测：不同 CPU/内存下同一组参数的耗时差异很大；复测后把结果写入 server.yaml 的 auth.argon2.*。
func BenchmarkArgon2idHash(b *testing.B) {
	p := Params{MemoryKiB: envU32("LE_ARGON2_M", 65536), Iterations: envU32("LE_ARGON2_T", 4), Parallelism: uint8(envU32("LE_ARGON2_P", 4))}
	h, err := NewHasher(p, 1)
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := h.Hash(ctx, "benchmark-input-"+strconv.Itoa(i)); err != nil {
			b.Fatal(err)
		}
	}
}

func envU32(k string, def uint32) uint32 {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil {
			return uint32(n)
		}
	}
	return def
}
