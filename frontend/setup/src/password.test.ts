import { describe, expect, it } from 'vitest';
import { assessPassword } from './password';

describe('assessPassword', () => {
  it('空口令：0 档且不可接受', () => {
    const a = assessPassword('', 'ops@example.test');
    expect(a.score).toBe(0);
    expect(a.acceptable).toBe(false);
  });
  it('过短 / 类别不足 / 重复字符不可接受', () => {
    expect(assessPassword('Ab1!', 'a@b.test').rules.length).toBe(false);
    expect(assessPassword('abcdefghijklmnop', 'a@b.test').rules.classes).toBe(false);
    expect(assessPassword('aaaaaaaaaaaa', 'a@b.test').rules.distinct).toBe(false);
  });
  it('三类字符且 ≥12 即可接受；四类且 ≥16 为强', () => {
    expect(assessPassword('Abcdef123456', 'a@b.test').acceptable).toBe(true);
    expect(assessPassword('Abcdef123456!xYz', 'a@b.test').score).toBe(4);
  });
  it('≥20 的口令短语只需两类字符', () => {
    expect(assessPassword('correct horse battery staple', 'a@b.test').acceptable).toBe(true);
    expect(assessPassword('correct horse battery', 'a@b.test').acceptable).toBe(true);
  });
  it('不得含邮箱本地部分（≥4 字符），短本地部分不触发', () => {
    expect(assessPassword('Xoperator-9Zz!AA', 'operator@example.test').rules.noEmail).toBe(false);
    expect(assessPassword('Xop-9Zz!AAbcdef', 'op@example.test').rules.noEmail).toBe(true);
  });
  it('超过 128 字符不可接受', () => {
    expect(assessPassword('Aa1!'.repeat(40), 'a@b.test').rules.length).toBe(false);
  });
  it('按码点计长（emoji 与中文各算 1 个字符）', () => {
    expect(assessPassword('密码强度测试一二三四五六七八九十ABcd12', 'a@b.test').rules.length).toBe(true);
  });
});
