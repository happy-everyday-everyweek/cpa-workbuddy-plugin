package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// 本文件锁定「积分路由红线」契约。
//
// 扣子不声明 scheduler 能力（宿主只调用排序后第一个 scheduler 插件），
// 因此低额账号只能由执行入口拦截：返回 403 让宿主按账号级故障换号，
// 与上游 1000521「积分耗尽」走同一条已验证通道。
//
// 阈值 100 是扣子专用：单次对话消耗远高于兄弟插件的按次计费套餐，
// 沿用 20 只够一两次请求，起不到缓冲作用。

// seedAccounts 在临时账号目录写入多个账号并返回落盘结果。
//
// 与 active_ping_test.go 的 seedAccount 不同，这里必须能建出多个账号：
// 「全池兜底」用例的关键就是池里只剩一个低额号时不得拦截。
// [参数] t: 测试上下文；remaining: 每个账号的剩余积分，按顺序生成。
// [返回] 已落盘的账号列表。
func seedAccounts(t *testing.T, remaining ...int64) []*StoredAuth {
	t.Helper()
	dir := t.TempDir()
	setAccountsDir(dir)
	t.Cleanup(func() { setAccountsDir("") })

	out := make([]*StoredAuth, 0, len(remaining))
	for i, value := range remaining {
		authID := "coze-r" + string(rune('a'+i))
		sa := &StoredAuth{
			Type:         providerName,
			Provider:     providerName,
			AuthID:       authID,
			CookieHeader: "sessionid=test-session; sid_tt=test-sid",
			SecUserID:    "sec-" + authID,
			Label:        "测试账号" + authID,
			CreatedAt:    "2026-10-20T00:00:00+08:00",
			UpdatedAt:    "2026-10-20T00:00:00+08:00",
		}
		if errWrite := writeAuthFileDirect(filepath.Join(dir, authID+".json"), sa); errWrite != nil {
			t.Fatalf("写入账号夹具失败：%v", errWrite)
		}
		storeCreditSummary(sa, &creditSummary{
			Total:     10000,
			Remain:    value,
			FetchedAt: time.Now().Format(time.RFC3339),
		})
		out = append(out, sa)
	}
	// 快照由积分循环在生产路径发布，测试夹具必须模拟这一步：
	// 否则闸门读到空池，会因「没有其他可用号」而一律放行，
	// 所有拦截类用例都会假通过。
	publishCreditPool(out)
	t.Cleanup(resetCreditCache)
	t.Cleanup(func() { publishCreditPool(nil) })
	return out
}

// TestMinRoutableCreditsDefaultIs100 锁定扣子的红线默认值是 100。
//
// 这个常量与兄弟插件的 20 不同，是产品判断而不是笔误，
// 用例把它钉住以防后续有人「统一口径」时误改回去。
// [参数] t: 测试上下文。
// [返回] 无。
func TestMinRoutableCreditsDefaultIs100(t *testing.T) {
	if defaultMinRoutableCredits != 100 {
		t.Fatalf("扣子积分红线默认值应为 100，实际 %d", defaultMinRoutableCredits)
	}
	if got := defaultConfig().MinRoutableCredits; got == nil || *got != 100 {
		t.Fatalf("默认配置的红线应为 100，实际 %v", got)
	}
}

// TestIsCreditsBelowRedlineBoundary 验证红线边界的精确判据。
//
// 等于红线视为可用（`<` 而不是 `<=`）：红线是「低于此值不再放行」，
// 恰好 100 分仍应承载请求。
// [参数] t: 测试上下文。
// [返回] 无。
func TestIsCreditsBelowRedlineBoundary(t *testing.T) {
	dir := t.TempDir()
	setAccountsDir(dir)
	t.Cleanup(func() { setAccountsDir("") })
	t.Cleanup(resetCreditCache)
	resetCreditCache()

	sa := &StoredAuth{AuthID: "coze-boundary"}
	storeCreditSummary(sa, &creditSummary{Remain: 100, Total: 1000})

	if isCreditsBelowRedline(sa.AuthID) {
		t.Fatal("恰好等于红线不应判定为低额")
	}
	storeCreditSummary(sa, &creditSummary{Remain: 99, Total: 1000})
	if !isCreditsBelowRedline(sa.AuthID) {
		t.Fatal("低于红线必须判定为低额")
	}
}

