package main

// 本文件验证 Cookie 解析、账号落盘与工作空间选择。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestParseCookieIdentity 校验 Cookie 三种输入形态。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，输入归一回归。
func TestParseCookieIdentity(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantUser  string
		wantToken string
		wantErr   bool
	}{
		{
			name:      "裸 header",
			input:     "token_v2=v03%3Aabc; notion_user_id=u-1; notion_browser_id=b-1",
			wantUser:  "u-1",
			wantToken: "v03%3Aabc",
		},
		{
			name:      "带 Cookie 前缀",
			input:     "Cookie: token_v2=tok; notion_user_id=u-2",
			wantUser:  "u-2",
			wantToken: "tok",
		},
		{
			name:      "多行粘贴",
			input:     "token_v2=tok\nnotion_user_id=u-3\nnotion_browser_id=b-3\n",
			wantUser:  "u-3",
			wantToken: "tok",
		},
		{
			name:    "缺 token_v2",
			input:   "notion_user_id=u-4",
			wantErr: true,
		},
		{
			name:    "缺 notion_user_id",
			input:   "token_v2=tok",
			wantErr: true,
		},
		{
			name:    "空输入",
			input:   "   ",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			identity, err := parseCookieIdentity(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatal("期望报错，实际通过")
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if identity.UserID != tc.wantUser {
				t.Fatalf("UserID = %q, 期望 %q", identity.UserID, tc.wantUser)
			}
			if identity.TokenV2 != tc.wantToken {
				t.Fatalf("TokenV2 = %q, 期望 %q", identity.TokenV2, tc.wantToken)
			}
			if strings.Contains(identity.CookieHeader, "\n") {
				t.Fatal("归一后不应含换行")
			}
		})
	}
}

// TestAuthIDForStable 校验账号标识稳定、按空间区分且不泄露原始值。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：标识改为「用户 + 空间」，补空间区分与 token 轮换回归。
func TestAuthIDForStable(t *testing.T) {
	identity := &CookieIdentity{UserID: "user-abc", TokenV2: "secret-token"}
	first := authIDFor(identity, "space-paid")
	second := authIDFor(identity, "space-paid")

	if first != second {
		t.Fatalf("同一凭据产生不同标识: %s vs %s", first, second)
	}
	if !strings.HasPrefix(first, notionAuthFilePrefix) {
		t.Fatalf("标识缺少前缀: %s", first)
	}
	if strings.Contains(first, "secret") || strings.Contains(first, "user-abc") {
		t.Fatalf("标识泄露了原始凭据: %s", first)
	}

	// 同一账号的不同空间必须得到不同标识，否则拆分后仍会互相覆盖。
	if other := authIDFor(identity, "space-free"); other == first {
		t.Fatal("不同空间应产生不同标识")
	}

	// token 会被上游轮换，轮换后同一空间的标识必须保持不变，
	// 否则每次重新导入都会生成新账号文件，同一空间被重复路由。
	rotated := &CookieIdentity{UserID: "user-abc", TokenV2: "rotated-token"}
	if got := authIDFor(rotated, "space-paid"); got != first {
		t.Fatalf("token 轮换改变了账号标识: %s vs %s", got, first)
	}
}

// TestIsSafeNotionAuthPath 校验路径穿越防护。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，安全回归。
func TestIsSafeNotionAuthPath(t *testing.T) {
	dir := t.TempDir()
	setAccountsDir(dir)
	defer setAccountsDir("")

	safe := filepath.Join(dir, "notion-abc.json")
	if !isSafeNotionAuthPath(safe) {
		t.Fatalf("合法路径被判为不安全: %s", safe)
	}

	unsafePaths := []string{
		filepath.Join(dir, "..", "notion-evil.json"),
		filepath.Join(dir, "..", "..", "etc", "notion-evil.json"),
		filepath.Join(dir, "other.json"),
		"",
	}
	for _, path := range unsafePaths {
		if isSafeNotionAuthPath(path) {
			t.Fatalf("不安全路径被放行: %s", path)
		}
	}
}

