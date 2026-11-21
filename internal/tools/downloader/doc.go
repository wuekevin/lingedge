// Package downloader 实现 LingEdge Tools 的下载器与离线 bundle，以及 §9.5 校验链的第 5–7 步（Story 1.28，FR-9，NFR-11/15）。
//
// 在线与离线共用同一条链：取得 manifest.json/.sig（在线经 HTTPS，离线从 bundle 读取）→ manifest.Verifier.Verify（§9.5 步 1–4）→
// 取得包到 staging（大小上限 = 清单 size；流式 SHA256；断点续传整体校验后才落名）→ 解压前读 META/SHA256SUMS(.sig) 并验签（域前缀
// LINGEDGE-PKG-V1）→ security.SafeExtract 逐文件校验 → META/package.json 与清单一致 → Verifier.Commit。任一步失败整个 staging/<tx> 被删除。
//
// 传输策略（tools-design §18.3）：仅 HTTPS；系统根证书 + 可选 release.ca_file；禁止重定向到非 HTTPS；没有 --insecure（也没有任何关闭证书校验的开关）；
// 超时/空闲超时/有限重试/代理可配。离线 bundle 本身不被信任：路径经 realpath 校验为普通文件，tar 索引严格受限，其内的清单、签名与包与在线走完全相同的验证。
//
// 本包不读系统时钟（时间由 manifest.Verifier 的 Now 注入）；归档解析只经 internal/tools/security（G-4）。
package downloader