// TestUnknownCreditsAreTreatedAsRoutable 验证查不到积分时放行。
//
// 积分接口抖动时若把账号判成低额，整池会在同一时刻同时不可用；
// 多打一次上游的代价远小于此，因此未知一律放行。
// 两条路径都要覆盖：判定函数本身，以及真正决定放行的闸门入口。
// [参数] t: 测试上下文。
// [返回] 无。
func TestUnknownCreditsAreTreatedAsRoutable(t *testing.T) {
	t.Cleanup(resetCreditCache)
	resetCreditCache()

	if isCreditsBelowRedline("coze-never-fetched") {
		t.Fatal("未查询过积分的账号不得被判为低额")
	}
	// 查询失败写入的摘要（Error 非空）同样视为未知。
	storeCreditSummary(&StoredAuth{AuthID: "coze-failed"}, &creditSummary{Error: "boom"})
	if isCreditsBelowRedline("coze-failed") {
		t.Fatal("积分查询失败不得被判为低额")
	}

	// 闸门入口：未知积分的号即使池里有健康号也必须放行，
	// 否则「查不到余额」会被当成「余额为 0」。
	accounts := seedAccounts(t, 5000)
	unknown := &StoredAuth{AuthID: "coze-unknown"}
	if errVerify := verifyAccountCredits(unknown); errVerify != nil {
		t.Fatalf("未知积分的账号必须放行，实际报错：%v", errVerify)
	}
	_ = accounts
}

// TestVerifyAccountCreditsSwitchesWhenPoolHasHealthy 验证池里还有达标号时拦截。
//
// 这是闸门存在意义的核心场景：低额号被宿主选中 → 直接 403 换号，
// 请求根本不打上游。
// [参数] t: 测试上下文。
// [返回] 无。
func TestVerifyAccountCreditsSwitchesWhenPoolHasHealthy(t *testing.T) {
	accounts := seedAccounts(t, 30, 5000)
	low := accounts[0]

	errVerify := verifyAccountCredits(low)
	if errVerify == nil {
		t.Fatal("低额账号在有健康号可选时必须拦截")
	}
	coder, ok := errVerify.(statusCoder)
	if !ok {
		t.Fatalf("拦截错误必须实现 StatusCode，实际类型 %T", errVerify)
	}
	if coder.StatusCode() != http.StatusForbidden {
		t.Fatalf("必须返回 403 触发宿主换号，实际 %d", coder.StatusCode())
	}
}

// TestVerifyAccountCreditsAllowsWhenAllLow 验证全池都低时放行。
//
// 只剩低额号时仍要允许请求：把最后一个残血号也挡掉，
// 用户会直接看到无号可用，而实际上游可能还能跑完这一次。
// [参数] t: 测试上下文。
// [返回] 无。
func TestVerifyAccountCreditsAllowsWhenAllLow(t *testing.T) {
	accounts := seedAccounts(t, 30, 50)
	if errVerify := verifyAccountCredits(accounts[0]); errVerify != nil {
		t.Fatalf("全池低额时必须放行，实际报错：%v", errVerify)
	}
}

// TestVerifyAccountCreditsAllowsHealthyAccount 验证达标账号直接放行。
// [参数] t: 测试上下文。
// [返回] 无。
func TestVerifyAccountCreditsAllowsHealthyAccount(t *testing.T) {
	accounts := seedAccounts(t, 5000, 30)
	if errVerify := verifyAccountCredits(accounts[0]); errVerify != nil {
		t.Fatalf("达标账号不应被拦截：%v", errVerify)
	}
}

// TestVerifyAccountCreditsIgnoresDisabledAndExpired 验证兜底不吃禁用/失效号。
//
// 「池里还有可用号」必须按真实可用性判定：禁用号、会话失效号都不该
// 被算作换号目的地，否则会把用户从低额号换到一个根本用不了的号。
// [参数] t: 测试上下文。
// [返回] 无。
func TestVerifyAccountCreditsIgnoresDisabledAndExpired(t *testing.T) {
	accounts := seedAccounts(t, 30, 5000)
	// 把唯一的健康号改坏：禁用 + 会话失效。
	dir := getAccountsDir()
	healthy := accounts[1]
	healthy.Disabled = true
	healthy.AuthInvalid = true
	if errWrite := writeAuthFileDirect(filepath.Join(dir, healthy.AuthID+".json"), healthy); errWrite != nil {
		t.Fatalf("改写账号夹具失败：%v", errWrite)
	}
	// 快照必须跟着账号状态更新，否则闸门看到的仍是改坏之前的状态。
	publishCreditPool(accounts)

	if errVerify := verifyAccountCredits(accounts[0]); errVerify != nil {
		t.Fatalf("没有其他可用号时必须放行，实际报错：%v", errVerify)
	}
}

