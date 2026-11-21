package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
)

const (
	planSchema   = "lingedge-release-plan-v1"
	toSignSchema = "lingedge-release-tosign-v1" // 与 scripts/release-sign 的约定（两个独立程序之间的接口，不共享代码）

	kindSums     = "package-sums"
	kindManifest = "manifest"
)

// releasePlan 是 build 写给 finalize 的计划（plan.json）：清单里除 artifacts 之外的全部元数据，以及每个包的 stage 位置。
type releasePlan struct {
	Schema     string          `json:"schema"`
	Release    string          `json:"release"`
	Commit     string          `json:"commit"`
	BuiltAt    string          `json:"built_at"` // 提交时间（SOURCE_DATE_EPOCH 语义），用于包内 built_at 与 tar 时间戳
	Components []planComponent `json:"components"`
	Packages   []planPackage   `json:"packages"`
}

type planComponent struct {
	Name        string                 `json:"name"`
	Status      string                 `json:"status"`
	Description string                 `json:"description,omitempty"`
	Internal    bool                   `json:"internal,omitempty"`
	BuiltinIn   string                 `json:"builtin_in,omitempty"`
	Version     *manifest.VersionEntry `json:"version,omitempty"` // 仅 available
}

type planPackage struct {
	ID           string `json:"id"` // <component>-<version>-<platform>
	Component    string `json:"component"`
	Version      string `json:"version"`
	Variant      string `json:"variant"`
	Platform     string `json:"platform"`
	Stage        string `json:"stage"` // 相对发行输出目录
	Filename     string `json:"filename"`
	FilesCount   int    `json:"files_count"`
	UnpackedSize int64  `json:"unpacked_size"`
}

// toSign / toSignItem 与 scripts/release-sign 的 ToSign/Item 字段一一对应（JSON 契约）。
type toSign struct {
	Schema  string       `json:"schema"`
	Release string       `json:"release"`
	Commit  string       `json:"commit"`
	Round   int          `json:"round"`
	Items   []toSignItem `json:"items"`
}

type toSignItem struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Sig    string `json:"sig"`

	Component    string `json:"component,omitempty"`
	Version      string `json:"version,omitempty"`
	Platform     string `json:"platform,omitempty"`
	FilesCount   int    `json:"files_count,omitempty"`
	UnpackedSize int64  `json:"unpacked_size,omitempty"`
	Channel      string `json:"channel,omitempty"`
	Sequence     int64  `json:"sequence,omitempty"`
	GeneratedAt  string `json:"generated_at,omitempty"`
	ValidUntil   string `json:"valid_until,omitempty"`
}

var rePlatformFlag = regexp.MustCompile(`^(any|linux-(amd64|arm64))$`)

func packageFilename(component, variant, version, platform string) string {
	if platform == "any" {
		if variant == "default" {
			return fmt.Sprintf("lingedge-%s-%s.tar.gz", component, version)
		}
		return fmt.Sprintf("lingedge-%s-%s-%s.tar.gz", component, variant, version)
	}
	return fmt.Sprintf("lingedge-%s-%s-%s.tar.gz", component, version, platform)
}

func loadPlan(out string) (*releasePlan, error) {
	var p releasePlan
	if err := readJSON(filepath.Join(out, "plan.json"), &p); err != nil {
		return nil, fmt.Errorf("读取 plan.json 失败（先运行 build）: %w", err)
	}
	if p.Schema != planSchema {
		return nil, fmt.Errorf("plan.json schema 必须是 %s", planSchema)
	}
	return &p, nil
}

// requireEmptyOut 保证输出目录不存在或为空：发行物不得与旧产物混杂。
func requireEmptyOut(out string) error {
	ents, err := os.ReadDir(out)
	if errors.Is(err, os.ErrNotExist) {
		return os.MkdirAll(out, 0o755)
	}
	if err != nil {
		return err
	}
	if len(ents) > 0 {
		return fmt.Errorf("输出目录 %s 非空：发行物不得与旧产物混杂（换目录或先清理）", out)
	}
	return nil
}

func parseTime(flagName, v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s 不是 RFC 3339 时间: %w", flagName, err)
	}
	return t.UTC(), nil
}

var reComponentFlag = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
