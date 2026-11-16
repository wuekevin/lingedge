package db

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db/internal/token"
)

func validCfg() Config {
	return Config{Host: "127.0.0.1", Port: 3306, Name: "lingedge_test", User: "lingedge_app", Password: config.NewSecret("x")}
}

func TestConfigValidate(t *testing.T) {
	long := strings.Repeat("a", 65)
	cases := []struct {
		name string
		mut  func(*Config)
		want string // 空 = 通过；否则错误须包含
	}{
		{"合法 TCP", func(*Config) {}, ""},
		{"合法 socket", func(c *Config) { c.Host, c.Port, c.Socket = "", 0, "/tmp/x.sock" }, ""},
		{"socket 与 host 互斥", func(c *Config) { c.Socket = "/tmp/x.sock" }, "只能设置其一"},
		{"缺少目标", func(c *Config) { c.Host, c.Port = "", 0 }, "host 或 socket"},
		{"端口越界", func(c *Config) { c.Port = 70000 }, "port"},
		{"主机名非法", func(c *Config) { c.Host = "bad host;" }, "host"},
		{"库名非法", func(c *Config) { c.Name = "a-b" }, "库名"},
		{"库名过长", func(c *Config) { c.Name = long }, "库名"},
		{"用户非法", func(c *Config) { c.User = "a b" }, "用户名"},
		{"无口令", func(c *Config) { c.Password = config.Secret{} }, "口令"},
		{"TLS 非法", func(c *Config) { c.TLS = "maybe" }, "tls"},
		{"非回环主机未设 tls", func(c *Config) { c.Host = "db.example.internal" }, "必须显式设置 tls"},
		{"非回环主机显式明文", func(c *Config) { c.Host, c.TLS = "10.1.2.3", "false" }, ""},
		{"非回环主机 TLS", func(c *Config) { c.Host, c.TLS = "10.1.2.3", "true" }, ""},
		{"localhost 免 tls", func(c *Config) { c.Host = "localhost" }, ""},
		{"::1 免 tls", func(c *Config) { c.Host = "::1" }, ""},
		{"TLS true", func(c *Config) { c.TLS = "true" }, ""},
		{"负的池参数", func(c *Config) { c.MaxOpenConns = -1 }, "不得为负"},
		{"idle 大于 open", func(c *Config) { c.MaxOpenConns, c.MaxIdleConns = 2, 3 }, "max_idle_conns"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := validCfg()
			c.mut(&cfg)
			err := cfg.Validate()
			if c.want == "" {
				if err != nil {
					t.Fatalf("应通过: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("应报告 %q，实际 %v", c.want, err)
			}
		})
	}
}

// AC1：UTC、utf8mb4、parseTime、参数化（不插值）、clientFoundRows。
func TestDriverConfigAC1(t *testing.T) {
	d := validCfg().driverConfig()
	if !d.ParseTime || d.Loc != time.UTC {
		t.Errorf("必须 parseTime 且 Loc=UTC: %+v", d)
	}
	if d.Collation != "utf8mb4_0900_ai_ci" {
		t.Errorf("握手排序规则应为 utf8mb4_0900_ai_ci: %s", d.Collation)
	}
	if d.InterpolateParams {
		t.Error("InterpolateParams 必须为 false（参数走服务端预处理，永不拼入 SQL 文本）")
	}
	if d.MultiStatements {
		t.Error("不得启用 multiStatements（堆叠注入面）")
	}
	if !d.ClientFoundRows {
		t.Error("必须启用 clientFoundRows，使 UPDATE 的受影响行数按匹配行计（越权判定为 0 行的前提）")
	}
	if d.Params["time_zone"] != "'+00:00'" {
		t.Errorf("每个连接必须设置 time_zone: %v", d.Params)
	}
	if d.AllowAllFiles || d.AllowCleartextPasswords || d.AllowOldPasswords {
		t.Error("不得放宽驱动安全开关")
	}
	if d.Net != "tcp" || d.Addr != "127.0.0.1:3306" {
		t.Errorf("TCP 目标: %s %s", d.Net, d.Addr)
	}
	sock := validCfg()
	sock.Host, sock.Port, sock.Socket = "", 0, "/tmp/m.sock"
	if d := sock.driverConfig(); d.Net != "unix" || d.Addr != "/tmp/m.sock" {
		t.Errorf("socket 目标: %s %s", d.Net, d.Addr)
	}
	v6 := validCfg()
	v6.Host = "::1"
	if d := v6.driverConfig(); d.Addr != "[::1]:3306" {
		t.Errorf("IPv6 地址应带方括号: %s", d.Addr)
	}
}

func TestPoolDefaultsAndLimits(t *testing.T) {
	app, maint, mig := PoolDefaults(RoleApp), PoolDefaults(RoleMaint), PoolDefaults(RoleMigrate)
	if app.MaxOpenConns <= maint.MaxOpenConns || maint.MaxOpenConns < mig.MaxOpenConns || mig.MaxOpenConns < 1 {
		t.Errorf("连接数应满足 app > maint >= migrate >= 1: %+v %+v %+v", app, maint, mig)
	}
	for _, l := range []PoolLimits{app, maint, mig} {
		if l.MaxIdleConns > l.MaxOpenConns || l.ConnMaxLifetime <= 0 {
			t.Errorf("缺省限额自相矛盾: %+v", l)
		}
	}
	c := validCfg()
	c.MaxOpenConns = 3
	if l := c.limits(RoleApp); l.MaxOpenConns != 3 || l.MaxIdleConns > 3 {
		t.Errorf("显式 open=3 时 idle 不得超过 open: %+v", l)
	}
	c.MaxIdleConns, c.ConnMaxLifetime = 2, time.Minute
	if l := c.limits(RoleApp); l.MaxIdleConns != 2 || l.ConnMaxLifetime != time.Minute {
		t.Errorf("显式值应覆盖缺省: %+v", l)
	}
}

// 口令不会经 fmt 泄漏。
func TestConfigNeverPrintsPassword(t *testing.T) {
	pw := "s3cr" + "et-value-123"
	c := validCfg()
	c.Password = config.NewSecret(pw)
	for _, s := range []string{fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c), fmt.Sprint(c)} {
		if strings.Contains(s, pw) {
			t.Fatalf("格式化输出泄漏口令: %s", s)
		}
	}
	bad := c
	bad.Name = "bad-name"
	if err := bad.Validate(); err == nil || strings.Contains(err.Error(), pw) {
		t.Fatalf("校验错误不得含口令: %v", err)
	}
}

