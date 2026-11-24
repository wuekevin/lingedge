package doctor

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/service"
)

// 证书检查（F-2、L24、ADR-003 §3–§5）：
//   - cert.node_mtls：节点叶证书（默认 90 天；Agent 在剩余 1/3 时自动续期）。**剩余 < 1/6（约 15 天）才告警**——续期窗口内不应告警，
//     只有续期持续失败才会触发（避免与续期阈值同为 1/3 而常态告警）；已过期 = fail（须 `node enroll --force-reenroll`）。
//   - cert.server_leaf：Server gRPC 叶证书（90 天，2/3 周期自动轮换）。同样 < 1/6 告警，过期 fail。
//   - cert.private_ca：私有 CA（默认 10 年）。剩余 < 1 年告警，过期 fail。
const (
	renewWarnDivisor = 6
	caWarnRemaining  = 365 * 24 * time.Hour
	maxCertFiles     = 20
	maxCertFileBytes = 256 << 10
)

func runCerts(ctx context.Context, w *World, f Filter) []Finding {
	var out []Finding
	var cas []caInfo
	if _, ok := w.component("node"); ok && (f.Component == "" || f.Component == "node") {
		leaf, nodeCAs, skip := w.nodeIdentity()
		out = append(out, nodeMTLS(w, leaf, skip))
		cas = append(cas, nodeCAs...)
	}
	if _, ok := w.component("server"); ok && (f.Component == "" || f.Component == "server") {
		x, chainCAs := w.serverLeaf(ctx)
		out = append(out, x)
		cas = append(cas, chainCAs...)
	}
	if len(out) > 0 {
		out = append(out, caFinding(w, cas))
	}
	return out
}

type caInfo struct {
	Source string
	Cert   *x509.Certificate
}

func parseCerts(b []byte) []*x509.Certificate {
	var out []*x509.Certificate
	for len(b) > 0 {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue // 私钥块一律忽略：doctor 从不解析私钥
		}
		if c, err := x509.ParseCertificate(blk.Bytes); err == nil {
			out = append(out, c)
		}
	}
	return out
}

// nodeIdentity 读取节点身份目录里的证书（只解析 CERTIFICATE 块，私钥块被忽略）。skip 非空 = 无法判定的原因。
func (w *World) nodeIdentity() (leaf *x509.Certificate, cas []caInfo, skip string) {
	dir := w.path(service.NodeDataDir + "/identity")
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, "节点尚未注册（没有 identity 目录）"
		}
		return nil, nil, "无法读取节点身份目录（需要 root）：" + errText(err)
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	n := 0
	for _, e := range ents {
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if !e.Type().IsRegular() || (ext != ".pem" && ext != ".crt") {
			continue
		}
		if n++; n > maxCertFiles {
			break
		}
		fi, err := e.Info()
		if err != nil || fi.Size() > maxCertFileBytes {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		for _, c := range parseCerts(b) {
			switch {
			case c.IsCA:
				cas = append(cas, caInfo{Source: "节点身份目录 " + name, Cert: c})
			case hasEKU(c, x509.ExtKeyUsageClientAuth):
				if leaf == nil || c.NotAfter.After(leaf.NotAfter) {
					leaf = c
				}
			}
		}
	}
	if leaf == nil {
		return nil, cas, "节点身份目录里没有客户端（clientAuth）证书：节点尚未注册，或证书文件命名不是 .pem/.crt"
	}
	return leaf, cas, ""
}

func hasEKU(c *x509.Certificate, eku x509.ExtKeyUsage) bool {
	for _, e := range c.ExtKeyUsage {
		if e == eku {
			return true
		}
	}
	return false
}

// evalLeaf 判定叶证书的剩余有效期：已过期 fail；剩余 < 总期限的 1/6 warn；否则 pass。
func evalLeaf(id, what string, c *x509.Certificate, now time.Time, expiredHint, warnHint string) Finding {
	life := c.NotAfter.Sub(c.NotBefore)
	rem := c.NotAfter.Sub(now)
	x := Finding{ID: id, Details: map[string]any{"not_after": c.NotAfter.UTC().Format(time.RFC3339), "remaining_hours": int64(rem.Hours()), "lifetime_hours": int64(life.Hours())}}
	switch {
	case rem <= 0:
		x.Level = LevelFail
		x.Message = fmt.Sprintf("%s已于 %s 过期", what, c.NotAfter.UTC().Format("2006-01-02 15:04 UTC"))
		x.Hint = expiredHint
	case life > 0 && rem < life/renewWarnDivisor:
		x.Level = LevelWarn
		x.Message = fmt.Sprintf("%s剩余 %s，不足总有效期（%s）的 1/6：自动续期很可能持续失败", what, fmtDays(rem), fmtDays(life))
		x.Hint = warnHint
	default:
		x.Level = LevelPass
		x.Message = fmt.Sprintf("%s剩余 %s（总有效期 %s；仅在剩余 < 1/6 时告警，续期窗口内不告警）", what, fmtDays(rem), fmtDays(life))
	}
	return x
}

