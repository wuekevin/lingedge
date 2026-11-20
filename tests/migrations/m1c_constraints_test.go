package migrationstest

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// Story 1.9：000012–000013 的约束、触发器、循环外键与 CC-1 跨租户外键的真实 MySQL 负向/正向用例（事务内回滚隔离）。

func TestM1CConstraintsOnRealDatabase(t *testing.T) {
	e, _ := m1cUp(t)
	c := e.conn(t)
	run := func(name string, fn func(c *cx)) {
		t.Run(name, func(t *testing.T) {
			inTx(t, e, c, func(cc *cx) {
				cc.ok("SET @lingedge_purge = 0") // 会话变量跨子用例保留：每个用例从未 purge 起步
				fn(cc)
			})
		})
	}
	hash := func() string { return fmt.Sprintf("%064d", next()) }
	node := func(c *cx) int64 {
		return c.ok("INSERT INTO nodes (uid, name) VALUES (?, ?)", uid26(), fmt.Sprintf("n%d", next()))
	}
	cluster := func(c *cx) int64 {
		return c.ok("INSERT INTO clusters (uid, name, settings_json) VALUES (?, ?, JSON_OBJECT())", uid26(), fmt.Sprintf("c%d", next()))
	}
	site := func(c *cx, tenantID int64) int64 {
		return c.ok("INSERT INTO sites (uid, tenant_id, name) VALUES (?, ?, ?)", uid26(), tenantID, fmt.Sprintf("s%d", next()))
	}
	block := func(c *cx) int64 {
		return c.ok("INSERT INTO config_blocks (sha256, kind, schema_version, size_raw, size_stored, content) VALUES (?, 'global', 1, 1, 1, 'x')", hash())
	}
	const insRev = `INSERT INTO config_revisions (uid, cluster_id, lineage, seq, parent_seq, kind, rollback_of_revision_id, schema_version, manifest_canonical, manifest_sha256, signature, key_id, block_count, total_size, request_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1, 'm', ?, 'sig', 'k1', 0, 0, ?)`
	rev := func(c *cx, clusterID, lineage, seq int64) int64 {
		return c.ok(insRev, uid26(), clusterID, lineage, seq, nil, "publish", nil, hash(), uid26())
	}
	purge := func(c *cx) { c.ok("SET @lingedge_purge = 1") }
	unpurge := func(c *cx) { c.ok("SET @lingedge_purge = 0") }
	deployment := func(c *cx, clusterID, revID int64) int64 {
		return c.ok("INSERT INTO deployments (uid, cluster_id, revision_id, request_id) VALUES (?, ?, ?, ?)", uid26(), clusterID, revID, uid26())
	}
	cert := func(c *cx, tenantID int64) int64 {
		return c.ok(`INSERT INTO certificates (uid, tenant_id, name, source, primary_domain, domains_json, serial, issuer, not_before, not_after,
			fingerprint_sha256, key_algorithm, chain_pem, private_key_enc, enc_key_id) VALUES (?, ?, 'c', 'upload', 'a.example.test', JSON_ARRAY('a.example.test'), ?, 'iss',
			'2026-01-01', '2027-01-01', ?, 'ecdsa_p256', 'PEM', 'v1:k:w:n:c', 'k1')`, uid26(), tenantID, fmt.Sprintf("s%d", next()), hash())
	}
	target := func(c *cx, depID, nodeID int64) int64 {
		return c.ok("INSERT INTO deployment_targets (uid, deployment_id, node_id) VALUES (?, ?, ?)", uid26(), depID, nodeID)
	}

	// ======== 000012 ========
	run("config_blocks：值域/site_uid 规则/哈希唯一/无默认；UPDATE 与未 purge 的 DELETE 被触发器拒绝", func(c *cx) {
		ins := "INSERT INTO config_blocks (sha256, kind, site_uid, tenant_id, schema_version, size_raw, size_stored, content) VALUES (?, ?, ?, ?, 1, 1, 1, 'x')"
		c.bad(errCheckViolated, ins, hash(), "bogus", nil, nil)
		c.bad(errCheckViolated, ins, hash(), "site", nil, nil)       // site 块必须带 site_uid
		c.bad(errCheckViolated, ins, hash(), "global", uid26(), nil) // 非 site 块不得带 site_uid
		c.bad(errCheckViolated, ins, hash(), "site", "SHORT", nil)   // site_uid 长度 26
		c.bad(errCheckViolated, "INSERT INTO config_blocks (sha256, kind, encoding, schema_version, size_raw, size_stored, content) VALUES (?, 'global', 'gzip', 1, 1, 1, 'x')", hash())
		c.bad(errNoRefRow, ins, hash(), "site", uid26(), 999999999)
		c.bad(errNoDefault, "INSERT INTO config_blocks (sha256, kind, schema_version, size_raw, size_stored) VALUES (?, 'global', 1, 1, 1)", hash()) // content 无默认
		h := hash()
		c.ok("INSERT INTO config_blocks (sha256, kind, schema_version, size_raw, size_stored, content) VALUES (?, 'global', 1, 1, 1, 'x')", h)
		c.bad(errDup, "INSERT INTO config_blocks (sha256, kind, schema_version, size_raw, size_stored, content) VALUES (?, 'l4', 1, 1, 1, 'x')", h) // 内容寻址：同哈希只存一份
		tn := c.tenant()
		id := c.ok(ins, hash(), "site", uid26(), tn)
		if c.count("SELECT COUNT(*) FROM config_blocks WHERE id = ? AND encoding = 'zstd'", id) != 1 {
			t.Error("encoding 默认应为 zstd")
		}
		c.bad(errSignal, "UPDATE config_blocks SET size_raw = 2 WHERE id = ?", id)
		c.bad(errSignal, "UPDATE config_blocks SET content = 'y' WHERE id = ?", id)
		c.bad(errSignal, "DELETE FROM config_blocks WHERE id = ?", id)
		purge(c)
		c.ok("DELETE FROM config_blocks WHERE id = ?", id) // 仅清理任务（@lingedge_purge=1）
	})

	run("config_revisions：唯一键/值域/回滚与 parent 约束；UPDATE 拒绝；DELETE 仅 purge 且级联清单", func(c *cx) {
		cl := cluster(c)
		r1 := rev(c, cl, 1, 1)
		c.bad(errDup, insRev, uid26(), cl, 1, 1, nil, "publish", nil, hash(), uid26()) // (cluster, lineage, seq) 唯一
		rev(c, cl, 2, 1)                                                               // 同 seq 不同 lineage 合法（备份恢复后 lineage 提升，ADR-002 §12）
		same := hash()
		c.ok(insRev, uid26(), cl, 1, 2, 1, "publish", nil, same, uid26())
		c.bad(errDup, insRev, uid26(), cl, 1, 3, 2, "publish", nil, same, uid26()) // (cluster, manifest_sha256) 唯一
		c.bad(errCheckViolated, insRev, uid26(), cl, 1, 4, nil, "bogus", nil, hash(), uid26())
		c.bad(errCheckViolated, insRev, uid26(), cl, 1, 0, nil, "publish", nil, hash(), uid26())  // seq >= 1
		c.bad(errCheckViolated, insRev, uid26(), cl, 1, 5, 5, "publish", nil, hash(), uid26())    // parent_seq < seq
		c.bad(errCheckViolated, insRev, uid26(), cl, 1, 6, nil, "rollback", nil, hash(), uid26()) // rollback 必须有 rollback_of
		c.bad(errCheckViolated, insRev, uid26(), cl, 1, 7, nil, "publish", r1, hash(), uid26())   // 非 rollback 不得有 rollback_of
		c.bad(errNoRefRow, insRev, uid26(), cl, 1, 8, nil, "rollback", 999999999, hash(), uid26())
		rb := c.ok(insRev, uid26(), cl, 1, 9, nil, "rollback", r1, hash(), uid26())
		c.bad(errNoRefRow, insRev, uid26(), 999999999, 1, 10, nil, "publish", nil, hash(), uid26())
		c.bad(errNoDefault, "INSERT INTO config_revisions (uid, cluster_id, lineage, seq, kind) VALUES (?, ?, 1, 20, 'publish')", uid26(), cl)
		// 不可变：任何列 UPDATE 拒绝；DELETE 未 purge 拒绝。
		c.bad(errSignal, "UPDATE config_revisions SET change_note = 'x' WHERE id = ?", r1)
		c.bad(errSignal, "UPDATE config_revisions SET seq = 99 WHERE id = ?", r1)
		c.bad(errSignal, "DELETE FROM config_revisions WHERE id = ?", rb)
		// 被引用（rollback_of RESTRICT）：即使 purge 也不能先删 r1。
		purge(c)
		c.bad(errRowIsRef, "DELETE FROM config_revisions WHERE id = ?", r1)
		c.ok("DELETE FROM config_revisions WHERE id = ?", rb)
		// 集群 RESTRICT；清单随修订级联清理（不触发行级 DELETE 触发器的 CASCADE）。
		b := block(c)
		c.ok("INSERT INTO config_revision_blocks (revision_id, block_id, block_key) VALUES (?, ?, 'global')", r1, b)
		c.bad(errRowIsRef, "DELETE FROM clusters WHERE id = ?", cl)
		c.ok("DELETE FROM config_revisions WHERE id = ?", r1)
		if c.count("SELECT COUNT(*) FROM config_revision_blocks WHERE revision_id = ?", r1) != 0 {
			t.Error("修订清理应级联清理其块清单")
		}
		unpurge(c)
	})

	run("config_revision_blocks：block_key 值域与唯一；UPDATE 拒绝；块 RESTRICT", func(c *cx) {
		cl := cluster(c)
		r1 := rev(c, cl, 1, 1)
		b1, b2 := block(c), block(c)
		ins := "INSERT INTO config_revision_blocks (revision_id, block_id, block_key) VALUES (?, ?, ?)"
		id := c.ok(ins, r1, b1, "global")
		c.bad(errDup, ins, r1, b2, "global") // (revision, block_key) 唯一
		c.ok(ins, r1, b1, "site:"+uid26())   // site:<26 位 uid>
		c.ok(ins, r1, b2, "l4")
		c.bad(errCheckViolated, ins, r1, b2, "site:short")
		c.bad(errCheckViolated, ins, r1, b2, "evil")
		c.bad(errNoRefRow, ins, r1, 999999999, "site:"+uid26())
		c.bad(errNoRefRow, ins, 999999999, b1, "l4")
		c.bad(errSignal, "UPDATE config_revision_blocks SET block_id = ? WHERE id = ?", b2, id)
		purge(c)
		c.bad(errRowIsRef, "DELETE FROM config_blocks WHERE id = ?", b1) // 被修订引用的块不可删
		unpurge(c)
	})

	run("site_versions：站点内版本唯一/值域/回滚约束；跨租户站点被复合外键拒绝；状态可前移", func(c *cx) {
		t1, t2 := c.tenant(), c.tenant()
		s1, s2 := site(c, t1), site(c, t2)
		b := block(c)
		ins := "INSERT INTO site_versions (uid, tenant_id, site_id, version_no, kind, rollback_of_version_no, block_id) VALUES (?, ?, ?, ?, ?, ?, ?)"
		v1 := c.ok(ins, uid26(), t1, s1, 1, "publish", nil, b)
		if c.count("SELECT COUNT(*) FROM site_versions WHERE id = ? AND status = 'pending'", v1) != 1 {
			t.Error("site_versions.status 默认应为 pending")
		}
		c.bad(errDup, ins, uid26(), t1, s1, 1, "publish", nil, b) // (site, version_no) 唯一
		c.ok(ins, uid26(), t2, s2, 1, "publish", nil, b)          // 另一站点版本号独立（站点内单调，MIN-14）
		c.bad(errCheckViolated, ins, uid26(), t1, s1, 0, "publish", nil, b)
		c.bad(errCheckViolated, ins, uid26(), t1, s1, 2, "bogus", nil, b)
		c.bad(errCheckViolated, ins, uid26(), t1, s1, 3, "rollback", nil, b) // rollback 必须有 rollback_of_version_no
		c.bad(errCheckViolated, ins, uid26(), t1, s1, 4, "publish", 1, b)    // publish 不得有
		c.ok(ins, uid26(), t1, s1, 5, "rollback", 1, b)
		c.bad(errNoRefRow, ins, uid26(), t1, s2, 6, "publish", nil, b) // 租户 t1 + 站点 s2（属 t2）：复合外键拒绝
		c.bad(errNoRefRow, ins, uid26(), t2, s1, 6, "publish", nil, b)
		c.bad(errNoRefRow, ins, uid26(), t1, s1, 7, "publish", nil, 999999999)
		c.ok("UPDATE site_versions SET status = 'live' WHERE id = ?", v1)
		c.bad(errCheckViolated, "UPDATE site_versions SET status = 'done' WHERE id = ?", v1)
		purge(c)
		c.bad(errRowIsRef, "DELETE FROM config_blocks WHERE id = ?", b) // 被站点版本引用的块 RESTRICT
		unpurge(c)
	})

	run("publish_queue：每集群每站点至多一个排队项（queued_key）、cluster_rollback 约束、复合外键、SET NULL", func(c *cx) {
		t1, t2 := c.tenant(), c.tenant()
		s1, s2 := site(c, t1), site(c, t2)
		cl1, cl2 := cluster(c), cluster(c)
		ins := "INSERT INTO publish_queue (uid, tenant_id, cluster_id, site_id, kind, status) VALUES (?, ?, ?, ?, ?, ?)"
		q1 := c.ok(ins, uid26(), t1, cl1, s1, "site_publish", "queued")
		if got := c.count("SELECT queued_key FROM publish_queue WHERE id = ?", q1); got != s1 {
			t.Errorf("queued 状态 queued_key 应等于 site_id: %d", got)
		}
		c.bad(errDup, ins, uid26(), t1, cl1, s1, "site_publish", "queued") // 同站点同集群排队项唯一
		c.ok(ins, uid26(), t1, cl2, s1, "site_publish", "queued")          // 不同集群可各排一个
		c.ok(ins, uid26(), t2, cl1, s2, "site_publish", "queued")          // 不同站点可并存
		c.ok(ins, uid26(), t1, cl1, s1, "site_rollback", "building")       // 非 queued 状态不占位，可多个
		c.ok(ins, uid26(), t1, cl1, s1, "site_rollback", "building")
		c.ok("UPDATE publish_queue SET status = 'superseded' WHERE id = ?", q1) // 合并覆盖旧项后释放排队位
		if !c.isNull("SELECT queued_key FROM publish_queue WHERE id = ?", q1) {
			t.Error("非 queued 状态 queued_key 应为 NULL")
		}
		c.ok(ins, uid26(), t1, cl1, s1, "site_publish", "queued")
		c.bad(errCheckViolated, ins, uid26(), t1, cl1, s1, "bogus", "queued")
		c.bad(errCheckViolated, ins, uid26(), t1, cl1, s1, "site_publish", "bogus")
		c.bad(errCheckViolated, ins, uid26(), nil, cl1, nil, "site_publish", "queued") // 非 cluster_rollback 必须有站点
		c.bad(errCheckViolated, ins, uid26(), t1, cl1, s1, "cluster_rollback", "queued")
		c.ok(ins, uid26(), nil, cl1, nil, "cluster_rollback", "queued")                 // 集群回滚：无站点、租户可空
		c.bad(errDup, ins, uid26(), nil, cl1, nil, "cluster_rollback", "queued")        // 每集群至多一个排队中的集群回滚（COALESCE(site_id, 0)）
		c.ok(ins, uid26(), nil, cl2, nil, "cluster_rollback", "queued")                 // 另一集群不受影响
		c.bad(errCheckViolated, ins, uid26(), nil, cl1, s1, "site_publish", "queued")   // 有站点必有租户（MATCH SIMPLE 的 NULL 绕过由 CHECK 堵住）
		c.bad(errNoRefRow, ins, uid26(), t1, cl1, s2, "site_publish", "building")       // 跨租户站点
		c.bad(errNoRefRow, ins, uid26(), t1, 999999999, s1, "site_publish", "building") // 集群不存在
		// site_version_id SET NULL；site 软/硬删除 RESTRICT。
		b := block(c)
		sv := c.ok("INSERT INTO site_versions (uid, tenant_id, site_id, version_no, block_id) VALUES (?, ?, ?, 1, ?)", uid26(), t1, s1, b)
		q2 := c.ok(ins, uid26(), t1, cl1, s1, "site_publish", "building")
		c.ok("UPDATE publish_queue SET site_version_id = ? WHERE id = ?", sv, q2)
		c.bad(errNoRefRow, "UPDATE publish_queue SET site_version_id = 999999999 WHERE id = ?", q2)
		c.ok("DELETE FROM site_versions WHERE id = ?", sv)
		if !c.isNull("SELECT site_version_id FROM publish_queue WHERE id = ?", q2) {
			t.Error("站点版本删除后 publish_queue.site_version_id 应置 NULL")
		}
		c.bad(errRowIsRef, "DELETE FROM sites WHERE id = ?", s1)
	})

	run("deployments / deployment_targets / deployment_target_steps：值域、唯一、级联与幂等键", func(c *cx) {
		cl := cluster(c)
		r1 := rev(c, cl, 1, 1)
		insD := "INSERT INTO deployments (uid, cluster_id, revision_id, kind, strategy, status, request_id) VALUES (?, ?, ?, ?, ?, ?, ?)"
		d := c.ok(insD, uid26(), cl, r1, "publish", "all", "pending", uid26())
		for _, bad := range [][3]string{{"bogus", "all", "pending"}, {"publish", "bogus", "pending"}, {"publish", "all", "bogus"}} {
			c.bad(errCheckViolated, insD, uid26(), cl, r1, bad[0], bad[1], bad[2], uid26())
		}
		for _, st := range []string{"pending", "running", "paused", "success", "partial_failed", "failed", "canceled", "rolled_back"} {
			c.ok("UPDATE deployments SET status = ? WHERE id = ?", st, d)
		}
		c.bad(errNoRefRow, insD, uid26(), cl, 999999999, "publish", "all", "pending", uid26())
		c.bad(errNoDefault, "INSERT INTO deployments (uid, cluster_id, revision_id) VALUES (?, ?, ?)", uid26(), cl, r1) // request_id 无默认
		// 修订被部署引用：即使 purge 也 RESTRICT。
		purge(c)
		c.bad(errRowIsRef, "DELETE FROM config_revisions WHERE id = ?", r1)
		unpurge(c)

		n1, n2 := node(c), node(c)
		dt := target(c, d, n1)
		if c.count("SELECT COUNT(*) FROM deployment_targets WHERE id = ? AND state = 'pending' AND attempt = 1 AND batch_no = 0", dt) != 1 {
			t.Error("deployment_targets 默认值异常")
		}
		c.bad(errDup, "INSERT INTO deployment_targets (uid, deployment_id, node_id) VALUES (?, ?, ?)", uid26(), d, n1) // (deployment, node) 唯一
		c.ok("INSERT INTO deployment_targets (uid, deployment_id, node_id) VALUES (?, ?, ?)", uid26(), d, n2)
		for _, st := range []string{"pending", "notified", "downloaded", "verified", "tested", "applied", "healthy", "failed", "rolled_back", "skipped", "waiting_offline", "expired"} {
			c.ok("UPDATE deployment_targets SET state = ? WHERE id = ?", st, dt)
		}
		c.bad(errCheckViolated, "UPDATE deployment_targets SET state = 'unknown' WHERE id = ?", dt)
		c.bad(errCheckViolated, "UPDATE deployment_targets SET failure_stage = 'boom' WHERE id = ?", dt)
		c.ok("UPDATE deployment_targets SET failure_stage = 'health' WHERE id = ?", dt)
		c.bad(errCheckViolated, "UPDATE deployment_targets SET attempt = 0 WHERE id = ?", dt)
		c.bad(errNoRefRow, "INSERT INTO deployment_targets (uid, deployment_id, node_id) VALUES (?, ?, 999999999)", uid26(), d)
		c.bad(errRowIsRef, "DELETE FROM nodes WHERE id = ?", n1) // 节点 RESTRICT

		// ReportApply 幂等键 (target, attempt, state)：重复上报 INSERT IGNORE 视为同一事件。
		step := "INSERT IGNORE INTO deployment_target_steps (deployment_target_id, attempt, state) VALUES (?, ?, ?)"
		if res, err := c.exec(step, dt, 1, "downloaded"); err != nil {
			t.Fatal(err)
		} else if n, _ := res.RowsAffected(); n != 1 {
			t.Errorf("首次上报应插入 1 行: %d", n)
		}
		if res, err := c.exec(step, dt, 1, "downloaded"); err != nil {
			t.Fatal(err)
		} else if n, _ := res.RowsAffected(); n != 0 {
			t.Errorf("重复上报应被幂等键忽略: %d", n)
		}
		c.bad(errDup, "INSERT INTO deployment_target_steps (deployment_target_id, attempt, state) VALUES (?, 1, 'downloaded')", dt)
		c.ok("INSERT INTO deployment_target_steps (deployment_target_id, attempt, state) VALUES (?, 2, 'downloaded')", dt) // 重试 attempt 递增
		c.ok("INSERT INTO deployment_target_steps (deployment_target_id, attempt, state) VALUES (?, 1, 'verified')", dt)
		c.bad(errCheckViolated, "INSERT INTO deployment_target_steps (deployment_target_id, attempt, state) VALUES (?, 1, 'pending')", dt) // 节点侧状态子集
		c.bad(errNoRefRow, "INSERT INTO deployment_target_steps (deployment_target_id, attempt, state) VALUES (999999999, 1, 'applied')")
		c.ok("DELETE FROM deployments WHERE id = ?", d) // 级联 targets → steps
		if c.count("SELECT COUNT(*) FROM deployment_targets WHERE deployment_id = ?", d) != 0 || c.count("SELECT COUNT(*) FROM deployment_target_steps WHERE deployment_target_id = ?", dt) != 0 {
			t.Error("部署删除应级联 targets 与 steps")
		}
	})

	run("node_cluster_state：复合主键、held_seq、值域、级联与修订 RESTRICT", func(c *cx) {
		cl := cluster(c)
		n := node(c)
		r1 := rev(c, cl, 1, 1)
		ins := "INSERT INTO node_cluster_state (node_id, cluster_id, desired_revision_id, applied_revision_id, applied_lineage, applied_seq, held_seq) VALUES (?, ?, ?, ?, 1, 1, ?)"
		c.ok(ins, n, cl, r1, r1, 5)
		if c.count("SELECT COUNT(*) FROM node_cluster_state WHERE node_id = ? AND state = 'idle' AND held_seq = 5", n) != 1 {
			t.Error("node_cluster_state 默认 state 应为 idle 且 held_seq 可写")
		}
		c.bad(errDup, ins, n, cl, r1, r1, nil) // (node, cluster) 主键
		for _, st := range []string{"idle", "applying", "failed", "rolled_back"} {
			c.ok("UPDATE node_cluster_state SET state = ? WHERE node_id = ? AND cluster_id = ?", st, n, cl)
		}
		c.bad(errCheckViolated, "UPDATE node_cluster_state SET state = 'weird' WHERE node_id = ?", n)
		c.bad(errNoRefRow, ins, node(c), cl, 999999999, nil, nil)
		purge(c)
		c.bad(errRowIsRef, "DELETE FROM config_revisions WHERE id = ?", r1) // 被节点状态引用
		unpurge(c)
		c.ok("DELETE FROM nodes WHERE id = ?", n) // 节点删除级联其状态
		if c.count("SELECT COUNT(*) FROM node_cluster_state WHERE cluster_id = ?", cl) != 0 {
			t.Error("节点删除应级联 node_cluster_state")
		}
	})

	run("修订与集群一致性（评审加固）：跨集群引用修订被复合外键拒绝", func(c *cx) {
		cl1, cl2 := cluster(c), cluster(c)
		r1, r2 := rev(c, cl1, 1, 1), rev(c, cl2, 1, 1)
		c.bad(errNoRefRow, insRev, uid26(), cl1, 1, 5, nil, "rollback", r2, hash(), uid26()) // 回滚目标属另一集群
		c.ok(insRev, uid26(), cl1, 1, 5, nil, "rollback", r1, hash(), uid26())
		c.bad(errNoRefRow, "INSERT INTO deployments (uid, cluster_id, revision_id, request_id) VALUES (?, ?, ?, ?)", uid26(), cl1, r2, uid26())
		c.bad(errNoRefRow, "UPDATE clusters SET confirmed_revision_id = ? WHERE id = ?", r2, cl1)
		n := node(c)
		c.bad(errNoRefRow, "INSERT INTO node_cluster_state (node_id, cluster_id, desired_revision_id) VALUES (?, ?, ?)", n, cl1, r2)
		c.bad(errNoRefRow, "INSERT INTO node_cluster_state (node_id, cluster_id, applied_revision_id) VALUES (?, ?, ?)", n, cl1, r2)
		c.ok("INSERT INTO node_cluster_state (node_id, cluster_id, desired_revision_id, applied_revision_id) VALUES (?, ?, ?, ?)", n, cl1, r1, r1)
	})

	run("循环外键：clusters.confirmed_revision_id / sites.last_deployment_id / publish_queue.deployment_id", func(c *cx) {
		tn := c.tenant()
		s := site(c, tn)
		cl := cluster(c)
		r1 := rev(c, cl, 1, 1)
		d := deployment(c, cl, r1)
		q := c.ok("INSERT INTO publish_queue (uid, tenant_id, cluster_id, site_id, kind) VALUES (?, ?, ?, ?, 'site_publish')", uid26(), tn, cl, s)
		c.bad(errNoRefRow, "UPDATE clusters SET confirmed_revision_id = 999999999 WHERE id = ?", cl)
		c.bad(errNoRefRow, "UPDATE sites SET last_deployment_id = 999999999 WHERE id = ?", s)
		c.bad(errNoRefRow, "UPDATE publish_queue SET deployment_id = 999999999 WHERE id = ?", q)
		c.ok("UPDATE clusters SET confirmed_revision_id = ? WHERE id = ?", r1, cl)
		c.ok("UPDATE sites SET last_deployment_id = ? WHERE id = ?", d, s)
		c.ok("UPDATE publish_queue SET deployment_id = ? WHERE id = ?", d, q)
		c.ok("UPDATE deployments SET queue_id = ? WHERE id = ?", q, d)
		purge(c)
		c.bad(errRowIsRef, "DELETE FROM config_revisions WHERE id = ?", r1) // confirmed 基线 RESTRICT
		unpurge(c)
		// deployments 删除 → SET NULL 两处（先解除对修订的引用：这里只删部署本身）。
		c.ok("DELETE FROM deployments WHERE id = ?", d)
		if !c.isNull("SELECT last_deployment_id FROM sites WHERE id = ?", s) || !c.isNull("SELECT deployment_id FROM publish_queue WHERE id = ?", q) {
			t.Error("部署删除后 sites.last_deployment_id 与 publish_queue.deployment_id 应置 NULL")
		}
		c.ok("DELETE FROM publish_queue WHERE id = ?", q)
	})

	// ======== 000013 ========
	run("metric_rollups：唯一键/默认值/值域；generation 覆盖规则（同键更大代覆盖、同代保留首次）", func(c *cx) {
		ins := `INSERT INTO metric_rollups (granularity, window_start, dim_type, metric, value, generation) VALUES ('1m', '2026-11-20 10:00:00.000', 'site_node', 'requests', ?, ?)
			ON DUPLICATE KEY UPDATE value = IF(VALUES(generation) > generation, VALUES(value), value), generation = GREATEST(generation, VALUES(generation))`
		c.ok(ins, 5, 1)
		c.ok(ins, 9, 0) // 更小代：不覆盖
		c.ok(ins, 7, 1) // 同代：保留首次（异值冲突由 rollup_windows 判定，不静默覆盖）
		if c.count("SELECT value FROM metric_rollups WHERE metric = 'requests'") != 5 {
			t.Error("同代/更小代不得覆盖已有值")
		}
		c.ok(ins, 8, 2) // 更大代：覆盖
		if c.count("SELECT value FROM metric_rollups WHERE metric = 'requests'") != 8 || c.count("SELECT generation FROM metric_rollups WHERE metric = 'requests'") != 2 {
			t.Error("更大代应覆盖值并前移 generation")
		}
		c.bad(errDup, "INSERT INTO metric_rollups (granularity, window_start, dim_type, metric, value) VALUES ('1m', '2026-11-20 10:00:00.000', 'site_node', 'requests', 1)")
		if c.count("SELECT COUNT(*) FROM metric_rollups WHERE dim_id = 0 AND node_id = 0 AND tenant_id = 0 AND dim_sub = '' AND agg = 'sum'") != 1 {
			t.Error("metric_rollups 默认值异常")
		}
		base := "INSERT INTO metric_rollups (granularity, window_start, dim_type, metric, value, agg) VALUES (?, '2026-11-20 10:01:00.000', ?, 'requests', 1, ?)"
		c.bad(errCheckViolated, base, "2m", "node", "sum")
		c.bad(errCheckViolated, base, "1m", "galaxy", "sum")
		c.bad(errCheckViolated, base, "1m", "node", "avg")
		c.bad(errNoDefault, "INSERT INTO metric_rollups (granularity, window_start, dim_type, metric) VALUES ('1m', '2026-11-20 10:02:00.000', 'node', 'requests')")                         // value 无默认
		c.ok("INSERT INTO metric_rollups (granularity, window_start, dim_type, metric, value, node_id, agg) VALUES ('1m', '2026-11-20 10:00:00.000', 'site_node', 'requests', 1, 7, 'sum')") // 节点维度区分
		// 孤儿引用不被拒（高频表无外键，引用完整性由写入路径保证）。
		c.ok("INSERT INTO metric_rollups (granularity, window_start, dim_type, dim_id, node_id, tenant_id, metric, value) VALUES ('5m', '2026-11-20 10:05:00.000', 'site', 999999999, 999999999, 999999999, 'bytes_out', 1)")
	})

	run("rollup_windows：幂等键唯一（同代同哈希 = DUPLICATE 的判定依据）", func(c *cx) {
		ins := "INSERT INTO rollup_windows (node_id, site_id, window_start, window_seconds, generation, content_sha256) VALUES (?, ?, '2026-11-20 10:00:00.000', 60, ?, ?)"
		h := hash()
		c.ok(ins, 1, 0, 1, h)
		c.bad(errDup, ins, 1, 0, 1, h)      // 同窗口重复上报 → 由唯一键兜底
		c.bad(errDup, ins, 1, 0, 1, hash()) // 同代异值（CONFLICT）同样撞唯一键：Service 先读后判，不得覆盖
		c.ok(ins, 1, 5, 1, h)               // 另一站点
		c.ok(ins, 2, 0, 1, h)               // 另一节点
		if c.count("SELECT COUNT(*) FROM rollup_windows WHERE node_id = 1 AND site_id = 0 AND generation = 1") != 1 {
			t.Error("同代异值不得覆盖首次")
		}
		c.ok("UPDATE rollup_windows SET generation = 2, content_sha256 = ? WHERE node_id = 1 AND site_id = 0", hash()) // 更大代 REPLACED
		c.bad(errNoDefault, "INSERT INTO rollup_windows (node_id, window_start, window_seconds, generation) VALUES (1, '2026-11-20 10:01:00.000', 60, 1)")
	})

	run("access_log_chunks：去重键含 log_date（跨 UTC 零点同键两行）、区间约束、未归属 site_id=0", func(c *cx) {
		ins := `INSERT INTO access_log_chunks (tenant_id, site_id, node_id, log_date, segment_seq, start_offset, end_offset, storage_path, file_offset, stored_length,
			line_count, raw_bytes, stored_bytes, min_ts, max_ts, sha256) VALUES (?, ?, ?, ?, 1, ?, ?, 'access/x/20261120/n-1.jsonl.zst', 0, 10, 1, 10, 5,
			'2026-11-20 23:59:59.000', '2026-11-21 00:00:01.000', ?)`
		c.ok(ins, 1, 2, 3, "2026-11-20", 0, 100, hash())
		c.ok(ins, 1, 2, 3, "2026-11-21", 0, 100, hash()) // 仅 log_date 不同：合法（IMP-08）
		c.bad(errDup, ins, 1, 2, 3, "2026-11-20", 0, 100, hash())
		c.ok(ins, 0, 0, 3, "2026-11-20", 100, 200, hash()) // 未归属（site_id=0，tenant 0）
		c.bad(errCheckViolated, ins, 1, 2, 3, "2026-11-22", 50, 10, hash())
		c.bad(errCheckViolated, `INSERT INTO access_log_chunks (tenant_id, site_id, node_id, log_date, segment_seq, start_offset, end_offset, storage_path, file_offset, stored_length,
			line_count, raw_bytes, stored_bytes, min_ts, max_ts, sha256) VALUES (1, 2, 3, '2026-11-23', 1, 0, 1, 'p', 0, 1, 1, 1, 1, '2026-11-23 00:00:02', '2026-11-23 00:00:01', ?)`, hash())
	})

	// ======== CC-1：跨租户外键的库层强制 ========
	run("CC-1：证书引用（site_domains / site_settings / certificate_deployments）跨租户被拒，同租户通过", func(c *cx) {
		t1, t2 := c.tenant(), c.tenant()
		s1 := site(c, t1)
		ce1, ce2 := cert(c, t1), cert(c, t2)
		d := c.ok("INSERT INTO site_domains (uid, tenant_id, site_id, hostname) VALUES (?, ?, ?, ?)", uid26(), t1, s1, fmt.Sprintf("h%d.example.test", next()))
		c.bad(errNoRefRow, "UPDATE site_domains SET certificate_id = ? WHERE id = ?", ce2, d) // 他租户证书
		c.bad(errNoRefRow, "INSERT INTO site_domains (uid, tenant_id, site_id, hostname, certificate_id) VALUES (?, ?, ?, ?, ?)", uid26(), t1, s1, fmt.Sprintf("h%d.example.test", next()), ce2)
		c.ok("UPDATE site_domains SET certificate_id = ? WHERE id = ?", ce1, d)
		st := c.ok("INSERT INTO site_settings (uid, tenant_id, site_id, proxy_json) VALUES (?, ?, ?, JSON_OBJECT())", uid26(), t1, s1)
		c.bad(errNoRefRow, "UPDATE site_settings SET default_certificate_id = ? WHERE id = ?", ce2, st)
		c.ok("UPDATE site_settings SET default_certificate_id = ? WHERE id = ?", ce1, st)
		n, cl := node(c), cluster(c)
		insCD := "INSERT INTO certificate_deployments (tenant_id, certificate_id, cert_version, node_id, cluster_id) VALUES (?, ?, 1, ?, ?)"
		c.bad(errNoRefRow, insCD, t1, ce2, n, cl)
		c.bad(errNoRefRow, insCD, t2, ce1, n, cl)
		c.ok(insCD, t1, ce1, n, cl)
		// NULL 引用不检查（未绑定证书）；被绑定的证书不可物理删除（RESTRICT，须先解绑；证书本身软删除）。
		c.ok("UPDATE site_domains SET certificate_id = NULL WHERE id = ?", d)
		c.bad(errRowIsRef, "DELETE FROM certificates WHERE id = ?", ce1)
	})

	run("CC-1：origin_health / cache_task_nodes / rule_versions 的租户必须与父行一致", func(c *cx) {
		t1, t2 := c.tenant(), c.tenant()
		s1, s1b, s2 := site(c, t1), site(c, t1), site(c, t2)
		g1 := c.ok("INSERT INTO origin_groups (uid, tenant_id, site_id, name) VALUES (?, ?, ?, 'g')", uid26(), t1, s1)
		o1 := c.ok("INSERT INTO origins (uid, tenant_id, site_id, origin_group_id, address, port) VALUES (?, ?, ?, ?, 'o.example.test', 80)", uid26(), t1, s1, g1)
		n := node(c)
		insOH := "INSERT INTO origin_health (tenant_id, origin_id, node_id) VALUES (?, ?, ?)"
		c.bad(errNoRefRow, insOH, t2, o1, n)
		oh := c.ok(insOH, t1, o1, n)
		c.ok("DELETE FROM origins WHERE id = ?", o1) // CASCADE 仍有效
		if c.count("SELECT COUNT(*) FROM origin_health WHERE id = ?", oh) != 0 {
			t.Error("源站删除应级联 origin_health")
		}

		ct := c.ok("INSERT INTO cache_tasks (uid, tenant_id, site_id, kind, request_id) VALUES (?, ?, ?, 'purge_url', ?)", uid26(), t1, s1, uid26())
		insCTN := "INSERT INTO cache_task_nodes (tenant_id, cache_task_id, node_id) VALUES (?, ?, ?)"
		c.bad(errNoRefRow, insCTN, t2, ct, n)
		ctn := c.ok(insCTN, t1, ct, n)
		c.ok("DELETE FROM cache_tasks WHERE id = ?", ct)
		if c.count("SELECT COUNT(*) FROM cache_task_nodes WHERE id = ?", ctn) != 0 {
			t.Error("缓存任务删除应级联回执")
		}

		hs := hash()
		r1 := c.ok("INSERT INTO site_rules (uid, tenant_id, site_id, rule_type, phase, name, doc_json, doc_sha256) VALUES (?, ?, ?, 'redirect', 'redirect', 'r', JSON_OBJECT(), ?)", uid26(), t1, s1, hs)
		insRV := `INSERT INTO rule_versions (uid, tenant_id, site_id, rule_id, version_no, rule_type, phase, priority, enabled, doc_json, doc_sha256)
			VALUES (?, ?, ?, ?, 1, 'redirect', 'redirect', 1000, 1, JSON_OBJECT(), ?)`
		c.bad(errNoRefRow, insRV, uid26(), t2, s2, r1, hs)  // 租户/站点自洽但规则属他租户
		c.bad(errNoRefRow, insRV, uid26(), t1, s1b, r1, hs) // 同租户、规则属另一站点
		c.bad(errNoRefRow, insRV, uid26(), t2, s1, r1, hs)  // 租户与站点不一致（站点复合外键）
		c.ok(insRV, uid26(), t1, s1, r1, hs)
	})

	run("CC-1：复合外键 + ON DELETE SET NULL 与 NOT NULL 的 tenant_id 冲突（ERROR 1830），故证书引用用 RESTRICT", func(c *cx) {
		c.bad(1830, "CREATE TABLE cc1_probe (id BIGINT UNSIGNED NOT NULL PRIMARY KEY, tenant_id BIGINT UNSIGNED NOT NULL, cert_id BIGINT UNSIGNED NULL, "+
			"CONSTRAINT fk_cc1_probe FOREIGN KEY (tenant_id, cert_id) REFERENCES certificates (tenant_id, id) ON DELETE SET NULL)")
	})
}

