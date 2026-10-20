package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// 本文件锁定「生图附件晚到」契约。
//
// 2026-10-21 生产实证：生图请求几乎全部失败（`上游未返回任何图片`），
// 但账号积分被烧光。根因是收尾时序错配：
//   - WS 的 event_final 帧在 6-9 秒就到，正文只有
//     `[图片](computer:///...)` 占位符，图片附件此时尚未落库；
//   - 图片真正写进 message/list 的 files 约需 18 秒；
//   - 旧逻辑只在「正文为空」时兜底轮询，生图正文非空 → 不轮询，
//     直接把空图片列表交给 buildImagesResponse → 报「上游未返回任何图片」；
//   - 该错误无状态码 → 兜底成 502 → 宿主判账号故障，冷却并换号重试，
//     于是同一笔额度被反复烧。
//
// 因此这里锁三件事：生图必须等到附件；等不到时必须是请求级 400；
// 对话路径不得因为该改动而多等。

// imageReplyMessage 构造一条带图片附件的回复消息。
// [参数] messageID: 回复消息 ID；userMessageID: 对应用户消息 ID；files: 附件 JSON 片段。
// [返回] message/list 的上游响应体。
func imageReplyMessage(messageID, userMessageID, files string) string {
	content := `{"id":"` + messageID + `","message_data":{"reply":{"content":"[图片](computer://%2Fx.jpg)","reply_to_message_id":"` +
		userMessageID + `","stream":false}}}`
	escaped, _ := json.Marshal(content)
	return `{"code":0,"data":{"messages":[{"id":"` + messageID + `","message_type":"reply","content":` +
		string(escaped) + `,"files":` + files + `}]}}`
}

// newImagePollEnv 构造一个「WS 只推收尾、图片附件稍后才落库」的上游环境。
//
// 用 atOnce 模拟附件出现需要第几次 message/list 调用：
// 1 表示首次就有（正常晚到），负数表示永远没有（上游没交付）。
// [参数] t: 测试上下文；availableAt: 附件在第几次轮询出现。
// [返回] 环境与 message/list 被调用次数指针。
func newImagePollEnv(t *testing.T, availableAt int) (*executorTestEnv, *int) {
	t.Helper()
	const userMessageID = "mid-1"
	callCount := 0

	env := newExecutorTestEnv(t, func(conn *websocket.Conn) {
		// 只推一帧收尾：正文是占位符，不带任何图片附件。
		// 这正是生产里生图回复的 WS 形态。
		frame := encodeFrontierFrame(&frontierFrame{
			Service:     frontierServiceClaw,
			PayloadType: "message",
			Payload:     replyFrame("event_final", "1", "", "[图片](computer://%2Fx.jpg)"),
		})
		_ = conn.WriteMessage(websocket.BinaryMessage, frame)
		// 保持连接，模拟上游收尾后仍在线。
		time.Sleep(2 * time.Second)
	})

	env.doer.handler = func(req *http.Request, _ []byte) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, clawInfoPath):
			return jsonResponse(200, `{"code":0,"data":{"user_claw_info":{"claw_id":"c1"}}}`), nil
		case strings.HasSuffix(req.URL.Path, clawMainSessionPath):
			return jsonResponse(200, `{"code":0,"data":{"session":{"id":"s1"}}}`), nil
		case strings.HasSuffix(req.URL.Path, frontierKeyPath):
			return jsonResponse(200, `{"code":0,"data":{"access_key":"k","device_id":"d"}}`), nil
		case strings.HasSuffix(req.URL.Path, clawChatPath):
			return jsonResponse(200, `{"code":0,"data":{"message_id":"`+userMessageID+`"}}`), nil
		case strings.HasSuffix(req.URL.Path, clawMessageListPath):
			callCount++
			if availableAt < 0 || callCount < availableAt {
				// 附件尚未落库：files 为空。
				return jsonResponse(200, imageReplyMessage("m1", userMessageID, `[]`)), nil
			}
			return jsonResponse(200, imageReplyMessage("m1", userMessageID,
				`[{"file_name":"/Coze/Drive/image/x.jpg","file_url":"https://static.coze.site/x.jpg?sign=Z"}]`)), nil
		default:
			return jsonResponse(404, `{}`), nil
		}
	}
	return env, &callCount
}

