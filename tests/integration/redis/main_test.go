package redisit

import (
	"os"
	"testing"

	"github.com/wuekevin/lingedge/tests/redistest"
)

func TestMain(m *testing.M) { os.Exit(redistest.Main(m)) }