func TestAppConfigFrom(t *testing.T) {
	d := config.Database{Host: "db.internal", Port: 3307, Name: "n", User: "u", Password: config.NewSecret("p")}
	c := AppConfigFrom(d)
	if c.Host != "db.internal" || c.Port != 3307 || c.Name != "n" || c.User != "u" || c.Password.Reveal() != "p" {
		t.Fatalf("映射不符: %+v", c)
	}
}

func TestQAndFromBuilder(t *testing.T) {
	if got := Q("SELECT 1").String(); got != "SELECT 1" {
		t.Fatal(got)
	}
	const part = "SELECT id FROM sites "
	if got := Q(part + "WHERE tenant_id = ?").String(); !strings.HasSuffix(got, "tenant_id = ?") {
		t.Fatal(got)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("零值凭据必须 panic")
		}
	}()
	_ = FromBuilder(token.Token{}, "DROP TABLE x")
}

func TestOpenRejectsInvalidConfigBeforeConnecting(t *testing.T) {
	bad := validCfg()
	bad.Host = ""
	if _, err := OpenApp(t.Context(), bad); err == nil {
		t.Fatal("无效配置必须在连接前被拒绝")
	}
	if _, err := OpenMaint(t.Context(), bad); err == nil {
		t.Fatal("无效配置必须在连接前被拒绝（maint）")
	}
	if _, err := OpenMigrate(t.Context(), bad); err == nil {
		t.Fatal("无效配置必须在连接前被拒绝（migrate）")
	}
}