// TestWriteAndReadAuthFile 校验落盘与回读一致。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，落盘回归。
func TestWriteAndReadAuthFile(t *testing.T) {
	dir := t.TempDir()
	setAccountsDir(dir)
	defer setAccountsDir("")

	auth := &StoredAuth{
		AuthID:       "notion-test",
		CookieHeader: "token_v2=t; notion_user_id=u",
		TokenV2:      "t",
		UserID:       "u",
		SpaceID:      "space-1",
		SpaceName:    "商业版",
		Spaces:       []NotionSpace{{ID: "space-1", Name: "商业版", PremiumCredits: 300}},
		Label:        "测试账号",
	}

	path := filepath.Join(dir, auth.AuthID+notionAuthFileSuffix)
	if errWrite := writeAuthFileDirect(path, auth); errWrite != nil {
		t.Fatalf("落盘失败: %v", errWrite)
	}
	if _, errStat := os.Stat(path); errStat != nil {
		t.Fatalf("文件不存在: %v", errStat)
	}

	loaded, errRead := readAuthFile(path)
	if errRead != nil {
		t.Fatalf("回读失败: %v", errRead)
	}
	if loaded.UserID != "u" || loaded.SpaceID != "space-1" {
		t.Fatalf("回读字段不一致: %+v", loaded)
	}
	if len(loaded.Spaces) != 1 || loaded.Spaces[0].PremiumCredits != 300 {
		t.Fatalf("空间额度未持久化: %+v", loaded.Spaces)
	}
	if loaded.LoadedDigest == "" {
		t.Fatal("未记录内容摘要")
	}

	list, errList := listAllAuthFiles()
	if errList != nil {
		t.Fatalf("枚举失败: %v", errList)
	}
	if len(list) != 1 {
		t.Fatalf("枚举到 %d 个账号, 期望 1", len(list))
	}

	if errDelete := deleteAuthFileDirect(auth.AuthID); errDelete != nil {
		t.Fatalf("删除失败: %v", errDelete)
	}
	if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
		t.Fatal("删除后文件仍存在")
	}
}

// TestPickSpacePrefersPremium 校验默认选择额度最高的空间。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，多 workspace 选择回归。
func TestPickSpacePrefersPremium(t *testing.T) {
	spaces := []NotionSpace{
		{ID: "free", Name: "免费版", PremiumCredits: 0},
		{ID: "paid", Name: "商业版", PremiumCredits: 300},
	}

	if got := pickSpace(spaces, ""); got == nil || got.ID != "paid" {
		t.Fatalf("默认选择 = %+v, 期望 paid", got)
	}
	if got := pickSpace(spaces, "free"); got == nil || got.ID != "free" {
		t.Fatalf("指定选择 = %+v, 期望 free", got)
	}
	if got := pickSpace(spaces, "missing"); got != nil {
		t.Fatalf("不存在的空间应返回 nil, 实际 %+v", got)
	}
}

// TestNormalizeNotionModel 校验模型名归一。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，模型名回归。
func TestNormalizeNotionModel(t *testing.T) {
	cases := map[string]string{
		"":                    defaultNotionModel,
		"notion/omodi":        "omodi",
		"omodi":               "omodi",
		"GPT-6 Luna":          "omodi",
		"gpt-6 luna":          "omodi",
		"albuquerque-quinn":   "albuquerque-quinn",
		"Opus 5.5":            "albuquerque-quinn",
		"unknown-model-name":  defaultNotionModel,
		"  notion/amla-jam  ": "amla-jam",
	}
	for input, want := range cases {
		if got := normalizeNotionModel(input); got != want {
			t.Fatalf("normalizeNotionModel(%q) = %q, 期望 %q", input, got, want)
		}
	}
}

