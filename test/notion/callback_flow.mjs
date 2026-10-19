// 回调页端到端行为验证（不依赖浏览器）。
//
// 浏览器自动化通道在本机不稳定（每次 evaluate 可能落到不同页面上下文），
// 导致观测互相矛盾。这里改用最小 DOM 桩，在 Node 里执行 Go 导出的真实脚本，
// 断言「自动读取剪贴板 → 提交 → 页面给出成功/失败终态」这条主链。
//
// 用法：node test/notion/callback_flow.mjs .tmp_pages

import { readFileSync } from "node:fs";
import { join } from "node:path";
import vm from "node:vm";

const dir = process.argv[2] || ".tmp_pages";
const html = readFileSync(join(dir, "callback.html"), "utf8");
const scriptMatch = html.match(/<script>([\s\S]*?)<\/script>/);
if (!scriptMatch) {
  throw new Error("callback.html 里没有内联脚本");
}
const source = scriptMatch[1];

// 最小 DOM 桩：只实现脚本真正用到的能力。
function createElement(id, tag = "DIV") {
  return {
    id,
    tagName: tag,
    textContent: "",
    className: "",
    value: "",
    disabled: false,
    listeners: {},
    addEventListener(type, fn) {
      this.listeners[type] = fn;
    },
    click() {
      if (!this.disabled && this.listeners.click) {
        this.listeners.click.call(this);
      }
    },
  };
}

function runCase({ name, clipboard, serverReply, expect }) {
  const elements = {
    status: createElement("status"),
    result: createElement("result"),
    retry: createElement("retry", "BUTTON"),
    curl: createElement("curl", "TEXTAREA"),
    submit: createElement("submit", "BUTTON"),
  };
  // 初始文案与真实页面一致，便于断言「等待 → 终态」的迁移。
  elements.status.textContent = "等待你从 Notion 复制 Copy as cURL；复制后切回本页会自动完成。";

  const body = {
    getAttribute(key) {
      if (key === "data-provider") return "notion-provider";
      if (key === "data-state") return "teststate";
      return null;
    },
  };

  const fetchCalls = [];
  const context = {
    document: {
      body,
      getElementById: (id) => elements[id] || null,
      addEventListener() {},
      hasFocus: () => true,
      hidden: false,
    },
    window: { addEventListener() {} },
    location: { hash: "", pathname: "/login/callback", search: "?state=teststate", href: "http://x/login/callback?state=teststate" },
    history: { replaceState() {} },
    navigator: {
      clipboard: clipboard
        ? { readText: async () => clipboard }
        : undefined,
    },
    URLSearchParams,
    TextEncoder,
    btoa,
    setTimeout,
    console,
    fetch: async (url, opts) => {
      fetchCalls.push({ url: String(url), headers: opts && opts.headers });
      return {
        status: 200,
        json: async () => serverReply,
      };
    },
  };

  vm.runInNewContext(source, context, { timeout: 3000 });

  return (async () => {
    // 触发自动路径：模拟用户点击「我已复制，立即授权」。
    elements.retry.click();
    await new Promise((r) => setTimeout(r, 50));

    // 第二道保险：tryClipboard 只拦截一次，submit 自己也要拦。
    // 直接把无关文本塞进手动粘贴框再点提交，验证 submit 内部的守卫独立生效。
    // 注意：上面 retry 已经跑过，状态已变成「不像是授权信息」；这里只看 fetch 是否新增。
    if (expect.checkSubmitGuard) {
      elements.curl.value = "随便一段别的文本，不该被当成凭据提交出去";
      const before = fetchCalls.length;
      elements.submit.click();
      await new Promise((r) => setTimeout(r, 50));
      if (fetchCalls.length !== before) {
        return {
          name,
          problems: ["submit 未拦截无关文本，仍然发起了提交"],
          statusText: elements.status.textContent,
          resultText: elements.result.textContent,
        };
      }
      return {
        name,
        problems: [],
        statusText: elements.status.textContent,
        resultText: elements.result.textContent,
      };
    }

    const statusText = elements.status.textContent;
    const statusClass = elements.status.className;
    const resultText = elements.result.textContent;

    const problems = [];
    if (fetchCalls.length !== expect.fetchCount) {
      problems.push(`fetch 调用次数应为 ${expect.fetchCount}，实际 ${fetchCalls.length}`);
    }
    if (expect.statusIncludes && !statusText.includes(expect.statusIncludes)) {
      problems.push(`状态应包含「${expect.statusIncludes}」，实际「${statusText}」`);
    }
    if (expect.statusClass && statusClass !== expect.statusClass) {
      problems.push(`状态样式应为 ${expect.statusClass}，实际 ${statusClass}`);
    }
    if (expect.resultIncludes && !resultText.includes(expect.resultIncludes)) {
      problems.push(`结果应包含「${expect.resultIncludes}」，实际「${resultText}」`);
    }
    // 提交内容必须是纯 Cookie，不能带 cURL 参数。
    if (fetchCalls.length > 0 && expect.payloadClean) {
      const b64 = fetchCalls[0].headers && fetchCalls[0].headers["X-Notion-Auth-Input"];
      const decoded = b64 ? Buffer.from(b64, "base64").toString("utf8") : "";
      for (const bad of ["curl ", "--data-raw", "{}"]) {
        if (decoded.includes(bad)) {
          problems.push(`提交内容残留 ${bad}: ${decoded}`);
        }
      }
      if (expect.payloadIncludes && !decoded.includes(expect.payloadIncludes)) {
        problems.push(`提交内容应包含「${expect.payloadIncludes}」，实际「${decoded}」`);
      }
    }
    return { name, problems, statusText, resultText };
  })();
}

const curlInput =
  "curl 'https://www.notion.so/api/v3/getSpaces' -H 'cookie: token_v2=v03%3Axyz; notion_user_id=u-9' --data-raw '{}'";

const cases = [
  () =>
    runCase({
      name: "剪贴板是 cURL，服务端成功",
      clipboard: curlInput,
      serverReply: { ok: true, labels: ["验证账号 · 订阅"], auth_ids: ["notion-x"] },
      expect: {
        fetchCount: 1,
        statusIncludes: "授权成功",
        statusClass: "ok",
        payloadClean: true,
        payloadIncludes: "token_v2=v03%3Axyz",
      },
    }),
  () =>
    runCase({
      name: "剪贴板是 cURL，服务端失败",
      clipboard: curlInput,
      serverReply: { ok: false, error: "Cookie 缺少 token_v2" },
      expect: {
        fetchCount: 1,
        statusIncludes: "授权失败",
        statusClass: "err",
        resultIncludes: "授权失败",
      },
    }),
  () =>
    runCase({
      name: "剪贴板无关内容不提交",
      clipboard: "随便一段别的文本，不该被当成凭据提交出去",
      serverReply: { ok: true },
      expect: {
        fetchCount: 0,
        statusIncludes: "等待",
        checkSubmitGuard: true,
      },
    }),
];

let failed = 0;
for (const run of cases) {
  const r = await run();
  if (r.problems.length) {
    failed += 1;
    console.error(`FAIL ${r.name}`);
    for (const p of r.problems) console.error(`   - ${p}`);
  } else {
    console.log(`OK   ${r.name}  →  status=${JSON.stringify(r.statusText)}`);
  }
}

if (failed > 0) {
  process.exit(1);
}
console.log("all callback flow cases passed");
