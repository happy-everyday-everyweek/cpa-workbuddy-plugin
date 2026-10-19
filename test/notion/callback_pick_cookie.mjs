// 校验回调页 pickCookie 的真实行为。
//
// 这段逻辑在 Go 里只是字符串拼接，编译期完全看不出问题；而它恰好出过一次
// 真实缺陷：按「字符集」裁剪尾部会把 `' --data-raw '{}` 一并吞进 Cookie 值，
// 导致提交的凭据被污染。因此从 Go 测试导出的真实页面里抽出该函数，用 Node
// 直接执行断言，而不是只检查源码里出现过这个名字。
//
// 用法：node test/notion/callback_pick_cookie.mjs .tmp_pages

import { readFileSync } from "node:fs";
import { join } from "node:path";
import vm from "node:vm";

const dir = process.argv[2] || ".tmp_pages";
const html = readFileSync(join(dir, "callback.html"), "utf8");

const scriptMatch = html.match(/<script>([\s\S]*?)<\/script>/);
if (!scriptMatch) {
  throw new Error("callback.html 里没有找到内联脚本");
}

// 只取 pickCookie 的定义：整段脚本依赖 DOM，不能整体执行。
// Go 侧是逐段字符串拼接，函数体落在同一行，因此不能按 `\n}` 收尾。
const start = scriptMatch[1].indexOf("function pickCookie");
if (start < 0) {
  throw new Error("脚本里没有 pickCookie 定义");
}
const rest = scriptMatch[1].slice(start);
const nextFn = rest.indexOf("function ", "function pickCookie".length);
const fnSource = nextFn > 0 ? rest.slice(0, nextFn) : rest;

// 在独立 vm 上下文里执行：只暴露该函数自身需要的内建对象，
// 不接触 process / require / 文件系统，避免被测脚本触达宿主能力。
const sandbox = Object.create(null);
const pickCookie = vm.runInNewContext(`${fnSource}; pickCookie;`, sandbox, {
  timeout: 1000,
});

const cases = [
  {
    name: "bash 单引号 + --data-raw",
    input: "curl 'https://www.notion.so/api/v3/getSpaces' -H 'cookie: token_v2=v03%3Aabc; notion_user_id=u-1' --data-raw '{}'",
    want: "token_v2=v03%3Aabc; notion_user_id=u-1",
  },
  {
    name: "cmd 双引号 + ^ 续行残留",
    input: 'curl "https://www.notion.so/api/v3/getSpaces" -H "cookie: token_v2=v03%3Aabc; notion_user_id=u-1"',
    want: "token_v2=v03%3Aabc; notion_user_id=u-1",
  },
  {
    name: "Cookie 值以分号结尾",
    input: "cookie: token_v2=v03%3Aabc; notion_user_id=u-1;",
    want: "token_v2=v03%3Aabc; notion_user_id=u-1",
  },
  {
    name: "纯 Cookie 行（无引号）",
    input: "cookie: token_v2=v03%3Aabc; notion_user_id=u-1",
    want: "token_v2=v03%3Aabc; notion_user_id=u-1",
  },
];

let failed = 0;
for (const c of cases) {
  const got = pickCookie(c.input);
  if (got !== c.want) {
    failed += 1;
    console.error(`FAIL ${c.name}\n  期望: ${JSON.stringify(c.want)}\n  实际: ${JSON.stringify(got)}`);
  } else {
    console.log(`OK   ${c.name}`);
  }
}

// 反向断言：结果里绝不能残留 cURL 参数，否则等于把整段请求当 Cookie 发出去。
const polluted = pickCookie(cases[0].input);
for (const bad of ["--data-raw", "{}", "curl "]) {
  if (polluted.includes(bad)) {
    failed += 1;
    console.error(`FAIL 结果残留 ${bad}: ${JSON.stringify(polluted)}`);
  }
}

if (failed > 0) {
  process.exit(1);
}
console.log("all pickCookie cases passed");
