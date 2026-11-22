package provision

import (
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func TestCheckVersion(t *testing.T) {
	ok := []string{"8.0.16", "8.0.46", "8.0.46-0ubuntu0.22.04.1", "8.4.3", "9.0.1", "8.1.0"}
	bad := []string{"8.0.15", "8.0.0", "5.7.44", "7.9.9", "10.11.6-MariaDB", "8.0.30-MariaDB-log", "", "abc", "8.0"}
	for _, v := range ok {
		if err := CheckVersion(v); err != nil {
			t.Errorf("%s 应通过: %v", v, err)
		}
	}
	for _, v := range bad {
		if err := CheckVersion(v); err == nil {
			t.Errorf("%q 应被拒绝", v)
		}
	}
	// 8.0.16 最低版本的真实验证需要 8.0.16 实例：等待环境验证（本机为 8.0.x 较新补丁；此处只验证判定逻辑）。
	if err := CheckVersion("8.0.15"); err == nil || !strings.Contains(err.Error(), "8.0.16") {
		t.Errorf("拒绝信息应说明最低版本 8.0.16: %v", err)
	}
}

func TestFactsCheckBranches(t *testing.T) {
	good := Facts{Version: "8.0.46", LogBin: true, TrustFunctions: false, SQLMode: "ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION", CharsetServer: "utf8mb4", HasCollation: true}
	has := func(f Facts, key string, ok bool, sev string) bool {
		for _, it := range f.Check() {
			if it.Key == key && it.OK == ok && (sev == "" || it.Severity == sev) {
				return true
			}
		}
		return false
	}
	if !has(good, "mysql.version", true, "info") || !has(good, "mysql.utf8mb4", true, "info") || !has(good, "mysql.sql_mode", true, "info") || !has(good, "mysql.binlog", true, "warn") {
		t.Errorf("good: %+v", good.Check())
	}
	for name, mut := range map[string]func(*Facts){
		"version":  func(f *Facts) { f.Version = "8.0.12" },
		"utf8mb4":  func(f *Facts) { f.HasCollation = false },
		"sql_mode": func(f *Facts) { f.SQLMode = "NO_ENGINE_SUBSTITUTION" },
	} {
		f := good
		mut(&f)
		key := map[string]string{"version": "mysql.version", "utf8mb4": "mysql.utf8mb4", "sql_mode": "mysql.sql_mode"}[name]
		if !has(f, key, false, "error") {
			t.Errorf("%s 分支应为 error: %+v", name, f.Check())
		}
	}
	for _, mode := range []string{"STRICT_ALL_TABLES", "TRADITIONAL,STRICT_TRANS_TABLES", "strict_trans_tables"} {
		f := good
		f.SQLMode = mode
		if !has(f, "mysql.sql_mode", true, "") {
			t.Errorf("%s 应视为严格模式", mode)
		}
	}
	f := good
	f.LogBin = false
	if !has(f, "mysql.binlog", true, "info") {
		t.Error("log_bin=OFF 无需额外权限")
	}
	f = good
	f.TrustFunctions = true
	if !has(f, "mysql.binlog", true, "info") {
		t.Error("trust=1 无需额外权限")
	}
}

// 唯一的字面量转义函数：单引号翻倍；默认转义反斜杠；NO_BACKSLASH_ESCAPES 下不转义反斜杠；拒绝 NUL；输出始终是单个字面量。
func TestLiteralEscaping(t *testing.T) {
	c := &Conn{}
	cases := map[string]string{
		"plain":              `'plain'`,
		"it's":               `'it''s'`,
		`back\slash`:         `'back\\slash'`,
		`'; DROP USER x; --`: `'''; DROP USER x; --'`,
		`\'`:                 `'\\'''`,
	}
	for in, want := range cases {
		got, err := c.literal(in)
		if err != nil || got != want {
			t.Errorf("literal(%q) = %q %v，期望 %q", in, got, err, want)
		}
	}
	c.noBackslash = true
	if got, _ := c.literal(`a\b'c`); got != `'a\b''c'` {
		t.Errorf("NO_BACKSLASH_ESCAPES: %q", got)
	}
	if _, err := c.literal("a\x00b"); err == nil {
		t.Error("NUL 应被拒绝")
	}
}

func TestErrorScrubbing(t *testing.T) {
	c := &Conn{secrets: []string{"hunter-secret-xyz"}}
	e := c.wrap("连接", &mysql.MySQLError{Number: 1045, Message: "Access denied for user 'x' (using password: YES) hunter-secret-xyz"})
	if strings.Contains(e.Error(), "hunter-secret-xyz") || MySQLNumber(e) != 1045 {
		t.Errorf("错误文本应抹掉口令并保留错误号: %v", e)
	}
}

func TestValidation(t *testing.T) {
	if err := (Target{Host: "db.example.test", Port: 3306}).validate(); err == nil {
		t.Error("非回环主机必须显式 TLS")
	}
	if err := (Target{Host: "db.example.test", Port: 3306, TLS: "true"}).validate(); err != nil {
		t.Error(err)
	}
	if err := (Target{Host: "127.0.0.1", Port: 3306}).validate(); err != nil {
		t.Errorf("回环可不设 TLS: %v", err)
	}
	if err := (Target{Host: "bad host", Port: 3306, TLS: "true"}).validate(); err == nil {
		t.Error("主机名非法")
	}
	for _, n := range []string{"lingedge", "a$b", "x_1"} {
		if !ValidDatabaseName(n) {
			t.Errorf("%s 应合法", n)
		}
	}
	for _, n := range []string{"", "a-b", "a`b", "a b", strings.Repeat("x", 65)} {
		if ValidDatabaseName(n) {
			t.Errorf("%q 应非法", n)
		}
	}
}

// CC-13：Unix 套接字目标的校验（互斥、绝对规范路径、无 TLS）与路径判定。
func TestTargetSocket(t *testing.T) {
	if err := (Target{Socket: "/var/run/mysqld/mysqld.sock"}).validate(); err != nil {
		t.Errorf("合法 socket 应通过: %v", err)
	}
	for name, tg := range map[string]Target{
		"与 host 同设": {Host: "127.0.0.1", Socket: "/tmp/m.sock"},
		"与 port 同设": {Port: 3306, Socket: "/tmp/m.sock"},
		"相对路径":      {Socket: "m.sock"},
		"含 ..":      {Socket: "/tmp/../m.sock"},
		"含换行":       {Socket: "/tmp/m\n.sock"},
		"启用 TLS":    {Socket: "/tmp/m.sock", TLS: "true"},
	} {
		if err := tg.validate(); err == nil {
			t.Errorf("%s 应被拒绝", name)
		}
	}
	if ValidSocketPath("") || ValidSocketPath("/"+strings.Repeat("a", 120)) {
		t.Error("空/过长路径应被拒绝")
	}
}
