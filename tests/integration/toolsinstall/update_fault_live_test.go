package toolsinstall

import (
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/registry"
)

// failJ 在 journal 的指定步骤写 begin/done 时失败（故障注入）。
type failJ struct {
	installer.Journal
	begin, done map[int]bool
}

func (f *failJ) Begin(step int, name string, u registry.Undo) error {
	if f.begin[step] {
		return fmt.Errorf("injected journal fault at begin %d", step)
	}
	return f.Journal.Begin(step, name, u)
}

func (f *failJ) Done(step int) error {
	if f.done[step] {
		return fmt.Errorf("injected journal fault at done %d", step)
	}
	return f.Journal.Done(step)
}

// crasher 模拟进程在某个步骤点被杀（等价 kill -9：journal 不写任何收尾记录、flock 释放、defer 之外无任何清理）：到达 at 时关闭 journal 并终止执行 goroutine。
type crasher struct {
	at string
	j  installer.Journal
}

func (c *crasher) wrapJ(j installer.Journal) installer.Journal { c.j = j; return j }
func (c *crasher) hook(p string) {
	if p == c.at {
		c.j.Close()
		runtime.Goexit()
	}
}

func (s *stack) runCrash(opt runOpt, args ...string) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runB(0, opt, args...)
	}()
	<-done
}

func (s *stack) assertRestoredToV1(what string) {
	s.t.Helper()
	if s.currentTarget() != "releases/0.1.0" || s.schemaVersion() != 18 || s.hasColumn("t43_rev") {
		s.t.Fatalf("%s：current=%s schema=%d col=%v", what, s.currentTarget(), s.schemaVersion(), s.hasColumn("t43_rev"))
	}
	s.waitReady()
	if v, prev, _, _, _ := s.stateComp(); v != "0.1.0" || prev != "" {
		s.t.Fatalf("%s：state=%s/%s", what, v, prev)
	}
	if journalsLeft(s) != 0 {
		s.t.Fatalf("%s：不应有活动 journal", what)
	}
}

// 故障注入（真实栈）：在升级各步骤的 journal 写入点注入失败，系统必须回到升级前（数据库 schema、current、进程健康、state），不留半状态。
func TestUpdateJournalFaultRealStack(t *testing.T) {
	s := startStack(t)
	wrap := s.useSysd()
	b := s.serverBundle("0.2.0", serverWithRev(t), 11, noticeOf("reversible"))
	type c struct {
		step  int
		begin bool
	}
	cases := []c{{5, true}, {6, true}, {7, true}, {8, true}, {9, true}, {10, true}, {11, true}, {12, true}, {13, true},
		{5, false}, {8, false}, {9, false}, {10, false}, {11, false}, {12, false}}
	for _, k := range cases {
		where := "done"
		if k.begin {
			where = "begin"
		}
		t.Run(fmt.Sprintf("step%02d_%s", k.step, where), func(t *testing.T) {
			f := &failJ{begin: map[int]bool{}, done: map[int]bool{}}
			if k.begin {
				f.begin[k.step] = true
			} else {
				f.done[k.step] = true
			}
			o := s.opt(wrap)
			o.wrapJournal = func(j installer.Journal) installer.Journal { f.Journal = j; return f }
			lineageBefore := s.lineage()
			code, so, se := s.runB(0, o, s.updArgs(b, "10s")...)
			want := 1 // 改动系统之前失败：沿原因分类（注入的是普通错误 → 1）
			if k.step >= installer.UpStepStop {
				want = 10
			}
			if code != want {
				t.Fatalf("退出码 %d，期望 %d\n%s\n%s\n%s", code, want, so, se, s.spec.logs.String())
			}
			s.assertRestoredToV1(fmt.Sprintf("step%d/%s", k.step, where))
			if s.lineage() != lineageBefore {
				t.Errorf("reversible 路径不应改变 lineage")
			}
			s.assertNoCredentialResidue()
		})
	}
}