// CC-1：外键清单一致性——凡「子表与父表都有 tenant_id 列、而外键不含 tenant_id」的外键，必须登记在下表（要么是被站点级复合外键传递保证，
// 要么是复合外键无法表达的 SET NULL/平台级父行，由 Service/Repository 校验）。新增此类外键而未登记则本用例失败，迫使作者做出显式决策。
func TestM1CCrossTenantForeignKeyInventory(t *testing.T) {
	e, _ := m1cUp(t)
	got := e.column(t, `SELECT CONCAT(k.TABLE_NAME, '.', GROUP_CONCAT(k.COLUMN_NAME ORDER BY k.ORDINAL_POSITION), ' -> ', k.REFERENCED_TABLE_NAME)
FROM information_schema.KEY_COLUMN_USAGE k
WHERE k.TABLE_SCHEMA = DATABASE() AND k.REFERENCED_TABLE_NAME IS NOT NULL AND k.REFERENCED_TABLE_NAME <> 'tenants'
  AND EXISTS (SELECT 1 FROM information_schema.COLUMNS c WHERE c.TABLE_SCHEMA = k.TABLE_SCHEMA AND c.TABLE_NAME = k.TABLE_NAME AND c.COLUMN_NAME = 'tenant_id')
  AND EXISTS (SELECT 1 FROM information_schema.COLUMNS c WHERE c.TABLE_SCHEMA = k.TABLE_SCHEMA AND c.TABLE_NAME = k.REFERENCED_TABLE_NAME AND c.COLUMN_NAME = 'tenant_id')
GROUP BY k.TABLE_NAME, k.CONSTRAINT_NAME, k.REFERENCED_TABLE_NAME
HAVING SUM(k.COLUMN_NAME = 'tenant_id') = 0
ORDER BY 1`)
	residual := map[string]string{
		// 由站点级复合外键传递保证（origins 与 origin_groups 同属 (tenant_id, site_id)）。
		"origins.site_id,origin_group_id -> origin_groups": "传递：origins 与 origin_groups 的 site_id 一致，且各自经复合外键绑定到同一站点租户",
		// SET NULL（复合外键不能 SET NULL，ERROR 1830）——追溯/弱引用，Service/Repository 校验同租户。
		"api_tokens.rotated_from_id -> api_tokens":              "SET NULL 自引用（A 批）",
		"cache_task_nodes.node_task_id -> node_tasks":           "SET NULL 弱引用（node_tasks 为平台级队列）",
		"cache_tasks.task_id -> tasks":                          "SET NULL 弱引用（tasks 为平台级任务表）",
		"deployments.queue_id -> publish_queue":                 "SET NULL 来源队列项，同在 publish.dispatch 事务内写入",
		"deployments.task_id -> tasks":                          "SET NULL 弱引用",
		"deployments.trigger_site_id -> sites":                  "SET NULL 追溯；租户视图按 (tenant_id, trigger_site_id) 联合过滤，不会越权",
		"node_tasks.task_id -> tasks":                           "SET NULL 弱引用",
		"publish_queue.deployment_id -> deployments":            "SET NULL（循环外键，dispatch 回填）",
		"publish_queue.site_version_id -> site_versions":        "SET NULL，dispatch 构建时写入同站点版本",
		"publish_queue.task_id -> tasks":                        "SET NULL 弱引用",
		"rule_versions.rollback_of_version_id -> rule_versions": "SET NULL 自引用，Service 保证同规则",
		"site_rules.current_version_id -> rule_versions":        "SET NULL（循环外键）",
		"site_rules.published_version_id -> rule_versions":      "SET NULL（循环外键）",
		"site_rules.template_id -> rule_templates":              "SET NULL；模板可为平台内置（tenant_id NULL），复合外键无法表达",
		"site_versions.block_id -> config_blocks":               "RESTRICT；内容寻址块（tenant_id 仅追溯用，可空）",
		"sites.group_id -> site_groups":                         "SET NULL（分组删除后站点脱组）",
		"sites.last_deployment_id -> deployments":               "SET NULL（循环外键）",
		"tasks.parent_task_id -> tasks":                         "CASCADE 自引用（A 批）",
		"user_roles.role_id -> roles":                           "CASCADE；角色可为平台级（tenant_id NULL），授予租户一致性由 RBAC Service 校验（1.18）",
	}
	var want []string
	for k := range residual {
		want = append(want, k)
	}
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("未登记的跨租户外键（或登记项已不存在）：新增此类外键须用复合外键 (tenant_id, …) 或登记理由\n实际:\n  %s\n登记:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	// 已强制的复合外键清单（CC-1）：外键列含 tenant_id，且引用父表的 (tenant_id, …) 唯一键。
	enforced := e.column(t, `SELECT CONCAT(k.TABLE_NAME, '.', k.CONSTRAINT_NAME) FROM information_schema.KEY_COLUMN_USAGE k
WHERE k.TABLE_SCHEMA = DATABASE() AND k.REFERENCED_TABLE_NAME IN ('certificates', 'origins', 'cache_tasks', 'site_rules') AND k.COLUMN_NAME = 'tenant_id' AND k.REFERENCED_COLUMN_NAME = 'tenant_id'
ORDER BY 1`)
	wantEnforced := []string{
		"cache_task_nodes.fk_cache_task_nodes_cache_tasks", "certificate_deployments.fk_certificate_deployments_certificates",
		"origin_health.fk_origin_health_origins", "rule_versions.fk_rule_versions_site_rules",
		"site_domains.fk_site_domains_certificates", "site_settings.fk_site_settings_certificates",
		// 父表自身的租户外键（certificates/origins/cache_tasks/site_rules → sites 复合）不在此列：它们引用 sites。
	}
	if strings.Join(enforced, ",") != strings.Join(wantEnforced, ",") {
		t.Errorf("CC-1 复合外键 = %v，期望 %v", enforced, wantEnforced)
	}
}
