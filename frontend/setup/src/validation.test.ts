import { describe, expect, it } from 'vitest';
import { isLoopbackHost, isValidBaseUrl, isValidDbName, isValidEmail, isValidTimeZone, normalizeBaseUrl, parseHostPort, randomPassword } from './validation';

describe('validation', () => {
  it('回环主机判定', () => {
    for (const h of ['localhost', '127.0.0.1', '127.1.2.3', '::1', '[::1]']) expect(isLoopbackHost(h)).toBe(true);
    for (const h of ['10.0.0.1', 'db.example.test', '128.0.0.1']) expect(isLoopbackHost(h)).toBe(false);
  });
  it('host:port 解析', () => {
    expect(parseHostPort('127.0.0.1:6379')).toEqual({ host: '127.0.0.1', port: 6379 });
    expect(parseHostPort('[::1]:6379')).toEqual({ host: '[::1]', port: 6379 });
    for (const bad of ['', 'host', 'host:0', 'host:70000', ':6379', 'a:b']) expect(parseHostPort(bad)).toBeNull();
  });
  it('访问地址：仅 http(s) 完整地址，不含用户信息与片段', () => {
    expect(isValidBaseUrl('https://admin.example.test')).toBe(true);
    expect(isValidBaseUrl('http://127.0.0.1:8080/')).toBe(true);
    for (const bad of ['admin.example.test', 'ftp://x.test', ['https://u', 'x@x.test'].join(':'), 'https://x.test/#a', '', 'javascript:alert(1)']) expect(isValidBaseUrl(bad)).toBe(false);
    expect(normalizeBaseUrl(' https://x.test/// ')).toBe('https://x.test');
  });
  it('库名与邮箱', () => {
    expect(isValidDbName('lingedge_w1')).toBe(true);
    for (const bad of ['', 'a-b', 'a b', 'x'.repeat(65), '库']) expect(isValidDbName(bad)).toBe(false);
    expect(isValidEmail('a@b.test')).toBe(true);
    expect(isValidEmail('a@b')).toBe(false);
  });
  it('时区', () => {
    expect(isValidTimeZone('Asia/Shanghai')).toBe(true);
    expect(isValidTimeZone('Not/AZone')).toBe(false);
    expect(isValidTimeZone('')).toBe(false);
  });
  it('随机口令：长度、字母表、两次不同；拒绝采样不产生越界字符', () => {
    const a = randomPassword(32);
    const b = randomPassword(32);
    expect(a).toHaveLength(32);
    expect(a).toMatch(/^[A-Za-z0-9]{32}$/);
    expect(a).not.toBe(b);
    // 注入的字节全是 255（≥ 248 的拒绝区）时先被丢弃，再取到合法字节
    let call = 0;
    const out = randomPassword(4, (buf) => buf.fill(call++ === 0 ? 255 : 3));
    expect(out).toBe('DDDD');
  });
});