// TestCreditsBelowRedlineErrorCarries403 锁定拦截错误的状态码语义。
// [参数] t: 测试上下文。
// [返回] 无。
func TestCreditsBelowRedlineErrorCarries403(t *testing.T) {
	errCredits := &creditsBelowRedlineError{remain: 12, redline: 100}
	if errCredits.StatusCode() != http.StatusForbidden {
		t.Fatalf("必须返回 403，实际 %d", errCredits.StatusCode())
	}
	if msg := errCredits.Error(); msg == "" {
		t.Fatal("错误文案不能为空")
	}
}

// TestConfigurePluginParsesRoutableCredits 验证配置项解析与零值语义。
//
// 关键点：缺省键保持默认 100，显式 0 才是关闭闸门的 kill switch。
// 若用值类型，两种情况都会塌缩成 0，闸门会静默失效。
// [参数] t: 测试上下文。
// [返回] 无。
func TestConfigurePluginParsesRoutableCredits(t *testing.T) {
	t.Cleanup(setMinRoutableCredits(defaultMinRoutableCredits))
	t.Cleanup(func() { activeConfig.Store(defaultConfig()) })

	// 缺省键：保持默认红线。
	payload, _ := json.Marshal(map[string][]byte{"config_yaml": []byte("timeout_seconds: 30\n")})
	if errConfigure := configurePlugin(payload); errConfigure != nil {
		t.Fatalf("配置解析失败：%v", errConfigure)
	}
	if got := loadedMinRoutableCredits(); got != 100 {
		t.Fatalf("缺省键应保持默认 100，实际 %d", got)
	}

	// 显式配置：覆盖默认值。
	payload, _ = json.Marshal(map[string][]byte{"config_yaml": []byte("min_routable_credits: 250\n")})
	if errConfigure := configurePlugin(payload); errConfigure != nil {
		t.Fatalf("配置解析失败：%v", errConfigure)
	}
	if got := loadedMinRoutableCredits(); got != 250 {
		t.Fatalf("应生效为 250，实际 %d", got)
	}

	// 显式 0：关闭闸门（kill switch）。
	payload, _ = json.Marshal(map[string][]byte{"config_yaml": []byte("min_routable_credits: 0\n")})
	if errConfigure := configurePlugin(payload); errConfigure != nil {
		t.Fatalf("配置解析失败：%v", errConfigure)
	}
	if got := loadedMinRoutableCredits(); got != 0 {
		t.Fatalf("显式 0 应关闭闸门，实际 %d", got)
	}
}

// TestRunCreditTickStoresAndIsolatesFailures 验证刷新循环的写入与失败隔离。
//
// 单账号失败不得影响其他账号，也不得写入错误摘要污染闸门。
// [参数] t: 测试上下文。
// [返回] 无。
func TestRunCreditTickStoresAndIsolatesFailures(t *testing.T) {
	t.Cleanup(resetCreditCache)
	resetCreditCache()

	good := &StoredAuth{AuthID: "coze-good"}
	bad := &StoredAuth{AuthID: "coze-bad"}
	deps := creditDeps{
		list: func() ([]*StoredAuth, error) { return []*StoredAuth{good, bad}, nil },
		fetch: func(sa *StoredAuth) (*creditSummary, error) {
			if sa.AuthID == "coze-bad" {
				return nil, http.ErrHandlerTimeout
			}
			return &creditSummary{Total: 1000, Remain: 777}, nil
		},
		store: storeCreditSummary,
		logf:  func(string, string, map[string]any) {},
	}

	if got := runCreditTick(deps); got != 1 {
		t.Fatalf("应只有 1 个账号刷新成功，实际 %d", got)
	}
	if remain, known := cachedCreditRemain("coze-good"); !known || remain != 777 {
		t.Fatalf("成功账号的积分应写入缓存，实际 remain=%d known=%v", remain, known)
	}
	if _, known := cachedCreditRemain("coze-bad"); known {
		t.Fatal("失败账号不得写入缓存")
	}
}

