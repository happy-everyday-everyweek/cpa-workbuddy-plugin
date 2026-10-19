package main

// 本文件负责 Notion 账号凭据的解析、探活、落盘与宿主注册。
//
// 为什么不走 host.auth.save 写盘：宿主保存时会丢掉它不认识的顶层字段，
// 本插件需要自行保存 space 列表、套餐快照与 client_version，
// 因此直写物理文件后再通过 host.auth.save 做一次注册同步。
//
// 最近修改时间：2026-10-18；改动原因：新增，Cookie 导入主链路。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	// notionAuthFilePrefix 是账号文件名前缀，宿主按此识别本插件账号。
	notionAuthFilePrefix = "notion-"
	// notionAuthFileSuffix 是账号文件扩展名。
	notionAuthFileSuffix = ".json"
)

var (
	// accountsDirMu 只保护 accountsDir 这个配置值。
	//
	// 不能用同一把 RWMutex 兼管文件 IO：路径校验会在持锁状态下再次
	// 读取 accountsDir，而 RWMutex 不可重入，会直接自锁。
	accountsDirMu sync.RWMutex
	accountsDir   string
)

// CookieIdentity 是从 Cookie header 中解析出的身份字段。
type CookieIdentity struct {
	// CookieHeader 是原始 Cookie header。
	CookieHeader string
	// TokenV2 是登录态 token；不要写入日志。
	TokenV2 string
	// UserID 是 notion_user_id，同时用于 x-notion-active-user-header。
	UserID string
	// BrowserID 是 notion_browser_id。
	BrowserID string
	// DeviceID 是设备标识，部分端点需要。
	DeviceID string
}

// NotionSpace 是一个可用的工作空间。
//
// Notion 的 getSpaces 返回双层结构：外层键是用户根 id，内层 space 才是
// workspace id。直接拿外层键当 spaceId 会得到 403 或不存在的空间。
type NotionSpace struct {
	// ID 是 workspace id，必须来自内层 space 键。
	ID string `json:"id"`
	// Name 是展示名。
	Name string `json:"name"`
	// PlanType 是套餐类型（如 business / free）。
	PlanType string `json:"plan_type,omitempty"`
	// PremiumCredits 是订阅额度剩余余额。
	PremiumCredits int `json:"premium_credits"`
	// PremiumLimit 是本服务周期的订阅额度上限，来自 limits.purchased.totalLimit。
	PremiumLimit int `json:"premium_limit,omitempty"`
	// PremiumUsage 是本服务周期已用的订阅额度，由上限减余额推导。
	PremiumUsage int `json:"premium_usage,omitempty"`
	// BasicLimit 与 BasicUsage 是基础消息额度。
	//
	// 订阅空间由上游标记为不限额，这两个值仍会累计增长并超过上限，
	// 因此不能用它算订阅空间的占用百分比。
	BasicLimit int `json:"basic_limit"`
	BasicUsage int `json:"basic_usage"`
	// UsageCheckedAt 是额度快照时间。
	UsageCheckedAt string `json:"usage_checked_at,omitempty"`
}

// StoredAuth 是落盘的 Notion 账号结构体。
type StoredAuth struct {
	Type         string `json:"type"`
	Provider     string `json:"provider"`
	AuthID       string `json:"auth_id"`
	CookieHeader string `json:"cookie_header"`
	// TokenV2 与 CookieHeader 并存：CookieHeader 用于发请求，
	// TokenV2 用于快速判定登录态并把账号导出恢复。
	TokenV2 string `json:"token_v2,omitempty"`
	// UserID 是 x-notion-active-user-header 的取值。
	UserID string `json:"user_id"`
	// SpaceID 是当前选用的 workspace id。
	SpaceID string `json:"space_id"`
	// SpaceName 是 workspace 展示名。
	SpaceName string `json:"space_name,omitempty"`
	// Spaces 是该账号下的全部 workspace 及其额度快照。
	Spaces []NotionSpace `json:"spaces,omitempty"`
	// ClientVersion 是该账号探活时使用的 notion-client-version。
	ClientVersion string `json:"client_version,omitempty"`
	// BrowserID 与 DeviceID 用于 Cookie 滚动时补齐。
	BrowserID string `json:"browser_id,omitempty"`
	DeviceID  string `json:"device_id,omitempty"`
	// ProxyURL 预留出口代理；Notion 把 AI 权限绑定到登录 IP。
	ProxyURL string `json:"proxy_url,omitempty"`
	// Label 是展示名。
	Label string `json:"label"`
	// Disabled 由宿主或面板控制。
	Disabled bool `json:"disabled"`
	// CreatedAt 与 UpdatedAt 是 ISO8601 时间戳。
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	// PhysicalPath 是账号文件绝对路径，不落盘。
	PhysicalPath string `json:"-"`
	// LoadedDigest 是读取该账号时的内容摘要，用于防止覆盖外部写入。
	LoadedDigest string `json:"-"`
}

