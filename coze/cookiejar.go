package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// 本文件实现可持久化的 Cookie 容器与会话有效性判定。
//
// 为什么不用标准 cookiejar 直接落盘：标准库只在内存里维护 Cookie，
// 进程重启或插件热重载后全部丢失，凭据无法随上游续期。
//
// 为什么键要用 (name, domain, path) 三元组：扣子的 token beat 实测下发 13 条
// Set-Cookie，其中 uid_tt / uid_tt_ss / session_tlb_tag 各出现两次（对应不同
// domain 与 path）。按 name 单键存储会让后一条覆盖前一条，直接丢凭据
// （2026-10-17 端到端验证实证）。

// cozeSessionCookieNames 是判定会话是否仍在的主 Cookie 名。
//
// 与 token beat 实际下发的 13 条对照：这 4 条是会话本体，
// 其余（odin_tt / uid_tt / ttwid 等）是设备与追踪标识。
var cozeSessionCookieNames = []string{"sessionid", "sessionid_ss", "sid_tt", "sid_guard"}

// savedCookie 是一条被持久化的 Cookie。
type savedCookie struct {
	// Source 记录下发该 Cookie 的 URL，重放时用于重建 Domain / Path 归属。
	Source string      `json:"source"`
	Cookie http.Cookie `json:"cookie"`
}

// cookieStore 是可持久化的 Cookie 容器。
//
// base 委托标准 jar 做常规读写；records 额外按三元组记录，供快照与重放。
// mu 保护 records 与 base——推理请求可能并发触发 SetCookies，无锁会 map 竞态。
type cookieStore struct {
	base    http.CookieJar
	mu      sync.Mutex
	records map[string]savedCookie
}

// newCookieStore 创建一个空的 Cookie 容器。
// [参数] 无。
// [返回] 已初始化 records 的容器。
// 最近修改时间：2026-10-18；改动原因：新增，容器构造入口。
func newCookieStore() *cookieStore {
	base, _ := cookiejar.New(nil)
	return &cookieStore{base: base, records: map[string]savedCookie{}}
}

// cookieKey 生成 (name, domain, path) 三元组键。
// [参数] name: Cookie 名；domain: 归属域；path: 归属路径。
// [返回] 归一化后的键。
// 最近修改时间：2026-10-18；改动原因：新增，同名 Cookie 分域分路径存储。
func cookieKey(name, domain, path string) string {
	domain = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
	path = strings.TrimSpace(path)
	if path == "" {
		path = "/"
	}
	return domain + "|" + path + "|" + name
}

// Cookies 返回对指定 URL 生效的 Cookie（委托标准 jar）。
// [参数] u: 目标 URL。
// [返回] 生效的 Cookie 列表。
// 最近修改时间：2026-10-18；改动原因：新增，实现 http.CookieJar 接口。
func (s *cookieStore) Cookies(u *url.URL) []*http.Cookie {
	return s.base.Cookies(u)
}

// SetCookies 接收上游下发的 Cookie 并记录快照。
//
// 归一化处理与原因：
//   - Path 为空时补 "/"，否则重放时无法命中目标路径；
//   - MaxAge > 0 折算成绝对 Expires，避免重放后相对时长再次起算；
//   - MaxAge < 0 或已过期时按三元组删除（上游以此表达登出/失效）。
//
// [参数] u: 下发 Cookie 的 URL；cookies: 本次下发的 Cookie 列表。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，Set-Cookie 捕获与快照记录。
func (s *cookieStore) SetCookies(u *url.URL, cookies []*http.Cookie) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.base.SetCookies(u, cookies)
	for _, c := range cookies {
		saved := *c
		if saved.Path == "" {
			saved.Path = "/"
		}
		if saved.MaxAge > 0 {
			saved.Expires = time.Now().Add(time.Duration(saved.MaxAge) * time.Second)
			saved.MaxAge = 0
		}
		domain := saved.Domain
		if domain == "" {
			domain = u.Hostname()
		}
		key := cookieKey(saved.Name, domain, saved.Path)
		if saved.MaxAge < 0 || (!saved.Expires.IsZero() && saved.Expires.Before(time.Now())) {
			delete(s.records, key)
			continue
		}
		s.records[key] = savedCookie{Source: u.Scheme + "://" + u.Host + "/", Cookie: saved}
	}
}

