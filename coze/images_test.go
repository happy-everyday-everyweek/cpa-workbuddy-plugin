package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// 本文件锁定标准图片端点的契约：请求解析、提示词翻译、响应包装。
//
// 这些用例对应 AC-104~AC-106：宿主只按 Type == "openai-image" 放行模型，
// 只按插件声明的 input format 投递请求，并要求插件返回 {created, data[]}。
// 任何一处与宿主口径不一致，生产表现都是「1ms 内 503/500」——请求根本没
// 进到上游，因此这里逐字段断言，而不是只断言「有返回」。

// TestParseImagesRequest 验证 images 请求解析。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，图片请求契约。
func TestParseImagesRequest(t *testing.T) {
	raw := []byte(`{"model":"coze/doubao-seedream-5-0-pro","prompt":"一只橘猫","size":"1024x1024","response_format":"url","n":2}`)
	req, err := parseImagesRequest(raw)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if req.Prompt != "一只橘猫" || req.Size != "1024x1024" || req.N != 2 {
		t.Errorf("解析结果不符：%+v", req)
	}

	// prompt 是上游必填：空 prompt 到上游只换来一次无效对话。
	if _, errEmpty := parseImagesRequest([]byte(`{"prompt":"   "}`)); errEmpty == nil {
		t.Error("空白 prompt 应当报错")
	}
	if _, errBody := parseImagesRequest(nil); errBody == nil {
		t.Error("空请求体应当报错")
	}
	if _, errBad := parseImagesRequest([]byte("not json")); errBad == nil {
		t.Error("非法 JSON 应当报错")
	}
}

// TestImagesResponseFormatDefaultsToB64 验证响应格式归一化与宿主一致。
//
// 宿主 normalizeImagesResponseFormat 只在显式 url 时用 url，
// 其余一律 b64_json；插件口径不一致会让客户端拿到与请求不符的字段。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，响应格式口径。
func TestImagesResponseFormatDefaultsToB64(t *testing.T) {
	cases := map[string]string{
		"url": "url", "URL": "url", " url ": "url",
		"b64_json": "b64_json", "": "b64_json", "garbage": "b64_json",
	}
	for input, want := range cases {
		if got := imagesResponseFormat(input); got != want {
			t.Errorf("imagesResponseFormat(%q) = %q，期望 %q", input, got, want)
		}
	}
}

// TestBuildImagesPromptCarriesSizeAndCount 验证尺寸与张数被写进提示词。
//
// 扣子对话通道只认自然语言；不把这些字段翻译成文字，
// 客户端传的 size/n 会被静默丢弃。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，请求语义翻译。
func TestBuildImagesPromptCarriesSizeAndCount(t *testing.T) {
	prompt := buildImagesPrompt(&imagesRequest{Prompt: "一只橘猫", Size: "1024x1024", N: 3})
	if !strings.HasPrefix(prompt, "一只橘猫") {
		t.Errorf("提示词应以原始 prompt 开头，实际 %q", prompt)
	}
	if !strings.Contains(prompt, "1024x1024") {
		t.Errorf("尺寸未写进提示词：%q", prompt)
	}
	if !strings.Contains(prompt, "3 张") {
		t.Errorf("张数未写进提示词：%q", prompt)
	}

	// 未指定尺寸/张数时不得凭空加约束。
	bare := buildImagesPrompt(&imagesRequest{Prompt: "一只橘猫"})
	if bare != "一只橘猫" {
		t.Errorf("无附加字段时提示词应等于原 prompt，实际 %q", bare)
	}
}

// TestBuildImagesResponseURL 验证 url 模式只给地址、不带 base64。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，url 响应契约。
func TestBuildImagesResponseURL(t *testing.T) {
	raw, err := buildImagesResponse([]string{"https://static.coze.site/a.jpg?sign=X"}, "url", nil)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	var resp imagesResponse
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		t.Fatalf("解析失败：%v", errUnmarshal)
	}
	if resp.Created <= 0 {
		t.Errorf("created 必须为正，实际 %d", resp.Created)
	}
	if len(resp.Data) != 1 || resp.Data[0].URL == "" {
		t.Fatalf("url 模式响应不符：%+v", resp.Data)
	}
	if resp.Data[0].B64JSON != "" {
		t.Errorf("url 模式不应带 b64_json：%+v", resp.Data[0])
	}

	// 空地址不能被包装成一条空结果：客户端会显示破图。
	if _, errEmpty := buildImagesResponse([]string{"", "  "}, "url", nil); errEmpty == nil {
		t.Error("全部地址为空时应当报错")
	}
}