// TestStaticModelListExcludesDisabled 校验禁用模型不注册。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，禁用过滤回归。
func TestStaticModelListExcludesDisabled(t *testing.T) {
	models := staticModelList()
	if len(models) == 0 {
		t.Fatal("静态模型列表为空")
	}

	for _, m := range models {
		if m.Name == "orlando-quinn" || m.Name == "assam-chai" || m.Name == "acai-budino-high" {
			t.Fatalf("禁用模型被注册: %s", m.Name)
		}
		if !strings.HasPrefix(m.ID, "notion/") {
			t.Fatalf("模型 ID 缺少前缀: %s", m.ID)
		}
	}
}

// TestAuthDataCarriesFullStorage 校验宿主认证数据带完整凭据。
//
// 宿主用 AuthData.StorageJSON 作为该账号的持久化存储；回传空值时
// 宿主会以自己的空存储覆盖账号文件，Cookie 与空间列表整体丢失，
// 后续所有推理都会变成「未找到可用账号」。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，凭据覆盖回归。
func TestAuthDataCarriesFullStorage(t *testing.T) {
	auth := &StoredAuth{
		Type:         providerName,
		Provider:     providerName,
		AuthID:       "notion-storage",
		CookieHeader: "token_v2=t; notion_user_id=u",
		TokenV2:      "t",
		UserID:       "u",
		SpaceID:      "space-1",
		SpaceName:    "space-1",
		Spaces:       []NotionSpace{{ID: "space-1", PremiumCredits: 300}},
	}

	data := authDataForLogin(auth)
	if len(data.StorageJSON) == 0 {
		t.Fatal("AuthData.StorageJSON 为空，宿主会覆盖掉账号凭据")
	}

	var roundTrip StoredAuth
	if errUnmarshal := json.Unmarshal(data.StorageJSON, &roundTrip); errUnmarshal != nil {
		t.Fatalf("StorageJSON 不是合法账号结构：%v", errUnmarshal)
	}
	if roundTrip.CookieHeader == "" {
		t.Fatal("StorageJSON 丢掉了 cookie_header")
	}
	if roundTrip.SpaceID != "space-1" {
		t.Fatalf("StorageJSON 丢掉了 space_id：%q", roundTrip.SpaceID)
	}
	if len(roundTrip.Spaces) != 1 || roundTrip.Spaces[0].PremiumCredits != 300 {
		t.Fatalf("StorageJSON 丢掉了工作空间额度：%+v", roundTrip.Spaces)
	}
}

// TestResolveAuthStripsJSONSuffix 校验宿主下发的 .json 标识能匹配本地账号。
//
// 宿主按凭据文件相对路径派生 AuthID（形如 notion-xxx.json），
// 本地账号标识不带扩展名，不剥离就永远取不到号。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，账号标识对齐回归。
func TestResolveAuthStripsJSONSuffix(t *testing.T) {
	dir := t.TempDir()
	setAccountsDir(dir)
	defer setAccountsDir("")

	auth := &StoredAuth{
		Type:         providerName,
		Provider:     providerName,
		AuthID:       "notion-suffix",
		CookieHeader: "token_v2=t; notion_user_id=u",
		TokenV2:      "t",
		UserID:       "u",
		SpaceID:      "space-1",
		Label:        "后缀测试",
	}
	if errWrite := writeAuthFileDirect(filepath.Join(dir, auth.AuthID+notionAuthFileSuffix), auth); errWrite != nil {
		t.Fatalf("落盘失败：%v", errWrite)
	}

	// 直接走本地池分支：storageJSON 为空，只能靠 AuthID 匹配。
	got, errResolve := resolveAuth(nil, "notion-suffix.json")
	if errResolve != nil {
		t.Fatalf("带 .json 的宿主标识应能解析到本地账号：%v", errResolve)
	}
	if got.AuthID != "notion-suffix" {
		t.Fatalf("解析到错误账号：%s", got.AuthID)
	}
}

