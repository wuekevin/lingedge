// 服务端消息的前端本地化（Story 1.37b）。
//
// 服务端的稳定标识（错误码 code、检查项 key、步骤 step）是契约；文案是服务端固定语言（检查项/成功回执为中文，错误 message/reason 多为英文）。
// 这里按稳定标识 + 固定句式把它们映射成 i18n 词条（zh-CN / en），带占位符的动态部分（时间、路径、版本、数量……）从原文里按固定句式取出。
// 任何没有命中的内容一律「回落到服务端原文」（并标注其语言），绝不丢失信息；错误码与 Request ID 的展示不受影响。
//
// 对账：`pnpm run check:setup-codes` 从 Go 源码（internal/platform/setup、auth bootstrap）与 OpenAPI 自动推导可能返回的错误码/检查项/步骤，
// 缺任何一个词条或本表缺项都会使 CI 失败；`servermsg.test.ts` 用从 Go 源码抽取的真实文案样本验证每个句式都能命中。

export interface MsgPart {
  /** `serverMsg.*` 下的词条 id（不含前缀）。 */
  id: string;
  params?: Record<string, string>;
}

/** 错误码 → 词条键：与 SetupError 的 hint 键同一约定（词条键里不出现 password 字样，G-6 扫描）。 */
export const codeKey = (code: string): string => code.replace('password', 'pw');

const group = (m: RegExpExecArray): Record<string, string> => {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(m.groups ?? {})) if (typeof v === 'string') out[k] = v;
  return out;
};

interface Pattern {
  re: RegExp;
  id: string;
}

function firstMatch(patterns: readonly Pattern[], text: string): MsgPart | null {
  for (const p of patterns) {
    const m = p.re.exec(text);
    if (m) {
      const params = group(m);
      return Object.keys(params).length > 0 ? { id: p.id, params } : { id: p.id };
    }
  }
  return null;
}

// ---- 环境检查项（key + 句式；ok/失败两态各一条）----
interface CheckPattern extends Pattern {
  key: string;
}
export const CHECK_PATTERNS: readonly CheckPattern[] = [
  { key: 'server.version', re: /^lingedge-server (?<version>.+)$/, id: 'check.server_version' },
  { key: 'system.time', re: /^系统时间异常（(?<time>.+?)）：/, id: 'check.system_time_bad' },
  { key: 'system.time', re: /^系统时间 (?<time>.+)（UTC）$/, id: 'check.system_time_ok' },
  { key: 'data_dir.writable', re: /^数据目录不可写（(?<path>.+?)）：/, id: 'check.data_dir_bad' },
  { key: 'data_dir.writable', re: /^数据目录可写$/, id: 'check.data_dir_ok' },
  { key: 'master_key.readable', re: /^主密钥不可读：/, id: 'check.master_key_bad' },
  { key: 'master_key.readable', re: /^主密钥可读（不展示内容）$/, id: 'check.master_key_ok' },
  { key: 'migrations.embedded', re: /^内嵌迁移集合无效/, id: 'check.migrations_bad' },
  { key: 'migrations.embedded', re: /^内嵌迁移 (?<count>\d+) 个$/, id: 'check.migrations_ok' },
  { key: 'server.env', re: /^server\.env 未配置：/, id: 'check.server_env_bad' },
  { key: 'server.env', re: /^server\.env 已配置$/, id: 'check.server_env_ok' },
  { key: 'listen.loopback', re: /^向导仅监听回环地址 (?<addr>.+?)（/, id: 'check.listen_loopback' },
  { key: 'listen.loopback', re: /^向导监听非回环地址 (?<addr>.+?)：/, id: 'check.listen_exposed' },
  { key: 'setup_token.configured', re: /^setup token 哈希文件不存在（(?<path>.+?)）：/, id: 'check.token_bad' },
  { key: 'setup_token.configured', re: /^setup token 已配置$/, id: 'check.token_ok' },
];

export function localizeCheck(item: { key: string; message: string }): MsgPart | null {
  return firstMatch(CHECK_PATTERNS.filter((p) => p.key === item.key), item.message);
}

// ---- 步骤成功回执（step + 固定句式；可能由多个片段拼成）----
export const STEP_MESSAGE_STEPS = ['database', 'redis', 'init_db', 'system', 'keys', 'complete'] as const;