// snapshot 返回当前记录的快照副本。
// [参数] 无。
// [返回] Cookie 快照列表（顺序不保证）。
// 最近修改时间：2026-10-18；改动原因：新增，快照导出。
func (s *cookieStore) snapshot() []savedCookie {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]savedCookie, 0, len(s.records))
	for _, rec := range s.records {
		out = append(out, rec)
	}
	return out
}

// restore 用已落盘的快照重建容器状态（账号加载时调用）。
// [参数] records: 待重放的 Cookie 快照。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，快照重放。
func (s *cookieStore) restore(records []savedCookie) {
	for _, rec := range records {
		u, errParse := url.Parse(rec.Source)
		if errParse != nil || u.Host == "" {
			continue
		}
		c := rec.Cookie
		s.SetCookies(u, []*http.Cookie{&c})
	}
}

// seedStoreFromHeader 用 Cookie 头字符串播种容器。
//
// 首次授权后立即落盘的账号可能还没有带属性的快照，此时只能从 header
// 重建：header 里没有 domain/path 信息，统一按源站根路径归属——
// 这与 token beat 实际下发的归属一致（全部指向 www.coze.cn）。
// [参数] header: 完整 Cookie 头；source: 归属 URL。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，从 header 重建容器。
func seedStoreFromHeader(s *cookieStore, header, source string) {
	u, errParse := url.Parse(source)
	if errParse != nil || u.Host == "" {
		return
	}
	var cookies []*http.Cookie
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.Index(part, "=")
		if eq <= 0 {
			continue
		}
		cookies = append(cookies, &http.Cookie{
			Name:  strings.TrimSpace(part[:eq]),
			Value: strings.TrimSpace(part[eq+1:]),
			Path:  "/",
		})
	}
	if len(cookies) > 0 {
		s.SetCookies(u, cookies)
	}
}

// header 拼装可直接放进 Cookie 请求头的字符串。
//
// 按 (name, domain, path) 去重后拼接：同名不同路径的 Cookie 都会带上，
// 由上游按 path 自行选取，这与浏览器的实际行为一致。
// [参数] 无。
// [返回] 形如 "a=1; b=2" 的 Cookie 头；无记录时返回空串。
// 最近修改时间：2026-10-18；改动原因：新增，请求头拼装出口。
func (s *cookieStore) header() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	parts := make([]string, 0, len(s.records))
	for _, rec := range s.records {
		if rec.Cookie.Name == "" || rec.Cookie.Value == "" {
			continue
		}
		parts = append(parts, rec.Cookie.Name+"="+rec.Cookie.Value)
	}
	return strings.Join(parts, "; ")
}

// hasLiveSession 判断容器中是否仍持有未过期的会话主 Cookie。
// [参数] 无。
// [返回] 存在任一未过期的会话主 Cookie 时返回 true。
// 最近修改时间：2026-10-18；改动原因：新增，登录态失效判定。
func (s *cookieStore) hasLiveSession() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, rec := range s.records {
		if !isCozeSessionCookie(rec.Cookie.Name) {
			continue
		}
		if rec.Cookie.Expires.IsZero() || rec.Cookie.Expires.After(now) {
			return true
		}
	}
	return false
}

// isCozeSessionCookie 判断 Cookie 名是否属于会话主 Cookie。
// [参数] name: Cookie 名。
// [返回] 属于时返回 true。
// 最近修改时间：2026-10-18；改动原因：新增，会话 Cookie 名判定。
func isCozeSessionCookie(name string) bool {
	for _, n := range cozeSessionCookieNames {
		if strings.EqualFold(name, n) {
			return true
		}
	}
	return false
}

// fileDigest 计算文件内容摘要；文件不存在或不可读时返回空串。
// [参数] path: 文件路径。
// [返回] 十六进制摘要。
// 最近修改时间：2026-10-18；改动原因：新增，内容级身份判定。
func fileDigest(path string) string {
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