// TestResolveAuthSkipsDisabled 校验停用账号不会被选中。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，停用过滤回归。
func TestResolveAuthSkipsDisabled(t *testing.T) {
	dir := t.TempDir()
	setAccountsDir(dir)
	defer setAccountsDir("")

	auth := &StoredAuth{
		Type:         providerName,
		Provider:     providerName,
		AuthID:       "notion-off",
		CookieHeader: "token_v2=t; notion_user_id=u",
		TokenV2:      "t",
		UserID:       "u",
		SpaceID:      "space-1",
		Disabled:     true,
		Label:        "停用测试",
	}
	if errWrite := writeAuthFileDirect(filepath.Join(dir, auth.AuthID+notionAuthFileSuffix), auth); errWrite != nil {
		t.Fatalf("落盘失败：%v", errWrite)
	}

	if _, errResolve := resolveAuth(nil, "notion-off"); errResolve == nil {
		t.Fatal("停用账号不应被解析为可用")
	}
}

// TestResolveAuthDistinguishesSpaces 校验同一凭据的两个空间账号可被分别路由。
//
// 拆分后的账号共用同一份 Cookie，唯一区别是 SpaceID。若解析层丢掉空间，
// 两个账号会拿同一个空间发请求，免费额度永远用不上。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，按空间拆分回归。
func TestResolveAuthDistinguishesSpaces(t *testing.T) {
	free := &StoredAuth{
		Type:         providerName,
		Provider:     providerName,
		AuthID:       "notion-free",
		CookieHeader: "token_v2=t; notion_user_id=u",
		TokenV2:      "t",
		UserID:       "u",
		SpaceID:      "space-free",
		SpaceName:    "Free",
		Spaces: []NotionSpace{
			{ID: "space-paid", Name: "Paid", PremiumCredits: 300},
			{ID: "space-free", Name: "Free", BasicLimit: 50, BasicUsage: 5},
		},
	}
	paid := *free
	paid.AuthID = "notion-paid"
	paid.SpaceID = "space-paid"
	paid.SpaceName = "Paid"

	rawFree, _ := json.Marshal(free)
	rawPaid, _ := json.Marshal(&paid)

	gotFree, errFree := resolveAuth(rawFree, free.AuthID)
	if errFree != nil {
		t.Fatalf("免费空间账号解析失败：%v", errFree)
	}
	gotPaid, errPaid := resolveAuth(rawPaid, paid.AuthID)
	if errPaid != nil {
		t.Fatalf("订阅空间账号解析失败：%v", errPaid)
	}

	// 关键断言：各自保留自己的空间，不能因为 Spaces[0] 是订阅空间而被改写。
	if gotFree.SpaceID != "space-free" || gotPaid.SpaceID != "space-paid" {
		t.Fatalf("空间被互相污染：free=%s paid=%s", gotFree.SpaceID, gotPaid.SpaceID)
	}
	if gotFree.AuthID != "notion-free" || gotPaid.AuthID != "notion-paid" {
		t.Fatalf("账号标识被互相污染：free=%s paid=%s", gotFree.AuthID, gotPaid.AuthID)
	}
}

// TestAccountLabelMarksTier 校验拆分后的账号备注名能区分套餐。
//
// 同一账号拆成多张卡片，若备注名相同，面板上无法辨认哪个是免费空间。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，拆分命名回归。
func TestAccountLabelMarksTier(t *testing.T) {
	paidSpace := NotionSpace{ID: "s1", PremiumCredits: 300}
	freeSpace := NotionSpace{ID: "s2", PremiumCredits: 0}

	if got := accountLabel("主账号", paidSpace, "notion-a"); got != "主账号 · 订阅" {
		t.Fatalf("订阅空间备注名错误：%s", got)
	}
	if got := accountLabel("主账号", freeSpace, "notion-b"); got != "主账号 · 免费" {
		t.Fatalf("免费空间备注名错误：%s", got)
	}
	if got := accountLabel("", freeSpace, "notion-abcdef123456"); !strings.Contains(got, "免费") {
		t.Fatalf("空备注名也应带套餐标记：%s", got)
	}
}