// setAccountsDir 设置账号存储目录（供测试覆盖）。
// [参数] dir: 目标目录。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，测试注入点。
func setAccountsDir(dir string) {
	accountsDirMu.Lock()
	defer accountsDirMu.Unlock()
	accountsDir = dir
}

// getAccountsDir 返回账号存储目录，未设置时取默认路径。
// [参数] 无。
// [返回] 账号目录绝对路径。
// 最近修改时间：2026-10-18；改动原因：新增，与既有插件同构的默认目录。
func getAccountsDir() string {
	accountsDirMu.RLock()
	dir := accountsDir
	accountsDirMu.RUnlock()
	if dir != "" {
		return dir
	}

	home, errHome := os.UserHomeDir()
	if errHome != nil {
		return notionAuthFilePrefix + "accounts"
	}
	return filepath.Join(home, ".antigravity_cockpit", "notion_accounts")
}

// parseCookieIdentity 从 Cookie header 中解析身份字段。
//
// 只有 token_v2 与 notion_user_id 同时存在时才算可用登录态：
// 缺 token_v2 无法访问任何 API，缺 user id 则无法构造 active-user 头。
// [参数] header: 裸 Cookie header、`Cookie:` 前缀、DevTools 请求头整段，
// 或 Network 面板 Copy as cURL / Copy as fetch 的整段文本。
// [返回] 解析结果与错误。
// 最近修改时间：2026-10-19；改动原因：入口接受整段请求，支持 Copy as cURL。
func parseCookieIdentity(header string) (*CookieIdentity, error) {
	normalized := normalizeCookieHeader(header)
	if normalized == "" {
		return nil, fmt.Errorf("Cookie 为空")
	}

	identity := &CookieIdentity{CookieHeader: normalized}
	for _, part := range strings.Split(normalized, ";") {
		key, value, ok := splitCookiePair(part)
		if !ok {
			continue
		}
		switch key {
		case "token_v2":
			identity.TokenV2 = value
		case "notion_user_id":
			identity.UserID = value
		case "notion_browser_id":
			identity.BrowserID = value
		case "device_id":
			identity.DeviceID = value
		}
	}

	if identity.TokenV2 == "" {
		return nil, fmt.Errorf("Cookie 缺少 token_v2，请从 Notion 网页端重新复制")
	}
	if identity.UserID == "" {
		return nil, fmt.Errorf("Cookie 缺少 notion_user_id，请复制完整 Cookie header")
	}
	return identity, nil
}

// cookieHeaderName 是请求头里 Cookie 的名字。
const cookieHeaderName = "cookie"

// normalizeCookieHeader 把用户输入规范成单行 Cookie header。
//
// 用户可能粘贴四种形态：裸的 `a=b; c=d`、完整的 `Cookie: a=b; c=d`、
// DevTools「Copy request headers」的整段请求头，或 Network 面板的
// 「Copy as cURL」/「Copy as fetch」。后两种里 Cookie 只是其中一段，
// 整段当 Cookie 发出去必然 401，因此先抽出 Cookie 头本身。
// [参数] header: 原始输入。
// [返回] 单行 Cookie header。
// 最近修改时间：2026-10-19；改动原因：真正解析 cURL / 请求头整段，插件自行识别。
func normalizeCookieHeader(header string) string {
	s := strings.TrimSpace(header)
	if s == "" {
		return ""
	}

	if value := extractCookieHeaderValue(s); value != "" {
		s = value
	} else {
		s = strings.TrimPrefix(s, "Cookie:")
		s = strings.TrimPrefix(s, "cookie:")
	}

	// 多行粘贴时只保留含 `=` 的行，拼成单行。
	if strings.ContainsAny(s, "\r\n") {
		var parts []string
		for _, line := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' }) {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			line = strings.TrimSuffix(line, ";")
			parts = append(parts, line)
		}
		s = strings.Join(parts, "; ")
	}

	return strings.TrimSpace(strings.TrimSuffix(s, ";"))
}

