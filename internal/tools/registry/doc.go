// Package registry 是 LingEdge Tools 的本机安装状态层（Story 1.29，tools-design §8、§12.3、§16，FR-4/FR-6）。
//
// 它提供四件互相配合的东西，全部只依赖标准库与 internal/tools/security（叶子，依赖图 registry → security）：
//
//   - state.json（state.go）：已装组件的持久状态。schema 版本化（未知更高版本失败关闭、严格解析不静默丢字段）、不含密钥
//     （写入前逐值过 security 脱敏检查，命中即拒绝写）、0644（普通用户可 list/status）、经 security.WriteFileAtomic 原子写
//     （临时文件 + fsync + rename + 目录 fsync）。读不加锁——原子 rename 保证读者看到的永远是旧版或新版的完整文件。
//   - 事务日志 journal（journal.go、journal_read.go）：journal/<tx>.jsonl，追加写、每条 fsync、先记 undo 再动手，
//     open/begin/done/undone/commit/abort；读取对撕裂的尾行（kill -9/断电）容错；可归档到 journal/archive/。
//     每步幂等的落地助手见 idempotent.go（已存在且一致视为完成，不一致即冲突、绝不覆盖）。
//   - 任务锁（lock.go、task.go）：flock(2) 的全局状态锁与组件锁（LOCK_EX|LOCK_NB；多组件按字典序获取）。进程退出（含 kill -9）
//     内核自动释放，所以没有陈旧锁；锁文件里的 {pid, run_id, command, started_at, host} 只用于给被拒绝的一方显示持有者，
//     绝不用来判活（不使用 pid 文件 + kill -0）。获取失败是 *LockedError（CLI 映射为退出码 6）。
//   - 写命令入口（task.go）：Registry.Begin 取组件锁、扫描 journal，存在已死亡且未结束的事务 → *UnfinishedError
//     （CLI 映射为退出码 13，提示 lingedge recover）。「死亡」由 journal 文件自身的 flock 判定：活着的事务持有它，
//     被杀的事务由内核释放，所以与「另一个组件上正在进行的合法事务」不会混淆。
//
// 与其他包的关系（「无跨进程锁，靠 1.29 的全局任务锁保证」的落点）：
//   - manifest.FileSequenceStore（1.27）与 downloader.Acquire/Verifier.Commit（1.28）自身不加锁。多进程并发时它们的
//     读-改-写（Advance）会丢更新或让 sequence 回退。调用方必须持有组件任务锁（Registry.Begin 返回的 *Task）调用 downloader.Acquire，
//     并通过 LockedSequence 包装 FileSequenceStore（每次 Max/Advance 在全局锁短临界区内完成）——见 sequence.go 与 registry_test 的跨进程测试。
//   - staging/<tx> 的 tx 取自 Task.TxID()（与 journal 同一个 ID）。
//
// 平台：flock/属主检查在 platform_unix.go（linux、darwin）；其他平台是 platform_other.go 的桩（返回 KindUnsupported，失败关闭）。
// 文件系统类型识别（doctor 对网络文件系统告警，Story 1.42）在 fsinfo_linux.go/fsinfo_darwin.go/fsinfo_other.go；
// /proc 进程启动时间（仅用于持有者诊断显示）在 proc_linux.go，解析函数 parseProcStat 与平台无关并有单元测试。
package registry
