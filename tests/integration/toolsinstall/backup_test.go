package toolsinstall

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/cli"
	"github.com/wuekevin/lingedge/internal/tools/probe"
	"github.com/wuekevin/lingedge/internal/tools/service"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

// Story 1.41 联调：真实的 backup create / list / verify / restore / uninstall（cli.Execute → backup 包 → 真实 mysqldump 与 mysql 客户端 →
// 本机临时 MySQL 8.0）+ 本机构建的真实 lingedge-server（lineage bump、migrate plan、/healthz）+ 真实向导产生的数据（加密的签名私钥、运行期凭据密文、deploy.lineage）。
// 伪的只有「操作系统」（伪 systemctl/useradd/chown，tests/toolsworld）。绝不触碰常驻 mysqld：全部是 tests/mysqltest 起的临时实例。

type stack struct {
	*env
	be      *backends
	wz      *wizard
	baseURL string
	bin     string // mysqltest 实例的 mysql 客户端目录
	clock   int    // 每次命令让伪时钟前进 1 秒（备份文件名含 UTC 秒，避免同秒冲突）
}

const migHost = "edge-host-1"

// startStack 完成：install server → 真实向导（MySQL/Redis 临时实例）→ 同进程切换到正常模式。之后 server 已初始化、运行中。
func startStack(t *testing.T) *stack {
	t.Helper()
	be := startBackends(t)
	e := newEnv(t, false)
	code, so, se := e.run(0, e.installArgs()...)
	if code != 0 {
		t.Fatalf("install server 退出码 %d\nstdout=%s\nstderr=%s\n%s", code, so, se, e.spec.logs.String())
	}
	token := accessOf(t, envelopeOf(t, so))["setup_token"]
	base := "http://127.0.0.1:" + strconv.Itoa(e.ports[0])
	w := &wizard{t: t, base: base, token: token}
	be.runWizard(w)
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		if err := httpGetOK(base + "/healthz/ready"); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := httpGetOK(base + "/healthz/ready"); err != nil {
		t.Fatalf("向导完成后 /healthz/ready 应 200: %v\n%s", err, e.spec.logs.String())
	}
	return &stack{env: e, be: be, wz: w, baseURL: base, bin: be.inst.BinDir()}
}

var httpClient = http.Client{Timeout: 3 * time.Second}

func httpGetOK(u string) error {
	resp, err := httpClient.Get(u)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s = %d", u, resp.StatusCode)
	}
	return nil
}

// runB 经真实 cli.Execute 运行 lingedge，带备份相关的装配：真实 mysqldump/mysql、真实 lingedge-server（lineage bump 用向导之后的 live 配置）、
// 低成本 argon2id 参数（仅为测试速度；格式与算法同生产）。pw=true 时经环境变量提供迁移账号口令（Tools 的凭据通道之一）。
func (s *stack) runB(uid int, opt runOpt, args ...string) (code int, stdout, stderr string) {
	s.t.Helper()
	r, err := service.NewRunner([]string{s.w.Bin})
	if err != nil {
		s.t.Fatal(err)
	}
	genv := map[string]string{}
	if !opt.noPassword {
		genv[backup.EnvDBPass] = s.be.migPW
	}
	for k, v := range opt.env {
		genv[k] = v
	}
	var so, se syncBuf
	ev := cli.Env{
		Stdin: strings.NewReader(opt.stdin), Stdout: &so, Stderr: &se, Getenv: func(k string) string { return genv[k] },
		Geteuid: func() int { return uid }, Hostname: func() (string, error) { return migHost, nil },
		Now: func() time.Time { return toolsworld.T0.Add(time.Duration(s.clock) * time.Second) }, NewRunID: func(time.Time) string { return "01JD5Y3C8Q5G7M0V1N2P3R4S5T" },
		StatePath: s.w.Layout.StateFile(), BuiltinKeys: func() ([]byte, error) { return toolsworld.KeysJSON(), nil },
		Install: cli.InstallEnv{Root: s.w.Root, Specs: s.w.Specs, Runner: r, Own: s.w.Own, Booted: func() bool { return true },
			Collect: func(context.Context) (probe.Result, error) { return s.w.Probe, nil }},
		Backup: cli.BackupEnv{
			DumpBin: filepath.Join(s.bin, "mysqldump"), ClientBin: filepath.Join(s.bin, "mysql"),
			ServerConfig: filepath.Join(s.w.Root, "run", "server-live.yaml"),
			KDF:          backup.KDFParams{Time: 1, MemoryKiB: 64, Threads: 1},
			FailAt:       opt.failAt, HealthTimeout: 30 * time.Second, HealthInterval: 100 * time.Millisecond,
		},
	}
	s.clock++
	code = cli.Execute(context.Background(), ev, args)
	return code, so.String(), se.String()
}

type runOpt struct {
	noPassword bool
	env        map[string]string
	stdin      string
	failAt     func(string) error
}

func (s *stack) dbFlags() []string {
	return []string{"--db-host", s.be.inst.Host(), "--db-port", strconv.Itoa(s.be.inst.Port), "--db-name", s.be.dbName}
}

func (s *stack) backupDir() string { return s.host("var/lib/lingedge/backups") }

func (s *stack) passFile(pass string) string {
	p := filepath.Join(s.t.TempDir(), "pass")
	if err := os.WriteFile(p, []byte(pass+"\n"), 0o400); err != nil {
		s.t.Fatal(err)
	}
	return p
}

func okData(t *testing.T, code int, so, se string, want int) map[string]any {
	t.Helper()
	if code != want {
		t.Fatalf("退出码 %d，期望 %d\nstdout=%s\nstderr=%s", code, want, so, se)
	}
	return envelopeOf(t, so)
}

func dataOf(m map[string]any) map[string]any { d, _ := m["data"].(map[string]any); return d }

func errCodeOf(m map[string]any) string {
	es, _ := m["errors"].([]any)
	if len(es) == 0 {
		return ""
	}
	return es[0].(map[string]any)["code"].(string)
}

// ---- 数据库状态指纹 ----

