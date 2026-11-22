// 格式化：日期时间 / 字节（1024 进制）/ 带宽（bps 十进制）/ 金额（整数最小货币单位）。
// 全部基于 Intl，按语言环境格式化；不做任何浮点金额运算（基线 §16、EXPERIENCE.md）。
import type { LeLocale } from '../i18n';

const PLACEHOLDER = '—';

export interface LocaleOption {
  locale?: LeLocale | string;
}

const loc = (o?: LocaleOption) => o?.locale ?? 'zh-CN';

// ---------- 日期时间 ----------

export type DateInput = string | number | Date | null | undefined;

function toDate(v: DateInput): Date | null {
  if (v === null || v === undefined || v === '') return null;
  const d = v instanceof Date ? v : new Date(v);
  return Number.isNaN(d.getTime()) ? null : d;
}

export interface DateTimeOptions extends LocaleOption {
  /** 缺省为浏览器时区（用户时区）；传 'UTC' 得到 UTC 显示。 */
  timeZone?: string;
  style?: 'datetime' | 'date' | 'time';
}

/** API 时间为 RFC 3339 UTC；显示为用户时区。空值/无效值显示占位符“—”。 */
export function formatDateTime(value: DateInput, options: DateTimeOptions = {}): string {
  const d = toDate(value);
  if (!d) return PLACEHOLDER;
  const style = options.style ?? 'datetime';
  const fmt: Intl.DateTimeFormatOptions = { timeZone: options.timeZone, hourCycle: 'h23' };
  if (style !== 'time') Object.assign(fmt, { year: 'numeric', month: '2-digit', day: '2-digit' });
  if (style !== 'date') Object.assign(fmt, { hour: '2-digit', minute: '2-digit', second: '2-digit' });
  return new Intl.DateTimeFormat(loc(options), fmt).format(d);
}

/** hover 提示用：固定 UTC、与语言无关，形如 2026-11-22 07:59:12 UTC。 */
export function formatUtc(value: DateInput): string {
  const d = toDate(value);
  if (!d) return PLACEHOLDER;
  return `${d.toISOString().slice(0, 19).replace('T', ' ')} UTC`;
}

/** 相对时间（“3 分钟前”）。now 可注入以便测试。 */
export function formatRelativeTime(value: DateInput, options: LocaleOption & { now?: Date | number } = {}): string {
  const d = toDate(value);
  if (!d) return PLACEHOLDER;
  const now = options.now instanceof Date ? options.now.getTime() : (options.now ?? Date.now());
  const diffSec = Math.round((d.getTime() - now) / 1000);
  const abs = Math.abs(diffSec);
  const rtf = new Intl.RelativeTimeFormat(loc(options), { numeric: 'auto' });
  if (abs < 60) return rtf.format(diffSec, 'second');
  if (abs < 3600) return rtf.format(Math.round(diffSec / 60), 'minute');
  if (abs < 86400) return rtf.format(Math.round(diffSec / 3600), 'hour');
  return rtf.format(Math.round(diffSec / 86400), 'day');
}

// ---------- 字节 / 带宽 ----------

function scale(value: number, base: number, units: readonly string[], fractionDigits: number) {
  let v = value;
  let i = 0;
  while (v >= base && i < units.length - 1) {
    v /= base;
    i++;
  }
  // 舍入后可能恰好等于 base（如 1023.999 KB → 1024.00 KB），再进一位。
  const f = 10 ** fractionDigits;
  if (Math.round(v * f) / f >= base && i < units.length - 1) {
    v /= base;
    i++;
  }
  return { v, unit: units[i] as string };
}

function assertFiniteNonNegative(name: string, n: number | bigint): number {
  const x = typeof n === 'bigint' ? Number(n) : n;
  if (!Number.isFinite(x) || x < 0) throw new RangeError(`${name} must be a non-negative finite number: ${String(n)}`);
  return x;
}

const BYTE_UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB', 'EB'] as const;

export interface ScaledOptions extends LocaleOption {
  fractionDigits?: number;
}

/** 流量/容量：1024 进制，标注 KB/MB/GB…（UX-26；计费口径 P5 可覆盖，导出表头需注明进制）。 */
export function formatBytes(bytes: number | bigint, options: ScaledOptions = {}): string {
  const x = assertFiniteNonNegative('bytes', bytes);
  const digits = options.fractionDigits ?? 2;
  const { v, unit } = scale(x, 1024, BYTE_UNITS, digits);
  const nf = new Intl.NumberFormat(loc(options), { maximumFractionDigits: unit === 'B' ? 0 : digits, minimumFractionDigits: 0 });
  return `${nf.format(v)} ${unit}`;
}

const BPS_UNITS = ['bps', 'Kbps', 'Mbps', 'Gbps', 'Tbps', 'Pbps'] as const;

/** 带宽：bps 十进制（1000），Kbps/Mbps/Gbps。入参单位为 bit/s。 */
export function formatBandwidth(bitsPerSecond: number | bigint, options: ScaledOptions = {}): string {
  const x = assertFiniteNonNegative('bitsPerSecond', bitsPerSecond);
  const digits = options.fractionDigits ?? 2;
  const { v, unit } = scale(x, 1000, BPS_UNITS, digits);
  const nf = new Intl.NumberFormat(loc(options), { maximumFractionDigits: unit === 'bps' ? 0 : digits, minimumFractionDigits: 0 });
  return `${nf.format(v)} ${unit}`;
}

// ---------- 金额 ----------

export type MinorUnits = number | bigint | string;

function toBigIntStrict(v: MinorUnits): bigint {
  if (typeof v === 'bigint') return v;
  if (typeof v === 'number') {
    if (!Number.isSafeInteger(v)) throw new RangeError(`amount must be a safe integer in minor units: ${v}; pass a bigint or decimal integer string for larger values`);
    return BigInt(v);
  }
  if (!/^-?\d+$/.test(v)) throw new RangeError(`amount string must be a decimal integer: ${v}`);
  return BigInt(v);
}

/**
 * 金额：入参是整数最小货币单位（如 CNY 的“分”，对应 OpenAPI 的 amount_minor）+ ISO 4217 币种代码。
 * 用 BigInt 拆整数/小数部分得到精确十进制字符串，再交给 Intl.NumberFormat（精确字符串输入），全程无浮点。
 * 币种的最小单位位数取自 Intl（CNY/USD=2、JPY=0 …）。
 */
export function formatMoney(amountMinor: MinorUnits, currency: string, options: LocaleOption = {}): string {
  if (!/^[A-Z]{3}$/.test(currency)) throw new RangeError(`currency must be an upper-case ISO 4217 code: ${currency}`);
  const minor = toBigIntStrict(amountMinor);
  const nf = new Intl.NumberFormat(loc(options), { style: 'currency', currency });
  const exp = nf.resolvedOptions().maximumFractionDigits ?? 2;
  const neg = minor < 0n;
  const abs = neg ? -minor : minor;
  const unit = 10n ** BigInt(exp);
  const intPart = abs / unit;
  const frac = (abs % unit).toString().padStart(exp, '0');
  const decimal = `${neg ? '-' : ''}${intPart}${exp > 0 ? '.' + frac : ''}`;
  // format 接受精确十进制字符串（Intl.NumberFormat v3）；类型声明里是 number | bigint，故窄口转换。
  return nf.format(decimal as unknown as number);
}