// TestBuildImagesResponseB64 验证 b64_json 模式真实下载并编码。
//
// 宿主与客户端都会把 b64_json 直接交给图片解码器，
// 伪造内容只会让用户看到破图；这里用本地 HTTP 服务验证真的下载了内容。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，b64 响应契约。
func TestBuildImagesResponseB64(t *testing.T) {
	payload := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	// verifyImageHost 只放行 coze.site，测试用本地 TLS 服务需要临时放行。
	original := imageHostAllowlist
	imageHostAllowlist = []string{"127.0.0.1"}
	defer func() { imageHostAllowlist = original }()

	raw, err := buildImagesResponse([]string{server.URL + "/a.jpg"}, "b64_json", server.Client())
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	var resp imagesResponse
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		t.Fatalf("解析失败：%v", errUnmarshal)
	}
	if len(resp.Data) != 1 || resp.Data[0].B64JSON == "" {
		t.Fatalf("b64_json 模式响应不符：%+v", resp.Data)
	}
	decoded, errDecode := base64.StdEncoding.DecodeString(resp.Data[0].B64JSON)
	if errDecode != nil {
		t.Fatalf("base64 解码失败：%v", errDecode)
	}
	if string(decoded) != string(payload) {
		t.Errorf("下载内容被改写：%x", decoded)
	}
}

// TestBuildImagesResponseB64PropagatesDownloadFailure 验证下载失败不伪造数据。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，失败传播守卫。
func TestBuildImagesResponseB64PropagatesDownloadFailure(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	original := imageHostAllowlist
	imageHostAllowlist = []string{"127.0.0.1"}
	defer func() { imageHostAllowlist = original }()

	if _, err := buildImagesResponse([]string{server.URL + "/a.jpg"}, "b64_json", server.Client()); err == nil {
		t.Error("上游 403 时必须报错，不得返回空 base64")
	}
}

// TestDownloadImageBase64RefusesNonCozeHost 验证下载链路真的执行了域名校验。
//
// 只测 verifyImageHost 单函数抓不到「调用点被删」这类变异：
// 函数本身还在，白名单却已经不在链路上。这里直接走下载入口。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，覆盖变异 M3。
func TestDownloadImageBase64RefusesNonCozeHost(t *testing.T) {
	// 命中本地服务会立刻暴露「白名单没生效」：请求真的发出去了。
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte{0xff, 0xd8, 0xff})
	}))
	defer server.Close()

	// 不放行 127.0.0.1：下载必须在建立连接前被拒绝。
	if _, err := downloadImageBase64(server.Client(), server.URL+"/a.jpg"); err == nil {
		t.Fatal("非白名单域名必须被拒绝")
	}
	if requests != 0 {
		t.Errorf("请求已发出 %d 次，域名校验未在连接前生效", requests)
	}
}

// TestVerifyImageHostRejectsNonCozeTargets 验证 SSRF 白名单。
//
// 图片地址来自上游响应，属于不可信输入；不限制目标就是让上游
// 决定插件去请求哪个地址。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，SSRF 守卫。
func TestVerifyImageHostRejectsNonCozeTargets(t *testing.T) {
	allowed := []string{
		"https://static.coze.site/a.jpg?sign=X",
		"https://coze.site/a.jpg",
		"https://s.coze.cn.evil.com/a.jpg", // 后缀匹配不能误放行
	}
	if err := verifyImageHost(allowed[0]); err != nil {
		t.Errorf("static.coze.site 应放行，实际 %v", err)
	}
	if err := verifyImageHost(allowed[1]); err != nil {
		t.Errorf("coze.site 应放行，实际 %v", err)
	}
	if err := verifyImageHost(allowed[2]); err == nil {
		t.Error("coze.cn.evil.com 不应放行（不是 coze.site 的子域）")
	}

	rejected := []string{
		"http://static.coze.site/a.jpg",     // 非 https
		"https://127.0.0.1/a.jpg",           // 回环
		"https://169.254.169.254/latest",    // 云元数据
		"https://evil.com/static.coze.site", // 路径伪装
	}
	for _, raw := range rejected {
		if err := verifyImageHost(raw); err == nil {
			t.Errorf("%q 不应放行", raw)
		}
	}
}

// TestIsImagesExecution 验证入口分流判据。
//
// 判错会让 images 请求走进 chat 解析，报「请求缺少 messages」——
// 正是 0.3.6 修过的那类入口误判。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，分流守卫。
func TestIsImagesExecution(t *testing.T) {
	if !isImagesExecution(pluginapi.ExecutorRequest{Format: openAIImageModelType}) {
		t.Error("Format=openai-image 应判为图片请求")
	}
	if !isImagesExecution(pluginapi.ExecutorRequest{SourceFormat: openAIImageModelType}) {
		t.Error("SourceFormat=openai-image 应判为图片请求")
	}
	if isImagesExecution(pluginapi.ExecutorRequest{Format: "openai", SourceFormat: "openai"}) {
		t.Error("对话请求不应被判为图片请求")
	}
	if isImagesExecution(pluginapi.ExecutorRequest{}) {
		t.Error("空请求不应被判为图片请求")
	}
}

