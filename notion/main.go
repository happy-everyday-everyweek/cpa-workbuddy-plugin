package main

/*
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

static int wb_call_host(cliproxy_host_api* api, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	return api->call(api->host_ctx, method, request, request_len, response);
}

static void wb_free_host_buffer(cliproxy_host_api* api, void* ptr, size_t len) {
	api->free_buffer(ptr, len);
}

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	providerName  = "notion-provider"
	pluginLogoURL = "https://raw.githubusercontent.com/luode0320/cpa-workbuddy-plugin/main/assets/icons/Notion.png"
)

var (
	// version 当前插件版本号。
	version      = "0.2.0"
	hostAPI      *C.cliproxy_host_api
	hostAPIMu    sync.RWMutex
	activeConfig atomic.Value
)

func init() {
	activeConfig.Store(defaultConfig())
}

func main() {}

// Config 是插件运行配置。
type Config struct {
	// TimeoutSeconds 非流式上游请求超时（秒）。
	TimeoutSeconds int `yaml:"timeout_seconds" json:"timeout_seconds"`
	// StreamTimeoutSeconds 流式对话整轮超时（秒）。
	StreamTimeoutSeconds int `yaml:"stream_timeout_seconds" json:"stream_timeout_seconds"`
	// ClientVersion 是 notion-client-version 覆盖值，空值用内置默认。
	ClientVersion string `yaml:"client_version" json:"client_version"`
	// SpaceID 指定默认工作空间；账号有多个 workspace 时用它消歧。
	SpaceID string `yaml:"space_id" json:"space_id"`
	// RetryOn4xx 是 401/403/404/405 触发换号的次数预算。
	RetryOn4xx int `yaml:"retry_on_4xx" json:"retry_on_4xx"`
	// CooldownSeconds 是账号首次失败后的冷却基数（秒），按阶梯倍增。
	CooldownSeconds int `yaml:"cooldown_seconds" json:"cooldown_seconds"`
}

// defaultConfig 返回插件默认配置。
// [参数] 无。
// [返回] 带默认值的配置结构。
// 最近修改时间：2026-10-18；改动原因：新增，Notion 插件默认配置。
func defaultConfig() Config {
	return Config{
		TimeoutSeconds:       30,
		StreamTimeoutSeconds: 300,
		ClientVersion:        browserClientVersion,
		RetryOn4xx:           3,
		CooldownSeconds:      30,
	}
}

// currentConfig 读取当前生效配置，未初始化时回退默认值。
// [参数] 无。
// [返回] 当前配置。
// 最近修改时间：2026-10-18；改动原因：新增，配置读取入口。
func currentConfig() Config {
	if cfg, ok := activeConfig.Load().(Config); ok {
		return cfg
	}
	return defaultConfig()
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	hostAPIMu.Lock()
	hostAPI = host
	hostAPIMu.Unlock()

	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}

	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}

	raw, err := handleMethod(C.GoString(method), requestBytes)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	hostAPIMu.Lock()
	hostAPI = nil
	hostAPIMu.Unlock()
}

// hostCall 通过宿主导出的 C 函数指针表发起一次 RPC 调用。
// [参数] method: 宿主方法名；request: JSON 请求体，可为空。
// [返回] 宿主返回的原始字节与错误。
// 最近修改时间：2026-10-18；改动原因：新增，宿主 RPC 桥入口。
func hostCall(method string, request []byte) ([]byte, error) {
	hostAPIMu.RLock()
	api := hostAPI
	hostAPIMu.RUnlock()

	if api == nil || api.call == nil {
		return nil, fmt.Errorf("宿主 API 不可用")
	}

	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var cReq unsafe.Pointer
	var reqLen C.size_t
	if len(request) > 0 {
		cReq = C.CBytes(request)
		defer C.free(cReq)
		reqLen = C.size_t(len(request))
	}

	var resp C.cliproxy_buffer
	rc := C.wb_call_host(api, cMethod, (*C.uint8_t)(cReq), reqLen, &resp)
	var out []byte
	if resp.ptr != nil && resp.len > 0 {
		out = C.GoBytes(resp.ptr, C.int(resp.len))
	}
	if resp.ptr != nil && api.free_buffer != nil {
		C.wb_free_host_buffer(api, resp.ptr, resp.len)
	}
	if rc != 0 {
		return out, fmt.Errorf("宿主调用 %s 返回 %d", method, int(rc))
	}
	return out, nil
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
	Scheduler             bool                         `json:"scheduler"`
	ManagementAPI         bool                         `json:"management_api"`
	UsagePlugin           bool                         `json:"usage_plugin"`
}

// notionRegistration 返回插件注册元数据与能力声明。
// [参数] 无。
// [返回] 注册结构。
// 最近修改时间：2026-10-18；改动原因：新增，声明模型目录与执行器能力。
func notionRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             providerName,
			Version:          version,
			Author:           "luode (cpa-workbuddy-plugin)",
			GitHubRepository: "https://github.com/luode0320/cpa-workbuddy-plugin",
			Logo:             pluginLogoURL,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "timeout_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "非流式上游请求超时（秒）。"},
				{Name: "stream_timeout_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "流式对话整轮超时（秒）。"},
				{Name: "client_version", Type: pluginapi.ConfigFieldTypeString, Description: "notion-client-version 覆盖值，留空用内置默认。"},
				{Name: "space_id", Type: pluginapi.ConfigFieldTypeString, Description: "默认工作空间 id；账号挂多个 workspace 时用它消歧。"},
				{Name: "retry_on_4xx", Type: pluginapi.ConfigFieldTypeInteger, Description: "401/403/404/405 触发换号的次数预算。"},
				{Name: "cooldown_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "账号首次失败后的冷却基数（秒）。"},
			},
		},
		Capabilities: registrationCapability{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeBoth,
			ExecutorInputFormats:  []string{"openai"},
			ExecutorOutputFormats: []string{"openai"},
			Scheduler:             false,
			ManagementAPI:         true,
			UsagePlugin:           false,
		},
	}
}

// handleMethod 按宿主方法名分发插件能力。
// [参数] method: 宿主方法名；request: 原始 JSON 请求体。
// [返回] 信封 JSON 与错误。
// 最近修改时间：2026-10-18；改动原因：新增，方法分发表。
func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configurePlugin(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(notionRegistration())

	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})

	case pluginabi.MethodAuthParse:
		return handleParseAuth(request)

	case pluginabi.MethodAuthRefresh:
		return handleRefreshAuth(request)

	case pluginabi.MethodModelRegister, pluginabi.MethodModelStatic:
		return modelStatic(request)

	case pluginabi.MethodModelForAuth:
		return modelForAuth(request)

	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})

	case pluginabi.MethodExecutorExecute:
		return handleExecExecute(request)

	case pluginabi.MethodExecutorExecuteStream:
		return handleExecStream(request)

	case pluginabi.MethodExecutorCountTokens:
		// Notion 没有独立的 token 计数端点；返回 0 让宿主跳过预估，
		// 而不是伪造一个看似合理的数字误导用量统计。
		return okEnvelope(pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":0}`)})

	case pluginabi.MethodManagementRegister:
		var regReq pluginapi.ManagementRegistrationRequest
		if errUnmarshal := json.Unmarshal(request, &regReq); errUnmarshal == nil && regReq.BasePath != "" {
			setManagementBasePath(regReq.BasePath)
		}
		return okEnvelope(managementRegistration())

	case pluginabi.MethodManagementHandle:
		return handleManagement(request)

	default:
		return errorEnvelope("unknown_method", "unknown_method: "+method), nil
	}
}

// configurePlugin 解析宿主下发的 YAML 配置并更新运行期配置。
//
// 缺省键保持当前值而不是回落到默认值：retry_on_4xx 这类 kill switch
// 一旦被无关的 reconfigure 重置，用户显式关掉的换号策略会悄悄复活。
// [参数] raw: 宿主 reconfigure 载荷，config_yaml 为 base64 解码后的 YAML 字节。
// [返回] 解析错误。
// 最近修改时间：2026-10-18；改动原因：新增，配置热更新入口。
func configurePlugin(raw []byte) error {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}

	cfg := currentConfig()
	if len(req.ConfigYAML) > 0 {
		var decoded Config
		if errUnmarshal := yaml.Unmarshal(req.ConfigYAML, &decoded); errUnmarshal != nil {
			return errUnmarshal
		}
		if decoded.TimeoutSeconds > 0 {
			cfg.TimeoutSeconds = decoded.TimeoutSeconds
		}
		if decoded.StreamTimeoutSeconds > 0 {
			cfg.StreamTimeoutSeconds = decoded.StreamTimeoutSeconds
		}
		if strings.TrimSpace(decoded.ClientVersion) != "" {
			cfg.ClientVersion = strings.TrimSpace(decoded.ClientVersion)
		}
		if strings.TrimSpace(decoded.SpaceID) != "" {
			cfg.SpaceID = strings.TrimSpace(decoded.SpaceID)
		}
		if decoded.RetryOn4xx >= 0 {
			cfg.RetryOn4xx = decoded.RetryOn4xx
		}
		if decoded.CooldownSeconds > 0 {
			cfg.CooldownSeconds = decoded.CooldownSeconds
		}
	}
	activeConfig.Store(cfg)
	return nil
}

// okEnvelope 包装成功信封。
//
// 不用 json.Marshal：它默认开启 HTML 转义，会把面板脚本里的 `>`、`'`、`&`
// 变成实体，导致前端拿到的脚本无法执行。
// [参数] v: 任意可序列化结果。
// [返回] 信封 JSON 与序列化错误。
// 最近修改时间：2026-10-18；改动原因：新增，关闭 HTML 转义。
func okEnvelope(v any) ([]byte, error) {
	result, errMarshal := marshalNoHTMLEscape(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return marshalNoHTMLEscape(envelope{OK: true, Result: result})
}

// marshalNoHTMLEscape 序列化且不做 HTML 转义。
// [参数] v: 任意可序列化值。
// [返回] JSON 字节与错误。
// 最近修改时间：2026-10-18；改动原因：新增，统一序列化出口。
func marshalNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if errEncode := enc.Encode(v); errEncode != nil {
		return nil, errEncode
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// errorEnvelope 构造错误信封。
// [参数] code: 错误码；message: 错误说明。
// [返回] 信封 JSON。
// 最近修改时间：2026-10-18；改动原因：新增，统一错误响应构造。
func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

// writeResponse 把响应字节写入宿主提供的缓冲区。
// [参数] response: 宿主缓冲区指针；raw: 响应字节。
// [返回] 无。
// 最近修改时间：2026-10-18；改动原因：新增，C ABI 回写出口。
func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
