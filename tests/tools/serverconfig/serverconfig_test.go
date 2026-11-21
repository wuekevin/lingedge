// Package serverconfig 验证 Tools 步骤 18 生成的 server.yaml / master.key / setup-token.hash 能被 Server 的真实实现加载与校验
// （Story 1.32 留给 1.33 的「格式与 internal/platform/config 一致并可被 server 真实加载校验」）。
// Tools 不得 import Server 的 internal 包（architecture §5.3 规则 5），所以这个跨界检查只能放在 tests/ 里。
package serverconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
	"github.com/wuekevin/lingedge/internal/tools/service"
	"github.com/wuekevin/lingedge/pkg/secretbox"
	"github.com/wuekevin/lingedge/pkg/setuptoken"
)

func TestGeneratedServerYAMLLoadsInRealConfig(t *testing.T) {
	for _, mode := range []service.CredentialMode{service.CredentialSystemd, service.CredentialKeyFile} {
		t.Run(string(mode), func(t *testing.T) {
			p, err := installer.ServerYAMLFromSettings(map[string]string{"env": "staging", "trusted-proxies": "10.0.0.0/8,127.0.0.1,::1"}, "127.0.0.1:18080", mode)
			if err != nil {
				t.Fatal(err)
			}
			b, err := installer.RenderServerYAML(p)
			if err != nil {
				t.Fatal(err)
			}
			// 真实加载（setup mode：向导尚未运行，无需数据库/Redis 配置）。
			cfg, err := config.Parse(b, config.Options{})
			if err != nil {
				t.Fatalf("Server 的 config.Parse 拒绝了生成的 server.yaml: %v\n%s", err, b)
			}
			if cfg.Server.Env != "staging" || cfg.Server.DataDir != service.ServerDataDir || cfg.HTTP.Listen != "127.0.0.1:18080" || len(cfg.Server.TrustedProxies) != 3 {
				t.Fatalf("加载结果与输入不符: %+v", cfg)
			}
			// ADR-027：LoadCredential 模式 master_key_file 必须留空；降级模式必须是主密钥文件的绝对路径。
			want := service.MasterKeyConfigValue(mode, service.ServerConfigDir+"/master.key")
			if cfg.Secrets.MasterKeyFile != want {
				t.Fatalf("secrets.master_key_file=%q，期望 %q", cfg.Secrets.MasterKeyFile, want)
			}
			if mode == service.CredentialSystemd && cfg.Secrets.MasterKeyFile != "" {
				t.Fatal("LoadCredential 模式不得设置 master_key_file（会绕过 LoadCredential）")
			}
			// 生成物不含口令。
			if strings.Contains(strings.ToLower(string(b)), "password") && !strings.Contains(string(b), "口令") {
				t.Errorf("server.yaml 不应含口令字段:\n%s", b)
			}
		})
	}
}

// 默认参数的生成物也能加载，且与示例配置的缺省取值一致（env=prod、127.0.0.1:8080）。
func TestDefaultServerYAMLMatchesDefaults(t *testing.T) {
	b, err := installer.RenderServerYAML(installer.DefaultServerYAMLParams())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(b, config.Options{})
	if err != nil {
		t.Fatal(err)
	}
	def := config.Defaults()
	if cfg.HTTP.Listen != def.HTTP.Listen || cfg.Server.DataDir != def.Server.DataDir || cfg.Server.Env != "prod" {
		t.Fatalf("%+v vs %+v", cfg, def)
	}
}

// 生成的 master.key 能被 pkg/secretbox 的真实 FileKeyProvider 加载（格式、32 字节、权限 0400/0440、属主），setup-token 哈希能被 setuptoken.Verify 校验。
func TestGeneratedMasterKeyAndTokenAreAccepted(t *testing.T) {
	key, err := installer.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	other, _ := installer.GenerateMasterKey()
	if string(key) == string(other) {
		t.Fatal("两次生成的主密钥不应相同")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "master.key")
	if err := os.WriteFile(p, key, 0o400); err != nil {
		t.Fatal(err)
	}
	kp, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: p})
	if err != nil {
		t.Fatalf("真实 FileKeyProvider 拒绝了生成的 master.key: %v", err)
	}
	defer kp.Close()
	// 降级模式的 0440 同样被接受。
	p2 := filepath.Join(dir, "master2.key")
	if err := os.WriteFile(p2, key, 0o440); err != nil {
		t.Fatal(err)
	}
	if kp2, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: p2}); err != nil {
		t.Fatalf("0440: %v", err)
	} else {
		kp2.Close()
	}
	tok, hash, err := setuptoken.Generate()
	if err != nil || !setuptoken.Verify(tok, hash) {
		t.Fatal("setup token 往返失败")
	}
}

// Story 1.35：随计划端口（--set 换端口、--setup-listen）渲染的 server.yaml 能被 Server 真实的配置加载器接受，四个监听与计划一致；
// metrics 绑定非回环在 Tools 一侧就被拒绝（与 Server 的「回环专用」规则一致，Story 1.22b）。
func TestServerYAMLFollowsPlanPortsAndLoadsInRealConfig(t *testing.T) {
	ports := []resolver.PlannedPort{
		{Component: "server", Proto: "tcp", Port: 18080, Bind: "127.0.0.1", Purpose: "rest", Overridden: true},
		{Component: "server", Proto: "tcp", Port: 19443, Bind: "10.0.0.5", Purpose: "grpc-mtls", Overridden: true},
		{Component: "server", Proto: "tcp", Port: 9444, Bind: "0.0.0.0", Purpose: "grpc-enroll", Conditional: true},
		{Component: "server", Proto: "tcp", Port: 19180, Bind: "127.0.0.1", Purpose: "metrics", Overridden: true},
	}
	for _, mode := range []service.CredentialMode{service.CredentialSystemd, service.CredentialKeyFile} {
		p, err := installer.ServerYAMLFromPlan(nil, "", mode, ports)
		if err != nil {
			t.Fatal(err)
		}
		b, err := installer.RenderServerYAML(p)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Parse(b, config.Options{})
		if err != nil {
			t.Fatalf("Server 的 config.Parse 拒绝了生成的 server.yaml: %v\n%s", err, b)
		}
		if cfg.HTTP.Listen != "127.0.0.1:18080" || cfg.GRPC.Listen != "10.0.0.5:19443" || cfg.GRPC.EnrollListen != "0.0.0.0:9444" || cfg.Metrics.Listen != "127.0.0.1:19180" {
			t.Errorf("监听与计划不符: %+v", cfg)
		}
	}
	bad := append([]resolver.PlannedPort(nil), ports...)
	bad[3].Bind = "0.0.0.0"
	if _, err := installer.ServerYAMLFromPlan(nil, "", service.CredentialSystemd, bad); err == nil {
		t.Error("metrics 绑定非回环必须在 Tools 一侧被拒绝")
	}
	// 同一份内容 Server 也拒绝（规则一致性的反向证明）。
	p, _ := installer.ServerYAMLFromPlan(nil, "", service.CredentialSystemd, ports)
	yml, _ := installer.RenderServerYAML(p)
	if _, err := config.Parse([]byte(strings.Replace(string(yml), "listen: 127.0.0.1:19180", "listen: 0.0.0.0:19180", 1)), config.Options{}); err == nil {
		t.Error("Server 应拒绝非回环的 metrics.listen")
	}
}
