//go:build !linux && !darwin

package probe

import (
	"errors"

	"github.com/wuekevin/lingedge/internal/tools/resolver"
)

const (
	defaultProcRoot = ""
	supportsProc    = false
)

var errUnsupported = errors.New("probe: 本平台不支持（生产目标是 Linux）")

func osMemory() (int64, int64, []string, error)   { return 0, 0, nil, errUnsupported }
func osDisks() ([]resolver.Disk, []string, error) { return nil, nil, errUnsupported }
