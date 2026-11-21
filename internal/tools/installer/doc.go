// Package installer 是 LingEdge Tools 的安装编排（tools-design §12，Story 1.33）：把基线 §4.5 的 22 步落成一个可回滚、可续做、
// 可 dry-run 的状态机，并把 1.25–1.32 的库接起来——registry（任务锁/journal/state.json）、downloader+manifest（取包与完整校验链）、
// resolver（依赖/版本/资源/端口求解）、probe（只读系统探测）、service（systemd 单元/运行用户/目录/systemctl）、security。
//
// 阶段：预检 Preflight 1–9（只读）→ 取包 Fetch 10–13 → 物化 Materialize 14–18 → 激活 Activate 19–20 → 提交 Commit 21–22。
//
// 硬规则（均有测试）：
//   - 先写 undo 再动手：每个有副作用的步骤先把「足以撤销」的信息 fsync 进 journal（registry.Journal.Begin），之后才执行；
//     Created 一律在动手前由只读检查决定（本事务是否会创建该对象），所以崩溃后 recover 知道该撤销什么、不会误删既有对象。
//   - 任一步失败 → 逆序撤销（每步撤销后记 undone，撤销本身幂等、可被中断后续做）；成功撤销后 journal 以 abort 结束并归档；
//     撤销失败 → 退出码 12 且保留 journal。
//   - 第 21 步是提交点：state.json 原子写（registry.UpdateState）。提交前崩溃视为未完成，由 journal 回滚或续跑。
//   - 绝不覆盖既有配置/单元/站点：内容不同 → 在预检结束时作为冲突停下并要求显式确认（--yes / 交互 y），确认后既有文件原样保留、新内容写入 `.new`。
//   - --dry-run 只做预检与 Plan，文件系统与 state 零变化（不取锁、不开 journal、不建目录）。
//   - 进度经 Event 通道输出（Deps.Events）；通道阻塞、关闭、慢消费者都不影响安装结果。
//
// 组件特有知识经 ComponentSpec 接口注入（初始配置/启动/健康检查）；具体组件由后续 Story 实现并注册（1.35 server 等）。
// 本包的生产构建**不含任何组件实现**：未注册的组件，install 以明确的错误失败（不会假装成功）。测试用的假组件只存在于 _test.go。
package installer