// TestRetireStaleAccountsArchivesSameUser 校验拆分后旧账号被归档而不是并行路由。
//
// 旧账号与新账号指向同一空间，留着会被重复路由、并发消耗同一份额度。
// 归档必须是改名而不是删除，用户数据不能丢。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，旧账号迁移回归。
func TestRetireStaleAccountsArchivesSameUser(t *testing.T) {
	dir := t.TempDir()
	setAccountsDir(dir)
	defer setAccountsDir("")

	legacy := &StoredAuth{
		Type:         providerName,
		Provider:     providerName,
		AuthID:       "notion-legacy",
		CookieHeader: "token_v2=old; notion_user_id=u",
		TokenV2:      "old",
		UserID:       "u",
		SpaceID:      "space-paid",
	}
	kept := &StoredAuth{
		Type:         providerName,
		Provider:     providerName,
		AuthID:       "notion-kept",
		CookieHeader: "token_v2=old; notion_user_id=u",
		TokenV2:      "old",
		UserID:       "u",
		SpaceID:      "space-paid",
	}
	other := &StoredAuth{
		Type:         providerName,
		Provider:     providerName,
		AuthID:       "notion-other",
		CookieHeader: "token_v2=x; notion_user_id=other-user",
		TokenV2:      "x",
		UserID:       "other-user",
		SpaceID:      "space-paid",
	}
	for _, a := range []*StoredAuth{legacy, kept, other} {
		path := filepath.Join(dir, a.AuthID+notionAuthFileSuffix)
		if errWrite := writeAuthFileDirect(path, a); errWrite != nil {
			t.Fatalf("落盘失败：%v", errWrite)
		}
	}

	retireStaleAccounts(&CookieIdentity{UserID: "u"}, map[string]bool{"notion-kept": true})

	if _, errStat := os.Stat(filepath.Join(dir, "notion-legacy.json")); errStat == nil {
		t.Fatal("同用户的旧账号应被归档，否则会被重复路由")
	}
	if _, errStat := os.Stat(filepath.Join(dir, "notion-legacy.json.migrated")); errStat != nil {
		t.Fatal("旧账号只应改名归档，不能删除用户数据")
	}
	if _, errStat := os.Stat(filepath.Join(dir, "notion-kept.json")); errStat != nil {
		t.Fatal("本次拆分保留的账号不能被归档")
	}
	if _, errStat := os.Stat(filepath.Join(dir, "notion-other.json")); errStat != nil {
		t.Fatal("其他用户的账号不能被归档")
	}
}

// TestMapLegacyAccountByUserAndSpace 校验拆分前的旧账号记录能映射到新账号。
//
// 宿主持久化存储里可能仍保留拆分前的账号记录并继续派发；这些记录与新账号
// 指向同一空间，按 user_id + space_id 映射回去即可，升级后不会报错。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，升级兼容回归。
func TestMapLegacyAccountByUserAndSpace(t *testing.T) {
	dir := t.TempDir()
	setAccountsDir(dir)
	defer setAccountsDir("")

	paid := &StoredAuth{
		Type: providerName, Provider: providerName, AuthID: "notion-paid",
		CookieHeader: "token_v2=new; notion_user_id=u", TokenV2: "new",
		UserID: "u", SpaceID: "space-paid",
	}
	free := &StoredAuth{
		Type: providerName, Provider: providerName, AuthID: "notion-free",
		CookieHeader: "token_v2=new; notion_user_id=u", TokenV2: "new",
		UserID: "u", SpaceID: "space-free",
	}
	for _, a := range []*StoredAuth{paid, free} {
		if errWrite := writeAuthFileDirect(filepath.Join(dir, a.AuthID+notionAuthFileSuffix), a); errWrite != nil {
			t.Fatalf("落盘失败：%v", errWrite)
		}
	}

	// 旧记录：旧 authID、旧 token、同样的 user 与 space。
	legacy := &StoredAuth{
		AuthID: "notion-legacy", UserID: "u", SpaceID: "space-free",
	}
	got := mapLegacyAccount(legacy)
	if got == nil || got.AuthID != "notion-free" {
		t.Fatalf("旧记录应映射到免费空间账号，实际：%+v", got)
	}

	// 空间对不上时不能随便挑一个账号，否则会把请求打到错误空间。
	if got := mapLegacyAccount(&StoredAuth{UserID: "u", SpaceID: "space-missing"}); got != nil {
		t.Fatalf("空间不匹配时不应映射：%s", got.AuthID)
	}
	// 缺 user 或 space 时也不映射。
	if got := mapLegacyAccount(&StoredAuth{SpaceID: "space-free"}); got != nil {
		t.Fatal("缺少 user_id 时不应映射")
	}
}

