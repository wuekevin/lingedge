package redisx

import (
	"errors"
	"testing"
)

// 构造的 INFO server 响应：Redis ≥8 通过，<8 拒绝（真实 <8 实例的拒绝路径等待环境验证：本机只有 Redis 8.x，不伪造）。
func TestParseInfoServerAndCheckMajor(t *testing.T) {
	mk := func(v string) string {
		return "# Server\r\nredis_version:" + v + "\r\nredis_git_sha1:00000000\r\nredis_mode:standalone\r\nos:Linux\r\n"
	}
	cases := []struct {
		version string
		major   int
		ok      bool
	}{
		{"8.0.2", 8, true}, {"8.10.1", 8, true}, {"9.1.0", 9, true}, {"7.4.1", 7, false}, {"7.2.4", 7, false}, {"6.2.14", 6, false}, {"5.0.14", 5, false}, {"255.255.255", 255, true},
	}
	for _, c := range cases {
		si, err := ParseInfoServer(mk(c.version))
		if err != nil || si.Version != c.version || si.Major != c.major {
			t.Errorf("%s: %+v %v", c.version, si, err)
			continue
		}
		err = si.CheckMajor()
		if c.ok && err != nil {
			t.Errorf("%s 应通过: %v", c.version, err)
		}
		if !c.ok && !errors.Is(err, ErrRedisVersion) {
			t.Errorf("%s 应被拒绝（ErrRedisVersion）: %v", c.version, err)
		}
	}
	for _, bad := range []string{"", "# Server\r\nos:Linux\r\n", "redis_version:abc\r\n", "redis_version:-1.0.0\r\n", "redis_version:\r\n"} {
		if _, err := ParseInfoServer(bad); err == nil {
			t.Errorf("%q 应解析失败", bad)
		}
	}
}

func TestProbeRejectsBadConfigWithoutNetwork(t *testing.T) {
	if _, err := Probe(t.Context(), Config{}); err == nil {
		t.Error("空地址应拒绝")
	}
	if _, err := Probe(t.Context(), Config{Addr: "127.0.0.1:1", DB: 99}); err == nil {
		t.Error("db 越界应拒绝")
	}
}