// extractCookieHeaderValue 从整段请求文本中抽出 Cookie 头的值。
//
// Chrome 的 Copy as cURL / Copy as fetch、DevTools 的 Copy request headers
// 都会把 Cookie 写成 `cookie: ...` 一行。这里按「头名 + 冒号」定位，
// 兼容 -H/--header 前缀、单双引号包裹以及 Windows cmd 的 ^" 转义。
// [参数] text: 用户粘贴的原文。
// [返回] Cookie 头的值；文本里没有 Cookie 头声明时返回空串。
// 最近修改时间：2026-10-19；改动原因：新增，支持 Copy as cURL 整段识别。
func extractCookieHeaderValue(text string) string {
	lower := strings.ToLower(text)
	for cursor := 0; cursor < len(lower); {
		idx := strings.Index(lower[cursor:], cookieHeaderName)
		if idx < 0 {
			return ""
		}
		nameAt := cursor + idx
		cursor = nameAt + len(cookieHeaderName)
		// 跳过 `notion_cookie_id` 这类把 cookie 当词根的字段名，
		// 也跳过响应头里的 `set-cookie`。
		if nameAt > 0 && isHeaderNameChar(text[nameAt-1]) {
			continue
		}
		if nameAt >= 4 && strings.EqualFold(text[nameAt-4:nameAt], "set-") {
			continue
		}

		rest := strings.TrimLeft(text[cursor:], " \t\"'")
		if !strings.HasPrefix(rest, ":") {
			continue
		}
		if value := readCookieHeaderValue(strings.TrimLeft(rest[1:], " \t")); value != "" {
			return value
		}
	}
	return ""
}

// isHeaderNameChar 判断字符能否构成请求头名。
//
// 用于排除 `notion_cookie_id` 这类把 cookie 当词根的更长字段名。
// [参数] c: 待判断字符。
// [返回] 是头名字符时返回 true。
// 最近修改时间：2026-10-19；改动原因：新增，避免误匹配字段名。
func isHeaderNameChar(c byte) bool {
	switch {
	case c == '_' || c == '-':
		return true
	case c >= '0' && c <= '9':
		return true
	case c >= 'a' && c <= 'z':
		return true
	case c >= 'A' && c <= 'Z':
		return true
	default:
		return false
	}
}

// readCookieHeaderValue 读出冒号之后的 Cookie 值。
//
// 引号包裹时读到同名引号为止；否则读到行尾。两种形态都要再清一次
// 尾部残留：cmd 的 Copy as cURL 会留下 `^"` 与续行符 `^`，
// 不清掉会污染最后一个 Cookie 的值。
// [参数] rest: 冒号之后的文本。
// [返回] 清理后的值。
// 最近修改时间：2026-10-19；改动原因：新增，引号与行尾两种取值形态。
func readCookieHeaderValue(rest string) string {
	if rest == "" {
		return ""
	}
	quote := rest[0]
	if quote == '\'' || quote == '"' {
		return trimHeaderValueTail(unquoteSegment(rest, quote))
	}
	if nl := strings.IndexAny(rest, "\r\n"); nl >= 0 {
		rest = rest[:nl]
	}
	return trimHeaderValueTail(rest)
}

