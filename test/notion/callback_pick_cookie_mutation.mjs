// 变异验证：把 pickCookie 换回「按字符集裁剪尾部」的旧实现，
// 断言 callback_pick_cookie.mjs 必须转红。用于证明该测试真的能抓到
// 那次把 `' --data-raw '{}` 吞进 Cookie 值的缺陷。
import { readFileSync, writeFileSync } from "node:fs";
import { execFileSync } from "node:child_process";

const path = ".tmp_pages/callback.html";
const original = readFileSync(path, "utf8");

const marker = "var raw=m[1];var out=";
const start = original.indexOf(marker);
if (start < 0) {
  console.error("未找到 pickCookie 函数体标记");
  process.exit(2);
}
// 变异体必须是合法 JS：只换取值实现，保留花括号，否则测到的是语法错误
// 而不是逻辑差异——那样「转红」并不能证明断言有效。
// 函数体里有 if(!m){return ""}，不能按第一个 } 截断，必须做括号配平。
const bodyStart = original.indexOf("{", start) + 1;
let depth = 1;
let bodyEnd = -1;
for (let i = bodyStart; i < original.length; i += 1) {
  const ch = original[i];
  if (ch === "{") depth += 1;
  if (ch === "}") {
    depth -= 1;
    if (depth === 0) {
      bodyEnd = i;
      break;
    }
  }
}
if (bodyStart <= 0 || bodyEnd < 0) {
  console.error("未找到 pickCookie 函数体边界");
  process.exit(2);
}
const oldBody = original.slice(bodyStart, bodyEnd);
const newBody = 'return m[1].replace(/^[\\s"\\\']+/,"").replace(/[\\s\\\\^"\\\',;]+$/,"")';
const mutated = original.slice(0, bodyStart) + newBody + original.slice(bodyEnd);

writeFileSync(path, mutated, "utf8");

let failedAsExpected = false;
try {
  execFileSync(process.execPath, ["test/notion/callback_pick_cookie.mjs", ".tmp_pages"], {
    stdio: "pipe",
    encoding: "utf8",
  });
} catch (e) {
  failedAsExpected = true;
  process.stdout.write(e.stdout || "");
  process.stderr.write(e.stderr || "");
}

writeFileSync(path, original, "utf8");

if (!failedAsExpected) {
  console.error("变异未被捕获：旧实现竟然通过了 pickCookie 测试");
  process.exit(1);
}
console.log("变异已被捕获（断言转红），源码已还原");
console.log("被替换的原函数体长度:", oldBody.length);
