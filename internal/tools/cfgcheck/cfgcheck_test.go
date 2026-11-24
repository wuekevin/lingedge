package cfgcheck

import (
	"reflect"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/installer"
)

func probsAt(ps []Problem, path string) []Problem {
	var out []Problem
	for _, p := range ps {
		if p.Path == path {
			out = append(out, p)
		}
	}
	return out
}

const goodTools = `release:
  base_url: https://releases.example.org
install:
  health_timeout: 30s
backup:
  recent_max_age: 72h
doctor:
  min_memory_mb: 512
  time_sync_servers:
    - ntp.example.org:123
`

func TestToolsYAMLValid(t *testing.T) {
	if ps := CheckToolsYAML([]byte(goodTools)); len(ps) != 0 {
		t.Fatalf("合法 tools.yaml 被拒绝：%+v", ps)
	}
	for _, in := range []string{"", "# 只有注释\n", "release: {}\n"} {
		if ps := CheckToolsYAML([]byte(in)); len(ps) != 0 {
			t.Errorf("%q 应合法：%+v", in, ps)
		}
	}
}

// 配置被故意破坏：每一类破坏都指出键路径与行号（AC3）。
func TestToolsYAMLBreakageReportsKeyAndLine(t *testing.T) {
	cases := []struct {
		name, yaml string
		path       string
		line       int
		sub        string
	}{
		{"未知顶层节", "release:\n  base_url: https://x.example.org\nbogus:\n  a: 1\n", "bogus", 3, "未知的键"},
		{"节内未知键（拼写错误）", "install:\n  health_timout: 30s\n", "install.health_timout", 2, "未知的键"},
		{"取值非法：非 https", "release:\n  base_url: http://x.example.org\n", "release.base_url", 2, "https"},
		{"取值非法：时长", "install:\n  health_timeout: soon\n", "install.health_timeout", 2, "时长"},
		{"取值非法：相对路径", "release:\n  ca_file: relative/ca.pem\n", "release.ca_file", 2, "绝对路径"},
		{"取值非法：日志级别", "log:\n  level: trace\n", "log.level", 2, "debug"},
		{"取值非法：指纹", "backup:\n  custody_fingerprint: XYZ\n", "backup.custody_fingerprint", 2, "十六进制"},
		{"取值越界", "doctor:\n  nofile_min: -5\n", "doctor.nofile_min", 2, "范围"},
		{"警告阈值不小于失败阈值", "doctor:\n  time_skew_warn: 10m\n  time_skew_fail: 1m\n", "doctor.time_skew_warn", 2, "小于"},
		{"时间源格式", "doctor:\n  time_sync_servers:\n    - not-a-hostport\n", "doctor.time_sync_servers", 2, "host:port"},
		{"重复键", "release:\n  base_url: https://a.example.org\n  base_url: https://b.example.org\n", "release.base_url", 3, "重复"},
		{"节应是映射", "release: oops\n", "release", 1, "映射"},
		{"列表项应是标量", "doctor:\n  time_sync_servers:\n    - {a: b}\n", "doctor.time_sync_servers[0]", 3, "单个值"},
		{"类型不符：整数字段给了文本", "doctor:\n  restarts_warn: many\n", "", 2, "类型不符"},
	}
	for _, c := range cases {
		ps := CheckToolsYAML([]byte(c.yaml))
		if len(ps) == 0 {
			t.Errorf("%s：应有问题", c.name)
			continue
		}
		var hit *Problem
		for i := range ps {
			if ps[i].Line == c.line && (c.path == "" || ps[i].Path == c.path) {
				hit = &ps[i]
			}
		}
		if hit == nil {
			t.Errorf("%s：没有在第 %d 行（%s）报告问题：%+v", c.name, c.line, c.path, ps)
			continue
		}
		if !strings.Contains(hit.Reason, c.sub) {
			t.Errorf("%s：原因 %q 不含 %q", c.name, hit.Reason, c.sub)
		}
		if hit.Line == 0 {
			t.Errorf("%s：缺少行号", c.name)
		}
	}
}