// unquoteSegment 读取引号内的内容，保留反斜杠转义后的字符。
// [参数] text: 以引号开头的文本；quote: 引号字符。
// [返回] 引号内的字符串。
// 最近修改时间：2026-10-19；改动原因：新增，引号取值。
func unquoteSegment(text string, quote byte) string {
	var b strings.Builder
	escaped := false
	for i := 1; i < len(text); i++ {
		c := text[i]
		if escaped {
			b.WriteByte(c)
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if c == quote {
			break
		}
		b.WriteByte(c)
	}
	return b.String()
}

// trimHeaderValueTail 清掉值两端的空白与命令行残留符号。
// [参数] raw: 原始值。
// [返回] 清理后的值。
// 最近修改时间：2026-10-19；改动原因：新增，清理 cmd 的 ^" 与续行符。
func trimHeaderValueTail(raw string) string {
	trimmed := strings.TrimSpace(raw)
	for {
		next := strings.TrimRight(trimmed, " \t\r\n\\^\"'")
		if next == trimmed {
			return trimmed
		}
		trimmed = next
	}
}

// splitCookiePair 拆分 `key=value`。
// [参数] part: 单个 Cookie 片段。
// [返回] 键、值与是否解析成功。
// 最近修改时间：2026-10-18；改动原因：新增，Cookie 解析辅助。
func splitCookiePair(part string) (string, string, bool) {
	part = strings.TrimSpace(part)
	if part == "" {
		return "", "", false
	}
	idx := strings.Index(part, "=")
	if idx <= 0 {
		return "", "", false
	}
	return strings.TrimSpace(part[:idx]), strings.TrimSpace(part[idx+1:]), true
}

// authIDFor 从用户与工作空间派生稳定的账号标识。
//
// 只取用户与空间、不取 token：Notion 会滚动刷新 token_v2，若把 token
// 放进哈希，每次轮换后重新导入都会生成一个新账号文件，同一空间被重复
// 路由；而「同一用户 + 同一空间」才是稳定的账号身份。
// 空间必须参与：AI 额度绑定在 workspace 上，商业版与免费版额度独立，
// 标识含空间后一次导入才能把同一个账号拆成多个可独立路由的账号。
// [参数] identity: Cookie 身份；spaceID: 目标工作空间。
// [返回] 形如 notion-<前12位> 的账号标识。
// 最近修改时间：2026-10-19；改动原因：改为用户 + 空间派生，支持按空间拆分账号。
func authIDFor(identity *CookieIdentity, spaceID string) string {
	sum := sha256.Sum256([]byte(identity.UserID + "|" + spaceID))
	suffix := hex.EncodeToString(sum[:])[:12]
	return notionAuthFilePrefix + suffix
}

// retireStaleAccounts 把同一用户下、不属于本次拆分结果的旧账号归档。
//
// 不能靠旧标识反查：旧标识含 token_v2，而这个值会被上游轮换，按它算出的
// 文件名往往已经不存在。改为扫描账号目录，凡同一 UserID 且不在本次保留
// 集合里的文件，都是拆分前的旧账号（或该空间已从账号下移除）。
// 留着会被重复路由，同一份额度被并发消耗；改名成 .migrated 后缀既让扫描与
// 宿主都不再识别，又保留原文件用于回溯，不删除用户数据。
// [参数] identity: Cookie 身份；keepIDs: 本次拆分保留的账号标识集合。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，按用户扫描迁移旧账号。
func retireStaleAccounts(identity *CookieIdentity, keepIDs map[string]bool) {
	accounts, errList := listAllAuthFiles()
	if errList != nil {
		return
	}
	for _, account := range accounts {
		if account.UserID != identity.UserID || keepIDs[account.AuthID] {
			continue
		}
		if account.PhysicalPath == "" || !isSafeNotionAuthPath(account.PhysicalPath) {
			continue
		}
		_ = os.Rename(account.PhysicalPath, account.PhysicalPath+".migrated")
	}
}

// isNotionAuthFileName 判断文件名是否属于本插件。
// [参数] name: 文件名。
// [返回] 是否匹配。
// 最近修改时间：2026-10-18；改动原因：新增，宿主扫描过滤条件。
func isNotionAuthFileName(name string) bool {
	base := filepath.Base(strings.TrimSpace(name))
	return strings.HasPrefix(base, notionAuthFilePrefix) && strings.HasSuffix(base, notionAuthFileSuffix)
}

// isSafeNotionAuthPath 判断路径是否落在账号目录内。
//
// 账号读写会拿到宿主传入的文件名，必须校验最终路径不越出账号目录，
// 否则一个构造过的名字就能读写任意文件。
// [参数] path: 待校验路径。
// [返回] 是否安全。
// 最近修改时间：2026-10-18；改动原因：新增，路径穿越防护。
func isSafeNotionAuthPath(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	clean := filepath.Clean(path)
	base := filepath.Base(clean)
	if !isNotionAuthFileName(base) {
		return false
	}
	dir := filepath.Clean(filepath.Dir(clean))
	root := filepath.Clean(getAccountsDir())
	return dir == root || strings.HasPrefix(dir, root+string(filepath.Separator))
}

// digestOf 计算内容摘要，用于检测外部写入。
// [参数] raw: 文件内容。
// [返回] 十六进制摘要。
// 最近修改时间：2026-10-18；改动原因：新增，防覆盖判据。
func digestOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// writeAuthFileDirect 原子写盘账号凭据，并在宿主在线时同步注册。
// [参数] filePath: 目标路径；auth: 账号结构。
// [返回] 写入错误。
// 最近修改时间：2026-10-18；改动原因：新增，凭据物理落盘。
func writeAuthFileDirect(filePath string, auth *StoredAuth) error {
	if auth.Type == "" {
		auth.Type = providerName
	}
	if auth.Provider == "" {
		auth.Provider = providerName
	}

	targetPath := filePath
	if auth.PhysicalPath != "" && isSafeNotionAuthPath(auth.PhysicalPath) {
		targetPath = auth.PhysicalPath
	}
	if !isSafeNotionAuthPath(targetPath) {
		return fmt.Errorf("不安全的凭据文件路径：%s", targetPath)
	}

	data, errMarshal := json.MarshalIndent(auth, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("序列化凭据失败：%w", errMarshal)
	}
	if errMkdir := os.MkdirAll(filepath.Dir(targetPath), 0755); errMkdir != nil {
		return fmt.Errorf("创建目录失败：%w", errMkdir)
	}

	tmpFile := fmt.Sprintf("%s.tmp.%d", targetPath, time.Now().UnixNano())
	if errWrite := os.WriteFile(tmpFile, data, 0600); errWrite != nil {
		return fmt.Errorf("写入临时文件失败：%w", errWrite)
	}
	if errRename := os.Rename(tmpFile, targetPath); errRename != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("重命名临时文件失败：%w", errRename)
	}

	auth.PhysicalPath = targetPath
	auth.LoadedDigest = digestOf(data)
	if !auth.Disabled {
		_ = hostAuthSaveJSON(filepath.Base(targetPath), data)
	}
	return nil
}