// 崩溃恢复（真实栈）：升级在不同位置被「杀掉」（journal 未收尾）→ 其他写命令得 13 → `recover --rollback` 逆序撤销并复核，系统回到升级前；
// 提交点之后被杀则 recover 只补写收尾，保留已提交的升级。
func TestUpdateCrashThenRecover(t *testing.T) {
	s := startStack(t)
	wrap := s.useSysd()
	b := s.serverBundle("0.2.0", serverWithRev(t), 11, noticeOf("reversible"))
	for _, at := range []string{"begun:8", "effect:8", "begun:9", "effect:9", "effect:10", "done:10", "effect:11", "done:12", "begun:13"} {
		t.Run(strings.ReplaceAll(at, ":", "_"), func(t *testing.T) {
			cr := &crasher{at: at}
			o := s.opt(wrap)
			o.wrapJournal, o.hook = cr.wrapJ, cr.hook
			s.runCrash(o, s.updArgs(b, "20s")...)
			if journalsLeft(s) != 1 {
				t.Fatalf("崩溃后应留有 1 个未完成 journal: %d", journalsLeft(s))
			}
			// 存在未完成事务 → 其他写命令得 13（提示 recover）。
			if code, so, _ := s.runB(0, s.opt(wrap), s.updArgs(b, "10s")...); code != 13 {
				t.Fatalf("存在未完成事务时应得 13: %d\n%s", code, so)
			}
			// --resume 对未提交的升级被拒绝（状态不确定）。
			if code, so, _ := s.runB(0, s.opt(wrap), "recover", "--resume", "--json", "--non-interactive", "--config", s.toolsCfg("20s")); code != 3 {
				t.Fatalf("recover --resume 应被拒绝（3）: %d\n%s", code, so)
			}
			code, so, se := s.runB(0, s.opt(wrap), "recover", "--rollback", "--json", "--non-interactive", "--config", s.toolsCfg("20s"))
			if code != 0 {
				t.Fatalf("recover --rollback: %d\n%s\n%s\n%s", code, so, se, s.spec.logs.String())
			}
			d := dataOf(envelopeOf(t, so))
			if tx := d["transactions"].([]any)[0].(map[string]any); tx["action"] != "rolled_back" {
				t.Fatalf("%v", tx)
			}
			s.assertRestoredToV1("recover@" + at)
			s.assertNoCredentialResidue()
		})
	}
	t.Run("提交点之后被杀_effect_13", func(t *testing.T) {
		cr := &crasher{at: "effect:13"}
		o := s.opt(wrap)
		o.wrapJournal, o.hook = cr.wrapJ, cr.hook
		s.runCrash(o, s.updArgs(b, "20s")...)
		if journalsLeft(s) != 1 {
			t.Fatalf("应留有未完成 journal")
		}
		code, so, se := s.runB(0, s.opt(wrap), "recover", "--rollback", "--json", "--non-interactive", "--config", s.toolsCfg("20s"))
		if code != 0 {
			t.Fatalf("recover: %d\n%s\n%s", code, so, se)
		}
		if tx := dataOf(envelopeOf(t, so))["transactions"].([]any)[0].(map[string]any); tx["action"] != "finalized" {
			t.Fatalf("提交点之后只应补写收尾（finalized）: %v", tx)
		}
		if v, prev, _, _, _ := s.stateComp(); v != "0.2.0" || prev != "0.1.0" || s.currentTarget() != "releases/0.2.0" || s.schemaVersion() != 19 {
			t.Fatalf("已提交的升级不得被回滚: %s/%s %s %d", v, prev, s.currentTarget(), s.schemaVersion())
		}
		s.waitReady()
		if journalsLeft(s) != 0 {
			t.Fatal("recover 后不应有活动 journal")
		}
	})
}