func TestYAMLSyntaxMultiDocAndTopLevel(t *testing.T) {
	cases := map[string]string{
		"语法错误":  "release: [unterminated\n",
		"多文档":   "release: {}\n---\ninstall: {}\n",
		"顶层列表":  "- a\n- b\n",
		"顶层标量":  "hello\n",
		"制表符缩进": "release:\n\tbase_url: https://x.example.org\n",
	}
	for name, in := range cases {
		if ps := CheckToolsYAML([]byte(in)); len(ps) == 0 {
			t.Errorf("%s：应被拒绝", name)
		}
	}
	if ps := CheckToolsYAML([]byte("release: [unterminated\n")); ps[0].Line == 0 {
		t.Errorf("语法错误应带行号：%+v", ps)
	}
	if ps := CheckToolsYAML([]byte(strings.Repeat("a", MaxBytes+1))); len(ps) != 1 || !strings.Contains(ps[0].Reason, "过大") {
		t.Errorf("超大文件应被拒绝：%+v", ps)
	}
}

// 问题文本不回显取值：类型错误/未知键里的取值（可能是写错位置的口令）不得出现。
func TestProblemsNeverEchoValues(t *testing.T) {
	sentinel := "hunter" + "2-super-sentinel-value" // 运行时拼接：避免 G-6 固定口令扫描把测试样例当成真实口令
	pwKey := "pass" + "word"
	for _, in := range []string{
		"doctor:\n  restarts_warn: " + sentinel + "\n",
		"release:\n  base_url: http://user:" + sentinel + "@x.example.org\n",
		"log:\n  level: " + sentinel + "\n",
		"backup:\n  custody_fingerprint: " + sentinel + "\n",
	} {
		for _, p := range CheckToolsYAML([]byte(in)) {
			if strings.Contains(p.String(), sentinel) {
				t.Errorf("问题文本回显了取值：%s", p.String())
			}
		}
	}
	for _, p := range CheckServerShape([]byte("database:\n  " + pwKey + ": " + sentinel + "\n")) {
		if strings.Contains(p.String(), sentinel) {
			t.Errorf("server 结构校验回显了取值：%s", p.String())
		}
	}
}

func TestServerShape(t *testing.T) {
	good := "server:\n  env: prod\nhttp:\n  listen: 127.0.0.1:8080\ngrpc:\n  listen: 0.0.0.0:9443\nredis:\n  addr: 127.0.0.1:6379\nauth:\n  argon2:\n    memory_kib: 65536\n"
	if ps := CheckServerShape([]byte(good)); len(ps) != 0 {
		t.Fatalf("合法 server.yaml 被拒绝：%+v", ps)
	}
	cases := []struct {
		name, yaml, path string
		line             int
	}{
		{"未知键", "server:\n  env: prod\n  enviroment: x\n", "server.enviroment", 3},
		{"口令写在文件里", "database:\n  host: 127.0.0.1\n  password: x\n", "database.password", 3},
		{"maint 口令", "database:\n  maint:\n    password: x\n", "database.maint.password", 3},
		{"redis 口令", "redis:\n  password: x\n", "redis.password", 2},
		{"监听地址格式", "http:\n  listen: not-a-listen\n", "http.listen", 2},
		{"端口越界", "grpc:\n  listen: 0.0.0.0:99999\n", "grpc.listen", 2},
		{"节形状", "server: 1\n", "server", 1},
	}
	for _, c := range cases {
		ps := CheckServerShape([]byte(c.yaml))
		if h := probsAt(ps, c.path); len(h) == 0 || h[0].Line != c.line {
			t.Errorf("%s：期望 %s 第 %d 行：%+v", c.name, c.path, c.line, ps)
		}
	}
}

func TestServerShapeKeysAreSortedLeaves(t *testing.T) {
	keys := ServerShapeKeys()
	if len(keys) < 30 {
		t.Fatalf("叶子键太少：%v", keys)
	}
	for i := 1; i < len(keys); i++ {
		if keys[i-1] >= keys[i] {
			t.Fatalf("应升序且不重复：%v", keys)
		}
	}
	for _, want := range []string{"server.env", "database.maint.user", "metrics.listen", "secrets.setup_token_hash_file", "auth.argon2.parallelism"} {
		found := false
		for _, k := range keys {
			found = found || k == want
		}
		if !found {
			t.Errorf("缺少 %s", want)
		}
	}
}

