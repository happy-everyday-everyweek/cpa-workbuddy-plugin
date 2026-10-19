// models_sign_body_test.go 覆盖周期 P0：模型发现 GET 的「签名 body == 实际 body」不变量。
//
// 根因诊断（BUG-QODER-MODELS-SIGNATURE-INVALID-20261012）：callModelsAPI 用
// qoderEncode("{}") 参与签名，但 GET 实际不带 body，服务端按「签名声明的
// body == 收到的 body」校验 → 恒 403 Signature invalid。修复形态参照
// 可正常工作的 qoder2api-hub read_dynamic_models（带体 GET）。
//
// 本用例不依赖网络：从请求头反推 COSY md5 签名，穷举候选 body，断言
// 能还原出合法签名的那个 body 恰等于请求真实携带的 body。
// 变异对照：把请求体改回 nil → 合法签名候选（qoderEncode("{}")）与实际
// body（""）不再相等 → FAIL，且报错直接打印两者。
package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"strings"
	"testing"
)

// TestModelsAPIRequest_SignatureMatchesActualBody 断言模型发现 GET 的
// 签名所覆盖 body 与请求实际发送 body 严格一致。
func TestModelsAPIRequest_SignatureMatchesActualBody(t *testing.T) {
	sa := &storedAuth{
		Auth: storedTokens{AccessToken: "jt-sign-body", RefreshToken: "jrt-sign-body"},
		Account: storedAccount{
			UID:      "u-sign-body",
			Nickname: "sign body test",
		},
	}

	req, err := newModelsAPIRequest(context.Background(), sa, endpointModels)
	if err != nil {
		t.Fatalf("newModelsAPIRequest 失败: %v", err)
	}
	if req.Method != "GET" {
		t.Fatalf("method = %s, want GET", req.Method)
	}

	// 1. 读出请求实际携带的 body（即上游服务端会看到的 body）。
	var actualBody string
	if req.Body != nil {
		b, rerr := io.ReadAll(req.Body)
		if rerr != nil {
			t.Fatalf("读取请求 body 失败: %v", rerr)
		}
		actualBody = string(b)
	}

	// 2. 从签名头反推：Bearer COSY.<payloadB64>.<sig>
	auth := req.Header.Get("authorization")
	const prefix = "Bearer COSY."
	if !strings.HasPrefix(auth, prefix) {
		t.Fatalf("authorization 头异常: %q", auth)
	}
	parts := strings.Split(auth[len(prefix):], ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("authorization 载荷异常: %q", auth)
	}
	payloadB64, sig := parts[0], parts[1]
	cosyKey := req.Header.Get("cosy-key")
	date := req.Header.Get("cosy-date")
	if cosyKey == "" || date == "" {
		t.Fatalf("cosy-key / cosy-date 头缺失: key=%q date=%q", cosyKey, date)
	}

	// 3. pathSig 与 buildBearer 同口径：去掉 /algo 前缀。
	pathSig := req.URL.Path
	if strings.HasPrefix(pathSig, "/algo") {
		pathSig = pathSig[len("/algo"):]
	}

	// 4. 穷举候选 body，找出哪个能还原出请求头里的合法签名。
	candidates := []string{"", qoderEncode([]byte("{}"))}
	var signedBody string
	matched := 0
	for _, c := range candidates {
		sum := md5.Sum([]byte(payloadB64 + "\n" + cosyKey + "\n" + date + "\n" + c + "\n" + pathSig))
		if hex.EncodeToString(sum[:]) == sig {
			signedBody = c
			matched++
		}
	}
	if matched != 1 {
		t.Fatalf("候选 body 命中签名数 = %d, want 1（签名算法或候选集需复核）", matched)
	}

	// 5. 核心断言：签名声明的 body 必须等于实际发送的 body。
	if signedBody != actualBody {
		t.Fatalf("签名 body=%q 与实际 body=%q 不一致：服务端将判 Signature invalid（回归旧缺陷）",
			truncateForCompare(signedBody), truncateForCompare(actualBody))
	}
	if actualBody == "" {
		t.Fatal("模型发现 GET 必须携带编码后的 {} 请求体，实际为空")
	}
}

// truncateForCompare 截断超长签名串，只用于失败信息可读性。
func truncateForCompare(s string) string {
	if len(s) > 48 {
		return s[:48] + "…"
	}
	return s
}