// hostAuthSaveJSON 通过宿主 RPC 注册认证文件。
// [参数] name: 文件名；raw: 文件内容。
// [返回] 同步错误。
// 最近修改时间：2026-10-18；改动原因：新增，宿主凭据注册通道。
func hostAuthSaveJSON(name string, raw []byte) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("凭据文件名为空")
	}
	saveBody, _ := json.Marshal(pluginapi.HostAuthSaveRequest{Name: name, JSON: raw})
	rawResp, errCall := hostCall(pluginabi.MethodHostAuthSave, saveBody)
	if errCall != nil {
		return errCall
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(rawResp, &env); errUnmarshal != nil || !env.OK {
		return fmt.Errorf("host.auth.save 调用失败")
	}
	return nil
}

// readAuthFile 读取单个账号凭据文件。
// [参数] filePath: 文件路径。
// [返回] 账号结构与读取错误。
// 最近修改时间：2026-10-18；改动原因：新增，单账号读取入口。
func readAuthFile(filePath string) (*StoredAuth, error) {
	if !isSafeNotionAuthPath(filePath) {
		return nil, fmt.Errorf("不安全的凭据文件路径：%s", filePath)
	}
	data, errRead := os.ReadFile(filePath)
	if errRead != nil {
		return nil, errRead
	}
	var auth StoredAuth
	if errUnmarshal := json.Unmarshal(data, &auth); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	auth.PhysicalPath = filePath
	auth.LoadedDigest = digestOf(data)
	return &auth, nil
}

// listAllAuthFiles 列出账号目录下的全部账号。
//
// 以磁盘为准而不是只信宿主缓存：重启后宿主可能尚未重建索引，
// 面板需要立即看到已落盘的账号。
// [参数] 无。
// [返回] 账号列表与错误。
// 最近修改时间：2026-10-18；改动原因：新增，账号枚举入口。
func listAllAuthFiles() ([]*StoredAuth, error) {
	dir := getAccountsDir()
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return nil, nil
		}
		return nil, errRead
	}

	var out []*StoredAuth
	for _, entry := range entries {
		if entry.IsDir() || !isNotionAuthFileName(entry.Name()) {
			continue
		}
		auth, errLoad := readAuthFile(filepath.Join(dir, entry.Name()))
		if errLoad != nil {
			continue
		}
		out = append(out, auth)
	}
	return out, nil
}

// deleteAuthFileDirect 删除账号文件。
// [参数] authID: 账号标识（可带或不带 .json）。
// [返回] 删除错误。
// 最近修改时间：2026-10-18；改动原因：新增，账号删除入口。
func deleteAuthFileDirect(authID string) error {
	authID = strings.TrimSpace(strings.TrimSuffix(authID, notionAuthFileSuffix))
	if authID == "" {
		return fmt.Errorf("账号标识为空")
	}
	if !strings.HasPrefix(authID, notionAuthFilePrefix) {
		authID = notionAuthFilePrefix + authID
	}

	target := filepath.Join(getAccountsDir(), authID+notionAuthFileSuffix)
	if !isSafeNotionAuthPath(target) {
		return fmt.Errorf("不安全的账号标识：%s", authID)
	}
	return os.Remove(target)
}

// maskIdentifier 对账号标识做脱敏，供展示与默认备注名使用。
// [参数] id: 账号标识。
// [返回] 脱敏后的字符串。
// 最近修改时间：2026-10-18；改动原因：新增，展示层脱敏。
func maskIdentifier(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 8 {
		return "******"
	}
	return id[:4] + "...." + id[len(id)-4:]
}

