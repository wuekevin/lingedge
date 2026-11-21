package cli

import (
	"context"
	"os"
	"os/signal"
)

// notifyInterrupt 返回在 SIGINT 时取消的 context（退出码 130）。
func notifyInterrupt() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}
