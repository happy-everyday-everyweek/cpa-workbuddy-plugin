package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// 本文件实现标准 OpenAI 图片端点（POST /v1/images/generations）的请求消费
// 与响应构造。
//
// 为什么需要这一层：扣子的生图通道是 claw 对话，请求里没有 images 语义。
// 宿主的 images handler 声明 handlerType=openai-image 后，会把客户端的
// images JSON 原样交给插件（Payload），并要求插件返回 {created, data[]}。
// 因此这里做两件事：把 images 字段翻成一条生图提示词，再把 claw 取回的
// file_url 包装成 images 响应。
//
// 关于 b64_json：扣子只给签名直链，不给 base64。宿主对 b64_json 的语义
// 是「data[].b64_json 必须是可解码的图片」，所以必须真实下载再编码，
// 不允许伪造。下载失败时整条请求失败，而不是返回一个空 base64 让客户端
// 拿到坏数据。

const (
	// imagesDownloadTimeout 是下载生图结果的单次超时。
	//
	// 上游图片在 500KB 量级，正常几百毫秒；给 60 秒是留给大图与跨国链路。
	imagesDownloadTimeout = 60 * time.Second

	// imagesDefaultCount 是客户端未指定 n 时的默认张数。
	//
	// 与 OpenAI 一致：缺省生成 1 张。
	imagesDefaultCount = 1
)

// imageHostAllowlist 是允许服务端下载的图片域名。
//
// 生图结果的签名直链只有 static.coze.site；限定白名单是为了防止
// 上游响应被污染后，插件拿宿主网络去请求内网地址（SSRF）。
// 用后缀匹配覆盖 coze.site 的各级子域。
var imageHostAllowlist = []string{"coze.site"}

// imagesRequest 是 OpenAI images 请求里本插件消费的字段。
//
// 只解析需要的字段：其余字段（quality / style / background 等）扣子对话
// 通道无法表达，强行塞进提示词只会让模型分心，因此不解析也不透传。
type imagesRequest struct {
	// Model 是请求里的模型名，仅用于回显与选择提示词模板。
	Model string `json:"model"`
	// Prompt 是生图提示词，必填。
	Prompt string `json:"prompt"`
	// Size 是期望尺寸，如 "1024x1024"；空表示不指定。
	Size string `json:"size"`
	// ResponseFormat 是 url 或 b64_json；空按 b64_json 处理（宿主默认）。
	ResponseFormat string `json:"response_format"`
	// N 是生成张数；0 表示未指定。
	N int `json:"n"`
}

// imagesResponse 是返回给宿主的 OpenAI images 响应。
type imagesResponse struct {
	Created int64         `json:"created"`
	Data    []imagesDatum `json:"data"`
}