// TestPremiumUsageOf 校验订阅额度的周期用量推导。
//
// 上游只给余额与上限，已用量必须由两者相减；余额高于上限（赠送、补额）
// 时不能算出负数，否则进度条会显示成负宽度。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，订阅额度口径回归。
func TestPremiumUsageOf(t *testing.T) {
	cases := []struct {
		name    string
		limit   int
		balance int
		want    int
	}{
		{name: "正常消耗", limit: 300, balance: 168, want: 132},
		{name: "额度用尽", limit: 300, balance: 0, want: 300},
		{name: "余额倒挂", limit: 300, balance: 400, want: 0},
		{name: "无订阅", limit: 0, balance: 0, want: 0},
	}

	for _, tc := range cases {
		if got := premiumUsageOf(tc.limit, tc.balance); got != tc.want {
			t.Fatalf("%s：已用 = %d, 期望 %d", tc.name, got, tc.want)
		}
	}
}

// TestPremiumCeilingFallsBackToBalance 校验旧快照没有上限时回落到余额。
//
// 0.2.1 及更早落盘的账号没有 premium_limit 字段，升级后若直接读上限，
// 所有订阅空间都会被当成免费空间。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，旧快照兼容回归。
func TestPremiumCeilingFallsBackToBalance(t *testing.T) {
	if got := premiumCeiling(&NotionSpace{PremiumCredits: 300}); got != 300 {
		t.Fatalf("旧快照应回落到余额，实际 %d", got)
	}
	if got := premiumCeiling(&NotionSpace{PremiumCredits: 300, PremiumLimit: 300}); got != 300 {
		t.Fatalf("有上限时应取上限，实际 %d", got)
	}
	// 额度用尽：余额 0 但上限仍在，必须仍判为订阅空间。
	if got := premiumCeiling(&NotionSpace{PremiumCredits: 0, PremiumLimit: 300}); got != 300 {
		t.Fatalf("额度用尽后上限不能丢，实际 %d", got)
	}
}

// TestAccountLabelKeepsTierWhenExhausted 校验额度用尽后账号仍标记为订阅。
//
// 若按余额判定，额度耗尽后重新导入会把订阅卡标成「免费」，
// 用户会误以为订阅空间丢失。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，额度耗尽后的套餐标记回归。
func TestAccountLabelKeepsTierWhenExhausted(t *testing.T) {
	exhausted := NotionSpace{ID: "s1", PremiumCredits: 0, PremiumLimit: 300}
	if got := accountLabel("主账号", exhausted, "notion-a"); got != "主账号 · 订阅" {
		t.Fatalf("额度用尽仍应标记订阅，实际：%s", got)
	}
}