// TestRunCreditTickPublishesPoolSnapshot 验证刷新结束时真的发布了闸门快照。
//
// 闸门只读快照（见 policy.go），而快照只有这里能更新：少了这一步，
// 余额变化永远不会反映到闸门，低额账号会一直被当成健康号放行。
// 断言方式是「刷新前后闸门结论翻转」——只看返回值无法区分
// 「积分写进缓存」与「快照也跟着发布」两件事。
// [参数] t: 测试上下文。
// [返回] 无。
func TestRunCreditTickPublishesPoolSnapshot(t *testing.T) {
	t.Cleanup(resetCreditCache)
	t.Cleanup(func() { publishCreditPool(nil) })
	resetCreditCache()
	publishCreditPool(nil)

	healthy := &StoredAuth{AuthID: "coze-p1", CookieHeader: "sessionid=x", Label: "h"}
	low := &StoredAuth{AuthID: "coze-p2", CookieHeader: "sessionid=y", Label: "l"}

	// 初始：两个号都充足，闸门不会拦任何号。
	storeCreditSummary(healthy, &creditSummary{Total: 10000, Remain: 9000})
	storeCreditSummary(low, &creditSummary{Total: 10000, Remain: 9000})
	deps := creditDeps{
		list: func() ([]*StoredAuth, error) { return []*StoredAuth{healthy, low}, nil },
		fetch: func(sa *StoredAuth) (*creditSummary, error) {
			return &creditSummary{Total: 10000, Remain: 9000}, nil
		},
		store: storeCreditSummary,
		logf:  func(string, string, map[string]any) {},
	}
	runCreditTick(deps)
	if errCredits := verifyAccountCredits(low); errCredits != nil {
		t.Fatalf("两个号都充足时不应拦截：%v", errCredits)
	}

	// 第二轮：low 掉到红线以下，快照必须跟着更新。
	deps.fetch = func(sa *StoredAuth) (*creditSummary, error) {
		if sa.AuthID == low.AuthID {
			return &creditSummary{Total: 10000, Remain: 10}, nil
		}
		return &creditSummary{Total: 10000, Remain: 9000}, nil
	}
	runCreditTick(deps)
	if errCredits := verifyAccountCredits(low); errCredits == nil {
		t.Fatal("刷新后低额号必须被拦截：快照未随缓存更新")
	}
}

// TestHandleExecExecuteRejectsLowCreditBeforeUpstream 验证执行入口真的拦截。
//
// 断言的是「没有发出上游请求」：只要闸门晚于建连，请求就已经白跑，
// 低额账号的存在也就失去了意义。
// [参数] t: 测试上下文。
// [返回] 无。
func TestHandleExecExecuteRejectsLowCreditBeforeUpstream(t *testing.T) {
	accounts := seedAccounts(t, 30, 5000)
	low := accounts[0]

	env := newExecutorTestEnv(t, func(conn *websocket.Conn) {})
	upstreamCalls := 0
	env.doer.handler = func(*http.Request, []byte) (*http.Response, error) {
		upstreamCalls++
		return jsonResponse(200, `{}`), nil
	}

	payload, _ := json.Marshal(map[string]any{
		"model":    "coze/doubao-seed-2-0-lite-260215",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	storage, _ := json.Marshal(low)
	raw, _ := json.Marshal(pluginapi.ExecutorRequest{
		Payload:     payload,
		StorageJSON: storage,
	})

	_, errExec := handleExecExecute(raw)
	if errExec == nil {
		t.Fatal("低额账号必须被入口拦截")
	}
	coder, ok := errExec.(statusCoder)
	if !ok {
		t.Fatalf("拦截错误必须实现 StatusCode，实际类型 %T", errExec)
	}
	if coder.StatusCode() != http.StatusForbidden {
		t.Fatalf("必须返回 403，实际 %d", coder.StatusCode())
	}
	if upstreamCalls != 0 {
		t.Fatalf("拦截必须在触达上游前完成，实际调用 %d 次", upstreamCalls)
	}
}

// TestVerifyAccountCreditsUsesMemorySnapshotOnly 锁定闸门不得发起宿主 RPC。
//
// 闸门位于每个请求的执行入口。若它调用 host.auth.list，每次推理都会
// 多出「一次列目录 + 每账号一次读取」的 RPC 扇出，把首字时间拖慢数毫秒
// 到数十毫秒。这里用「清空账号目录」制造反证：只要闸门还依赖磁盘/宿主，
// 清空后它就读不到池子，会退化成放行；只有纯内存快照才能继续拦截。
// [参数] t: 测试上下文。
// [返回] 无。
func TestVerifyAccountCreditsUsesMemorySnapshotOnly(t *testing.T) {
	accounts := seedAccounts(t, 30, 5000)
	low := accounts[0]

	// 抹掉磁盘账号：宿主 RPC 与目录扫描都会失败或返回空。
	dir := getAccountsDir()
	if errRemove := os.RemoveAll(dir); errRemove != nil {
		t.Fatalf("清理账号目录失败：%v", errRemove)
	}

	if errVerify := verifyAccountCredits(low); errVerify == nil {
		t.Fatal("闸门必须只依赖内存快照，账号目录消失不应让它失效")
	}
}
