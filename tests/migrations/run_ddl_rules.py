#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""LingEdge DDL 规则回归：逐用例在全新空库中执行 ddl-rules.sql 的 CASE，比对期望（OK / ERR <code>）。

用法：python3 run_ddl_rules.py --host 127.0.0.1 --port 33399 --user root [--mysql /path/to/mysql]
要求：MySQL >= 8.0.16；连接账号可建库。每个用例使用库 le_ddl_rules（运行前后各自 DROP）。
仅依赖标准库；不连接任何生产库（务必指向独立测试实例）。
"""
import argparse
import re
import subprocess
import sys
import os

HERE = os.path.dirname(os.path.abspath(__file__))


def parse_cases(text):
    prelude = ''
    m = re.search(r'-- PRELUDE\n(.*?)-- END PRELUDE', text, re.S)
    if m:
        prelude = m.group(1)
    cases = []
    parts = re.split(r'^-- CASE (\S+) \| EXPECT (OK|ERR \d+)\s*$', text, flags=re.M)
    # parts[0] = 头部；之后每三个一组：id, expect, body
    for i in range(1, len(parts), 3):
        cases.append((parts[i], parts[i + 1], parts[i + 2]))
    return prelude, cases


def run_sql(args, sql, db=None):
    cmd = [args.mysql, '--no-defaults', '-h', args.host, '-P', str(args.port), '-u', args.user, '-f', '--batch', '--skip-column-names']
    if args.password:
        cmd.append('-p' + args.password)
    if db:
        cmd.append(db)
    p = subprocess.run(cmd, input=sql, capture_output=True, text=True)
    return p.stdout, p.stderr


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--host', default='127.0.0.1')
    ap.add_argument('--port', type=int, default=3306)
    ap.add_argument('--user', default='root')
    ap.add_argument('--password', default='')
    ap.add_argument('--mysql', default='mysql')
    ap.add_argument('--file', default=os.path.join(HERE, 'ddl-rules.sql'))
    args = ap.parse_args()

    out, err = run_sql(args, 'SELECT VERSION();')
    version = out.strip()
    if not version:
        print('无法连接 MySQL：' + err.strip())
        return 2
    print('MySQL 版本：' + version)

    with open(args.file, encoding='utf-8') as f:
        prelude, cases = parse_cases(f.read())

    failed = 0
    for cid, expect, body in cases:
        run_sql(args, 'DROP DATABASE IF EXISTS le_ddl_rules; CREATE DATABASE le_ddl_rules CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;')
        _, stderr = run_sql(args, prelude + '\n' + body, db='le_ddl_rules')
        errs = re.findall(r'ERROR (\d+)', stderr)
        got = 'OK' if not errs else 'ERR ' + errs[0]
        ok = (got == expect)
        print('%-34s 期望 %-9s 实际 %-9s %s' % (cid, expect, got, 'PASS' if ok else 'FAIL'))
        if not ok:
            failed += 1
            print('   stderr: ' + stderr.strip().replace('\n', '\n           '))
    run_sql(args, 'DROP DATABASE IF EXISTS le_ddl_rules;')
    print('合计 %d 个用例，失败 %d' % (len(cases), failed))
    return 1 if failed else 0


if __name__ == '__main__':
    sys.exit(main())