const R = {
  database: /^MySQL 预检通过；app(?<maint>\/maint)? 口令已加密写入运行期凭据文件（迁移口令仅内存）(?<warn>（管理员无法授予全局 SET_USER_ID，已交由触发器探针判定是否需要）)?(?<nomaint>。未配置 maint 账号：分区维护作业将明确跳过并告警)?$/,
  redis: /^Redis (?<version>.+?) 校验通过（主版本 ≥8）；凭据已加密写入运行期凭据文件$/,
  migrated: /^迁移完成：(?<from>\d+) → (?<to>\d+)，本次应用 (?<count>\d+) 个(?:：.*)?$/,
  upToDate: /^数据库已是最新版本 (?<version>\d+)（无待执行迁移）$/,
  system: /^系统基础配置已写入（运行模式 (?<mode>.+)）$/,
  keys: /^内部身份与密钥已初始化（私有 CA、配置签名密钥、Server gRPC 叶证书、deploy\.lineage）；CA 指纹 (?<fp>.+?)。请把主密钥 master\.key 与数据库备份分开保管（丢失主密钥将无法解密任何私钥，F-7）$/,
  complete:
    /^安装已完成(?<lock>；警告：install\.lock 写入失败，请检查数据目录权限，重启 Server 时会据数据库记录补写)?(?:(?<delfail>；警告：setup token 哈希文件删除失败，请手工删除 (?<p1>.+?))|(?<ro>；setup token 哈希文件所在目录不可写，已清空其内容使 token 永久失效，请手工删除 (?<p2>.+?)))?。向导接口此后永久返回 410；请以刚创建的管理员登录 Admin。(?<gen> 迁移账号口令由向导自动生成，仅在本响应展示一次，不会被保存：请立即在 root 侧执行 `sudo lingedge-server migrate credential init` 保存（预填 host\/port\/database，只需键入口令）；若来不及保存，之后用 `migrate credential reset --admin-user <管理员>` 重置。临时数据库管理员账号请自行删除。)?(?<manual> 升级前请在 root 侧执行 `sudo lingedge-server migrate credential init` 保存迁移账号口令（或之后用 credential reset）；临时数据库管理员账号请自行删除。)?$/,
} as const;

/** 步骤成功回执 → 片段列表；任何一处对不上就整体返回 null（回落原文）。 */
export function localizeStepMessage(step: string, message: string): MsgPart[] | null {
  const one = (re: RegExp, id: string): MsgPart[] | null => {
    const m = re.exec(message);
    if (!m) return null;
    const params = group(m);
    return [Object.keys(params).length > 0 ? { id, params } : { id }];
  };
  switch (step) {
    case 'database': {
      const m = R.database.exec(message);
      if (!m) return null;
      const parts: MsgPart[] = [{ id: 'step.database_ok', params: { accounts: m.groups?.maint ? 'app / maint' : 'app' } }];
      if (m.groups?.warn) parts.push({ id: 'step.database_setuserid' });
      if (m.groups?.nomaint) parts.push({ id: 'step.database_nomaint' });
      return parts;
    }
    case 'redis':
      return one(R.redis, 'step.redis_ok');
    case 'init_db':
      return one(R.migrated, 'step.init_migrated') ?? one(R.upToDate, 'step.init_uptodate');
    case 'system':
      return one(R.system, 'step.system_ok');
    case 'keys':
      return one(R.keys, 'step.keys_ok');
    case 'complete': {
      const m = R.complete.exec(message);
      if (!m) return null;
      const parts: MsgPart[] = [{ id: 'step.complete_ok' }];
      if (m.groups?.lock) parts.push({ id: 'step.complete_lock_failed' });
      if (m.groups?.delfail) parts.push({ id: 'step.complete_token_delete_failed', params: { path: m.groups.p1 ?? '' } });
      if (m.groups?.ro) parts.push({ id: 'step.complete_token_cleared', params: { path: m.groups.p2 ?? '' } });
      parts.push({ id: 'step.complete_closed' });
      if (m.groups?.gen) parts.push({ id: 'step.complete_migrate_generated' });
      if (m.groups?.manual) parts.push({ id: 'step.complete_migrate_manual' });
      return parts;
    }
    default:
      return null;
  }
}

