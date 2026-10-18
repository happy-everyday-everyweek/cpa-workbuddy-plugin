package main

// 本文件负责把上游 Set-Cookie 滚动写回账号文件。
//
// Notion 会在响应里偶尔刷新 token_v2 等会话 Cookie。若不落盘，进程重启
// 或热重载后仍用旧值，账号会在用户毫无操作的情况下失效。
//
// 为什么不用 cookiejar：本插件的鉴权是手工拼 Cookie header（与浏览器端
// 一致），全部请求都带同一份 header。引入 jar 反而要额外把 jar 的内容
// 再拼回 header，绕一圈没有收益。这里只做「按名覆盖 + 原子落盘」。
//
// 防覆盖：写盘前比对文件内容摘要。用户在面板重新导入 Cookie 后，磁盘内容
// 已变，此时滚动写回必须放弃，否则旧会话 Cookie 会盖掉刚导入的新凭据。

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// cookieRollMu 串行化同一进程内的滚动写回。
//
// 滚动不频繁，用一把全局锁即可；按账号分锁只会增加状态与泄漏面。
var cookieRollMu sync.Mutex

// rollingCookieNames 是需要写回的 Cookie 名。
//
// 只回写身份相关字段：上游还会下发大量统计/实验类 Cookie，写回它们
// 既无意义，也会让账号文件频繁变更、触发宿主反复重载。
var rollingCookieNames = map[string]bool{
	"token_v2":          true,
	"notion_user_id":    true,
	"notion_browser_id": true,
	"device_id":         true,
}

// applySetCookies 捕获响应里的滚动 Cookie 并写回账号文件。
//
// 失败不阻断本轮推理：Cookie 滚动是增强能力，写盘失败最多让下次重启
// 回到旧会话，不应因此让用户拿不到回复。
// [参数] auth: 当前账号；resp: 上游响应。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，TASK-007 Cookie 滚动。
func applySetCookies(auth *StoredAuth, resp *http.Response) {
	if auth == nil || resp == nil {
		return
	}

	updates := map[string]string{}
	for _, cookie := range resp.Cookies() {
		if !rollingCookieNames[cookie.Name] {
			continue
		}
		if cookie.MaxAge < 0 || cookie.Value == "" {
			// MaxAge<0 表示上游要求删除该 Cookie，不能当成新值写入。
			continue
		}
		updates[cookie.Name] = cookie.Value
	}
	if len(updates) == 0 {
		return
	}

	cookieRollMu.Lock()
	defer cookieRollMu.Unlock()

	// 以磁盘为准重新读一次：内存里的 auth 可能是宿主缓存，用户刚重新
	// 导入过的话这里能立刻发现。
	current := auth
	if auth.PhysicalPath != "" {
		if loaded, errRead := readAuthFile(auth.PhysicalPath); errRead == nil {
			current = loaded
		}
	}
	if auth.LoadedDigest != "" && current.LoadedDigest != "" && auth.LoadedDigest != current.LoadedDigest {
		// 磁盘内容已被外部改写（典型是用户重新导入），放弃本次滚动。
		return
	}

	merged, changed := mergeCookieHeader(current.CookieHeader, updates)
	if !changed {
		return
	}
	// 拆分后同一会话会有多个空间账号文件，滚动前先记下旧 token，
	// 写盘成功后用它识别哪些兄弟文件还是同一份旧凭据。
	oldToken := current.TokenV2
	current.CookieHeader = merged
	if v, ok := updates["token_v2"]; ok {
		current.TokenV2 = v
	}
	if v, ok := updates["notion_user_id"]; ok {
		current.UserID = v
	}
	if v, ok := updates["notion_browser_id"]; ok {
		current.BrowserID = v
	}
	if v, ok := updates["device_id"]; ok {
		current.DeviceID = v
	}
	current.UpdatedAt = time.Now().Format(time.RFC3339)

	path := firstNonEmpty(current.PhysicalPath, auth.PhysicalPath)
	if path == "" {
		return
	}
	_ = writeAuthFileDirect(path, current)
	// 把最新凭据回写给调用方，后续轮次直接使用新 Cookie。
	auth.CookieHeader = current.CookieHeader
	auth.TokenV2 = current.TokenV2
	auth.UserID = current.UserID
	auth.BrowserID = current.BrowserID
	auth.DeviceID = current.DeviceID
	auth.UpdatedAt = current.UpdatedAt
	auth.LoadedDigest = current.LoadedDigest

	// 同一账号已按空间拆成多个文件，它们共用一份 Cookie。只更新当前文件
	// 会让其它空间副本停留在旧 token，那次请求之后就会 401。
	propagateCookieToSiblings(current, auth.PhysicalPath, oldToken, updates)
}

