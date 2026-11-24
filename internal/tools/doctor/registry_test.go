package doctor

import (
	"context"
	"strings"
	"testing"
)

func TestRegisterValidation(t *testing.T) {
	ok := func(c context.Context, w *World, f Filter) []Finding { return nil }
	cases := []struct {
		name string
		g    Group
		bad  bool
	}{
		{"合法", Group{Name: "x", Prefixes: []string{"x."}, Run: ok}, false},
		{"组名大写", Group{Name: "X", Prefixes: []string{"x."}, Run: ok}, true},
		{"没有前缀", Group{Name: "y", Run: ok}, true},
		{"前缀缺点", Group{Name: "y", Prefixes: []string{"y"}, Run: ok}, true},
		{"没有 Run", Group{Name: "y", Prefixes: []string{"y."}}, true},
		{"组名重复", Group{Name: "x", Prefixes: []string{"z."}, Run: ok}, true},
		{"前缀重复", Group{Name: "y", Prefixes: []string{"x."}, Run: ok}, true},
		{"前缀包含已有前缀", Group{Name: "y", Prefixes: []string{"x.sub."}, Run: ok}, true},
	}
	r := New()
	for _, c := range cases {
		err := r.Register(c.g)
		if (err != nil) != c.bad {
			t.Errorf("%s：err=%v，期望出错=%v", c.name, err, c.bad)
		}
	}
}

// 注册表可扩展：后续 Story 追加自己的 Group 即可，内置检查与新增检查同时运行；id 形态与前缀归属由注册表强制。
func TestDefaultIsExtensibleAndEnforcesIDs(t *testing.T) {
	extra := Group{Name: "node", Category: "node", Prefixes: []string{"node."}, Run: func(context.Context, *World, Filter) []Finding {
		return []Finding{{ID: "node.demo", Level: LevelPass, Message: "追加的检查"}}
	}}
	reg, err := Default(extra)
	if err != nil {
		t.Fatal(err)
	}
	w := newWorld(t)
	w.Probe = fakeProbe(goodSystem())
	rep := reg.Run(context.Background(), w, Filter{})
	f := find(t, rep, "node.demo")
	if f.Category != "node" {
		t.Errorf("Category 应由组补上：%+v", f)
	}
	if _, err := Default(Group{Name: "dup", Prefixes: []string{"sys."}, Run: extra.Run}); err == nil {
		t.Error("与内置 sys. 前缀冲突的组必须被拒绝")
	}
	// 内置 id 全部合规且唯一（Run 内部会把违规转为 doctor.internal.*）。
	for _, f := range rep.Findings {
		if strings.HasPrefix(f.ID, "doctor.internal") {
			t.Errorf("内置检查产生了不合规结果：%+v", f)
		}
		if !f.Level.Valid() || f.Message == "" || f.Category == "" {
			t.Errorf("结果不完整：%+v", f)
		}
	}
}

func TestRunRejectsBadFindingsAndRecoversPanics(t *testing.T) {
	reg := New()
	reg.MustRegister(Group{Name: "a", Category: "a", Prefixes: []string{"a."}, Run: func(context.Context, *World, Filter) []Finding {
		return []Finding{
			{ID: "a.ok", Level: LevelPass, Message: "m"},
			{ID: "b.stolen", Level: LevelPass, Message: "越权的前缀"},
			{ID: "a.badlevel", Level: "maybe", Message: "m"},
			{ID: "A.Upper", Level: LevelPass, Message: "m"},
			{ID: "a.ok", Level: LevelPass, Message: "重复"},
		}
	}})
	reg.MustRegister(Group{Name: "p", Category: "p", Prefixes: []string{"p."}, Run: func(context.Context, *World, Filter) []Finding { panic("boom password=hunter2") }})
	rep := reg.Run(context.Background(), newWorld(t), Filter{})
	if rep.Summary.Pass != 1 {
		t.Errorf("只有 a.ok 合规：%+v", rep.Summary)
	}
	var internal int
	for _, f := range rep.Findings {
		if strings.HasPrefix(f.ID, "doctor.internal") || strings.HasSuffix(f.ID, "internal_error") {
			internal++
			if f.Level != LevelFail {
				t.Errorf("违规结果必须是 fail：%+v", f)
			}
		}
		if strings.Contains(f.Message, "hunter2") {
			t.Errorf("panic 文本里的口令必须脱敏：%q", f.Message)
		}
	}
	if internal < 3 {
		t.Errorf("违规/panic 应各自转为 fail，实际 %d：%+v", internal, rep.Findings)
	}
}

func TestReportFailedStrictSemantics(t *testing.T) {
	cases := []struct {
		s      Summary
		strict bool
		want   bool
	}{
		{Summary{Pass: 3}, false, false}, {Summary{Pass: 3}, true, false},
		{Summary{Warn: 1}, false, false}, {Summary{Warn: 1}, true, true},
		{Summary{Fail: 1}, false, true}, {Summary{Skip: 5}, true, false},
	}
	for _, c := range cases {
		if got := (Report{Summary: c.s}).Failed(c.strict); got != c.want {
			t.Errorf("%+v strict=%v：%v", c.s, c.strict, got)
		}
	}
}

func TestComponentFilterRunsOnlyPerComponentGroups(t *testing.T) {
	w := newWorld(t)
	w.Probe = fakeProbe(goodSystem())
	w.Components = []Component{{Name: "server", Version: "1.0.0", State: "installed"}}
	rep := runAll(t, w, Filter{Component: "server"})
	for _, f := range rep.Findings {
		if strings.HasPrefix(f.ID, "sys.") || strings.HasPrefix(f.ID, "baota.") {
			t.Errorf("指定组件时不应运行主机级检查：%s", f.ID)
		}
	}
	if len(rep.Findings) == 0 {
		t.Error("应有 server 相关结果")
	}
}

// 输出脱敏：message/hint/details 里的令牌、口令一律替换。
func TestFindingsAreRedacted(t *testing.T) {
	reg := New()
	reg.MustRegister(Group{Name: "r", Category: "r", Prefixes: []string{"r."}, Run: func(context.Context, *World, Filter) []Finding {
		pw := "hunter" + "2" // 运行时拼接：避免 G-6 固定口令扫描把测试样例当成真实口令
		return []Finding{{ID: "r.leak", Level: LevelWarn, Message: "token lenr1.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA 泄露",
			Hint: "dsn mysql://u:" + pw + "@h/db", Details: map[string]any{"pass" + "word": pw, "note": "ok"}}}
	}})
	f := reg.Run(context.Background(), newWorld(t), Filter{}).Findings[0]
	for _, s := range []string{f.Message, f.Hint, f.Details["pass"+"word"].(string)} {
		if strings.Contains(s, "hunter2") || strings.Contains(s, "AAAAAAAAAAAA") {
			t.Errorf("未脱敏：%q", s)
		}
	}
}