// TestImageExecutorWaitsForLateAttachments 验证生图会等到晚到的图片附件。
//
// 这是本轮修复的核心：收尾帧不含图时必须继续轮询，而不是直接判失败。
// [参数] t: 测试上下文。
// [返回] 无。
func TestImageExecutorWaitsForLateAttachments(t *testing.T) {
	env, calls := newImagePollEnv(t, 1)

	exec := newImageExecutor(&StoredAuth{CookieHeader: "sessionid=x"}, "coze/m", env.doer)
	exec.client.baseURL = cozeSiteHost
	exec.frontierServerURL = env.ws.server.URL

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, images, errRun := exec.runConversation(ctx, "画一只猫", func(string) error { return nil })
	if errRun != nil {
		t.Fatalf("晚到的图片不应判失败：%v", errRun)
	}
	if len(images) != 1 || images[0] != "https://static.coze.site/x.jpg?sign=Z" {
		t.Fatalf("必须取到晚到的图片附件，实际 %#v", images)
	}
	if *calls == 0 {
		t.Fatal("必须实际轮询 message/list")
	}
}

// TestImageExecutorPollsUntilAttachmentAppears 验证附件在多轮后才出现也能取到。
//
// 生产里附件落库有十几秒延迟，不会一次就绪；这里要求第 3 次轮询才给出，
// 用例必须仍能成功——否则实现退化成「只补查一次」就会漏图。
// [参数] t: 测试上下文。
// [返回] 无。
func TestImageExecutorPollsUntilAttachmentAppears(t *testing.T) {
	env, calls := newImagePollEnv(t, 3)

	exec := newImageExecutor(&StoredAuth{CookieHeader: "sessionid=x"}, "coze/m", env.doer)
	exec.client.baseURL = cozeSiteHost
	exec.frontierServerURL = env.ws.server.URL

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, images, errRun := exec.runConversation(ctx, "画一只猫", func(string) error { return nil })
	if errRun != nil {
		t.Fatalf("多轮后出现的图片不应判失败：%v", errRun)
	}
	if len(images) != 1 {
		t.Fatalf("必须取到图片，实际 %#v", images)
	}
	if *calls < 3 {
		t.Fatalf("必须持续轮询到附件出现，实际只轮询 %d 次", *calls)
	}
}

// TestImageExecutorReportsRequestScopedErrorWhenNoImage 验证取不到图时的错误分类。
//
// 关键断言是状态码 400：上游已扣额度却没交付图片时，若回 5xx
// 会被宿主当账号故障冷却并换号重试，等于把同一笔额度再烧一遍。
// [参数] t: 测试上下文。
// [返回] 无。
func TestImageExecutorReportsRequestScopedErrorWhenNoImage(t *testing.T) {
	env, _ := newImagePollEnv(t, -1)

	exec := newImageExecutor(&StoredAuth{CookieHeader: "sessionid=x"}, "coze/m", env.doer)
	exec.client.baseURL = cozeSiteHost
	exec.frontierServerURL = env.ws.server.URL

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, images, errRun := exec.runConversation(ctx, "画一只猫", func(string) error { return nil })
	if len(images) != 0 {
		t.Fatalf("上游没交付图片时不应凭空造出图片：%#v", images)
	}
	if errRun == nil {
		t.Fatal("取不到图片必须报错")
	}
	coder, ok := errRun.(statusCoder)
	if !ok {
		t.Fatalf("错误必须实现 StatusCode，实际类型 %T", errRun)
	}
	if coder.StatusCode() != http.StatusBadRequest {
		t.Fatalf("必须是请求级 400（不换号不冷却），实际 %d", coder.StatusCode())
	}
}

// TestChatExecutorDoesNotWaitForImages 验证对话路径不长等。
//
// 文本对话同样可能带图（模型自主出图），但它的正文就有价值：
// 为了等附件把回复拖住十几秒会让所有普通对话变慢。
// 因此只要求生图轮次等附件。
// [参数] t: 测试上下文。
// [返回] 无。
func TestChatExecutorDoesNotWaitForImages(t *testing.T) {
	env, calls := newImagePollEnv(t, -1)

	exec := newCozeExecutor(&StoredAuth{CookieHeader: "sessionid=x"}, "coze/m", env.doer)
	exec.client.baseURL = cozeSiteHost
	exec.frontierServerURL = env.ws.server.URL

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	content, images, errRun := exec.runConversation(ctx, "你好", func(string) error { return nil })
	elapsed := time.Since(start)

	if errRun != nil {
		t.Fatalf("有正文的对话不应失败：%v", errRun)
	}
	if strings.TrimSpace(content) == "" {
		t.Fatal("正文必须保留")
	}
	if len(images) != 0 {
		t.Fatalf("没交付附件时不应有图片：%#v", images)
	}
	if *calls != 0 {
		t.Fatalf("对话路径不应触发附件轮询，实际轮询 %d 次", *calls)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("对话路径不应为等附件而变慢，实际耗时 %v", elapsed)
	}
}

