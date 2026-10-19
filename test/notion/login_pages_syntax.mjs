#!/usr/bin/env node
// 校验 notion 登录桥接页 / 回调页内联 JS 的真实语法。
//
// 输入是 Go 测试导出的真实渲染产物（不是源码里再解析一遍），
// 这样校验的就是用户浏览器真正会执行的那份脚本。
//
// 用法：node test/notion/login_pages_syntax.mjs .tmp_pages

import { readFileSync, writeFileSync, mkdtempSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const dirArg = process.argv[2] || '.tmp_pages';
const pages = ['bridge.html', 'callback.html'];
const dir = mkdtempSync(join(tmpdir(), 'notion-js-'));
let failed = 0;

for (const page of pages) {
  let html;
  try {
    html = readFileSync(join(dirArg, page), 'utf8');
  } catch (err) {
    console.error(`FAIL ${page}: 读不到导出产物（先用 NOTION_DUMP_DIR 跑导出测试）`);
    failed++;
    continue;
  }

  // 逐个 <script> 块校验；页面里还有一个 pre#authScript，它同样是可执行 JS。
  const blocks = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].map((m) => m[1]);
  const authMatch = html.match(/<pre id="authScript">([\s\S]*?)<\/pre>/);
  if (authMatch) {
    // pre 里是 HTML 转义后的文本，先还原实体再校验。
    blocks.push(
      authMatch[1]
        .replace(/&lt;/g, '<')
        .replace(/&gt;/g, '>')
        .replace(/&quot;/g, '"')
        .replace(/&#39;/g, "'")
        .replace(/&amp;/g, '&'),
    );
  }

  if (blocks.length === 0) {
    console.error(`FAIL ${page}: 未找到任何脚本块`);
    failed++;
    continue;
  }

  blocks.forEach((code, i) => {
    const file = join(dir, `${page}.${i}.js`);
    writeFileSync(file, code, 'utf8');
    try {
      execFileSync(process.execPath, ['--check', file], { stdio: 'pipe' });
      console.log(`OK ${page}#${i} (${code.length} chars)`);
    } catch (err) {
      failed++;
      console.error(`FAIL ${page}#${i}: ${(err.stderr || '').toString().split('\n').slice(0, 8).join('\n')}`);
    }
  });
}

if (failed > 0) {
  console.error(`${failed} 个脚本块存在语法问题`);
  process.exit(1);
}
console.log('all login page scripts parse');