// propagateCookieToSiblings 把滚动后的 Cookie 同步到同一账号的其他空间账号。
//
// 识别口径是「同 UserID 且 TokenV2 仍等于滚动前的旧值」：满足条件的文件
// 一定还是同一份旧会话，可以安全覆盖；已经单独导入过新凭据的文件 token
// 不同，会被跳过，不会盖掉用户刚导入的凭据。
// [参数] source: 已更新的账号；skipPath: 已写盘的路径；oldToken: 滚动前的 token；updates: 本轮新值。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，拆分后多副本凭据同步。
func propagateCookieToSiblings(source *StoredAuth, skipPath, oldToken string, updates map[string]string) {
	if source == nil || source.UserID == "" {
		return
	}
	accounts, errList := listAllAuthFiles()
	if errList != nil {
		return
	}
	for _, sibling := range accounts {
		if sibling.PhysicalPath == "" || sibling.PhysicalPath == skipPath {
			continue
		}
		if sibling.UserID != source.UserID || sibling.TokenV2 != oldToken {
			continue
		}
		merged, changed := mergeCookieHeader(sibling.CookieHeader, updates)
		if !changed {
			continue
		}
		sibling.CookieHeader = merged
		if v, ok := updates["token_v2"]; ok {
			sibling.TokenV2 = v
		}
		if v, ok := updates["notion_user_id"]; ok {
			sibling.UserID = v
		}
		if v, ok := updates["notion_browser_id"]; ok {
			sibling.BrowserID = v
		}
		if v, ok := updates["device_id"]; ok {
			sibling.DeviceID = v
		}
		sibling.UpdatedAt = source.UpdatedAt
		_ = writeAuthFileDirect(sibling.PhysicalPath, sibling)
	}
}

// mergeCookieHeader 把新值合并进单行 Cookie header。
//
// 保留原有顺序：浏览器习惯与上游日志对顺序敏感的场景极少，但保持稳定
// 能让 diff 更容易看懂。新出现的键追加到末尾。
// [参数] header: 现有 Cookie header；updates: 待合并的键值。
// [返回] 合并结果与是否发生变化。
// 最近修改时间：2026-10-18；改动原因：新增，Cookie 合并。
func mergeCookieHeader(header string, updates map[string]string) (string, bool) {
	if len(updates) == 0 {
		return header, false
	}

	parts := strings.Split(header, ";")
	seen := map[string]bool{}
	changed := false
	out := make([]string, 0, len(parts)+len(updates))

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, currentValue, ok := splitCookiePair(part)
		if !ok {
			out = append(out, part)
			continue
		}
		newValue, hasUpdate := updates[key]
		if !hasUpdate {
			out = append(out, part)
			continue
		}
		seen[key] = true
		if currentValue == newValue {
			out = append(out, part)
			continue
		}
		out = append(out, key+"="+newValue)
		changed = true
	}

	for key, value := range updates {
		if seen[key] {
			continue
		}
		out = append(out, key+"="+value)
		changed = true
	}
	return strings.Join(out, "; "), changed
}
