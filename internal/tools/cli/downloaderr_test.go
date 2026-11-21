package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/downloader"
)

// DownloadError 的映射必须覆盖 downloader 的每一种失败分类（新增分类而忘记映射会使本测试失败），
// 且校验类一律退出码 5、传输类 1、用法类 2、未发布 7/内置 8。
func TestDownloadErrorMappingIsExhaustive(t *testing.T) {
	want := map[downloader.Kind]struct {
		exit ExitCode
		code string
	}{
		downloader.KindInsecureURL:  {ExitVerifyFailed, "verify.insecure_transport"},
		downloader.KindTLS:          {ExitVerifyFailed, "verify.tls_untrusted"},
		downloader.KindSize:         {ExitVerifyFailed, "verify.size_mismatch"},
		downloader.KindSHA256:       {ExitVerifyFailed, "verify.sha256_mismatch"},
		downloader.KindExtract:      {ExitVerifyFailed, "verify.package_extract"},
		downloader.KindBundle:       {ExitVerifyFailed, "verify.bundle_invalid"},
		downloader.KindBundleAbsent: {ExitUsage, "usage.invalid_value"},
		downloader.KindNetwork:      {ExitGeneral, "download.network"},
		downloader.KindOptions:      {ExitUsage, "usage.invalid_value"},
		downloader.KindUnreleased:   {ExitNotReleased, "component.not_released"},
		downloader.KindBuiltin:      {ExitBuiltin, "component.builtin"},
		downloader.KindNoVersion:    {ExitUsage, "usage.invalid_value"},
		downloader.KindStaging:      {ExitPrecondition, "precondition.staging"},
	}
	for _, k := range downloader.Kinds() {
		w, ok := want[k]
		if !ok {
			t.Fatalf("分类 %s 没有在本测试中声明预期映射", k)
		}
		e := DownloadError(&downloader.Error{Kind: k, Msg: "m", Details: map[string]any{"a": 1}})
		if e.Exit != w.exit || e.Code != w.code || e.Hint == "" || e.Details["a"] != 1 {
			t.Errorf("%s -> %+v，期望 exit=%d code=%s", k, e, w.exit, w.code)
		}
	}
	if len(want) != len(downloader.Kinds()) {
		t.Fatalf("本测试声明了 %d 个分类，downloader.Kinds() 有 %d 个", len(want), len(downloader.Kinds()))
	}
	if e := DownloadError(&downloader.Error{Kind: "nope"}); e.Exit != ExitGeneral || e.Code != "download.unknown" {
		t.Fatalf("未知分类应兜底为通用失败：%+v", e)
	}
}

// 经真实的 Run.finish：下载器错误（含被包装的）得到映射后的退出码与合法 envelope。
func TestDownloaderErrorsThroughFinish(t *testing.T) {
	for _, c := range []struct {
		k    downloader.Kind
		exit int
		code string
	}{
		{downloader.KindSHA256, 5, "verify.sha256_mismatch"},
		{downloader.KindInsecureURL, 5, "verify.insecure_transport"},
		{downloader.KindNetwork, 1, "download.network"},
		{downloader.KindBundleAbsent, 2, "usage.invalid_value"},
	} {
		err := fmt.Errorf("安装 node: %w", &downloader.Error{Kind: c.k, Msg: "x", Cause: errors.New("底层")})
		got, env := finishWith(t, err)
		if got != c.exit {
			t.Errorf("%s: 退出码 = %d，期望 %d", c.k, got, c.exit)
		}
		errs, _ := env["errors"].([]any)
		if len(errs) != 1 || errs[0].(map[string]any)["code"] != c.code {
			t.Errorf("%s: envelope errors = %v", c.k, env["errors"])
		}
	}
}