// dbState 返回库内每张表的内容摘要（行集合的 SHA-256）。skip 里的表与行被排除：恢复之后 Tools 会 lineage bump，它必然改变
// system_settings 的 deploy.lineage 行并追加一条 audit_logs。
func dbState(t *testing.T, adm *sql.DB, db string) map[string]string {
	t.Helper()
	rows, err := adm.Query("SELECT table_name FROM information_schema.tables WHERE table_schema = ? AND table_type = 'BASE TABLE' ORDER BY table_name", db)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	_ = rows.Close()
	out := map[string]string{}
	for _, tb := range tables {
		q := "SELECT * FROM `" + db + "`.`" + tb + "`"
		switch tb {
		case "audit_logs", "schema_migrations_runs", "job_locks": // 运行中的 Server 持续写入的租约/日志类表（恢复后 lineage bump 还会追加审计）
			out[tb] = "(excluded)"
			continue
		case "system_settings":
			q += " WHERE setting_key <> 'deploy.lineage'"
		}
		rs, err := adm.Query(q + " ORDER BY 1")
		if err != nil {
			t.Fatalf("%s: %v", tb, err)
		}
		cols, _ := rs.Columns()
		h := sha256.New()
		n := 0
		for rs.Next() {
			vals := make([]sql.RawBytes, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rs.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for _, v := range vals {
				h.Write(v)
				h.Write([]byte{0})
			}
			n++
		}
		_ = rs.Close()
		out[tb] = fmt.Sprintf("%d:%s", n, hex.EncodeToString(h.Sum(nil))[:16])
	}
	return out
}

func sameState(a, b map[string]string) string {
	var d []string
	for k, v := range a {
		if b[k] != v {
			d = append(d, fmt.Sprintf("%s: %s -> %s", k, v, b[k]))
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			d = append(d, "+"+k)
		}
	}
	sort.Strings(d)
	return strings.Join(d, "\n")
}

func (s *stack) wipeDB() {
	s.t.Helper()
	var names []string
	rs, err := s.be.adm.Query("SELECT table_name FROM information_schema.tables WHERE table_schema = ?", s.be.dbName)
	if err != nil {
		s.t.Fatal(err)
	}
	for rs.Next() {
		var n string
		_ = rs.Scan(&n)
		names = append(names, n)
	}
	_ = rs.Close()
	conn, err := s.be.adm.Conn(context.Background())
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = conn.ExecContext(context.Background(), "SET FOREIGN_KEY_CHECKS=0")
	for _, n := range names {
		if _, err := conn.ExecContext(context.Background(), "DROP TABLE IF EXISTS `"+s.be.dbName+"`.`"+n+"`"); err != nil {
			s.t.Fatal(err)
		}
	}
}

func (s *stack) lineage() int64 {
	s.t.Helper()
	var v string
	if err := s.be.adm.QueryRow("SELECT value_json FROM `" + s.be.dbName + "`.system_settings WHERE setting_key = 'deploy.lineage'").Scan(&v); err != nil {
		s.t.Fatal(err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		s.t.Fatal(err)
	}
	return n
}

// 受备份约束的文件指纹（配置 + 服务数据；排除运行中会变化的锁与日志）。
func (s *stack) filesState() map[string]string {
	s.t.Helper()
	out := map[string]string{}
	for _, root := range []string{"etc/lingedge/server", "var/lib/lingedge/server"} {
		_ = filepath.WalkDir(s.host(root), func(p string, d os.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return nil
			}
			rel, _ := filepath.Rel(s.w.Root, p)
			fi, _ := d.Info()
			b, _ := os.ReadFile(p)
			h := sha256.Sum256(b)
			out[rel] = fi.Mode().Perm().String() + " " + hex.EncodeToString(h[:8])
			return nil
		})
	}
	return out
}

// assertNoCredentialResidue：Tools 的暂存区里不得残留任何凭据选项文件/暂存目录（用后立即删除）。
func (s *stack) assertNoCredentialResidue() {
	s.t.Helper()
	ents, _ := os.ReadDir(s.w.Layout.StagingDir())
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "my-") {
			s.t.Errorf("暂存区残留凭据目录 %s", e.Name())
		}
	}
	_ = filepath.WalkDir(s.w.Layout.StagingDir(), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, "client.cnf") {
			s.t.Errorf("残留凭据选项文件 %s", p)
		}
		return nil
	})
}

func (s *stack) assertNoLeftoverStage() {
	s.t.Helper()
	ents, _ := os.ReadDir(s.w.Layout.StagingDir())
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "backup-") || strings.HasPrefix(e.Name(), "restore-") || strings.HasPrefix(e.Name(), "rollback-") || strings.HasPrefix(e.Name(), "my-") {
			s.t.Errorf("暂存区残留 %s（备份/恢复的中间文件应在结束时清除）", e.Name())
		}
	}
}

