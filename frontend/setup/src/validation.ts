// 向导表单的客户端校验与小工具（纯函数，有单元测试）。服务端校验才是权威；这里只负责即时、本地化的字段级反馈。

export const FIXED_APP_USER = 'lingedge_app';
export const FIXED_MAINT_USER = 'lingedge_maint';
export const DEFAULT_MIGRATE_USER = 'lingedge_migrate';

const RE_DB_NAME = /^[A-Za-z0-9_$]{1,64}$/;
const RE_EMAIL = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

export function isLoopbackHost(h: string): boolean {
  const x = h.trim().toLowerCase();
  return x === 'localhost' || x === '::1' || x === '[::1]' || /^127(?:\.\d{1,3}){3}$/.test(x);
}

export const isValidDbName = (n: string): boolean => RE_DB_NAME.test(n);
export const isValidEmail = (e: string): boolean => RE_EMAIL.test(e.trim());

/** host:port（IPv6 用方括号）。 */
export function parseHostPort(addr: string): { host: string; port: number } | null {
  const a = addr.trim();
  const m = /^(\[[^\]]+\]|[^:\s]+):(\d{1,5})$/.exec(a);
  if (!m) return null;
  const port = Number(m[2]);
  if (port < 1 || port > 65535) return null;
  return { host: m[1] ?? '', port };
}

/** http(s):// 开头的完整地址（不含用户信息、片段）。 */
export function isValidBaseUrl(v: string): boolean {
  try {
    const u = new URL(v.trim());
    return (u.protocol === 'http:' || u.protocol === 'https:') && u.hostname !== '' && u.username === '' && u.password === '' && u.hash === '';
  } catch {
    return false;
  }
}

/** 规整访问地址：去掉末尾的 /（保持与服务端存储一致，便于拼接 Admin 链接）。 */
export const normalizeBaseUrl = (v: string): string => v.trim().replace(/\/+$/, '');

/** IANA 时区名是否被当前运行环境识别。 */
export function isValidTimeZone(tz: string): boolean {
  if (tz.trim() === '') return false;
  try {
    new Intl.DateTimeFormat('en', { timeZone: tz });
    return true;
  } catch {
    return false;
  }
}

export function browserTimeZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
  } catch {
    return 'UTC';
  }
}

export function listTimeZones(): string[] {
  const f = (Intl as unknown as { supportedValuesOf?: (k: string) => string[] }).supportedValuesOf;
  try {
    return f ? f('timeZone') : [];
  } catch {
    return [];
  }
}

/**
 * 生成随机口令（建账号模式下留空的应用账号口令：只加密存于运行期凭据文件，无需人记）。
 * 用 crypto.getRandomValues + 拒绝采样，字母表 62 个字符，无取模偏差。
 */
export function randomPassword(length = 32, rng: (b: Uint8Array) => Uint8Array = (b) => crypto.getRandomValues(b)): string {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789';
  const limit = 256 - (256 % alphabet.length);
  let out = '';
  while (out.length < length) {
    for (const b of rng(new Uint8Array(length * 2))) {
      if (b < limit) out += alphabet[b % alphabet.length];
      if (out.length === length) break;
    }
  }
  return out;
}