// TestApplyUsageEligibilityUsesThreeFields 校验额度字段按各自语义写入。
//
// 生产实测同一份响应里：上限在 limits.purchased.totalLimit(=300)、余额在
// premiumCredits.totalCreditBalance(=300)、基础计数在 basicCredits(=152/50)。
// 若误用 basicCredits 当订阅额度，面板会显示成 152/50 满额红灯。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，额度字段口径回归。
func TestApplyUsageEligibilityUsesThreeFields(t *testing.T) {
	raw := `{
  "basicCredits": {"spaceUsage": 152, "userUsage": 152, "spaceLimit": 50, "userLimit": 50},
  "limits": {"purchased": {"totalLimit": 300}},
  "premiumCredits": {"totalCreditBalance": 168, "creditsInOverage": 0,
    "perSource": {"monthlyAllocated": {"usageTotal": 0, "limit": 300}}},
  "usage": {"currentServicePeriod": {"spaceUsage": 0, "userUsage": 0},
    "lifetime": {"spaceUsage": 152, "userUsage": 152}, "totalCreditBalance": 168}
}`
	var resp usageEligibilityResponse
	if errUnmarshal := json.Unmarshal([]byte(raw), &resp); errUnmarshal != nil {
		t.Fatalf("解析失败：%v", errUnmarshal)
	}

	space := &NotionSpace{ID: "space-paid"}
	applyUsageEligibility(space, &resp)

	if space.PremiumLimit != 300 {
		t.Fatalf("订阅上限应取 limits.purchased.totalLimit=300，实际 %d", space.PremiumLimit)
	}
	if space.PremiumCredits != 168 {
		t.Fatalf("订阅余额应取 totalCreditBalance=168，实际 %d", space.PremiumCredits)
	}
	if space.PremiumUsage != 132 {
		t.Fatalf("订阅已用应推导为 132，实际 %d", space.PremiumUsage)
	}
	// 基础计数原样保留，但它不能冒充订阅上限。
	if space.BasicLimit != 50 || space.BasicUsage != 152 {
		t.Fatalf("基础计数写入错误：%d / %d", space.BasicUsage, space.BasicLimit)
	}
	if space.UsageCheckedAt == "" {
		t.Fatal("未记录快照时间")
	}
}

// rewriteHostTransport 把请求转发到测试服务，保留原路径与请求体。
type rewriteHostTransport struct {
	target string
}

// RoundTrip 实现 http.RoundTripper。
// [参数] req: 原始请求。
// [返回] 测试服务的响应与错误。
// 最近修改时间：2026-10-19；改动原因：新增，额度刷新测试的上游替身。
func (t rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	parsed, errParse := url.Parse(t.target)
	if errParse != nil {
		return nil, errParse
	}
	clone.URL.Scheme = parsed.Scheme
	clone.URL.Host = parsed.Host
	clone.Host = parsed.Host
	return http.DefaultTransport.RoundTrip(clone)
}

