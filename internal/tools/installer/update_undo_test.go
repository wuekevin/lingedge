package installer_test

import (
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/registry"
)

// journal 内容在恢复时不被盲信：伪造/损坏的撤销动作被拒绝（路径形状、版本号、单元名、数值），而不是被执行。
func TestUpdateUndoActionsValidateJournalContent(t *testing.T) {
	w := newUpdW(t)
	in := w.NewInstaller("run-undo", recDeps(w))
	u := &installer.Undoer{Root: w.Root, Reg: in.Deps().Reg, Update: in.Deps().Update}
	bad := []registry.UndoAction{
		{Action: installer.ActUpdateSwitchCurrent, Extra: map[string]string{"component": "../etc", "to": "0.1.0"}},
		{Action: installer.ActUpdateSwitchCurrent, Extra: map[string]string{"component": "node", "to": "../../x"}},
		{Action: installer.ActUpdateSwitchCurrent, Extra: map[string]string{"component": "node", "to": "latest"}},
		{Action: installer.ActUpdateStartUnits, Extra: map[string]string{"units": "sshd.service", "was_active": "true"}},
		{Action: installer.ActUpdateStartUnits, Extra: map[string]string{"units": "lingedge-node.service;rm -rf /", "was_active": "true"}},
		{Action: installer.ActUpdateDBRevert, Extra: map[string]string{"component": "node", "path": "migrate_down", "new_bin": "/opt/lingedge/server/releases/1.0.0/bin/lingedge-server", "pre": "1", "target": "2"}},
		{Action: installer.ActUpdateDBRevert, Extra: map[string]string{"component": "server", "path": "migrate_down", "new_bin": "/tmp/evil/lingedge-server", "pre": "1", "target": "2"}},
		{Action: installer.ActUpdateDBRevert, Extra: map[string]string{"component": "server", "path": "migrate_down", "new_bin": "/opt/lingedge/server/releases/../../../tmp/x/bin/lingedge-server", "pre": "1", "target": "2"}},
		{Action: installer.ActUpdateDBRevert, Extra: map[string]string{"component": "server", "path": "rm_rf", "new_bin": "/opt/lingedge/server/releases/1.0.0/bin/lingedge-server", "pre": "1", "target": "2"}},
		{Action: installer.ActUpdateDBRevert, Extra: map[string]string{"component": "server", "path": "migrate_down", "new_bin": "/opt/lingedge/server/releases/1.0.0/bin/lingedge-server", "pre": "x", "target": "2"}},
		{Action: installer.ActUpdateDBRevert, Extra: map[string]string{"component": "server", "path": "restore_backup", "new_bin": "/opt/lingedge/server/releases/1.0.0/bin/lingedge-server", "pre": "1", "target": "2", "backup": "/etc/passwd"}},
		{Action: installer.ActUpdateDBRevert, Extra: map[string]string{"component": "server", "path": "restore_backup", "new_bin": "/opt/lingedge/server/releases/1.0.0/bin/lingedge-server", "pre": "1", "target": "2", "backup": "relative/lingedge-backup-h-server-20261124T000000Z.tar.gz"}},
	}
	for i, a := range bad {
		_, err := u.Apply(t.Context(), a)
		if err == nil {
			t.Errorf("伪造的撤销动作 #%d (%s) 应被拒绝", i, a.Action)
			continue
		}
		// 必须是「校验拒绝」，而不是碰巧在后面的步骤（缺执行器/缺凭据…）失败：校验失败的消息都含「不合法」或「拒绝」。
		if !strings.Contains(err.Error(), "不合法") && !strings.Contains(err.Error(), "拒绝") {
			t.Errorf("撤销动作 #%d (%s) 不是被校验拒绝，而是别处失败：%v", i, a.Action, err)
		}
	}
	// 请求/备份记录是空操作；没有装配回滚执行器时撤销明确失败（而不是假装成功）。
	if _, err := u.Apply(t.Context(), registry.UndoAction{Action: installer.ActUpdateRequest}); err != nil {
		t.Error(err)
	}
	if _, err := (&installer.Undoer{Root: w.Root, Reg: in.Deps().Reg}).Apply(t.Context(), registry.UndoAction{Action: installer.ActUpdateSwitchCurrent, Extra: map[string]string{"component": "node", "to": "0.1.0"}}); err == nil {
		t.Error("没有回滚执行器时必须失败")
	}
}
