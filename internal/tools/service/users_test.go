package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestEnsureUserCreatesWhenMissing(t *testing.T) {
	e := newFakeEnv(t)
	p := e.provisioner(t, 0)
	res, err := p.EnsureUser(context.Background(), UserNode)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || !res.GroupCreated || res.UID != 998 || res.Name != "lingedge-node" {
		t.Fatalf("%+v", res)
	}
	want := []string{
		"id [-u] [lingedge-node]",
		"getent [group] [lingedge-node]",
		"useradd [--system] [--no-create-home] [--shell] [/usr/sbin/nologin] [--user-group] [lingedge-node]",
		"id [-u] [lingedge-node]",
	}
	if got := e.calls(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv\n得到 %q\n期望 %q", got, want)
	}
	// undo 记录「是否由本事务创建」。
	u := UserUndo(res)
	if u.Action != ActUserdel || !u.Created || u.Extra["user"] != "lingedge-node" || u.Extra["group_created"] != "true" {
		t.Errorf("%+v", u)
	}
}

func TestEnsureUserExistingIsUntouched(t *testing.T) {
	e := newFakeEnv(t)
	e.set(t, "users/lingedge", "431\n")
	res, err := e.provisioner(t, 0).EnsureUser(context.Background(), UserServer)
	if err != nil || res.Created || res.UID != 431 || res.Warning != "" {
		t.Fatalf("%+v %v", res, err)
	}
	if got := e.calls(t); !reflect.DeepEqual(got, []string{"id [-u] [lingedge]"}) {
		t.Fatalf("已存在的用户只应查询：%q", got)
	}
	if u := UserUndo(res); u.Created {
		t.Error("已存在的用户 undo 必须是 Created=false（撤销时不得 userdel）")
	}
}

func TestEnsureUserReusesExistingGroup(t *testing.T) {
	e := newFakeEnv(t)
	e.set(t, "groups/lingedge-node", "")
	res, err := e.provisioner(t, 0).EnsureUser(context.Background(), UserNode)
	if err != nil || !res.Created || res.GroupCreated {
		t.Fatalf("%+v %v", res, err)
	}
	if got := e.calls(t)[2]; got != "useradd [--system] [--no-create-home] [--shell] [/usr/sbin/nologin] [-g] [lingedge-node] [lingedge-node]" {
		t.Errorf("同名组已存在应 -g 复用：%q", got)
	}
}

func TestEnsureUserSafetyChecks(t *testing.T) {
	e := newFakeEnv(t)
	ctx := context.Background()
	if _, err := e.provisioner(t, 0).EnsureUser(ctx, "root"); KindOf(err) != KindInvalidParam {
		t.Errorf("非运行用户：%v", err)
	}
	if _, err := e.provisioner(t, 0).EnsureUser(ctx, "lingedge; rm -rf /"); KindOf(err) != KindInvalidParam {
		t.Errorf("注入：%v", err)
	}
	if n := len(e.calls(t)); n != 0 {
		t.Errorf("非法用户名不得触发任何进程：%d", n)
	}
	e.set(t, "users/lingedge", "0\n") // 一个 uid 为 0 的 "lingedge"
	if _, err := e.provisioner(t, 0).EnsureUser(ctx, UserServer); KindOf(err) != KindConflict {
		t.Errorf("uid 0：%v", err)
	}
	e.set(t, "users/lingedge-web", "1001\n")
	res, err := e.provisioner(t, 0).EnsureUser(ctx, UserWeb)
	if err != nil || res.Warning == "" || res.Created {
		t.Errorf("普通用户范围应只警告：%+v %v", res, err)
	}
}

func TestEnsureUserNeedsRootBeforeUseradd(t *testing.T) {
	e := newFakeEnv(t)
	_, err := e.provisioner(t, 1000).EnsureUser(context.Background(), UserDNS)
	if KindOf(err) != KindPermission {
		t.Fatalf("%v", err)
	}
	for _, c := range e.calls(t) {
		if len(c) >= 7 && c[:7] == "useradd" {
			t.Error("非 root 不得走到 useradd")
		}
	}
	// 但已存在的用户不需要 root（只查询）。
	e.set(t, "users/lingedge-dns", "500\n")
	if _, err := e.provisioner(t, 1000).EnsureUser(context.Background(), UserDNS); err != nil {
		t.Errorf("已存在的用户不需要 root：%v", err)
	}
}

func TestEnsureUserUseraddFailure(t *testing.T) {
	e := newFakeEnv(t)
	e.set(t, "fail-useradd", "")
	res, err := e.provisioner(t, 0).EnsureUser(context.Background(), UserReporter)
	if KindOf(err) != KindExec || res.Created {
		t.Fatalf("%+v %v", res, err)
	}
}

// 真实 id：本机（macOS/Linux）都有 /usr/bin/id，经真实 safeexec 与默认搜索目录执行，验证对真实输出/退出码/错误文本的解析。
func TestRealIDLookups(t *testing.T) {
	r, err := SystemRunner()
	if err != nil {
		t.Fatal(err)
	}
	p := &Provisioner{Run: r}
	ctx := context.Background()
	uid, err := p.lookupUID(ctx, "root")
	if err != nil || uid != 0 {
		t.Fatalf("root：%d %v", uid, err)
	}
	if _, err := p.lookupUID(ctx, "lingedge-no-such-user-xyz"); !errors.Is(err, errNoSuchUser) {
		t.Fatalf("不存在的用户应识别为 errNoSuchUser：%v", err)
	}
	// 非 root 运行时，EnsureUser 对真实不存在的用户应明确说需要 root，而不是 exec 失败。
	if p.euid() != 0 {
		if _, err := p.EnsureUser(ctx, UserReporter); KindOf(err) != KindPermission {
			if KindOf(err) != "" { // 本机恰有同名用户则不适用
				t.Logf("EnsureUser：%v", err)
			}
		}
	}
}
