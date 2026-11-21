package server

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/config"
)

// Story 1.22b：绕过 config.Parse 直接构造的 Config 若 metrics.listen 非回环，Run 在触碰任何基础设施之前拒绝启动（此前只告警）。
func TestRunRefusesNonLoopbackMetrics(t *testing.T) {
	rs, _ := ParseRoles("api")
	for _, l := range []string{"0.0.0.0:9180", ":9180", "[::]:9180", "203.0.113.5:9180", "localhost:9180", ""} {
		cfg := config.Defaults()
		cfg.Metrics.Listen = l
		err := Run(context.Background(), Options{Config: &cfg, Roles: rs, Logger: zap.NewNop()})
		if err == nil || !strings.Contains(err.Error(), "metrics.listen") {
			t.Errorf("metrics.listen=%q 应拒绝启动: %v", l, err)
		}
	}
}