func fmtDays(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%d 天", int(d.Hours()/24))
	}
	return fmt.Sprintf("%.0f 小时", d.Hours())
}

func nodeMTLS(w *World, leaf *x509.Certificate, skip string) Finding {
	const id = "cert.node_mtls"
	var x Finding
	if leaf == nil {
		x = fnd(id, LevelSkip, skip, "")
	} else {
		x = evalLeaf(id, "节点 mTLS 证书", leaf, w.now(),
			"节点证书过期后数据面继续服务但无法重连主控：在 Admin 新建注册令牌后运行 lingedge node enroll --force-reenroll（F-2）",
			"检查 Agent 与 Server 的连通与时钟（lingedge logs node）；Agent 在剩余 1/3 时就会自动续期，持续失败才会走到这里")
	}
	x.Component = "node"
	return x
}

func (w *World) serverLeaf(ctx context.Context) (Finding, []caInfo) {
	const id = "cert.server_leaf"
	mk := func(l Level, m, h string) Finding {
		x := fnd(id, l, m, h)
		x.Component = "server"
		return x
	}
	if w.PeerCerts == nil || w.ServerListens == nil {
		return mk(LevelSkip, "无法探测 Server gRPC 证书", ""), nil
	}
	_, grpc := w.ServerListens()
	if h := w.serverHealth(ctx); h.initialized == initNo {
		return mk(LevelSkip, "Server 仍处于安装向导阶段（setup mode），gRPC 与证书尚未启用", ""), nil
	}
	addr := loopbackize(grpc)
	if h, _, err := net.SplitHostPort(grpc); err != nil || h == "" {
		return mk(LevelSkip, "server.yaml 的 grpc.listen 无法解析", ""), nil
	}
	certs, err := w.PeerCerts(ctx, addr)
	if err != nil || len(certs) == 0 {
		return mk(LevelSkip, "Server gRPC（"+grpc+"）没有在监听或握手失败，无法读取叶证书："+errText(err),
			"gRPC 角色随 Epic 3 交付；Server 运行且启用 grpc 角色后此项自动生效"), nil
	}
	x := evalLeaf(id, "Server gRPC 叶证书", certs[0], w.now(),
		"Server 叶证书由 Scheduler 任务 server.cert_rotate 自动轮换；已过期说明轮换持续失败：检查 Scheduler 日志（lingedge logs server），必要时轮换",
		"Server 叶证书有效期 90 天，在 2/3 周期自动轮换；剩余 < 1/6 说明轮换持续失败：lingedge logs server")
	x.Component = "server"
	var cas []caInfo
	for _, c := range certs[1:] {
		if c.IsCA {
			cas = append(cas, caInfo{Source: "Server gRPC 握手链", Cert: c})
		}
	}
	return x, cas
}

func caFinding(w *World, cas []caInfo) Finding {
	const id = "cert.private_ca"
	if len(cas) == 0 {
		return fnd(id, LevelSkip, "没有读到私有 CA 证书（节点身份目录与 Server 握手链里都没有）：CA 有效期无法在不访问数据库的前提下检查", "CA 默认有效期 10 年；Server 侧的到期提示见 Admin 系统页")
	}
	now := w.now()
	sort.Slice(cas, func(i, j int) bool { return cas[i].Cert.NotAfter.Before(cas[j].Cert.NotAfter) })
	worst := cas[0]
	rem := worst.Cert.NotAfter.Sub(now)
	x := Finding{ID: id, Details: map[string]any{"not_after": worst.Cert.NotAfter.UTC().Format(time.RFC3339), "remaining_days": int(rem.Hours() / 24), "source": worst.Source}}
	switch {
	case rem <= 0:
		x.Level, x.Message = LevelFail, fmt.Sprintf("私有 CA（%s）已过期", worst.Source)
		x.Hint = "CA 过期后所有节点证书失效：按 ADR-003 §12 的分阶段 CA 轮换流程处理"
	case rem < caWarnRemaining:
		x.Level, x.Message = LevelWarn, fmt.Sprintf("私有 CA（%s）剩余 %s，不足 1 年", worst.Source, fmtDays(rem))
		x.Hint = "尽快按 ADR-003 §12 分阶段轮换 CA（S1–S5），不要等到过期"
	default:
		x.Level, x.Message = LevelPass, fmt.Sprintf("私有 CA 剩余 %s", fmtDays(rem))
	}
	return x
}
