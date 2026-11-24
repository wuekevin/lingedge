package backup

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/wuekevin/lingedge/internal/tools/service"
)

// ApplyOptions 是 ApplyBackup 的输入。
type ApplyOptions struct {
	Component  string
	Archive    string
	Passphrase []byte
	// DB 仅 server 需要（迁移账号）。
	DB *DBConn
}

// ApplyResult 是 ApplyBackup 的结果。
type ApplyResult struct {
	BackupVersion string      `json:"backup_version"`
	SchemaBackup  int64       `json:"schema_backup"`
	FilesRestored []string    `json:"files_restored"`
	FilesKept     []string    `json:"files_kept"`
	Probe         ProbeResult `json:"probe"`
	LineageBefore int64       `json:"lineage_before,omitempty"`
	LineageAfter  int64       `json:"lineage_after,omitempty"`
	BackupLineage *int64      `json:"backup_lineage"`
}

// ApplyBackup 是升级回滚的「恢复升级前备份」路径（Story 1.43，tools-design §17.3/§17.5）：把备份里的配置/服务数据写回，server 再清空并重灌数据库、
// 探针解密核对、并在**启动 Server 之前**用 `current` 指向的（此时已是旧版本的）二进制执行 `lingedge-server lineage bump`。
// 它不停止/启动服务、不开 journal、不做恢复前安全备份、不处理主密钥（升级不触碰 master.key，主机没变）——这些由调用方（升级事务/手动回滚事务）负责；
// 本方法的每一步都是幂等的，可重复执行直到成功（journal 驱动的 recover --rollback 依赖这一点）。
// 要求：`current` 已经指回旧 release（lineage bump 与 schema 校验用它），否则 bump 会由新版本二进制执行（语义上无害，但违背「旧版本启动前 bump」的验收口径）。
func (s *Service) ApplyBackup(ctx context.Context, o ApplyOptions) (*ApplyResult, error) {
	stage, err := s.newStage("apply-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	st, err := s.unpack(o.Archive, o.Passphrase, stage)
	if err != nil {
		return nil, fmt.Errorf("读取升级前备份失败（%s）: %w", o.Archive, err)
	}
	m := st.m
	if o.Component != "" && o.Component != m.Component {
		return nil, fmt.Errorf("备份的组件是 %s，与回滚的组件 %s 不一致", m.Component, o.Component)
	}
	res := &ApplyResult{BackupVersion: m.Version, SchemaBackup: m.SchemaVersion, BackupLineage: m.DeployLineage, FilesRestored: []string{}, FilesKept: []string{}}
	if m.Component == "server" {
		if o.DB == nil {
			return nil, errors.New("恢复 server 的备份需要数据库连接")
		}
		binSchema, err := s.binarySchema(ctx)
		if err != nil {
			return nil, err
		}
		if m.SchemaVersion > binSchema {
			return nil, fmt.Errorf("备份的 schema 版本 %d 比 current 二进制支持的 %d 更新：current 尚未切回备份对应的旧版本", m.SchemaVersion, binSchema)
		}
	}
	keep := false
	if m.Component == "server" {
		keep, _ = s.runtimeCredsDecryptable()
	}
	restored, kept, err := s.applyFiles(st, keep)
	if err != nil {
		return nil, err
	}
	res.FilesRestored, res.FilesKept = nonNil(restored), nonNil(kept)
	if m.Component != "server" {
		return res, nil
	}
	if err := s.MySQL.Wipe(ctx, *o.DB); err != nil {
		return nil, fmt.Errorf("清空数据库失败: %w", err)
	}
	for _, e := range m.Files {
		if e.Kind == KindDB {
			if err := s.MySQL.Load(ctx, *o.DB, st.path(e)); err != nil {
				return nil, fmt.Errorf("导入数据库转储失败: %w", err)
			}
		}
	}
	pr, err := s.probe(ctx, *o.DB, m.KEKIDsRequired, s.host(ServerMasterKey), s.host(service.ServerDataDir+"/"+RuntimeCredentialsRel))
	if err != nil {
		return nil, err
	}
	res.Probe = pr
	before, after, err := s.bumpLineage(ctx)
	if err != nil {
		return nil, err
	}
	res.LineageBefore, res.LineageAfter = before, after
	if m.DeployLineage != nil && after <= *m.DeployLineage {
		return nil, fmt.Errorf("lineage bump 之后的值 %d 未严格大于备份记录的 %d", after, *m.DeployLineage)
	}
	return res, nil
}