// TestHandleExecExecuteDispatchesImagesBeforeChatParsing 验证入口分流发生在 chat 解析之前。
//
// 这是生产实际故障的回归守卫：images 请求体里没有 messages，
// 若先走 parseChatRequest 会立刻报「请求缺少 messages」，
// 表现就是 1ms 内 503/500——请求根本没到上游。
// 账号必须有效：否则会在 parseStoredAuth 就被拦下，
// 分流失效也照样「通过」，用例就抓不到真实缺陷。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，入口分流回归（覆盖变异 M1）。
func TestHandleExecExecuteDispatchesImagesBeforeChatParsing(t *testing.T) {
	body := []byte(`{"model":"coze/doubao-seedream-5-0-pro","prompt":"一只橘猫","response_format":"url"}`)
	storage, errStorage := json.Marshal(StoredAuth{CookieHeader: "sessionid=x"})
	if errStorage != nil {
		t.Fatalf("序列化账号失败：%v", errStorage)
	}
	req := pluginapi.ExecutorRequest{
		Model:        "coze/doubao-seedream-5-0-pro",
		Format:       openAIImageModelType,
		SourceFormat: openAIImageModelType,
		Payload:      body,
		StorageJSON:  storage,
	}
	raw, errMarshal := json.Marshal(req)
	if errMarshal != nil {
		t.Fatalf("序列化失败：%v", errMarshal)
	}

	_, errExec := handleExecExecute(raw)
	if errExec == nil {
		t.Fatal("用例前提不成立：应立即在上游握手处失败")
	}
	message := errExec.Error()
	if strings.Contains(message, "缺少 messages") {
		t.Fatalf("images 请求被当成 chat 解析：%v", message)
	}
}

// TestStaticFallbackImageModelIsOpenAIImage 验证静态兜底目录同样使用新类型。
//
// 宿主启动期、以及一个账号都没有时会走 staticFallbackModels；
// 只改动态发现而漏掉静态兜底，表现为「有账号时能生图、无账号时 400」，
// 属于隐蔽的双轨不一致。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，静态兜底类型守卫。
func TestStaticFallbackImageModelIsOpenAIImage(t *testing.T) {
	for _, model := range staticFallbackModels() {
		switch {
		case strings.Contains(model.ID, "seedream"):
			if model.Type != openAIImageModelType {
				t.Errorf("%s 类型为 %q，期望 %q", model.ID, model.Type, openAIImageModelType)
			}
		case strings.Contains(model.ID, "seedance"):
			if model.Type != "video" {
				t.Errorf("%s 类型为 %q，期望 video", model.ID, model.Type)
			}
		}
	}
}

// TestImageModelTypeIsOpenAIImage 验证生图模型对外类型。
//
// 宿主 /v1/images/generations 的白名单只认 Type == "openai-image"，
// 填 "image" 会被 rejectUnsupportedImagesModel 直接 400 拒绝。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，AC-104 类型契约。
func TestImageModelTypeIsOpenAIImage(t *testing.T) {
	for _, entry := range modelListTypes {
		if entry.Upstream == modelTypeImage && entry.Host != openAIImageModelType {
			t.Errorf("image_generation 映射为 %q，期望 %q", entry.Host, openAIImageModelType)
		}
	}
	if got := hostTypeForCapability("image_gen"); got != openAIImageModelType {
		t.Errorf("image_gen 能力映射为 %q，期望 %q", got, openAIImageModelType)
	}
}

// TestChatModelTypeNotChanged 验证对话与视频模型类型不回归。
// [参数] t: 测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-19；改动原因：新增，类型回归守卫。
func TestChatModelTypeNotChanged(t *testing.T) {
	want := map[string]string{
		modelTypeLLM:   "chat",
		modelTypeVideo: "video",
	}
	for _, entry := range modelListTypes {
		if expected, ok := want[entry.Upstream]; ok && entry.Host != expected {
			t.Errorf("%s 映射为 %q，期望 %q", entry.Upstream, entry.Host, expected)
		}
	}
	if got := hostTypeForCapability("generate_video"); got != "video" {
		t.Errorf("generate_video 能力映射为 %q，期望 video", got)
	}
}
