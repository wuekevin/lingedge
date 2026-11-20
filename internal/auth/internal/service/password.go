package service

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id 口令哈希（ADR-018 §4）：PHC 字符串 `$argon2id$v=19$m=<KiB>,t=<迭代>,p=<并行度>$<salt b64>$<hash b64>`（base64 无填充）。
// 参数来自配置（config.Auth.Argon2，由基准测定）；校验时按「哈希串自带的参数」计算，因此改配置不会让旧哈希失效，
// 登录成功后发现参数不同再透明地重新哈希（NeedsRehash）。

// Params 是 argon2id 参数。
type Params struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
}

const (
	saltLen = 16
	keyLen  = 32

	// 解析 PHC 串时对参数设上限：哈希串来自数据库，若被篡改成极大的 m/t，校验会变成资源耗尽攻击。
	maxVerifyMemoryKiB  = 1024 * 1024
	maxVerifyIterations = 32
	maxVerifyParallel   = 32
)

// ErrBadHash 表示口令哈希串不是受支持的 argon2id PHC 格式。
var ErrBadHash = errors.New("auth: 口令哈希格式无效")

// Hasher 计算与校验口令哈希。内部以信号量限制并发的哈希计算数（每次占用 m KiB 内存），
// 防止并发登录把进程内存耗尽；等待受 ctx 约束。
type Hasher struct {
	p   Params
	sem chan struct{}
}

// NewHasher 创建 Hasher；maxConcurrent<=0 时取 max(2, CPU 数)。
func NewHasher(p Params, maxConcurrent int) (*Hasher, error) {
	if p.MemoryKiB < 8 || p.Iterations < 1 || p.Parallelism < 1 {
		return nil, errors.New("auth: argon2 参数无效")
	}
	if maxConcurrent <= 0 {
		maxConcurrent = max(2, runtime.NumCPU())
	}
	return &Hasher{p: p, sem: make(chan struct{}, maxConcurrent)}, nil
}

// Params 返回当前参数。
func (h *Hasher) Params() Params { return h.p }

func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hasher) release() { <-h.sem }

// Hash 用当前参数哈希口令（随机 16 字节盐）。
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: 生成盐: %w", err)
	}
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	key := argon2.IDKey([]byte(password), salt, h.p.Iterations, h.p.MemoryKiB, h.p.Parallelism, keyLen)
	return encodePHC(h.p, salt, key), nil
}

// Verify 校验口令；phc 无法解析返回 ErrBadHash（调用方应按「口令错误」处理，不得放行）。
// needsRehash 为 true 表示哈希串的参数与当前配置不同（调用方在口令正确后可升级）。
func (h *Hasher) Verify(ctx context.Context, password, phc string) (ok, needsRehash bool, err error) {
	p, salt, want, err := decodePHC(phc)
	if err != nil {
		return false, false, err
	}
	if err := h.acquire(ctx); err != nil {
		return false, false, err
	}
	defer h.release()
	got := argon2.IDKey([]byte(password), salt, p.Iterations, p.MemoryKiB, p.Parallelism, uint32(len(want)))
	ok = subtle.ConstantTimeCompare(got, want) == 1
	return ok, ok && (p != h.p || len(want) != keyLen), nil
}

func encodePHC(p Params, salt, key []byte) string {
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.MemoryKiB, p.Iterations, p.Parallelism, b64.EncodeToString(salt), b64.EncodeToString(key))
}

func decodePHC(s string) (Params, []byte, []byte, error) {
	var zero Params
	parts := strings.Split(s, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash]
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return zero, nil, nil, ErrBadHash
	}
	if parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return zero, nil, nil, ErrBadHash
	}
	var m, t, par uint64
	kv := strings.Split(parts[3], ",")
	if len(kv) != 3 {
		return zero, nil, nil, ErrBadHash
	}
	for i, want := range []string{"m=", "t=", "p="} {
		if !strings.HasPrefix(kv[i], want) {
			return zero, nil, nil, ErrBadHash
		}
		n, err := strconv.ParseUint(kv[i][2:], 10, 32)
		if err != nil {
			return zero, nil, nil, ErrBadHash
		}
		switch i {
		case 0:
			m = n
		case 1:
			t = n
		case 2:
			par = n
		}
	}
	if m < 8 || m > maxVerifyMemoryKiB || t < 1 || t > maxVerifyIterations || par < 1 || par > maxVerifyParallel {
		return zero, nil, nil, ErrBadHash
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return zero, nil, nil, ErrBadHash
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil || len(key) < 16 || len(key) > 64 {
		return zero, nil, nil, ErrBadHash
	}
	return Params{MemoryKiB: uint32(m), Iterations: uint32(t), Parallelism: uint8(par)}, salt, key, nil
}