// ---- 错误 details[].reason（固定句式；动态内容来自第三方库/驱动的原文无法映射，回落原文）----
export const REASON_PATTERNS: readonly Pattern[] = [
  { re: /^required$/, id: 'reason.required' },
  { re: /^is required$/, id: 'reason.required' },
  { re: /^must be at most 128 bytes without NUL or line breaks$/, id: 'reason.max128' },
  { re: /^must be at most 512 bytes without NUL or line breaks$/, id: 'reason.max512' },
  { re: /^must be at least 12 characters$/, id: 'reason.min12' },
  { re: /^must be (?<min>\d+)-(?<max>\d+) characters$/, id: 'reason.charRange' },
  { re: /^is mutually exclusive with host\/port: set either host\+port or socket$/, id: 'reason.socketExclusive' },
  { re: /^must be a normalized absolute path \(at most 100 bytes, no NUL or line breaks\)$/, id: 'reason.socketPath' },
  { re: /^must be a host name or IP \(or set socket instead\)$/, id: 'reason.host' },
  { re: /^(?:port )?must be between 1 and 65535$/, id: 'reason.port' },
  { re: /^must be a MySQL identifier \(letters, digits, _, \$; at most 64\)$/, id: 'reason.dbName' },
  { re: /^must be (?<name>lingedge_app) \(the migrations grant table privileges to this fixed account name\)$/, id: 'reason.fixedAccount' },
  { re: /^must be (?<name>lingedge_maint) \(fixed by the migrations\)$/, id: 'reason.fixedAccount' },
  { re: /^must be 1-32 characters of letters, digits, _, \. or -$/, id: 'reason.userName' },
  { re: /^must differ from the app and maint accounts$/, id: 'reason.migrateUserDiffers' },
  { re: /^must be false for a Unix socket connection \(it does not cross the network\)$/, id: 'reason.tlsSocket' },
  { re: /^must be true for a non-loopback database host \(credentials and data must not cross the network in clear text\)$/, id: 'reason.tlsRemote' },
  { re: /^the wizard only creates dedicated databases named lingedge or lingedge_\* /, id: 'reason.createDbName' },
  { re: /^required when create_if_missing is true$/, id: 'reason.adminUserRequired' },
  { re: /^must be <host>:<port>$/, id: 'reason.redisAddress' },
  { re: /^must be between 0 and 15$/, id: 'reason.redisDb' },
  { re: /^must be true when totp_code is provided$/, id: 'reason.totpFlag' },
  { re: /^required \(self_hosted or commercial\)$/, id: 'reason.runMode' },
  { re: /^the stored base_url is not a valid URL with a host name or IP; resubmit the system step$/, id: 'reason.baseUrlInvalid' },
  { re: /^submit the system step with base_url first: /, id: 'reason.baseUrlMissing' },
  { re: /^complete step (?<step>\S+) first$/, id: 'reason.stepFirst' },
  { re: /^complete the previous steps first$/, id: 'reason.previousSteps' },
  { re: /^the wizard process restarted since step database: /, id: 'reason.migrateGone' },
  { re: /^database "(?<name>[^"]*)" already contains (?<count>\d+) table\(s\); /, id: 'reason.dbNotEmpty' },
  { re: /^use an SSH tunnel to the loopback listener, or terminate TLS /, id: 'reason.insecureTransport' },
  { re: /^主密钥不可用/, id: 'reason.masterKey' },
  { re: /^must be a valid email address \(at most 255 characters\)$/, id: 'reason.email' },
  { re: /^must be zh-CN or en$/, id: 'reason.locale' },
  { re: /^must be a 6-digit code$/, id: 'reason.totpCode' },
  { re: /^already enabled$/, id: 'reason.totpEnabled' },
  { re: /^start enrollment first$/, id: 'reason.totpNotStarted' },
  { re: /^must not contain the local part of the email address$/, id: 'reason.pwEmailLocal' },
  { re: /^must not be the email address$/, id: 'reason.pwEmail' },
  { re: /^must differ from the current password$/, id: 'reason.pwSameAsCurrent' },
  { re: /^must not contain a common weak password pattern$/, id: 'reason.pwWeak' },
  { re: /^must contain at least 5 distinct characters$/, id: 'reason.pwFewUnique' },
  { re: /^must contain at least two character classes$/, id: 'reason.pwTwoClasses' },
  { re: /^must contain at least three of: lowercase, uppercase, digit, symbol$/, id: 'reason.pwThreeClasses' },
];

export function localizeReason(reason: string): MsgPart | null {
  return firstMatch(REASON_PATTERNS, reason);
}

/** 原文的语言标注：含 CJK 字符按 zh-CN，否则 en（服务端文案语言固定，不随界面语言变化）。 */
export function langOf(text: string): 'zh-CN' | 'en' {
  return /[㐀-鿿]/.test(text) ? 'zh-CN' : 'en';
}