// 回滚本身失败（AC5）：旧版本回滚后复核失败（旧版本起不来）→ 退出码 12，journal 保留，升级前备份保留；排除故障后 recover --rollback 收尾。
func TestUpdateRollbackFailureExit12ThenRecover(t *testing.T) {
	s := startStack(t)
	wrap := s.useSysd()
	b := s.serverBundle("0.2.0", serverWithRev(t), 11, noticeOf("reversible"))
	s.spec.failVersions = map[string]bool{"0.2.0": true, "0.1.0": true} // 新旧版本都起不来：升级失败 → 回滚后旧版本也不健康
	code, so, se := s.runB(0, s.opt(wrap), s.updArgs(b, "3s")...)
	env := envelopeOf(t, so)
	if code != 12 || env["exit_name"] != "ROLLBACK_FAILED" {
		t.Fatalf("应为 12: %d\n%s\n%s", code, so, se)
	}
	e := env["errors"].([]any)[0].(map[string]any)
	if e["system_state"] != "needs_manual" || !strings.Contains(e["hint"].(string), "recover --rollback") {
		t.Fatalf("%v", e)
	}
	if journalsLeft(s) != 1 || s.backupCount() != 1 {
		t.Fatalf("journal 与升级前备份必须保留: journals=%d backups=%d", journalsLeft(s), s.backupCount())
	}
	// 故障排除（旧版本可以启动了）后 recover --rollback 收尾（撤销幂等：数据库已 down、current 已切回，剩余的是重启与复核）。
	s.spec.failVersions = nil
	code, so, se = s.runB(0, s.opt(wrap), "recover", "--rollback", "--json", "--non-interactive", "--config", s.toolsCfg("20s"))
	if code != 0 {
		t.Fatalf("recover: %d\n%s\n%s\n%s", code, so, se, s.spec.logs.String())
	}
	s.assertRestoredToV1("exit12 之后的 recover")
}

// update check / version --check：经真实 HTTPS 发行源取得签名清单，列出可升级组件与迁移预告（classes[]、has_irreversible）；篡改的清单被拒（5）。
func TestUpdateCheckRealHTTPSManifest(t *testing.T) {
	s := startStack(t)
	b2 := s.serverBundle("0.2.0", serverWithIrr(t), 11, noticeOf("reversible", "irreversible"))
	orig := s.w.Bundle
	s.w.Bundle = b2
	relURL, caFile := startReleaseServer(t, s.w)
	s.w.Bundle = orig
	opsWrite(t, s, "etc/lingedge/tools/tools.yaml", "release:\n  base_url: "+relURL+"\n  ca_file: "+caFile+"\n")
	code, so, se := s.runOps(0, opsOpt{}, "update", "check", "--json")
	if code != 0 {
		t.Fatalf("%d\n%s\n%s", code, so, se)
	}
	d := dataOf(envelopeOf(t, so))
	var server map[string]any
	for _, c := range d["components"].([]any) {
		if c.(map[string]any)["component"] == "server" {
			server = c.(map[string]any)
		}
	}
	m, _ := server["migrations"].(map[string]any)
	if server["status"] != "upgrade_available" || server["latest"] != "0.2.0" || m["has_irreversible"] != true || len(m["classes"].([]any)) != 2 || d["upgradable"] != float64(1) {
		t.Fatalf("%v", server)
	}
	if code, so, se := s.runOps(0, opsOpt{}, "version", "--check", "--json"); code != 0 || dataOf(envelopeOf(t, so))["check"] == nil {
		t.Fatalf("version --check: %d\n%s\n%s", code, so, se)
	}
	// 篡改签名 → 5。
	srv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sig") {
			_, _ = rw.Write([]byte(`{"key_id":"x","alg":"ed25519","sig":"AAAA"}`))
			return
		}
		_, _ = rw.Write([]byte(`{}`))
	}))
	defer srv.Close()
	ca2 := writeCA(t, srv)
	opsWrite(t, s, "etc/lingedge/tools/tools.yaml", "release:\n  base_url: "+srv.URL+"\n  ca_file: "+ca2+"\n")
	if code, so, _ := s.runOps(0, opsOpt{}, "update", "check", "--json"); code != 5 {
		t.Errorf("篡改的清单应被拒绝（5）：%d\n%s", code, so)
	}
	if code, so, _ := s.runOps(0, opsOpt{}, "version", "--check", "--json"); code != 5 {
		t.Errorf("version --check 同样应拒绝（5）：%d\n%s", code, so)
	}
}

func writeCA(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ca2.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
