// read_all_upstream_err_limit_test.go 覆盖周期 P2：错误体读取上限与换号退避常量。
//
// 生产 OOM 次级放大器之一：readAllUpstreamErr 曾无上限 io.ReadAll，上游异常
// 吐大 body 时每个失败请求都把整段正文吃进内存。修复对齐仓库惯例 1MiB。
// 变异对照：去掉 LimitReader → 3MiB 用例读满全量 → FAIL。
//
// failoverBackoff 的存在性护栏：换号路径本身依赖宿主 RPC，无法在单测里
// 真实走到「选定候选→退避→返回」分支（宿主不可用时提前返回），因此这里
// 只断言常量存在、非零且在合理区间——退避被删（改回 0）时本用例 FAIL。
package main

import (
	"strings"
	"testing"
	"time"
)

func TestReadAllUpstreamErr_CapsAtOneMiB(t *testing.T) {
	big := strings.Repeat("x", 3<<20) // 3MiB
	got := readAllUpstreamErr(strings.NewReader(big))
	if len(got) != 1<<20 {
		t.Fatalf("错误体应截断到 1MiB，got %d bytes", len(got))
	}
	// 上限内的正常短错误体必须原样保留（截断不能吃掉可用信息）。
	plain := `{"code":"101","message":"Signature invalid"}`
	if s := readAllUpstreamErr(strings.NewReader(plain)); s != plain {
		t.Fatalf("短错误体应原样返回，got %q", s)
	}
}

func TestFailoverBackoff_ExistsAndSane(t *testing.T) {
	if failoverBackoff <= 0 {
		t.Fatalf("failoverBackoff = %v, 必须 > 0 才能压住换号风暴（等于旧缺陷）", failoverBackoff)
	}
	if failoverBackoff > 500*time.Millisecond {
		t.Fatalf("failoverBackoff = %v, 过大将显著放大端到端时延", failoverBackoff)
	}
}
