// Package sdnotify 实现 systemd `Type=notify` 的就绪通知（Story 1.13 AC3，sd_notify(3) 协议的最小子集）。
//
// 只做一件事：把状态串（如 "READY=1"）作为一个数据报写到 $NOTIFY_SOCKET 指向的 Unix 数据报 socket；无任何外部依赖。
//   - $NOTIFY_SOCKET 未设置或为空 → no-op（非 systemd 环境），返回 (false, nil)；
//   - 路径以 '@' 开头表示 Linux 抽象 socket，按协议把首字符换成 NUL 字节；
//   - 其余只接受绝对路径（systemd 只会给出这两种形态），其他取值视为配置错误，返回 error 而不是静默忽略。
//
// 通知失败不应使进程退出（是否因此失败由调用方决定）：Type=notify 的 unit 在超时后由 systemd 判定启动失败。
package sdnotify

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/clock"
)

// 常用状态串。
const (
	Ready    = "READY=1"
	Stopping = "STOPPING=1"
)

// EnvSocket 是 systemd 传入的通知 socket 环境变量名。
const EnvSocket = "NOTIFY_SOCKET"

// Notify 向 $NOTIFY_SOCKET 发送 state；返回是否真的发送了（未设置 NOTIFY_SOCKET 时为 false, nil）。
func Notify(state string) (bool, error) {
	return NotifyTo(os.Getenv(EnvSocket), state)
}

// NotifyTo 向指定 socket 发送 state（socket 语义同 $NOTIFY_SOCKET）。
func NotifyTo(socket, state string) (bool, error) {
	if socket == "" {
		return false, nil
	}
	if state == "" || strings.ContainsRune(state, 0) {
		return false, errors.New("sdnotify: 状态串为空或含 NUL")
	}
	addr, err := resolve(socket)
	if err != nil {
		return false, err
	}
	c, err := net.DialUnix("unixgram", nil, addr)
	if err != nil {
		return false, fmt.Errorf("sdnotify: 连接通知 socket: %w", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetWriteDeadline(clock.System().Now().Add(2 * time.Second))
	if _, err := c.Write([]byte(state)); err != nil {
		return false, fmt.Errorf("sdnotify: 写通知: %w", err)
	}
	return true, nil
}

func resolve(socket string) (*net.UnixAddr, error) {
	switch {
	case strings.HasPrefix(socket, "@"):
		if len(socket) == 1 {
			return nil, errors.New("sdnotify: NOTIFY_SOCKET 抽象 socket 名为空")
		}
		return &net.UnixAddr{Name: "\x00" + socket[1:], Net: "unixgram"}, nil
	case strings.HasPrefix(socket, "/"):
		return &net.UnixAddr{Name: socket, Net: "unixgram"}, nil
	}
	return nil, errors.New("sdnotify: NOTIFY_SOCKET 必须是绝对路径或以 '@' 开头的抽象 socket")
}