// imagesDatum 是单张图片结果。
//
// 两个字段互斥：response_format=url 时填 URL，b64_json 时填 B64JSON。
type imagesDatum struct {
	URL           string `json:"url,omitempty"`
	B64JSON       string `json:"b64_json,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

// parseImagesRequest 解析 OpenAI images 请求体。
//
// 必须是独立结构而不是复用 chat 解析：images 请求没有 messages，
// 走 chat 解析只会得到「请求缺少 messages」。
// [参数] raw: 请求体字节。
// [返回] 解析结果与错误。
// 最近修改时间：2026-10-19；改动原因：新增，标准图片端点接入。
func parseImagesRequest(raw []byte) (*imagesRequest, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("图片请求体为空")
	}
	var req imagesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("图片请求解析失败：%w", err)
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("图片请求缺少 prompt")
	}
	return &req, nil
}

// imagesResponseFormat 归一化响应格式，与宿主行为保持一致。
//
// 宿主 normalizeImagesResponseFormat 只在显式 "url" 时用 url，
// 其余（含空串与非法值）一律 b64_json；这里必须同口径，
// 否则客户端拿到与声明不一致的字段。
// [参数] raw: 客户端传入的 response_format。
// [返回] "url" 或 "b64_json"。
// 最近修改时间：2026-10-19；改动原因：新增，响应格式归一化。
func imagesResponseFormat(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), "url") {
		return "url"
	}
	return "b64_json"
}

// imagesCount 返回本次要生成的张数。
//
// 扣子一次对话只产出一组图片，n 只能靠提示词表达；
// 低于 1 的值按默认 1 处理，避免出现 0 张的无效响应。
// [参数] n: 客户端传入的张数。
// [返回] 归一化后的张数。
// 最近修改时间：2026-10-19；改动原因：新增，张数归一化。
func imagesCount(n int) int {
	if n < 1 {
		return imagesDefaultCount
	}
	return n
}

// buildImagesPrompt 把 images 请求字段拼成一条生图提示词。
//
// 尺寸与张数在 OpenAI images 里是独立字段，扣子对话通道只认自然语言，
// 所以必须显式写进提示词；不指定尺寸时不加任何约束，让模型自选比例。
// [参数] req: 已解析的图片请求。
// [返回] 可直接发给 claw 的提示词。
// 最近修改时间：2026-10-19；改动原因：新增，请求语义翻译。
func buildImagesPrompt(req *imagesRequest) string {
	var builder strings.Builder
	builder.WriteString(strings.TrimSpace(req.Prompt))

	size := strings.TrimSpace(req.Size)
	if size != "" {
		builder.WriteString("\n\n画面尺寸：")
		builder.WriteString(size)
	}

	if count := imagesCount(req.N); count > 1 {
		builder.WriteString(fmt.Sprintf("\n\n请生成 %d 张不同的图片。", count))
	}
	return builder.String()
}

// buildImagesResponse 把 claw 取回的图片地址包装成 images 响应。
//
// b64_json 模式必须真实下载：客户端会直接把它交给图片解码器，
// 填入伪造内容会让客户端显示破图而不是报错。
// [参数] urls: 图片地址；format: 归一化后的响应格式；client: 下载用 HTTP 客户端。
// [返回] 响应字节与错误。
// 最近修改时间：2026-10-19；改动原因：新增，images 响应构造。
func buildImagesResponse(urls []string, format string, client *http.Client) ([]byte, error) {
	usable := make([]string, 0, len(urls))
	for _, url := range urls {
		if trimmed := strings.TrimSpace(url); trimmed != "" {
			usable = append(usable, trimmed)
		}
	}
	if len(usable) == 0 {
		return nil, fmt.Errorf("上游未返回任何图片")
	}

	response := imagesResponse{Created: time.Now().Unix(), Data: make([]imagesDatum, 0, len(usable))}
	if format == "url" {
		for _, url := range usable {
			response.Data = append(response.Data, imagesDatum{URL: url})
		}
		return json.Marshal(response)
	}

	downloadClient := client
	if downloadClient == nil {
		downloadClient = &http.Client{Timeout: imagesDownloadTimeout}
	}
	for _, url := range usable {
		encoded, errDownload := downloadImageBase64(downloadClient, url)
		if errDownload != nil {
			return nil, errDownload
		}
		response.Data = append(response.Data, imagesDatum{B64JSON: encoded})
	}
	return json.Marshal(response)
}

// downloadImageBase64 下载图片并编码成 base64。
//
// 不可用 FileURI 代替：FileURI 是云盘内部标识，无法直接访问。
// URL 来自上游响应属于不可信输入，因此先校验目标是扣子的图片 CDN：
// 服务端下载任意 URL 会变成 SSRF（内网探活、云元数据读取）。
// [参数] client: HTTP 客户端；url: 图片直链。
// [返回] base64 字符串与错误。
// 最近修改时间：2026-10-19；改动原因：新增，b64_json 真实编码 + 目标校验。
func downloadImageBase64(client *http.Client, url string) (string, error) {
	if errHost := verifyImageHost(url); errHost != nil {
		return "", errHost
	}
	resp, errGet := client.Get(url)
	if errGet != nil {
		return "", fmt.Errorf("下载图片失败：%w", errGet)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载图片失败：HTTP %d", resp.StatusCode)
	}
	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return "", fmt.Errorf("读取图片失败：%w", errRead)
	}
	if len(body) == 0 {
		return "", fmt.Errorf("下载图片失败：响应为空")
	}
	return base64.StdEncoding.EncodeToString(body), nil
}

// verifyImageHost 校验图片地址只指向扣子的图片 CDN。
//
// URL 来自上游响应，属于不可信输入：若上游被污染返回内网地址，
// 插件就会用宿主网络去请求内网（SSRF）。这里只放行扣子自有域名，
// 且必须是 https——签名直链从不走明文，降级请求可能泄露签名。
// [参数] raw: 图片地址。
// [返回] 校验错误；通过时返回 nil。
// 最近修改时间：2026-10-19；改动原因：新增，防止服务端请求伪造。
func verifyImageHost(raw string) error {
	parsed, errParse := url.Parse(strings.TrimSpace(raw))
	if errParse != nil {
		return fmt.Errorf("图片地址非法：%w", errParse)
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return fmt.Errorf("图片地址必须是 https：%s", parsed.Scheme)
	}
	host := strings.ToLower(parsed.Hostname())
	for _, allowed := range imageHostAllowlist {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return nil
		}
	}
	return fmt.Errorf("图片地址不在允许的域名内：%s", host)
}

// isImagesExecution 判断本次执行是否走标准图片端点。
//
// 两个字段都要看：Payload 由宿主按输入格式翻译，SourceFormat 是原始协议。
// 只要任一为 openai-image 就说明请求来自 /v1/images/*，
// 不能按 chat 结构解析——images 请求里根本没有 messages。
// [参数] req: 宿主下发的执行请求。
// [返回] 是否为图片请求。
// 最近修改时间：2026-10-19；改动原因：新增，入口分流判据。
func isImagesExecution(req pluginapi.ExecutorRequest) bool {
	return strings.EqualFold(strings.TrimSpace(req.Format), openAIImageModelType) ||
		strings.EqualFold(strings.TrimSpace(req.SourceFormat), openAIImageModelType)
}

// handleImagesExecute 处理标准图片端点的一次生成请求。
//
// 复用对话链路的 executor 与 claw 通道：生图在扣子侧本来就是一轮对话，
// 只是结果在附件里而不是正文里。因此这里不新开 HTTP 路径，
// 只把 images 语义翻成提示词，再把取回的地址包装成 images 响应。
// [参数] sa: 已还原的账号；req: 宿主下发的执行请求。
// [返回] 信封 JSON 与错误。
// 最近修改时间：2026-10-19；改动原因：新增，标准图片端点实现。
func handleImagesExecute(sa *StoredAuth, req pluginapi.ExecutorRequest) ([]byte, error) {
	imageReq, errParse := parseImagesRequest(executorRequestBody(req))
	if errParse != nil {
		return nil, errParse
	}

	prompt := buildImagesPrompt(imageReq)
	if errText := verifyChatContent(prompt); errText != nil {
		return nil, errText
	}

	model := req.Model
	if strings.TrimSpace(imageReq.Model) != "" {
		model = imageReq.Model
	}

	ctx, cancel := context.WithTimeout(context.Background(), executorReplyTimeout)
	defer cancel()

	exec := newCozeExecutor(sa, model, nil)
	_, images, errRun := exec.runConversation(ctx, prompt, func(string) error { return nil })

	// 正文为空但有图是生图的正常形态：图片只在附件里。
	// 只有两者都为空才算上游失败，避免把成功生图当错误丢弃。
	if errRun != nil && len(images) == 0 {
		return nil, errRun
	}

	payload, errBuild := buildImagesResponse(images, imagesResponseFormat(imageReq.ResponseFormat), nil)
	if errBuild != nil {
		return nil, errBuild
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: payload})
}