// scopeForAuth 由账号构造请求作用域。
// [参数] auth: 账号；clientVersion: 版本覆盖值。
// [返回] 请求作用域。
// 最近修改时间：2026-10-18；改动原因：新增，账号到请求的转换。
func scopeForAuth(auth *StoredAuth, clientVersion string) requestScope {
	if clientVersion == "" {
		clientVersion = auth.ClientVersion
	}
	return requestScope{
		Cookie:        auth.CookieHeader,
		UserID:        auth.UserID,
		SpaceID:       auth.SpaceID,
		ClientVersion: clientVersion,
	}
}

// loginHintForProbeError 为上游鉴权类失败补一句可执行的下一步。
//
// 401 的原文只有一句 `Token is invalid.`，用户看不出该重来还是该换账号。
// 这里只补操作指引，不改变错误判定与状态码。
// [参数] err: 探测工作空间返回的错误。
// [返回] 追加文案；非鉴权类错误返回空串。
// 最近修改时间：2026-10-19；改动原因：新增，把 401 转成可操作提示。
func loginHintForProbeError(err error) string {
	var apiErr *notionAPIError
	if !errors.As(err, &apiErr) {
		return ""
	}
	switch apiErr.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "（凭据已失效：请确认粘贴的是当前已登录标签页的请求，并重新执行授权脚本后重试）"
	default:
		return ""
	}
}

// saveCredentials 校验并落盘一份导入的登录态。
//
// 流程：解析 Cookie → 拉取 workspace 列表 → 刷新全部空间额度 → 按空间逐个落盘。
// 探活失败时**不落盘**，避免把失效 Cookie 写成可用账号。
//
// 为什么按空间拆分：Notion 的 AI 额度绑定在 workspace 上，同一账号的
// 商业版与免费版额度互相独立。拆成多个账号后，额度耗尽、冷却与故障
// 转移都只影响对应空间，不会互相拖累。
// [参数] header: Cookie header；label: 备注名前缀；spaceID: 只导入指定空间（可空表示全部）。
// [返回] 落盘的账号列表与错误。
// 最近修改时间：2026-10-19；改动原因：按工作空间拆分账号。
func saveCredentials(header, label, spaceID string) ([]*StoredAuth, error) {
	identity, errParse := parseCookieIdentity(header)
	if errParse != nil {
		return nil, errParse
	}

	cfg := currentConfig()
	cl := newNotionHTTPClient(notionRequestTimeout())
	ctx, cancel := context.WithTimeout(context.Background(), notionRequestTimeout())
	defer cancel()

	probeScope := requestScope{
		Cookie:        identity.CookieHeader,
		UserID:        identity.UserID,
		ClientVersion: cfg.ClientVersion,
	}
	spaces, errSpaces := fetchSpaces(ctx, cl, probeScope)
	if errSpaces != nil {
		return nil, fmt.Errorf("探测工作空间失败：%w%s", errSpaces, loginHintForProbeError(errSpaces))
	}
	if len(spaces) == 0 {
		return nil, fmt.Errorf("账号下没有可用工作空间")
	}

	wanted := strings.TrimSpace(firstNonEmpty(spaceID, cfg.SpaceID))
	targets := spaces
	if wanted != "" {
		chosen := pickSpace(spaces, wanted)
		if chosen == nil {
			return nil, fmt.Errorf("指定的工作空间 %s 不在账号下", wanted)
		}
		targets = []NotionSpace{*chosen}
	}

	// 额度快照单独拉取；失败不影响账号可用性，只是面板暂时看不到余额。
	probeScope.SpaceID = targets[0].ID
	enrichSpaceUsage(ctx, cl, probeScope, spaces)

	now := time.Now().Format(time.RFC3339)
	out := make([]*StoredAuth, 0, len(targets))
	keptIDs := make(map[string]bool, len(targets))
	for _, space := range targets {
		authID := authIDFor(identity, space.ID)
		keptIDs[authID] = true
		auth := &StoredAuth{
			Type:          providerName,
			Provider:      providerName,
			AuthID:        authID,
			CookieHeader:  identity.CookieHeader,
			TokenV2:       identity.TokenV2,
			UserID:        identity.UserID,
			SpaceID:       space.ID,
			SpaceName:     space.Name,
			Spaces:        spaces,
			ClientVersion: cfg.ClientVersion,
			BrowserID:     identity.BrowserID,
			DeviceID:      identity.DeviceID,
			Label:         accountLabel(label, space, authID),
			CreatedAt:     now,
			UpdatedAt:     now,
		}

		fullPath := filepath.Join(getAccountsDir(), authID+notionAuthFileSuffix)
		if errWrite := writeAuthFileDirect(fullPath, auth); errWrite != nil {
			return out, errWrite
		}
		out = append(out, auth)
	}

	// 只有全量导入才归档旧账号：指定单个空间导入时，其它空间账号仍然有效，
	// 归档它们会把用户的可用账号一起收掉。
	if wanted == "" {
		// 新账号已全部落盘，再归档旧账号，避免中途失败后既无新号也无旧号。
		retireStaleAccounts(identity, keptIDs)
	}
	return out, nil
}

