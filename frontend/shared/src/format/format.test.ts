import { describe, expect, it } from 'vitest';
import { formatBandwidth, formatBytes, formatDateTime, formatMoney, formatRelativeTime, formatUtc } from './index';

describe('formatBytes（1024 进制）', () => {
  it.each([
    [0, 'B', '0 B'],
    [1023, 'B', '1,023 B'],
    [1024, 'KB', '1 KB'],
    [1536, 'KB', '1.5 KB'],
    [1024 ** 2, 'MB', '1 MB'],
    [1024 ** 3 * 2.5, 'GB', '2.5 GB'],
    [1024 ** 4, 'TB', '1 TB'],
  ])('%d -> %s', (n, _u, want) => {
    expect(formatBytes(n, { locale: 'en' })).toBe(want);
  });
  it('舍入进位：1048575 字节不得显示 1024 KB', () => {
    expect(formatBytes(1024 * 1024 - 1, { locale: 'en' })).toBe('1 MB');
  });
  it('bigint 与千分位随语言', () => {
    expect(formatBytes(1024n ** 2n, { locale: 'en' })).toBe('1 MB');
    expect(formatBytes(1500, { locale: 'de' })).toContain('KB');
  });
  it('1000 进制不是 1024：1000 字节仍显示 B', () => {
    expect(formatBytes(1000, { locale: 'en' })).toBe('1,000 B');
  });
  it.each([-1, Number.NaN, Number.POSITIVE_INFINITY])('非法输入 %s 抛 RangeError', (n) => {
    expect(() => formatBytes(n)).toThrow(RangeError);
  });
});

describe('formatBandwidth（bps 十进制）', () => {
  it.each([
    [0, '0 bps'],
    [999, '999 bps'],
    [1000, '1 Kbps'],
    [1_500_000, '1.5 Mbps'],
    [10_000_000_000, '10 Gbps'],
  ])('%d -> %s', (n, want) => {
    expect(formatBandwidth(n, { locale: 'en' })).toBe(want);
  });
  it('1024 不是 Kbps 边界', () => {
    expect(formatBandwidth(1024, { locale: 'en' })).toBe('1.02 Kbps');
  });
  it('非法输入抛 RangeError', () => {
    expect(() => formatBandwidth(-5)).toThrow(RangeError);
  });
});

describe('formatMoney（整数最小货币单位，无浮点）', () => {
  it('CNY 分 -> 元（两位小数）', () => {
    expect(formatMoney(123456, 'CNY', { locale: 'en' })).toBe('CN¥1,234.56');
    expect(formatMoney(5, 'USD', { locale: 'en' })).toBe('$0.05');
    expect(formatMoney(0, 'USD', { locale: 'en' })).toBe('$0.00');
  });
  it('JPY 无小数位（取自 Intl，不是写死的 100）', () => {
    expect(formatMoney(1234, 'JPY', { locale: 'en' })).toBe('¥1,234');
  });
  it('负数', () => {
    expect(formatMoney(-150, 'USD', { locale: 'en' })).toBe('-$1.50');
  });
  it('超过 2^53 的 bigint / 字符串不丢精度（浮点会错）', () => {
    expect(formatMoney(9007199254740993n, 'USD', { locale: 'en' })).toBe('$90,071,992,547,409.93');
    expect(formatMoney('9007199254740993', 'USD', { locale: 'en' })).toBe('$90,071,992,547,409.93');
  });
  it('拒绝小数/不安全整数/非法币种', () => {
    expect(() => formatMoney(1.5, 'USD')).toThrow(RangeError);
    expect(() => formatMoney(2 ** 53, 'USD')).toThrow(RangeError);
    expect(() => formatMoney('12.5', 'USD')).toThrow(RangeError);
    expect(() => formatMoney(1, 'usd')).toThrow(RangeError);
  });
});

describe('formatDateTime / formatUtc / formatRelativeTime', () => {
  const iso = '2026-11-22T07:59:12.345Z';
  it('指定时区显示', () => {
    expect(formatDateTime(iso, { locale: 'en', timeZone: 'UTC' })).toBe('11/22/2026, 07:59:12');
    expect(formatDateTime(iso, { locale: 'zh-CN', timeZone: 'Asia/Shanghai' })).toBe('2026/11/22 15:59:12');
    expect(formatDateTime(iso, { locale: 'zh-CN', timeZone: 'UTC', style: 'date' })).toBe('2026/11/22');
    expect(formatDateTime(iso, { locale: 'zh-CN', timeZone: 'UTC', style: 'time' })).toBe('07:59:12');
  });
  it('空值与无效值显示占位符而不是 Invalid Date', () => {
    expect(formatDateTime(null)).toBe('—');
    expect(formatDateTime('not-a-date')).toBe('—');
    expect(formatUtc(undefined)).toBe('—');
  });
  it('UTC 提示文本固定格式', () => {
    expect(formatUtc(iso)).toBe('2026-11-22 07:59:12 UTC');
  });
  it('相对时间', () => {
    const now = new Date('2026-11-22T08:00:00Z');
    expect(formatRelativeTime('2026-11-22T07:57:00Z', { locale: 'en', now })).toBe('3 minutes ago');
    expect(formatRelativeTime('2026-11-22T08:00:30Z', { locale: 'en', now })).toBe('in 30 seconds');
    expect(formatRelativeTime('2026-11-20T08:00:00Z', { locale: 'en', now })).toBe('2 days ago');
  });
});
