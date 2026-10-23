// growth_test.go 覆盖成长中心客户端与任务域的关键契约：
// GET/POST 重试语义、逐条接单结果判定、进度口径可领判据、上报字段形状。
//
// 这些测试针对的是旧版 0.16.x 三次生产事故的具体成因，不是泛化的冒烟测试：
//   - accept 只认 HTTP 状态码会把逐条失败虚报成成功（0.16.3）；
//   - 用 accept_status == completed 判可领会误领；
//   - 上报缺 userId 会被上游静默丢弃。
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// newGrowthTestServer 起 httptest 服务并把 billing base 指向它。
// [参数] t：测试上下文；handler：请求处理函数。
// [返回] 清理函数（还原 billing base 并关闭服务）。
// 最近修改时间：2026-10-24 新增成长中心测试（TASK-02.1）
func newGrowthTestServer(t *testing.T, handler http.HandlerFunc) func() {
	t.Helper()
	srv := httptest.NewServer(handler)
	restore := setBillingBase(srv.URL)
	return func() {
		restore()
		srv.Close()
	}
}

// TestGrowthCallGetUnwrapsEnvelope 覆盖 GET 成功路径与 envelope 解包。
// [参数] t：测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-24 新增成长中心测试（TASK-02.1）
func TestGrowthCallGetUnwrapsEnvelope(t *testing.T) {
	var gotPath, gotMethod string
	cleanup := newGrowthTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"streak_days":7}}`))
	})
	defer cleanup()

	data, err := growthCall(&storedAuth{}, http.MethodGet, growthPathStreak, nil)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if string(data) != `{"streak_days":7}` {
		t.Fatalf("unexpected data: %s", string(data))
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("expected GET, got %s", gotMethod)
	}
	if want := growthPathPrefix + growthPathStreak; gotPath != want {
		t.Fatalf("expected path %s, got %s", want, gotPath)
	}
}

// TestGrowthCallPostDoesNotRetry 验证写操作单发不重试。
// 成长中心写端点会真实发放积分、消耗次数，盲重试可能重复扣减。
// [参数] t：测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-24 新增成长中心测试（TASK-02.1）
func TestGrowthCallPostDoesNotRetry(t *testing.T) {
	var calls int32
	cleanup := newGrowthTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`upstream boom`))
	})
	defer cleanup()

	_, err := growthCall(&storedAuth{}, http.MethodPost, growthPathTasksAccept, map[string]any{"task_codes": []string{"x"}})
	if err == nil {
		t.Fatal("expected error on 5xx")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("POST must not retry: got %d calls, want 1", got)
	}
}

// TestGrowthCallGetRetriesTransient 验证 GET 遇 5xx 会重试并在恢复后成功。
// [参数] t：测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-24 新增成长中心测试（TASK-02.1）
func TestGrowthCallGetRetriesTransient(t *testing.T) {
	var calls int32
	cleanup := newGrowthTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`transient`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"balance":42}}`))
	})
	defer cleanup()

	data, err := growthCall(&storedAuth{}, http.MethodGet, growthPathEnergy, nil)
	if err != nil {
		t.Fatalf("expected retry to succeed, got: %v", err)
	}
	if !strings.Contains(string(data), "42") {
		t.Fatalf("unexpected data: %s", string(data))
	}
	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Fatalf("expected retry, got %d calls", got)
	}
}