// accountLabel 生成账号展示名。
//
// 同一账号会拆成多个空间账号，备注名必须能区分空间，否则面板上一排
// 同名卡片无法辨认。默认用「订阅 / 免费」标记套餐，用户指定前缀时
// 前缀优先，标记仍然保留。
// [参数] header: 用户输入的前缀；space: 目标空间；authID: 账号标识。
// [返回] 展示名。
// 最近修改时间：2026-10-19；改动原因：新增，拆分后的账号命名。
func accountLabel(header string, space NotionSpace, authID string) string {
	// 订阅空间用「上限」判定而不是余额：额度用尽后余额会变成 0，
	// 若按余额判定，重新导入会把一张订阅卡错标成免费。
	tier := "免费"
	if space.PremiumLimit > 0 || space.PremiumCredits > 0 {
		tier = "订阅"
	}
	base := strings.TrimSpace(header)
	if base == "" {
		base = "Notion " + maskIdentifier(authID)
	}
	return base + " · " + tier
}

// premiumCeiling 返回空间的订阅额度规模。
//
// 优先用本周期上限；旧账号文件没有 premium_limit 字段，回落到余额，
// 避免升级后面板把所有订阅空间都算成 0。
// [参数] space: 工作空间。
// [返回] 订阅额度上限或余额。
// 最近修改时间：2026-10-19；改动原因：新增，兼容旧快照的上限取值。
func premiumCeiling(space *NotionSpace) int {
	if space == nil {
		return 0
	}
	if space.PremiumLimit > 0 {
		return space.PremiumLimit
	}
	return space.PremiumCredits
}

// pickSpace 在候选空间中选出目标空间。
//
// 优先精确匹配指定 id；未指定时选订阅额度最大的空间——商业版通常有
// premium 上限，免费版为 0，这样默认就落在订阅空间上。
// [参数] spaces: 候选空间；want: 期望 id。
// [返回] 选中的空间；无匹配时返回 nil。
// 最近修改时间：2026-10-19；改动原因：改用上限比较，额度耗尽后仍能选中订阅空间。
func pickSpace(spaces []NotionSpace, want string) *NotionSpace {
	want = strings.TrimSpace(want)
	if want != "" {
		for i := range spaces {
			if strings.EqualFold(spaces[i].ID, want) {
				return &spaces[i]
			}
		}
		return nil
	}

	// 按上限比较而不是余额：订阅额度耗尽后余额归零，按余额挑空间
	// 会误选到免费空间。
	best := 0
	for i := 1; i < len(spaces); i++ {
		if premiumCeiling(&spaces[i]) > premiumCeiling(&spaces[best]) {
			best = i
		}
	}
	return &spaces[best]
}

// firstNonEmpty 返回第一个非空字符串。
// [参数] values: 候选值。
// [返回] 第一个非空值；全空时返回空串。
// 最近修改时间：2026-10-18；改动原因：新增，配置回退辅助。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// handleParseAuth 响应宿主 auth.parse。
// [参数] raw: 宿主请求载荷。
// [返回] 解析响应信封与错误。
// 最近修改时间：2026-10-18；改动原因：新增，宿主扫描账号文件时的解析入口。
func handleParseAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	fileName := strings.TrimSpace(req.FileName)
	if !isNotionAuthFileName(fileName) {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}

	var auth StoredAuth
	if errUnmarshal := json.Unmarshal(req.RawJSON, &auth); errUnmarshal != nil || strings.TrimSpace(auth.CookieHeader) == "" {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	if auth.AuthID == "" {
		auth.AuthID = strings.TrimSuffix(fileName, notionAuthFileSuffix)
	}

	// authDataForLogin 已回传带 .json 的文件名形态，与宿主按相对路径
	// 派生的 ID 同形；同一次扫描不会既登记路径 ID、又登记裸 ID。
	return okEnvelope(pluginapi.AuthParseResponse{Handled: true, Auth: authDataForLogin(&auth)})
}

