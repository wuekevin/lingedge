package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testdata/systemd 下的样本按各发行版 `systemctl --version` 的真实格式构造（本机是 macOS，没有 systemd；真实输出复验属 ENV-LNX）。
func TestParseVersionFixtures(t *testing.T) {
	cases := map[string]struct {
		major       int
		units, cred bool
	}{
		"rhel7-219.txt":                  {219, false, false},
		"rhel8-239.txt":                  {239, true, false},
		"ubuntu2004-245.txt":             {245, true, false},
		"ubuntu2004-245-noparen.txt":     {245, true, false},
		"debian11-247.txt":               {247, true, true},
		"ubuntu2204-249.txt":             {249, true, true},
		"debian12-252.txt":               {252, true, true},
		"rhel9-252.txt":                  {252, true, true},
		"ubuntu2404-255.txt":             {255, true, true},
		"fedora41-257.txt":               {257, true, true},
		"amzn2023-253-leading-blank.txt": {253, true, true},
	}
	files, _ := filepath.Glob("testdata/systemd/*.txt")
	if len(files) != len(cases) {
		t.Fatalf("样本 %d 个，用例 %d 个：每个样本都必须有期望", len(files), len(cases))
	}
	for name, c := range cases {
		b, err := os.ReadFile(filepath.Join("testdata", "systemd", name))
		if err != nil {
			t.Fatal(err)
		}
		v, err := ParseVersion(string(b))
		if err != nil {
			t.Errorf("%s：%v", name, err)
			continue
		}
		if v.Major != c.major || v.SupportsUnits() != c.units || v.SupportsCredentials() != c.cred {
			t.Errorf("%s：得到 %+v units=%v cred=%v", name, v, v.SupportsUnits(), v.SupportsCredentials())
		}
	}
}

func TestParseVersionRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "\n\n", "garbage", "systemd", "systemd abc", "systemd -1", "Systemd 252", "xsystemd 252", "systemd 99999999", "version 252", "systemd252"} {
		if v, err := ParseVersion(in); err == nil {
			t.Errorf("%q 应解析失败，得到 %+v", in, v)
		}
	}
}

func TestVersionThresholdBoundaries(t *testing.T) {
	for _, c := range []struct {
		n           int
		units, cred bool
		mode        CredentialMode
		err         bool
	}{
		{234, false, false, "", true}, {235, true, false, CredentialKeyFile, false}, {246, true, false, CredentialKeyFile, false},
		{247, true, true, CredentialSystemd, false}, {300, true, true, CredentialSystemd, false},
	} {
		v := Version{Major: c.n}
		if v.SupportsUnits() != c.units || v.SupportsCredentials() != c.cred {
			t.Errorf("%d：units=%v cred=%v", c.n, v.SupportsUnits(), v.SupportsCredentials())
		}
		m, err := SelectCredentialMode(v)
		if (err != nil) != c.err || m != c.mode {
			t.Errorf("%d：mode=%q err=%v", c.n, m, err)
		}
		if err != nil && KindOf(err) != KindUnsupported {
			t.Errorf("%d：错误类别 %q", c.n, KindOf(err))
		}
	}
	if CredentialMinSystemd != 247 || MinSystemd != 235 {
		t.Errorf("门槛常量被改动：%d/%d（须同步 ADR-027 与 tools-design §21）", CredentialMinSystemd, MinSystemd)
	}
}

func TestCheckSystemdVersionLevels(t *testing.T) {
	for _, c := range []struct {
		n     int
		level string
	}{{219, LevelFail}, {234, LevelFail}, {239, LevelWarn}, {246, LevelWarn}, {247, LevelPass}, {255, LevelPass}} {
		f := CheckSystemdVersion(Version{Major: c.n})
		if f.Level != c.level || f.ID != "service.systemd_version" || f.Category != "service" || f.Message == "" {
			t.Errorf("%d：%+v，期望 %s", c.n, f, c.level)
		}
		if c.level != LevelPass && f.Hint == "" {
			t.Errorf("%d：fail/warn 必须带 hint", c.n)
		}
	}
}

func TestCheckCredentialConsistency(t *testing.T) {
	load := mustRender(t, UnitServer).Content
	p := DefaultServerParams()
	p.Credential = CredentialKeyFile
	u, err := RenderServer(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		content []byte
		major   int
		level   string
		sub     string
	}{
		{"单元用 LoadCredential 但 systemd 245", load, 245, LevelFail, "无法读取主密钥"},
		{"单元用 LoadCredential 且 systemd 252", load, 252, LevelPass, "LoadCredential"},
		{"降级单元但 systemd 252", u.Content, 252, LevelWarn, "已支持"},
		{"降级单元且 systemd 239", u.Content, 239, LevelPass, "master_key_file"},
		{"注释里的 LoadCredential 不算", []byte("# LoadCredential=x\n[Service]\n"), 245, LevelPass, "master_key_file"},
	} {
		f := CheckCredentialConsistency(c.content, Version{Major: c.major})
		if f.Level != c.level || !strings.Contains(f.Message, c.sub) {
			t.Errorf("%s：%+v", c.name, f)
		}
	}
}

// 默认变体与降级变体的选择与渲染接线：按版本选模式 → 渲染 → 单元是否含 LoadCredential 与版本一致，且 master.key 规格随之变化。
func TestModeSelectionDrivesRenderAndKeySpec(t *testing.T) {
	for _, c := range []struct {
		major    int
		hasLoad  bool
		wantMode uint32
		group    string
	}{{239, false, 0o440, "lingedge"}, {252, true, 0o400, "root"}} {
		mode, err := SelectCredentialMode(Version{Major: c.major})
		if err != nil {
			t.Fatal(err)
		}
		p := DefaultServerParams()
		p.Credential = mode
		u, err := RenderServer(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := has(parseINI(t, string(u.Content)), "Service", "LoadCredential"); got != c.hasLoad {
			t.Errorf("systemd %d：LoadCredential 存在=%v", c.major, got)
		}
		sp := SpecMasterKey(mode, "lingedge")
		if uint32(sp.Mode) != c.wantMode || sp.Owner != "root" || sp.Group != c.group {
			t.Errorf("systemd %d：master.key 规格 %+v", c.major, sp)
		}
	}
}
