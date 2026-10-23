// growth.go 是 WorkBuddy 成长中心（CN 活动页）的 HTTP 客户端骨架：端点常量、
// 共用 GET/POST 调用器、幂等 token 与脱敏助手。
//
// 契约来源：2026-10-24 真实账号实测冻结，详见
// doc/2-需求/2026-10-24_120000_workbuddy成长中心重新接入需求.md §3。
//
// 写语义：GET 遇上游 5xx 重试；POST 一律不重试——成长中心的写端点会真实发放
// 积分、消耗补登卡与抽奖次数，盲重试可能重复扣减。
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// growthPathPrefix 是成长中心全部端点的公共前缀，拼接在账号所属 billing base 之后。
const growthPathPrefix = "/v2/activity/growth"

// 成长中心端点相对路径。GET 为只读查询，POST 为会改变账号状态的写操作。
const (
	growthPathStreak         = "/streak"
	growthPathHeatmap        = "/heatmap"
	growthPathEnergy         = "/energy"
	growthPathRedeemSummary  = "/redeem/summary"
	growthPathRedeem         = "/redeem"
	growthPathLotteryChances = "/lottery/chances"
	growthPathLotteryDraw    = "/lottery/draw"
	growthPathMakeupCardsUse = "/makeup-cards/use"
	growthPathTasks          = "/tasks"
	growthPathTasksAccept    = "/tasks/accept"
	growthPathTravelStatus   = "/buddy/travel/status"
	growthPathTravelConfig   = "/buddy/travel/config"
	growthPathTravelClaim    = "/buddy/travel/claim"
	growthPathTravelDepart   = "/buddy/travel/depart"
	growthPathBuddyQuota     = "/buddy/quota"
	growthPathBuddyOpen      = "/buddy/open"
	growthPathBuddyAgreement = "/buddy/agreement"
	growthPathBuddyFirst     = "/buddy/first"
)

// growthRedeemTiers 是连登阶梯兑换的合法档位。
var growthRedeemTiers = map[string]bool{"7d": true, "14d": true, "28d": true}

// growthBaseFor 返回账号所属区域的成长中心 API base。
// [参数] sa：账号凭据；nil 时回落 CN base。
// [返回] {billingBase}/v2/activity/growth 形式的完整前缀。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func growthBaseFor(sa *storedAuth) string {
	return billingBaseFor(sa) + growthPathPrefix
}

// growthCall 发起一次成长中心 API 调用，按方法区分重试语义。
// [参数] sa：账号凭据；method：HTTP 方法；path：growthPathPrefix 下的相对路径；body：POST 请求体（GET 传 nil）。
// [返回] envelope 解包后的 data 原始 JSON；失败时返回分类错误。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func growthCall(sa *storedAuth, method, path string, body any) (json.RawMessage, error) {
	// 1. 首次调用
	data, err := growthCallOnce(sa, method, path, body)

	// 2. 仅 GET 重试：写操作幂等性未经证实，一律单发
	if method != http.MethodGet {
		if err != nil {
			log.Printf("[workbuddy] growth %s %s failed for %s: %v", method, path, sa.Account.UID, err)
		}
		return data, err
	}
	for _, delay := range billingRetryDelays {
		if err == nil || !isTransientBillingErr(err) {
			break
		}
		time.Sleep(delay)
		data, err = growthCallOnce(sa, method, path, body)
	}
	if err != nil {
		log.Printf("[workbuddy] growth %s %s failed for %s: %v", method, path, sa.Account.UID, err)
	}
	return data, err
}

// growthCallOnce 执行单次成长中心请求并解包 {code,msg,data} envelope。
// 与 billingCallOnce 同族但不复用函数体：billing 链路只发 POST 且始终带重试，
// 成长中心需要 GET 无 body 与写操作不重试两种语义。
// [参数] sa：账号凭据；method：HTTP 方法；path：相对路径；body：请求体（nil 时 GET 不带体、POST 发空对象）。
// [返回] data 原始 JSON；5xx / 解析失败 / 业务 code != 0 时返回带脱敏片段的错误。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func growthCallOnce(sa *storedAuth, method, path string, body any) (json.RawMessage, error) {
	// 1. 组装请求体：显式 body 优先；POST 无 body 时补空对象，GET 不带体
	var reqBody io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(raw)
	} else if method == http.MethodPost {
		reqBody = bytes.NewReader([]byte("{}"))
	}

	// 2. 复用 billing 鉴权头发起请求；走 host.http.do 让宿主请求日志可观测
	req, err := http.NewRequest(method, growthBaseFor(sa)+path, reqBody)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}

	// 3. 5xx 归类为瞬时错误供 GET 重试；其余状态由 envelope 判定
	raw := resp.Body
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("http %d from %s: %s", resp.StatusCode, path, growthSnippet(raw))
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, growthSnippet(raw))
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("code=%d msg=%s", env.Code, truncateRedacted(env.Msg, 120))
	}
	return env.Data, nil
}

// growthSnippet 把响应正文脱敏并截断成单行片段，供错误信息使用。
// [参数] raw：响应原始字节。
// [返回] 最长 120 字符的单行片段。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func growthSnippet(raw []byte) string {
	snippet := strings.TrimSpace(redactSecrets(string(raw)))
	if len(snippet) > 120 {
		snippet = snippet[:120]
	}
	return snippet
}

// growthGetMap 执行一次只读 GET 并把 data 解成顶层 map。
// [参数] sa：账号凭据；path：growth 相对路径。
// [返回] 顶层字段 map；失败时返回错误。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func growthGetMap(sa *storedAuth, path string) (map[string]any, error) {
	data, err := growthCall(sa, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// growthPostMap 执行一次写操作 POST 并把 data 解成顶层 map。
// [参数] sa：账号凭据；path：growth 相对路径；body：请求体。
// [返回] 顶层字段 map；失败时返回错误。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func growthPostMap(sa *storedAuth, path string, body any) (map[string]any, error) {
	data, err := growthCall(sa, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	// 部分写端点成功时 data 为 null（例如活跃上报），按空 map 处理
	if len(data) == 0 || string(data) == "null" {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// growthClientToken 生成 "u-<uuid4>" 形式的幂等 token，供上游做重复请求去重。
// [参数] 无。
// [返回] "u-" 前缀的 UUID v4 字符串；随机源失败时退化为时间戳纳秒串。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func growthClientToken() string {
	// 1. 取随机源；失败时退化时间戳串，保证仍能返回可去重 token
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("u-%d", time.Now().UnixNano())
	}
	// 2. 按 RFC 4122 置版本位与变体位
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("u-%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// claimGrowthTaskPath 拼接领奖路径：任务码需转义后拼进路径。
// [参数] code：任务码。
// [返回] 形如 /tasks/{code}/claim 的相对路径。
// 最近修改时间：2026-10-24 新增成长中心客户端（TASK-02.1）
func claimGrowthTaskPath(code string) string {
	return growthPathTasks + "/" + url.PathEscape(code) + "/claim"
}