// handleRefreshAuth 响应宿主 auth.refresh。
//
// Notion 的会话 Cookie 只能靠用户重新导出，插件无法自助续期：
// 原样返回现有凭据并把下次刷新推远，避免宿主把账号标成刷新失败。
// [参数] raw: 宿主刷新请求载荷。
// [返回] 刷新响应信封与错误。
// 最近修改时间：2026-10-18；改动原因：新增，刷新入口（无自助续期能力）。
func handleRefreshAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	var auth StoredAuth
	if len(req.StorageJSON) > 0 {
		_ = json.Unmarshal(req.StorageJSON, &auth)
	}
	if strings.TrimSpace(auth.CookieHeader) == "" {
		return nil, fmt.Errorf("账号凭据为空，无法刷新")
	}
	if auth.AuthID == "" {
		auth.AuthID = strings.TrimSpace(req.AuthID)
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth:             authDataForLogin(&auth),
		NextRefreshAfter: time.Now().Add(24 * time.Hour).UTC(),
	})
}

// authDataForLogin 把账号转换成宿主可识别的认证数据。
//
// StorageJSON 必须是完整凭据：宿主用它作为该账号的持久化存储，
// 回传空值时宿主会以自己的空存储覆盖账号文件，Cookie 与空间列表
// 会整体丢失，后续所有推理都变成「未找到可用账号」。
// [参数] auth: 账号。
// [返回] 宿主认证数据结构。
// 最近修改时间：2026-10-19；改动原因：补 StorageJSON，修复宿主覆盖凭据导致账号失效。
func authDataForLogin(auth *StoredAuth) pluginapi.AuthData {
	storage, _ := json.Marshal(auth)
	fileName := authFileNameOf(auth.AuthID)
	return pluginapi.AuthData{
		ID:          fileName,
		FileName:    fileName,
		Provider:    providerName,
		Label:       auth.Label,
		Disabled:    auth.Disabled,
		ProxyURL:    auth.ProxyURL,
		StorageJSON: storage,
		Metadata: map[string]any{
			"user_id":    auth.UserID,
			"space_id":   auth.SpaceID,
			"space_name": auth.SpaceName,
		},
	}
}

// authFileNameOf 把账号标识规范成宿主 auth 目录下的文件名。
//
// 宿主有两条注册路径：host.auth.save 走「相对路径派生 ID」（含 .json），
// 登录结果走 AuthData.ID（原样使用）。两者不一致时同一个物理文件会被注册成
// 两个 auth 记录，面板与宿主管理页都会出现两条一模一样的账号。
// [参数] authID: 账号标识（可带或不带 .json 后缀）。
// [返回] 形如 notion-<hash>.json 的文件名。
// 最近修改时间：2026-10-19；改动原因：新增，登录回传 ID 与文件名统一口径。
func authFileNameOf(authID string) string {
	authID = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(authID), notionAuthFileSuffix))
	return authID + notionAuthFileSuffix
}

// listHostAuthFiles 从宿主获取账号索引，宿主不可用时回落到本地扫描。
//
// 宿主在部分调用路径下不会下发 StorageJSON，此时只能靠 AuthID 定位账号；
// 若宿主 RPC 也不可用，必须回落到磁盘，否则一次失败就会把整池账号判成不可用。
// [参数] 无。
// [返回] 账号列表；宿主与磁盘都为空时返回 nil。
// 最近修改时间：2026-10-19；改动原因：补本地回落，修复宿主列表不可用时取不到号。
func listHostAuthFiles() []*StoredAuth {
	raw, errCall := hostCall(pluginabi.MethodHostAuthList, nil)
	if errCall != nil {
		accounts, _ := listAllAuthFiles()
		return accounts
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil || !env.OK {
		accounts, _ := listAllAuthFiles()
		return accounts
	}
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		accounts, _ := listAllAuthFiles()
		return accounts
	}

	var out []*StoredAuth
	for _, f := range resp.Files {
		if !isNotionAuthFileName(f.Name) {
			continue
		}
		auth, errLoad := readAuthFile(filepath.Join(getAccountsDir(), filepath.Base(f.Name)))
		if errLoad != nil {
			continue
		}
		auth.Disabled = auth.Disabled || f.Disabled
		out = append(out, auth)
	}
	// 宿主索引可能尚未重建（例如刚热重载），磁盘是最终事实来源。
	if len(out) == 0 {
		accounts, _ := listAllAuthFiles()
		return accounts
	}
	return out
}

// ensureHTTPClient 返回一个带默认超时的客户端。
// [参数] timeout: 超时。
// [返回] 客户端。
// 最近修改时间：2026-10-18；改动原因：新增，测试可替换入口。
func ensureHTTPClient(timeout time.Duration) *http.Client {
	return newNotionHTTPClient(timeout)
}