func (s *stack) backupFiles() []string {
	ents, _ := os.ReadDir(s.backupDir())
	var out []string
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func (s *stack) unfinishedJournals() int {
	un, _ := s.w.Registry("probe").Unfinished(context.Background())
	return len(un)
}

// leakScan：口令、迁移口令、主密钥字节不得出现在 Tools 的输出与日志里。
func (s *stack) leakScan(texts ...string) {
	s.t.Helper()
	key, _ := os.ReadFile(s.host("etc/lingedge/server/master.key"))
	var needles []string
	needles = append(needles, s.be.migPW, s.be.appPW, s.be.maintPW, s.be.adminPW)
	if i := bytes.IndexByte(key, ':'); i > 0 {
		needles = append(needles, strings.TrimSpace(string(key[i+1:])))
	}
	if lb, err := os.ReadFile(s.log); err == nil {
		texts = append(texts, string(lb))
	}
	for _, tx := range texts {
		for _, n := range needles {
			if len(n) >= 8 && strings.Contains(tx, n) {
				s.t.Errorf("输出/日志含机密 %.4s…", n)
			}
		}
	}
}

// ShortFP 是 master.key 内容 SHA-256 的前 16 位十六进制（与 `lingedge install server` 输出的「主密钥指纹」同源同格式）。
func ShortFP(t *testing.T, s *stack) string {
	t.Helper()
	return fpOfFile(t, s.host("etc/lingedge/server/master.key"))[:16]
}

func fpOfFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ---- 用例 ----

// AC1/AC2/AC3/AC6：备份（加密）→ 清空库 → 恢复 → 数据一致；探针解密、lineage bump、健康检查、安全备份、journal 归档、无凭据残留。
func TestBackupWipeRestoreRealMySQL(t *testing.T) {
	s := startStack(t)
	e := s.env
	pass := "correct horse battery staple"
	pf := s.passFile(pass)
	keyFP := fpOfFile(t, s.host("etc/lingedge/server/master.key"))
	before := dbState(t, s.be.adm, s.be.dbName)
	filesBefore := s.filesState()
	lineageBefore := s.lineage()
	if len(before) < 20 {
		t.Fatalf("向导应已迁移出完整的表集合: %d", len(before))
	}

	// --- backup create --encrypt ---
	args := append([]string{"backup", "create", "--component", "server", "--encrypt", "--passphrase-file", pf, "--json", "--non-interactive", "--log-file", e.log}, s.dbFlags()...)
	code, so, se := s.runB(0, runOpt{}, args...)
	env := okData(t, code, so, se, 0)
	d := dataOf(env)
	path := d["path"].(string)
	if !strings.HasPrefix(path, s.backupDir()+"/lingedge-backup-edge-host-1-server-") || !strings.HasSuffix(path, ".tar.gz.enc") {
		t.Errorf("备份路径/命名: %s", path)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("备份文件应为 0600: %v %v", fi, err)
	}
	if d["key_fingerprint"] != keyFP {
		t.Errorf("清单里的主密钥指纹应为 master.key 的 SHA-256: %v vs %s", d["key_fingerprint"], keyFP)
	}
	if got := int64(d["deploy_lineage"].(float64)); got != lineageBefore {
		t.Errorf("deploy_lineage = %d，库里是 %d", got, lineageBefore)
	}
	if d["notice"] == nil || !strings.Contains(d["notice"].(string), "分开保管") {
		t.Errorf("成功备份必须醒目输出主密钥保管提示（F-7）: %v", d["notice"])
	}
	kek := d["kek_ids_required"].([]any)
	if len(kek) == 0 {
		t.Fatalf("向导写入了加密的签名私钥与运行期凭据，kek_ids_required 不应为空: %v", d)
	}
	if int(d["schema_version"].(float64)) < 18 {
		t.Errorf("schema_version=%v", d["schema_version"])
	}
	// 人类输出同样有醒目提示。
	// 加密备份的原始字节里不得出现任何明文：主密钥字节、库名里的业务词、SQL 关键字。
	raw, _ := os.ReadFile(path)
	keyb, _ := os.ReadFile(s.host("etc/lingedge/server/master.key"))
	for _, needle := range []string{strings.TrimSpace(string(keyb[bytes.IndexByte(keyb, ':')+1:])), "CREATE TABLE", "INSERT INTO", "listen:", s.be.dbName, "lingedge_app"} {
		if bytes.Contains(raw, []byte(needle)) {
			t.Errorf("加密备份里出现明文 %q", needle)
		}
	}
	s.assertNoCredentialResidue()
	s.assertNoLeftoverStage()

	// --- backup list（无需口令）：展示所需 kek_id；verify（需要口令）---
	code, so, se = s.runB(0, runOpt{}, "backup", "list", "--json")
	lst := dataOf(okData(t, code, so, se, 0))["backups"].([]any)
	if len(lst) != 1 {
		t.Fatalf("list: %v", lst)
	}
	le := lst[0].(map[string]any)
	if le["encrypted"] != true || le["status"] != "ok" || le["meta_authenticated"] != false {
		t.Errorf("加密备份的 list 项: %v", le)
	}
	if m := le["meta"].(map[string]any); fmt.Sprint(m["kek_ids_required"]) != fmt.Sprint(kek) || m["key_fingerprint"] != keyFP {
		t.Errorf("list 应展示备份所需的 kek_id 与指纹: %v", m)
	}
	code, so, se = s.runB(0, runOpt{}, "backup", "verify", path, "--passphrase-file", pf, "--json")
	vd := dataOf(okData(t, code, so, se, 0))
	if vd["ok"] != true || vd["encrypted"] != true {
		t.Errorf("verify: %v", vd)
	}
	// verify 不改动系统。
	if d := sameState(before, dbState(t, s.be.adm, s.be.dbName)); d != "" {
		t.Errorf("list/verify 不得改动数据库:\n%s", d)
	}
	// 错误口令 → 5，不泄露；缺口令（非交互）→ 3。
	wrong := s.passFile("a different passphrase")
	code, so, se = s.runB(0, runOpt{}, "backup", "verify", path, "--passphrase-file", wrong, "--json")
	if em := okData(t, code, so, se, 5); errCodeOf(em) != "verify.bad_passphrase" {
		t.Errorf("错误口令: %s", so)
	}
	if code, so, se = s.runB(0, runOpt{}, "backup", "verify", path, "--json", "--non-interactive"); code != 3 {
		t.Errorf("缺口令应退出码 3: %d %s %s", code, so, se)
	}
	// 篡改一个字节 → verify 失败（5）。
	tampered := filepath.Join(t.TempDir(), "lingedge-backup-x-server-20260101T000000Z.tar.gz.enc")
	bad := append([]byte(nil), raw...)
	bad[len(bad)/2] ^= 1
	_ = os.WriteFile(tampered, bad, 0o600)
	if code, so, se = s.runB(0, runOpt{}, "backup", "verify", tampered, "--passphrase-file", pf, "--json"); code != 5 {
		t.Errorf("篡改应退出码 5: %d %s %s", code, so, se)
	}

	// --- 清空测试库 → restore ---
	s.wipeDB()
	rargs := append([]string{"restore"}, path, "--passphrase-file", pf, "--json", "--non-interactive", "--yes", "--log-file", e.log)
	code, so, se = s.runB(0, runOpt{}, append([]string{"backup"}, append(rargs, s.dbFlags()...)...)...)
	renv := okData(t, code, so, se, 0)
	rd := dataOf(renv)
	if rd["outcome"] != "restored" || rd["key_check"] != "match" {
		t.Fatalf("恢复结果: %v", rd)
	}
	if after := dbState(t, s.be.adm, s.be.dbName); sameState(before, after) != "" {
		t.Errorf("备份→清空→恢复后数据应一致:\n%s", sameState(before, after))
	}
	if probe := rd["probe"].(map[string]any); probe["skipped"] != false || len(probe["probed"].([]any)) != len(kek) {
		t.Errorf("恢复后应对每个所需 kek_id 解密一条探针记录: %v", probe)
	}
	// lineage：严格大于恢复前节点已应用的值（IMP-07/H6：max(DB 值+1, 当前 UNIX 秒)）。
	la := int64(rd["lineage_after"].(float64))
	if la <= lineageBefore || la != s.lineage() || la < time.Now().Unix()-60 {
		t.Errorf("lineage bump: %d（备份时 %d，库里 %d）", la, lineageBefore, s.lineage())
	}
	if rd["health"] == nil || !strings.HasPrefix(rd["health"].(string), "healthz") {
		t.Errorf("健康检查结果: %v", rd["health"])
	}
	if d := sameFiles(filesBefore, s.filesState()); d != "" {
		t.Errorf("配置与服务数据恢复后应与备份时一致（权限/内容）:\n%s", d)
	}
	// 恢复前安全备份存在且可校验；journal 已归档；暂存区干净；凭据无残留；服务在运行。
	safety := rd["safety_backup"].(string)
	if _, err := os.Stat(safety); err != nil {
		t.Fatalf("安全备份应保留: %v", err)
	}
	code, so, se = s.runB(0, runOpt{}, "backup", "verify", safety, "--passphrase-file", pf, "--json")
	if vd := dataOf(okData(t, code, so, se, 0)); vd["purpose"] != "pre-restore" {
		t.Errorf("安全备份的用途: %v", vd)
	}
	if n := s.unfinishedJournals(); n != 0 {
		t.Errorf("恢复成功后不应有未完成事务: %d", n)
	}
	if !e.w.FakeHas("active-lingedge-server.service") {
		t.Error("恢复后服务应在运行")
	}
	s.assertNoCredentialResidue()
	s.assertNoLeftoverStage()
	s.leakScan(so, se)
	if err := httpGetOK(s.baseURL + "/healthz/ready"); err != nil {
		t.Errorf("恢复后真实 Server 应就绪: %v", err)
	}
}

func sameFiles(a, b map[string]string) string { return sameState(a, b) }

// otherKey 由 master.key 内容得到「同 kek_id、不同密钥字节」的另一把合法主密钥（格式同生产：<kek_id>:<base64url 32 字节>）。
func otherKey(orig []byte) []byte {
	i := bytes.IndexByte(orig, ':')
	id := string(orig[:i])
	k := make([]byte, 32)
	for j := range k {
		k[j] = byte(200 - j)
	}
	return []byte(id + ":" + base64.RawURLEncoding.EncodeToString(k) + "\n")
}

// kek_ids_required 的扫描范围必须覆盖迁移后真实库里的全部 *_enc 列：新增密文列而没有登记会在这里失败（否则备份会漏记所需的 kek_id，MIN-05 的核对就不完整）。
func TestEncTargetsMatchMigrations(t *testing.T) {
	s := startStack(t)
	rows, err := s.be.adm.Query("SELECT TABLE_NAME, COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND COLUMN_NAME LIKE '%\\_enc' ORDER BY TABLE_NAME, COLUMN_NAME", s.be.dbName)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var actual, declared []string
	for rows.Next() {
		var tb, c string
		if err := rows.Scan(&tb, &c); err != nil {
			t.Fatal(err)
		}
		actual = append(actual, tb+"."+c)
	}
	for _, tc := range backup.EncTargets() {
		declared = append(declared, tc[0]+"."+tc[1])
	}
	sort.Strings(declared)
	if strings.Join(actual, ",") != strings.Join(declared, ",") {
		t.Fatalf("备份扫描的密文列与迁移后的库不一致\n库内: %v\n声明: %v", actual, declared)
	}
}

// 未加密备份：0600、root 0700 目录、醒目警告；不含 master.key 与迁移凭据；--include-master-key 必须同时 --encrypt（退出码 2）。
func TestBackupUnencryptedRulesAndMasterKeyHandling(t *testing.T) {
	s := startStack(t)
	e := s.env
	code, so, se := s.runB(0, runOpt{}, append([]string{"backup", "create", "--component", "server", "--json", "--non-interactive", "--log-file", e.log}, s.dbFlags()...)...)
	env := okData(t, code, so, se, 0)
	d := dataOf(env)
	path := d["path"].(string)
	if d["encrypted"] != false || !strings.HasSuffix(path, ".tar.gz") {
		t.Errorf("未加密备份: %v", d)
	}
	hasWarn := false
	for _, w := range env["warnings"].([]any) {
		hasWarn = hasWarn || w.(map[string]any)["code"] == "backup.unencrypted"
	}
	if !hasWarn {
		t.Errorf("未加密备份应警告: %v", env["warnings"])
	}
	if fi, err := os.Stat(s.backupDir()); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("缺省备份目录应为 0700: %v %v", fi, err)
	}
	// 清单：不含 master.key、migrate.yaml、setup-token.hash；含配置、数据、数据库转储、Tools 状态。
	vr, err := (&backup.Service{}).Verify(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, f := range vr.Manifest.Files {
		kinds[f.Kind]++
		if strings.HasSuffix(f.Dest, "master.key") || strings.HasSuffix(f.Dest, "migrate.yaml") || strings.HasSuffix(f.Dest, "setup-token.hash") {
			t.Errorf("默认备份不得含 %s", f.Dest)
		}
	}
	if kinds[backup.KindDB] != 1 || kinds[backup.KindConfig] == 0 || kinds[backup.KindData] == 0 || kinds[backup.KindToolsState] != 1 {
		t.Errorf("备份内容类别: %v", kinds)
	}
	if vr.Manifest.MasterKeyFingerprint == "" || len(vr.Manifest.KEKFingerprints) == 0 || vr.Manifest.Database == nil || vr.Manifest.DeployLineage == nil || len(vr.Manifest.MigrationHistory) < 18 {
		t.Errorf("清单字段不全: %+v", vr.Manifest)
	}
	// 未加密备份的文件里含 SQL（它是明文）——这正是需要警告的原因；但 master.key 的密钥字节绝不在其中。
	raw, _ := os.ReadFile(path)
	keyb, _ := os.ReadFile(s.host("etc/lingedge/server/master.key"))
	if bytes.Contains(raw, []byte(strings.TrimSpace(string(keyb[bytes.IndexByte(keyb, ':')+1:])))) {
		t.Error("备份不得含主密钥字节")
	}
	// 人类可读输出：醒目的 F-7 提示（原文）、主密钥指纹（与 install 输出同格式）、所需 kek_id。
	code, so, se = s.runB(0, runOpt{}, append([]string{"backup", "create", "--component", "server", "--non-interactive"}, s.dbFlags()...)...)
	if code != 0 || !strings.Contains(so, "数据库与私钥为密文，恢复必须有对应 master.key，请与数据库备份分开保管") || !strings.Contains(so, ShortFP(t, s)) || !strings.Contains(so, "所需 kek_id：k1") {
		t.Errorf("人类输出应醒目提示主密钥保管并给出指纹/kek_id（%d）:\n%s\n%s", code, so, se)
	}
	// --include-master-key 必须同时 --encrypt。
	code, so, _ = s.runB(0, runOpt{}, append([]string{"backup", "create", "--component", "server", "--include-master-key", "--json", "--non-interactive"}, s.dbFlags()...)...)
	if code != 2 || errCodeOf(envelopeOf(t, so)) != "usage.flag_requires" {
		t.Errorf("--include-master-key 无 --encrypt 应退出码 2: %d %s", code, so)
	}
	// 目录对 group/other 开放时拒绝未加密备份（退出码 3）。
	open := filepath.Join(t.TempDir(), "open")
	_ = os.MkdirAll(open, 0o755)
	_ = os.Chmod(open, 0o755)
	code, so, _ = s.runB(0, runOpt{}, append([]string{"backup", "create", "--component", "server", "--output", open, "--json", "--non-interactive"}, s.dbFlags()...)...)
	if code != 3 || errCodeOf(envelopeOf(t, so)) != "precondition.backup_dir" {
		t.Errorf("开放目录里的未加密备份应被拒绝: %d %s", code, so)
	}
	if ents, _ := os.ReadDir(open); len(ents) != 0 {
		t.Errorf("被拒绝时不得写文件: %v", ents)
	}
	// 非交互且没有数据库口令 → 3，尚未改动任何东西。
	before := s.backupFiles()
	code, so, _ = s.runB(0, runOpt{noPassword: true}, append([]string{"backup", "create", "--component", "server", "--json", "--non-interactive"}, s.dbFlags()...)...)
	if code != 3 || errCodeOf(envelopeOf(t, so)) != "precondition.db_credentials" {
		t.Errorf("无口令应退出码 3: %d %s", code, so)
	}
	if fmt.Sprint(before) != fmt.Sprint(s.backupFiles()) {
		t.Error("失败的备份不得留下文件")
	}
	// --dry-run：不写任何东西，输出计划。
	code, so, se = s.runB(0, runOpt{}, "backup", "create", "--component", "server", "--dry-run", "--json", "--non-interactive")
	dd := dataOf(okData(t, code, so, se, 0))
	if dd["dry_run"] != true || dd["path"] != nil || len(dd["files"].([]any)) == 0 || fmt.Sprint(before) != fmt.Sprint(s.backupFiles()) {
		t.Errorf("dry-run: %v", dd)
	}
	s.assertNoCredentialResidue()
	s.assertNoLeftoverStage()
	s.leakScan(so, se)
}

// AC4：目标库默认须为空；--replace 需二次确认主机名；恢复到非空库（replace）后数据回到备份状态。
func TestRestoreRequiresEmptyTargetUnlessReplace(t *testing.T) {
	s := startStack(t)
	e := s.env
	code, so, se := s.runB(0, runOpt{}, append([]string{"backup", "create", "--component", "server", "--json", "--non-interactive"}, s.dbFlags()...)...)
	path := dataOf(okData(t, code, so, se, 0))["path"].(string)
	before := dbState(t, s.be.adm, s.be.dbName)
	// 备份之后产生的新数据（恢复应使之消失）。
	if _, err := s.be.adm.Exec("UPDATE `" + s.be.dbName + "`.system_settings SET group_name = 'changed-after-backup' WHERE setting_key = 'deploy.lineage'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.be.adm.Exec("INSERT INTO `" + s.be.dbName + "`.system_settings (uid, setting_key, value_json, group_name) VALUES ('01JD5Y3C8Q5G7M0V1N2P3R4S5Z', 'marker.after_backup', '1', 'test')"); err != nil {
		t.Fatal(err)
	}
	// 备份之后新建的表（转储里没有它：只有真正「清空」才会让它消失，仅靠转储里的 DROP TABLE IF EXISTS 不够）。视图的 DROP 由 backup 包的单元测试覆盖——
	// 测试里由超级管理员创建的视图带 SYSTEM_USER 定义者，迁移账号本来就无权删除，与生产（视图由迁移账号自己创建）不同。
	if _, err := s.be.adm.Exec("CREATE TABLE `" + s.be.dbName + "`.zz_extra_after_backup (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	dirty := dbState(t, s.be.adm, s.be.dbName)
	filesBefore := s.filesState()
	nBackups := len(s.backupFiles())

	// 非空且未 --replace → 3，且一切原样（连安全备份都不创建）。
	rest := append([]string{"backup", "restore", path, "--json", "--non-interactive", "--yes"}, s.dbFlags()...)
	code, so, se = s.runB(0, runOpt{}, rest...)
	if code != 3 || errCodeOf(envelopeOf(t, so)) != "precondition.db_not_empty" {
		t.Fatalf("非空目标应拒绝: %d %s %s", code, so, se)
	}
	if d := sameState(dirty, dbState(t, s.be.adm, s.be.dbName)); d != "" || len(s.backupFiles()) != nBackups || sameState(filesBefore, s.filesState()) != "" {
		t.Errorf("拒绝时不得改动任何东西: %s", d)
	}
	// --replace 非交互：缺 --yes/--confirm-name → 9；名字不符 → 9；都对 → 恢复。
	noYes := append([]string{"backup", "restore", path, "--replace", "--json", "--non-interactive"}, s.dbFlags()...)
	if code, so, _ = s.runB(0, runOpt{}, noYes...); code != 9 {
		t.Errorf("--replace 缺确认应退出码 9: %d %s", code, so)
	}
	wrongName := append([]string{"backup", "restore", path, "--replace", "--yes", "--confirm-name", "other-host", "--json", "--non-interactive"}, s.dbFlags()...)
	if code, so, _ = s.runB(0, runOpt{}, wrongName...); code != 9 {
		t.Errorf("--replace 主机名不符应退出码 9: %d %s", code, so)
	}
	if sameState(dirty, dbState(t, s.be.adm, s.be.dbName)) != "" {
		t.Fatal("确认未通过时不得改动数据库")
	}
	good := append([]string{"backup", "restore", path, "--replace", "--yes", "--confirm-name", migHost, "--json", "--non-interactive", "--log-file", e.log}, s.dbFlags()...)
	code, so, se = s.runB(0, runOpt{}, good...)
	rd := dataOf(okData(t, code, so, se, 0))
	if rd["replace"] != true || int(rd["db_objects_before"].(float64)) < 20 {
		t.Errorf("replace 结果: %v", rd)
	}
	after := dbState(t, s.be.adm, s.be.dbName)
	if d := sameState(before, after); d != "" {
		t.Errorf("--replace 恢复后应回到备份时的数据:\n%s", d)
	}
	var n int
	_ = s.be.adm.QueryRow("SELECT COUNT(*) FROM `" + s.be.dbName + "`.system_settings WHERE setting_key = 'marker.after_backup'").Scan(&n)
	if n != 0 {
		t.Error("备份之后产生的数据应随恢复消失")
	}
	_ = s.be.adm.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = ? AND table_name = 'zz_extra_after_backup'", s.be.dbName).Scan(&n)
	if n != 0 {
		t.Errorf("--replace 应先清空目标库：备份之后新建的表不应留下（%d 个）", n)
	}
	s.assertNoCredentialResidue()
	s.assertNoLeftoverStage()
	s.leakScan(so, se)
}

// AC4：主密钥指纹不匹配或缺失 → 拒绝并说明；提供匹配的 --master-key-file 才继续（旧文件保留）；备份自带主密钥时 --use-backup-master-key。
func TestRestoreMasterKeyFingerprintGate(t *testing.T) {
	s := startStack(t)
	pf := s.passFile("another long passphrase!")
	keyPath := s.host("etc/lingedge/server/master.key")
	orig, _ := os.ReadFile(keyPath)
	origCopy := filepath.Join(t.TempDir(), "orig.key")
	if err := os.WriteFile(origCopy, orig, 0o400); err != nil {
		t.Fatal(err)
	}
	// 一份带主密钥的加密备份（--include-master-key）与一份普通备份。
	code, so, se := s.runB(0, runOpt{}, append([]string{"backup", "create", "--component", "server", "--encrypt", "--include-master-key", "--passphrase-file", pf, "--json", "--non-interactive"}, s.dbFlags()...)...)
	withKey := dataOf(okData(t, code, so, se, 0))["path"].(string)
	vr, err := (&backup.Service{}).Verify(withKey, []byte("another long passphrase!"))
	if err != nil || !vr.Manifest.MasterKeyIncluded {
		t.Fatalf("带主密钥的备份: %v %+v", err, vr)
	}
	plain := func() string {
		code, so, se := s.runB(0, runOpt{}, append([]string{"backup", "create", "--component", "server", "--encrypt", "--passphrase-file", pf, "--json", "--non-interactive"}, s.dbFlags()...)...)
		return dataOf(okData(t, code, so, se, 0))["path"].(string)
	}()
	before := dbState(t, s.be.adm, s.be.dbName)
	filesBefore := s.filesState()
	keyFiles := func() []string {
		ents, _ := os.ReadDir(filepath.Dir(keyPath))
		var n []string
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "master.key") {
				n = append(n, e.Name())
			}
		}
		return n
	}

	// 1) 本机主密钥换成另一把（同 kek_id、不同字节）→ 指纹不匹配，拒绝。
	other := otherKey(orig)
	swapKey := func(b []byte) {
		_ = os.Chmod(keyPath, 0o600)
		if err := os.WriteFile(keyPath, b, 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(keyPath, 0o400)
	}
	swapKey(other)
	nb := len(s.backupFiles())
	rest := append([]string{"backup", "restore", plain, "--passphrase-file", pf, "--replace", "--yes", "--confirm-name", migHost, "--json", "--non-interactive"}, s.dbFlags()...)
	code, so, se = s.runB(0, runOpt{}, rest...)
	if code != 3 || errCodeOf(envelopeOf(t, so)) != "precondition.master_key_mismatch" {
		t.Fatalf("指纹不匹配应退出码 3: %d %s %s", code, so, se)
	}
	if !strings.Contains(so, "master.key") {
		t.Errorf("应说明需要对应的 master.key: %s", so)
	}
	if sameState(before, dbState(t, s.be.adm, s.be.dbName)) != "" || len(s.backupFiles()) != nb {
		t.Error("拒绝时不得改动数据库或创建备份")
	}
	// 2) 本机主密钥缺失 → 同样拒绝。
	_ = os.Chmod(keyPath, 0o600)
	_ = os.Remove(keyPath)
	code, so, se = s.runB(0, runOpt{}, rest...)
	if code != 3 || errCodeOf(envelopeOf(t, so)) != "precondition.master_key_mismatch" {
		t.Fatalf("主密钥缺失应退出码 3: %d %s %s", code, so, se)
	}
	// 3) 候选主密钥本身也不匹配 → 仍拒绝。
	wrongCand := filepath.Join(t.TempDir(), "wrong.key")
	_ = os.WriteFile(wrongCand, other, 0o400)
	code, so, _ = s.runB(0, runOpt{}, append(rest, "--master-key-file", wrongCand)...)
	if code != 3 || errCodeOf(envelopeOf(t, so)) != "precondition.master_key_mismatch" {
		t.Fatalf("不匹配的候选主密钥应被拒绝: %d %s", code, so)
	}
	// 4) 备份没带主密钥却要求 --use-backup-master-key → 拒绝。
	code, so, _ = s.runB(0, runOpt{}, append(rest, "--use-backup-master-key")...)
	if code != 3 {
		t.Fatalf("备份不含主密钥时 --use-backup-master-key 应被拒绝: %d %s", code, so)
	}
	if sameState(before, dbState(t, s.be.adm, s.be.dbName)) != "" {
		t.Fatal("所有拒绝路径都不得改动数据库")
	}
	// 5) 提供匹配的 --master-key-file：安装它（本机没有 → 新装），恢复成功，且探针解密通过。
	code, so, se = s.runB(0, runOpt{}, append(rest, "--master-key-file", origCopy)...)
	rd := dataOf(okData(t, code, so, se, 0))
	if rd["key_check"] != "installed_from_file" {
		t.Errorf("key_check=%v", rd["key_check"])
	}
	if b, _ := os.ReadFile(keyPath); !bytes.Equal(b, orig) {
		t.Error("主密钥应已装为备份对应的那把")
	}
	if fi, _ := os.Stat(keyPath); fi.Mode().Perm() != 0o400 {
		t.Errorf("安装后的 master.key 应为 0400: %v", fi.Mode().Perm())
	}
	if sameState(before, dbState(t, s.be.adm, s.be.dbName)) != "" {
		t.Error("恢复后数据应与备份一致")
	}
	// 6) 本机是另一把主密钥 → 用 --master-key-file 替换：旧文件保留为 master.key.pre-restore-*（Tools 绝不删除主密钥）。
	swapKey(other)
	code, so, se = s.runB(0, runOpt{}, append(rest, "--master-key-file", origCopy)...)
	rd = dataOf(okData(t, code, so, se, 0))
	saved, _ := rd["saved_key_file"].(string)
	if !strings.Contains(saved, "master.key.pre-restore-") {
		t.Fatalf("被替换的旧主密钥应保留: %v", rd)
	}
	if b, _ := os.ReadFile(saved); !bytes.Equal(b, other) {
		t.Error("保留的应是被替换的那把")
	}
	if fi, _ := os.Stat(saved); fi.Mode().Perm() != 0o400 {
		t.Errorf("保留的旧主密钥应为 0400: %v", fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(keyPath); !bytes.Equal(b, orig) {
		t.Error("主密钥应已换为备份对应的那把")
	}
	// 7) 备份自带主密钥：本机缺失时 --use-backup-master-key 直接装回。
	_ = os.Chmod(keyPath, 0o600)
	_ = os.Remove(keyPath)
	rest2 := append([]string{"backup", "restore", withKey, "--passphrase-file", pf, "--replace", "--yes", "--confirm-name", migHost, "--use-backup-master-key", "--json", "--non-interactive"}, s.dbFlags()...)
	code, so, se = s.runB(0, runOpt{}, rest2...)
	rd = dataOf(okData(t, code, so, se, 0))
	if rd["key_check"] != "installed_from_backup" {
		t.Errorf("key_check=%v", rd["key_check"])
	}
	if b, _ := os.ReadFile(keyPath); !bytes.Equal(b, orig) {
		t.Error("应从备份装回主密钥")
	}
	_ = keyFiles
	_ = filesBefore
	s.assertNoCredentialResidue()
	s.assertNoLeftoverStage()
	s.leakScan(so, se)
}

// AC4：拒绝把更新 schema 的备份恢复到旧二进制（先升级二进制）；一切原样。
func TestRestoreRefusesNewerSchemaBackup(t *testing.T) {
	s := startStack(t)
	if _, err := s.be.adm.Exec("INSERT INTO `" + s.be.dbName + "`.schema_migrations (version_id, is_applied) VALUES (999, 1)"); err != nil {
		t.Skipf("schema_migrations 的列与预期不同，无法构造「更新 schema」的备份: %v", err)
	}
	code, so, se := s.runB(0, runOpt{}, append([]string{"backup", "create", "--component", "server", "--json", "--non-interactive"}, s.dbFlags()...)...)
	d := dataOf(okData(t, code, so, se, 0))
	if int(d["schema_version"].(float64)) != 999 {
		t.Fatalf("备份应记录 schema_version=999: %v", d["schema_version"])
	}
	path := d["path"].(string)
	if _, err := s.be.adm.Exec("DELETE FROM `" + s.be.dbName + "`.schema_migrations WHERE version_id = 999"); err != nil {
		t.Fatal(err)
	}
	before := dbState(t, s.be.adm, s.be.dbName)
	filesBefore := s.filesState()
	nb := len(s.backupFiles())
	code, so, se = s.runB(0, runOpt{}, append([]string{"backup", "restore", path, "--replace", "--yes", "--confirm-name", migHost, "--json", "--non-interactive"}, s.dbFlags()...)...)
	if code != 3 || errCodeOf(envelopeOf(t, so)) != "precondition.schema_newer" {
		t.Fatalf("更新 schema 的备份应被拒绝: %d %s %s", code, so, se)
	}
	if !strings.Contains(so, "999") || !strings.Contains(so, "18") {
		t.Errorf("应说明两个版本: %s", so)
	}
	if sameState(before, dbState(t, s.be.adm, s.be.dbName)) != "" || sameState(filesBefore, s.filesState()) != "" || len(s.backupFiles()) != nb {
		t.Error("拒绝时不得改动任何东西")
	}
}

// AC4：恢复中途失败 → 回滚到恢复前安全备份（文件、数据库、master.key、服务状态），退出码 10，journal 以 abort 归档，不留半状态。
func TestRestoreFailureRollsBackToSafetyBackup(t *testing.T) {
	for _, point := range []string{"after:stop_service", "after:master_key", "after:restore_files", "after:restore_database", "after:probe_decrypt", "after:lineage_bump", "after:start_and_health"} {
		t.Run(point, func(t *testing.T) {
			s := startStack(t)
			keyPath := s.host("etc/lingedge/server/master.key")
			orig, _ := os.ReadFile(keyPath)
			origCopy := filepath.Join(t.TempDir(), "orig.key")
			_ = os.WriteFile(origCopy, orig, 0o400)
			code, so, se := s.runB(0, runOpt{}, append([]string{"backup", "create", "--component", "server", "--json", "--non-interactive"}, s.dbFlags()...)...)
			path := dataOf(okData(t, code, so, se, 0))["path"].(string)

			// 备份之后：改库、改配置，并把主密钥换成另一把（使 after:master_key 这一步真实发生），这就是「恢复前状态」。
			if _, err := s.be.adm.Exec("INSERT INTO `" + s.be.dbName + "`.system_settings (uid, setting_key, value_json, group_name) VALUES ('01JD5Y3C8Q5G7M0V1N2P3R4S5Y', 'marker.pre_restore', '1', 'test')"); err != nil {
				t.Fatal(err)
			}
			cfg := s.host("etc/lingedge/server/server.yaml")
			cb, _ := os.ReadFile(cfg)
			_ = os.WriteFile(cfg, append(cb, []byte("# edited after backup\n")...), 0o640)
			other := otherKey(orig)
			_ = os.Chmod(keyPath, 0o600)
			_ = os.WriteFile(keyPath, other, 0o600)
			_ = os.Chmod(keyPath, 0o400)
			// 备份的 kek 需要原主密钥；本机现在是另一把 → 需要 --master-key-file 才能进入 master_key 步骤。
			preDB := dbState(t, s.be.adm, s.be.dbName)
			preFiles := s.filesState()
			preLineage := s.lineage()
			boom := fmt.Errorf("注入故障 %s", point)
			opt := runOpt{failAt: func(p string) error {
				if p == point {
					return boom
				}
				return nil
			}}
			args := append([]string{"backup", "restore", path, "--replace", "--yes", "--confirm-name", migHost, "--master-key-file", origCopy, "--json", "--non-interactive", "--log-file", s.log}, s.dbFlags()...)
			code, so, se = s.runB(0, opt, args...)
			em := envelopeOf(t, so)
			if code != 10 || errCodeOf(em) != "restore.rolled_back" || em["exit_name"] != "ROLLED_BACK" {
				t.Fatalf("失败应退出码 10 ROLLED_BACK: %d %s %s", code, so, se)
			}
			if d := sameState(preDB, dbState(t, s.be.adm, s.be.dbName)); d != "" {
				t.Errorf("数据库应回到恢复前:\n%s", d)
			}
			if d := sameFiles(preFiles, s.filesState()); d != "" {
				t.Errorf("文件（含 master.key）应回到恢复前:\n%s", d)
			}
			if b, _ := os.ReadFile(keyPath); !bytes.Equal(b, other) {
				t.Error("master.key 应回到恢复前（另一把）")
			}
			ents, _ := os.ReadDir(filepath.Dir(keyPath))
			for _, en := range ents {
				if strings.HasPrefix(en.Name(), "master.key.pre-restore-") {
					t.Errorf("回滚后不应残留 %s", en.Name())
				}
			}
			if !s.w.FakeHas("active-lingedge-server.service") {
				t.Error("服务在恢复前是运行的，回滚后应恢复运行")
			}
			if n := s.unfinishedJournals(); n != 0 {
				t.Errorf("回滚后事务应以 abort 结束并归档: %d", n)
			}
			if s.lineage() < preLineage {
				t.Errorf("lineage 不得倒退: %d < %d", s.lineage(), preLineage)
			}
			s.assertNoCredentialResidue()
			s.assertNoLeftoverStage()
			s.leakScan(so, se)
			// 安全备份保留（可人工核对），用途为 pre-restore。
			found := false
			for _, n := range s.backupFiles() {
				found = found || strings.Contains(n, "-server-")
			}
			if !found {
				t.Error("安全备份应保留")
			}
		})
	}
}

// 探针校验是「恢复后数据与密钥一致可用」的权威判据：主密钥指纹相同、数据库也灌进去了，但密文本身解不开（此处在备份前把签名私钥密文破坏）→ 回滚，退出码 10。
func TestRestoreProbeFailureRollsBack(t *testing.T) {
	s := startStack(t)
	if _, err := s.be.adm.Exec("UPDATE `" + s.be.dbName + "`.signing_keys SET private_key_enc = CONCAT(SUBSTRING(private_key_enc, 1, CHAR_LENGTH(private_key_enc) - 6), 'AAAAAA')"); err != nil {
		t.Fatal(err)
	}
	code, so, se := s.runB(0, runOpt{}, append([]string{"backup", "create", "--component", "server", "--json", "--non-interactive"}, s.dbFlags()...)...)
	path := dataOf(okData(t, code, so, se, 0))["path"].(string)
	if _, err := s.be.adm.Exec("INSERT INTO `" + s.be.dbName + "`.system_settings (uid, setting_key, value_json, group_name) VALUES ('01JD5Y3C8Q5G7M0V1N2P3R4S5W', 'marker.pre_restore', '1', 'test')"); err != nil {
		t.Fatal(err)
	}
	preDB := dbState(t, s.be.adm, s.be.dbName)
	preFiles := s.filesState()
	code, so, se = s.runB(0, runOpt{}, append([]string{"backup", "restore", path, "--replace", "--yes", "--confirm-name", migHost, "--json", "--non-interactive"}, s.dbFlags()...)...)
	em := envelopeOf(t, so)
	if code != 10 || errCodeOf(em) != "restore.rolled_back" || !strings.Contains(so, "probe_decrypt") {
		t.Fatalf("探针解密失败应回滚（10）并点名步骤: %d %s %s", code, so, se)
	}
	if d := sameState(preDB, dbState(t, s.be.adm, s.be.dbName)); d != "" {
		t.Errorf("数据库应回到恢复前:\n%s", d)
	}
	if d := sameFiles(preFiles, s.filesState()); d != "" {
		t.Errorf("文件应回到恢复前:\n%s", d)
	}
	s.assertNoCredentialResidue()
	s.assertNoLeftoverStage()
	s.leakScan(so, se)
}

// 恢复事务在「停服务」之后崩溃（journal 未结束）：recover --rollback 经 journal 里的撤销动作回到安全备份（文件、数据库、master.key、服务）。
func TestRecoverRollsBackInterruptedRestore(t *testing.T) {
	s := startStack(t)
	code, so, se := s.runB(0, runOpt{}, append([]string{"backup", "create", "--component", "server", "--json", "--non-interactive"}, s.dbFlags()...)...)
	path := dataOf(okData(t, code, so, se, 0))["path"].(string)
	if _, err := s.be.adm.Exec("INSERT INTO `" + s.be.dbName + "`.system_settings (uid, setting_key, value_json, group_name) VALUES ('01JD5Y3C8Q5G7M0V1N2P3R4S5X', 'marker.pre_restore', '1', 'test')"); err != nil {
		t.Fatal(err)
	}
	preDB := dbState(t, s.be.adm, s.be.dbName)
	preFiles := s.filesState()

	// 模拟进程死亡：故障注入点触发 panic，journal 不会被结束；锁与 flock 随 goroutine 退出释放（由 Release 的 defer 完成）。
	// 用 runtime.Goexit 的等价方式：在子 goroutine 里 panic 并 recover，使 cli.Execute 栈被展开，journal 保持未完成。
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }()
		s.runB(0, runOpt{failAt: func(p string) error {
			if p == "after:restore_database" {
				panic("simulated crash")
			}
			return nil
		}}, append([]string{"backup", "restore", path, "--replace", "--yes", "--confirm-name", migHost, "--json", "--non-interactive"}, s.dbFlags()...)...)
	}()
	<-done
	// 数据库已被恢复覆盖（崩溃时半途状态）。
	if sameState(preDB, dbState(t, s.be.adm, s.be.dbName)) == "" {
		t.Fatal("崩溃点之后数据库应已被恢复覆盖（否则本用例没有测到半状态）")
	}
	// 事务因持有者（本进程）仍存活而被视为活的？journal 的 flock 在 Close 时释放：Task.Release 的 defer 在展开时执行 → 事务变为「未完成」。
	un, _ := s.w.Registry("probe").Unfinished(context.Background())
	if len(un) != 1 {
		t.Fatalf("应有 1 个未完成事务: %v", un)
	}
	// 新写命令被拦住（13）。
	code, so, _ = s.runB(0, runOpt{}, "uninstall", "server", "--yes", "--json", "--non-interactive")
	if code != 13 {
		t.Fatalf("存在未完成事务时写命令应退出码 13: %d %s", code, so)
	}
	// recover --rollback 需要数据库迁移账号凭据与连接目标（recover 没有命令行标志，走环境变量通道）：缺目标时明确失败（12）并保留 journal——绝不假装回滚成功。
	code, so, se = s.runB(0, runOpt{}, "recover", "--rollback", "--json", "--non-interactive")
	if code != 12 || !strings.Contains(so, "restore_safety_backup") {
		t.Fatalf("缺数据库连接目标时 recover 应明确失败（12）: %d %s", code, so)
	}
	if n := s.unfinishedJournals(); n != 1 {
		t.Fatalf("失败的 recover 必须保留 journal: %d", n)
	}
	s.leakScan(so, se)
	genv := map[string]string{backup.EnvHost: s.be.inst.Host(), backup.EnvPort: strconv.Itoa(s.be.inst.Port), backup.EnvName: s.be.dbName}
	code, so, se = s.runB(0, runOpt{env: genv}, "recover", "--rollback", "--json", "--non-interactive")
	rd := dataOf(okData(t, code, so, se, 0))
	txs := rd["transactions"].([]any)
	if len(txs) != 1 || !strings.Contains(txs[0].(map[string]any)["message"].(string), "安全备份") {
		t.Fatalf("recover 结果: %v", rd)
	}
	if d := sameState(preDB, dbState(t, s.be.adm, s.be.dbName)); d != "" {
		t.Errorf("recover --rollback 后数据库应回到恢复前:\n%s", d)
	}
	if d := sameFiles(preFiles, s.filesState()); d != "" {
		t.Errorf("recover --rollback 后文件应回到恢复前:\n%s", d)
	}
	if n := s.unfinishedJournals(); n != 0 {
		t.Errorf("recover 后不应有未完成事务: %d", n)
	}
	if !s.w.FakeHas("active-lingedge-server.service") {
		t.Error("服务应恢复运行")
	}
	s.assertNoCredentialResidue()
	s.leakScan(so, se)
}

// uninstall：真实 server 安装之上——默认保留；--purge 的确认规则；之后可以重新安装。
func TestUninstallRealServer(t *testing.T) {
	s := startStack(t)
	// 默认卸载：保留配置/数据/备份；主密钥与 install.lock 仍在。
	code, so, se := s.runB(0, runOpt{}, append([]string{"backup", "create", "--component", "server", "--json", "--non-interactive"}, s.dbFlags()...)...)
	okData(t, code, so, se, 0)
	nb := s.backupFiles()
	code, so, se = s.runB(0, runOpt{}, "uninstall", "server", "--yes", "--json", "--non-interactive")
	ud := dataOf(okData(t, code, so, se, 0))
	if ud["outcome"] != "uninstalled" || ud["purge"] != false {
		t.Fatalf("uninstall: %v", ud)
	}
	for _, rel := range []string{"etc/lingedge/server/master.key", "etc/lingedge/server/server.yaml", "var/lib/lingedge/server/install.lock", "var/lib/lingedge/server/secrets/runtime-credentials.json"} {
		if !s.w.Has(rel) {
			t.Errorf("默认卸载必须保留 %s", rel)
		}
	}
	if s.w.Has("opt/lingedge/server") || s.w.Has("etc/systemd/system/lingedge-server.service") {
		t.Error("运行时应已摘除")
	}
	if fmt.Sprint(nb) != fmt.Sprint(s.backupFiles()) {
		t.Error("备份应保留")
	}
	// 数据库不被删除。
	var n int
	_ = s.be.adm.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = ?", s.be.dbName).Scan(&n)
	if n < 20 {
		t.Errorf("Tools 从不删除数据库内容: %d 张表", n)
	}
}
