// Package platform 是 Server 依赖图最底层的基础设施容器：根包只承载本说明，实现位于子包（子包布局自由，ADR-001 §3）。
//
// 已交付（Story 1.4）：
//   - config：YAML 配置加载（schema 校验、环境变量覆盖、未知键拒绝、Secret 类型）；
//   - logx：Zap 集成与日志脱敏（引擎在 pkg/redact）；
//   - apierr：统一错误码、HTTP 状态映射与错误体；
//   - requestid：Request ID（ULID）的入口读取/生成与 context/gRPC 元数据传递；
//   - clock：唯一允许读取系统时钟的位置（其余 internal/ 业务代码由 lint 禁止直接调用 time.Now）；
//   - uid：ULID 生成与校验。
//
// 其余基础设施（db、httpx、task 运行时、keyprovider、eventbus、audit 等）随后续 Story 增加；ports 子包见其自身说明。
package platform