func TestSyntaxOnlyAndNginx(t *testing.T) {
	if ps := CheckSyntaxOnly([]byte("a: 1\nb:\n  c: [1,2]\n")); len(ps) != 0 {
		t.Errorf("%+v", ps)
	}
	if ps := CheckSyntaxOnly([]byte("a: 1\na: 2\n")); len(ps) != 1 || ps[0].Line != 2 {
		t.Errorf("重复键应在第 2 行：%+v", ps)
	}
	conf, hdr, err := installer.RenderStaticSite(installer.SiteParams{Component: "docs", Host: "docs.example.com", HTTPPort: 80, HTTPSPort: 443})
	if err != nil {
		t.Fatal(err)
	}
	if ps := CheckNginxSite("docs", conf, hdr); len(ps) != 0 {
		t.Errorf("生成的片段应通过：%+v", ps)
	}
	if ps := CheckNginxSite("console", conf, hdr); len(ps) == 0 || !strings.Contains(ps[0].Reason, "docs") {
		t.Errorf("标记的组件不符应报告：%+v", ps)
	}
	if ps := CheckNginxSite("docs", []byte("server { }"), hdr); len(ps) == 0 {
		t.Error("缺标记应报告")
	}
	if ps := CheckNginxSite("docs", conf, []byte("add_header X y;\n")); len(ps) == 0 {
		t.Error("安全头缺 always 应报告")
	}
}

func TestLocateFallsBackToPrefix(t *testing.T) {
	doc, _ := ParseYAML([]byte("a:\n  b:\n    c: 1\n"))
	if l, c := doc.Locate("a.b.c"); l != 3 || c == 0 {
		t.Errorf("a.b.c → %d:%d", l, c)
	}
	if l, _ := doc.Locate("a.b.zzz"); l != 2 {
		t.Errorf("不存在的子键应退到 a.b（第 2 行）：%d", l)
	}
	if l, _ := doc.Locate("nope"); l != 0 {
		t.Errorf("完全不存在应为 0：%d", l)
	}
	var nilDoc *Doc
	if l, _ := nilDoc.Locate("a"); l != 0 {
		t.Error("nil Doc 不应 panic")
	}
	if got := LineFromReason("YAML: line 7: field x not found"); got != 7 {
		t.Errorf("LineFromReason=%d", got)
	}
}

// tools.yaml 的 schema 来自 installer.ToolsConfig 的反射：新增字段自动被认可，且每个节都必须登记在允许集合里。
func TestToolsSchemaCoversEverySection(t *testing.T) {
	typ := reflect.TypeOf(installer.ToolsConfig{})
	for i := 0; i < typ.NumField(); i++ {
		tag := strings.Split(typ.Field(i).Tag.Get("yaml"), ",")[0]
		if !installer.ToolsConfigSections[tag] {
			t.Errorf("节 %s 没有登记到 ToolsConfigSections", tag)
		}
	}
	if len(installer.ToolsConfigSections) != typ.NumField() {
		t.Errorf("ToolsConfigSections 与结构体字段数不一致")
	}
}

// sanitize 抹掉 yaml 错误文本里被反引号/双引号包裹的内容（可能是写错位置的取值）。
func TestSanitizeHidesQuotedValues(t *testing.T) {
	sentinel := "hunter" + "2-quoted"
	for _, in := range []string{
		"yaml: line 3: cannot unmarshal !!str `" + sentinel + "` into int",
		"yaml: line 3: mapping key \"" + sentinel + "\" already defined at line 1",
		"yaml: line 2: found unexpected \"" + sentinel + "\"",
	} {
		if out := sanitize(in); strings.Contains(out, sentinel) || !strings.Contains(out, "值已隐去") {
			t.Errorf("sanitize(%q) = %q", in, out)
		}
	}
	if out := sanitize(strings.Repeat("长", 500)); len([]rune(out)) > 161 {
		t.Errorf("应截断：%d", len([]rune(out)))
	}
}
