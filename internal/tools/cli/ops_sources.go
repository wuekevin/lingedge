package cli

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/doctor"
	"github.com/wuekevin/lingedge/internal/tools/downloader"
	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
)

// fetchManifest 获取并验证通道清单：tools.yaml 的 release.* + downloader（TLS≥1.2、限长、重定向仅 https）+ 内置公钥验证链。
// 只读：Verify 不推进 sequence（那是 install/update 在整条校验链成功后的 Commit）。
func (r *Run) fetchManifest(ctx context.Context, channel string) (*manifest.Manifest, error) {
	if f := r.ops().FetchManifest; f != nil {
		return f(ctx, channel)
	}
	cfg, err := r.loadToolsConfig()
	if err != nil {
		return nil, err
	}
	url, err := cfg.ManifestURL(channel)
	if err != nil {
		return nil, err
	}
	newClient := r.env.Install.NewClient
	if newClient == nil {
		newClient = downloader.New
	}
	cl, err := newClient(cfg.DownloaderConfig("", false))
	if err != nil {
		return nil, err
	}
	defer cl.Close()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	mf, err := cl.FetchBytes(cctx, url, 8<<20)
	if err != nil {
		return nil, err
	}
	sig, err := cl.FetchBytes(cctx, url+".sig", 4<<10)
	if err != nil {
		return nil, err
	}
	v, err := r.env.NewVerifier()
	if err != nil {
		return nil, err
	}
	res, err := v.Verify(mf, sig, manifest.VerifyOptions{Channel: channel})
	if err != nil {
		return nil, err
	}
	return res.Manifest, nil
}

// doctorManifest 是 doctor 的清单来源：没有配置发行源（且没有注入）= nil（ver.* 报告 skip）。
func (r *Run) doctorManifest(tc installer.ToolsConfig) func(ctx context.Context) (*manifest.Manifest, error) {
	if tc.Release.BaseURL == "" && r.ops().FetchManifest == nil {
		return nil
	}
	return func(ctx context.Context) (*manifest.Manifest, error) { return r.fetchManifest(ctx, "stable") }
}

// dbQuery 以迁移账号（来源同 backup；绝不交互提示）对目标库执行一条只读查询。
func (r *Run) dbQuery(ctx context.Context, queries ...string) ([][][]string, error) {
	ce := backup.CredEnv{Getenv: r.env.Getenv, AllowedUIDs: r.env.Backup.AllowedUIDs, ServerYAML: r.hostPath(backup.ServerConfigFile)}
	if p := r.env.Getenv(backup.EnvMigrateCfg); p != "" {
		ce.MigrateYAML, ce.MigrateYAMLExplicit = p, true
	} else {
		ce.MigrateYAML = r.hostPath(backup.DefaultMigrateYAML)
	}
	conn, err := backup.ResolveDB(ce)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	my, err := r.mysqlTool()
	if err != nil {
		return nil, err
	}
	var out [][][]string
	for _, q := range queries {
		rows, err := my.Query(ctx, conn, q)
		if err != nil {
			return nil, errors.New("查询失败：" + redactString(err.Error()))
		}
		out = append(out, rows)
	}
	return out, nil
}

var reSQLIdent = regexp.MustCompile(`^[A-Za-z0-9_$]{1,64}$`)

// doctorPmax 列出有 pmax 分区的表及其 pmax 里的行数（表名来自 information_schema，经标识符白名单后才拼进 SQL）。
func (r *Run) doctorPmax() func(ctx context.Context) ([]doctor.PartitionRow, error) {
	if f := r.ops().PmaxRows; f != nil {
		return f
	}
	return func(ctx context.Context) ([]doctor.PartitionRow, error) {
		res, err := r.dbQuery(ctx, "SELECT DISTINCT table_name FROM information_schema.partitions WHERE table_schema = DATABASE() AND partition_name = 'pmax' ORDER BY table_name")
		if err != nil {
			return nil, err
		}
		var out []doctor.PartitionRow
		for _, row := range res[0] {
			if len(row) != 1 || !reSQLIdent.MatchString(row[0]) {
				return nil, fmt.Errorf("表名无法识别")
			}
			cnt, err := r.dbQuery(ctx, "SELECT COUNT(*) FROM `"+row[0]+"` PARTITION (pmax)")
			if err != nil {
				return nil, err
			}
			if len(cnt[0]) != 1 || len(cnt[0][0]) != 1 {
				return nil, fmt.Errorf("查询结果无法识别")
			}
			n, perr := strconv.ParseInt(strings.TrimSpace(cnt[0][0][0]), 10, 64)
			if perr != nil {
				return nil, fmt.Errorf("查询结果无法识别")
			}
			out = append(out, doctor.PartitionRow{Table: row[0], Rows: n})
		}
		return out, nil
	}
}

// doctorMySQL 以迁移账号（来源同 backup：环境变量/口令文件/root 属主的 migrate.yaml；绝不交互提示）查询 MySQL 服务端设置。
func (r *Run) doctorMySQL() func(ctx context.Context) (doctor.MySQLSettings, error) {
	if f := r.ops().MySQLSettings; f != nil {
		return f
	}
	return func(ctx context.Context) (doctor.MySQLSettings, error) {
		ce := backup.CredEnv{Getenv: r.env.Getenv, AllowedUIDs: r.env.Backup.AllowedUIDs, ServerYAML: r.hostPath(backup.ServerConfigFile)}
		if p := r.env.Getenv(backup.EnvMigrateCfg); p != "" {
			ce.MigrateYAML, ce.MigrateYAMLExplicit = p, true
		} else {
			ce.MigrateYAML = r.hostPath(backup.DefaultMigrateYAML)
		}
		conn, err := backup.ResolveDB(ce)
		if err != nil {
			return doctor.MySQLSettings{}, err
		}
		defer conn.Close()
		my, err := r.mysqlTool()
		if err != nil {
			return doctor.MySQLSettings{}, err
		}
		rows, err := my.Query(ctx, conn, "SELECT @@character_set_server, @@collation_server, @@sql_mode, @@version")
		if err != nil {
			return doctor.MySQLSettings{}, errors.New("查询失败：" + redactString(err.Error()))
		}
		if len(rows) != 1 || len(rows[0]) != 4 {
			return doctor.MySQLSettings{}, fmt.Errorf("查询结果无法识别")
		}
		return doctor.MySQLSettings{CharacterSet: rows[0][0], Collation: rows[0][1], SQLMode: rows[0][2], Version: rows[0][3]}, nil
	}
}