// TestRefreshUsageForAuthPersists 校验刷新额度会把上游新值写回账号文件。
//
// 只刷新内存不落盘时，面板下一次读 /accounts 又会退回导入时的旧快照，
// 用户看到的仍然是 02:19 那组数字。这条链路必须端到端可验证。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，刷新落盘回归。
func TestRefreshUsageForAuthPersists(t *testing.T) {
	dir := t.TempDir()
	setAccountsDir(dir)
	defer setAccountsDir("")

	paid := &StoredAuth{
		Type: providerName, Provider: providerName, AuthID: "notion-paid",
		CookieHeader: "token_v2=t; notion_user_id=u", TokenV2: "t",
		UserID: "u", SpaceID: "space-paid",
		Spaces:    []NotionSpace{{ID: "space-paid", PremiumCredits: 300, BasicUsage: 152}},
		CreatedAt: "2026-10-19T02:19:04+08:00",
	}
	path := filepath.Join(dir, paid.AuthID+notionAuthFileSuffix)
	if errWrite := writeAuthFileDirect(path, paid); errWrite != nil {
		t.Fatalf("落盘失败：%v", errWrite)
	}

	// 上游替身：返回订阅余额 168 / 上限 300。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "basicCredits": {"spaceUsage": 152, "spaceLimit": 50},
  "limits": {"purchased": {"totalLimit": 300}},
  "premiumCredits": {"totalCreditBalance": 168}
}`))
	}))
	defer srv.Close()

	// 上游前缀是常量，用改写 URL 的传输层把请求引到测试服务。
	origClient := usageHTTPClientFn
	usageHTTPClientFn = func(timeout time.Duration) *http.Client {
		return &http.Client{Transport: rewriteHostTransport{target: srv.URL}}
	}
	defer func() { usageHTTPClientFn = origClient }()

	auth, errFind := findAccount("notion-paid")
	if errFind != nil {
		t.Fatalf("查找账号失败：%v", errFind)
	}
	if errRefresh := refreshUsageForAuth(auth); errRefresh != nil {
		t.Fatalf("刷新额度失败：%v", errRefresh)
	}

	reloaded, errRead := readAuthFile(path)
	if errRead != nil {
		t.Fatalf("回读失败：%v", errRead)
	}
	if len(reloaded.Spaces) != 1 {
		t.Fatalf("空间数量异常：%+v", reloaded.Spaces)
	}
	space := reloaded.Spaces[0]
	if space.PremiumCredits != 168 || space.PremiumLimit != 300 || space.PremiumUsage != 132 {
		t.Fatalf("刷新结果未落盘：%+v", space)
	}
	if space.UsageCheckedAt == "" || space.UsageCheckedAt == "2026-10-19T02:19:04+08:00" {
		t.Fatalf("快照时间未更新：%s", space.UsageCheckedAt)
	}
}

// TestPersistUsageSnapshotSyncsSiblings 校验刷新额度会写回同账号的全部空间文件。
//
// 同一份 Cookie 拆成多个账号文件，每个文件都保存完整空间列表；只写当前
// 文件会让另一张卡片一直显示旧数字。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，额度快照跨空间同步回归。
func TestPersistUsageSnapshotSyncsSiblings(t *testing.T) {
	dir := t.TempDir()
	setAccountsDir(dir)
	defer setAccountsDir("")

	paid := &StoredAuth{
		Type: providerName, Provider: providerName, AuthID: "notion-paid",
		CookieHeader: "token_v2=t; notion_user_id=u", TokenV2: "t",
		UserID: "u", SpaceID: "space-paid",
		Spaces: []NotionSpace{{ID: "space-paid", PremiumCredits: 300, BasicUsage: 152}},
	}
	free := &StoredAuth{
		Type: providerName, Provider: providerName, AuthID: "notion-free",
		CookieHeader: "token_v2=t; notion_user_id=u", TokenV2: "t",
		UserID: "u", SpaceID: "space-free",
		Spaces: []NotionSpace{{ID: "space-free", BasicLimit: 50, BasicUsage: 5}},
	}
	other := &StoredAuth{
		Type: providerName, Provider: providerName, AuthID: "notion-other",
		CookieHeader: "token_v2=o; notion_user_id=other", TokenV2: "o",
		UserID: "other", SpaceID: "space-x",
		Spaces: []NotionSpace{{ID: "space-x", BasicLimit: 50, BasicUsage: 1}},
	}
	for _, a := range []*StoredAuth{paid, free, other} {
		if errWrite := writeAuthFileDirect(filepath.Join(dir, a.AuthID+notionAuthFileSuffix), a); errWrite != nil {
			t.Fatalf("落盘失败：%v", errWrite)
		}
	}

	fresh := []NotionSpace{
		{ID: "space-paid", PremiumCredits: 168, PremiumLimit: 300, PremiumUsage: 132},
		{ID: "space-free", BasicLimit: 50, BasicUsage: 6},
	}
	updated, errFind := findAccount("notion-paid")
	if errFind != nil {
		t.Fatalf("查找账号失败：%v", errFind)
	}
	updated.Spaces = fresh
	if errPersist := persistUsageSnapshot(updated); errPersist != nil {
		t.Fatalf("写回额度失败：%v", errPersist)
	}

	for _, id := range []string{"notion-paid", "notion-free"} {
		got, errRead := readAuthFile(filepath.Join(dir, id+notionAuthFileSuffix))
		if errRead != nil {
			t.Fatalf("%s 回读失败：%v", id, errRead)
		}
		if len(got.Spaces) != 2 || got.Spaces[0].PremiumLimit != 300 || got.Spaces[1].BasicUsage != 6 {
			t.Fatalf("%s 额度未同步：%+v", id, got.Spaces)
		}
	}

	// 其他用户的账号不能被这次刷新改动。
	untouched, errRead := readAuthFile(filepath.Join(dir, "notion-other"+notionAuthFileSuffix))
	if errRead != nil {
		t.Fatalf("其他账号回读失败：%v", errRead)
	}
	if untouched.Spaces[0].BasicUsage != 1 {
		t.Fatalf("其他用户的额度被误改：%+v", untouched.Spaces)
	}
}