// TestHandleImagesExecuteWaitsForLateAttachment 端到端验证图片入口会等附件。
//
// 前面的用例都直接构造 newImageExecutor，无法证明真实入口用的是它。
// 这里穿过 handleImagesExecute（宿主实际调用的那层），并用注入点把
// 执行器接到测试桩上：若入口漏掉 wantImages 标记，本用例立即失败。
// [参数] t: 测试上下文。
// [返回] 无。
func TestHandleImagesExecuteWaitsForLateAttachment(t *testing.T) {
	const userMessageID = "mid-1"
	listCalls := 0

	env := newExecutorTestEnv(t, func(conn *websocket.Conn) {
		frame := encodeFrontierFrame(&frontierFrame{
			Service:     frontierServiceClaw,
			PayloadType: "message",
			Payload:     replyFrame("event_final", "1", "", "[图片](computer://%2Fx.jpg)"),
		})
		_ = conn.WriteMessage(websocket.BinaryMessage, frame)
		time.Sleep(2 * time.Second)
	})
	env.doer.handler = func(req *http.Request, _ []byte) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, clawInfoPath):
			return jsonResponse(200, `{"code":0,"data":{"user_claw_info":{"claw_id":"c1"}}}`), nil
		case strings.HasSuffix(req.URL.Path, clawMainSessionPath):
			return jsonResponse(200, `{"code":0,"data":{"session":{"id":"s1"}}}`), nil
		case strings.HasSuffix(req.URL.Path, frontierKeyPath):
			return jsonResponse(200, `{"code":0,"data":{"access_key":"k","device_id":"d"}}`), nil
		case strings.HasSuffix(req.URL.Path, clawChatPath):
			return jsonResponse(200, `{"code":0,"data":{"message_id":"`+userMessageID+`"}}`), nil
		case strings.HasSuffix(req.URL.Path, clawMessageListPath):
			listCalls++
			if listCalls < 2 {
				return jsonResponse(200, imageReplyMessage("m1", userMessageID, `[]`)), nil
			}
			return jsonResponse(200, imageReplyMessage("m1", userMessageID,
				`[{"file_url":"https://static.coze.site/x.jpg?sign=Z"}]`)), nil
		default:
			return jsonResponse(404, `{}`), nil
		}
	}

	// 把入口内部的执行器构造接到桩上（与 runExecutorConversation 同构的注入点）。
	original := newImageExecutorFn
	newImageExecutorFn = func(sa *StoredAuth, model string, client httpDoer) *cozeExecutor {
		exec := original(sa, model, env.doer)
		exec.client.baseURL = cozeSiteHost
		exec.frontierServerURL = env.ws.server.URL
		return exec
	}
	t.Cleanup(func() { newImageExecutorFn = original })

	sa := &StoredAuth{CookieHeader: "sessionid=x"}
	req := pluginapi.ExecutorRequest{
		Model:        "coze/doubao-seedream-5-0-260128",
		Format:       openAIImageModelType,
		SourceFormat: openAIImageModelType,
		Payload:      []byte(`{"model":"coze/doubao-seedream-5-0-260128","prompt":"一只橘猫","response_format":"url"}`),
	}

	out, errExec := handleImagesExecute(sa, req)
	if errExec != nil {
		t.Fatalf("附件晚到时图片入口不应失败：%v", errExec)
	}
	if listCalls < 2 {
		t.Fatalf("图片入口必须等附件出现，实际只轮询 %d 次", listCalls)
	}
	// 执行响应里的 Payload 是 base64，必须解码后再断言：
	// 直接搜信封字符串会因编码而永远搜不到，用例就成了假通过。
	var envResp struct {
		OK     bool `json:"ok"`
		Result struct {
			Payload string `json:"Payload"`
		} `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(out, &envResp); errUnmarshal != nil {
		t.Fatalf("信封解析失败：%v", errUnmarshal)
	}
	decoded, errDecode := base64.StdEncoding.DecodeString(envResp.Result.Payload)
	if errDecode != nil {
		t.Fatalf("Payload 不是合法 base64：%v", errDecode)
	}
	if !strings.Contains(string(decoded), "static.coze.site") {
		t.Fatalf("响应必须带上晚到的图片地址，实际 %s", string(decoded))
	}
}

// TestImagesUnavailableErrorCarries400 锁定错误类型的状态码语义。
// [参数] t: 测试上下文。
// [返回] 无。
func TestImagesUnavailableErrorCarries400(t *testing.T) {
	errImages := &imagesUnavailableError{messageID: "m1"}
	if errImages.StatusCode() != http.StatusBadRequest {
		t.Fatalf("必须是 400，实际 %d", errImages.StatusCode())
	}
	if !strings.Contains(errImages.Error(), "m1") {
		t.Fatalf("错误文案应带上 message_id 便于定位，实际 %q", errImages.Error())
	}
}
