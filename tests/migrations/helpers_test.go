package migrationstest

import (
	"testing"

	"github.com/wuekevin/lingedge/tests/mysqltest"
)

func mysqltestStart(t testing.TB) *mysqltest.Instance { return mysqltest.Start(t) }
