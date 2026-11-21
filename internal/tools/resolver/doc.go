// Package resolver 是 LingEdge Tools 的依赖 / 版本 / 资源 / 端口求解器（Story 1.30，tools-design §10.3）。
//
// 它是**纯函数**：输入 Input（用户选择 want、已装状态 installed、已验签的清单 manifest、系统探测结果 system），
// 输出 Plan{components, order, steps, ports, units, files, warnings, blockers}。
// 无 IO、无时钟、无随机、不读环境；同输入同输出（order 稳定，map 迭代序无关，输入切片顺序无关）。
//
// 复用：版本与约束解析一律走 internal/tools/manifest（Masterminds semver v3.5.0 的固定封装，不另造第二套）；
// 已装组件取 internal/tools/registry 的 State/Component。系统探测的真实产出属 Story 1.31（baota/端口/依赖只读探测），
// 本包只消费「探测结果作为输入」，结构见 System（字段即 1.31 的产出契约）。
//
// 失败的表达：不返回 error 而是把原因放进 Plan.Blockers（--dry-run 同样展示）；Plan.Err() 返回 *Error，
// cli.ResolveError 把它映射为退出码（BUILTIN 8、NOT_RELEASED 7、无解/资源/系统 3、端口 4、吊销 5、需确认 9、用法 2）。
package resolver
