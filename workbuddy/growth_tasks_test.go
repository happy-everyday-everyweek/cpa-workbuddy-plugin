// growth_tasks_test.go 覆盖任务域的逐条结果判定与进度口径判据。
// 这些用例直接对应旧版 0.16.x 的三次生产事故。
package main

import (
	"net/http"
	"strings"
	"testing"
)

// TestAcceptGrowthTasksReadsPerItemStatus 是 0.16.3 事故的回归守卫：
// 上游对前置未满足的任务返回 HTTP 200 + code=0，但逐条把失败写在
// data.results[i].status=error。只认状态码会把全部失败虚报为成功。
// [参数] t：测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-24 新增成长中心测试（TASK-02.1）
func TestAcceptGrowthTasksReadsPerItemStatus(t *testing.T) {
	cleanup := newGrowthTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"results":[
			{"task_code":"chat_5","status":"error","message":"prerequisite not met: first_buddy"},
			{"task_code":"library","status":"ok"}
		]}}`))
	})
	defer cleanup()

	outcome, err := acceptGrowthTasks(&storedAuth{}, []string{"chat_5", "library"})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if outcome.Accepted != 1 {
		t.Fatalf("Accepted = %d, want 1 (only the ok item)", outcome.Accepted)
	}
	if len(outcome.Errors) != 1 || !strings.Contains(outcome.Errors[0], "1 项失败") {
		t.Fatalf("expected one aggregated failure line, got %v", outcome.Errors)
	}
	if !strings.Contains(outcome.Errors[0], "first_buddy") {
		t.Fatalf("failure reason must be surfaced, got %v", outcome.Errors)
	}
}

// TestGrowthTaskClaimableUsesProgress 验证可领判据只看进度口径。
// 这些用例覆盖旧版用 accept_status 判断导致误领的四类边界。
// [参数] t：测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-24 新增成长中心测试（TASK-02.1）
func TestGrowthTaskClaimableUsesProgress(t *testing.T) {
	cases := []struct {
		name string
		task growthTask
		want bool
	}{
		{"达标可领", growthTask{AcceptStatus: "accepted", Progress: &growthProgress{Current: 5, Target: 5}}, true},
		{"超额可领", growthTask{AcceptStatus: "in_progress", Progress: &growthProgress{Current: 6, Target: 5}}, true},
		{"未达标不可领", growthTask{AcceptStatus: "accepted", Progress: &growthProgress{Current: 4, Target: 5}}, false},
		{"已领取不可领", growthTask{AcceptStatus: "claimed", Progress: &growthProgress{Current: 5, Target: 5}}, false},
		{"锁定不可领", growthTask{AcceptStatus: "accepted", Locked: true, Progress: &growthProgress{Current: 5, Target: 5}}, false},
		{"未接单无进度不可领", growthTask{AcceptStatus: "not_accepted", Progress: nil}, false},
		{"目标为零不可领", growthTask{AcceptStatus: "accepted", Progress: &growthProgress{Current: 0, Target: 0}}, false},
	}
	for _, tc := range cases {
		if got := tc.task.growthTaskClaimable(); got != tc.want {
			t.Errorf("%s: claimable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestFetchGrowthTasksParsesNullProgress 验证未接单时 progress 为 null 的解析。
// 上游未接单任务的 progress 恒为 null，强类型非指针会解析失败或误判。
// [参数] t：测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-24 新增成长中心测试（TASK-02.1）
func TestFetchGrowthTasksParsesNullProgress(t *testing.T) {
	cleanup := newGrowthTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[
			{"task_code":"create_canvas","accept_status":"not_accepted","progress":null,"locked":false},
			{"task_code":"chat_5","accept_status":"accepted","progress":{"current":1,"target":5}}
		]}}`))
	})
	defer cleanup()

	tasks, err := fetchGrowthTasks(&storedAuth{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(tasks))
	}
	if tasks[0].Progress != nil {
		t.Fatalf("null progress must stay nil, got %+v", tasks[0].Progress)
	}
	if tasks[1].Progress == nil || tasks[1].Progress.Current != 1 || tasks[1].Progress.Target != 5 {
		t.Fatalf("unexpected progress: %+v", tasks[1].Progress)
	}
}

// TestClaimGrowthTaskTreatsAlreadyClaimedAsSuccess 验证重复领奖幂等语义。
// [参数] t：测试上下文。
// [返回] 无。
// 最近修改时间：2026-10-24 新增成长中心测试（TASK-02.1）
func TestClaimGrowthTaskTreatsAlreadyClaimedAsSuccess(t *testing.T) {
	cleanup := newGrowthTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != growthPathPrefix+"/tasks/chat_5/claim" {
			t.Errorf("unexpected claim path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":true,"credit":0,"energy":0}}`))
	})
	defer cleanup()

	m, err := claimGrowthTask(&storedAuth{}, "chat_5")
	if err != nil {
		t.Fatalf("already_claimed must be treated as success, got: %v", err)
	}
	if m["success"] != true {
		t.Fatalf("expected success=true, got %v", m["success"])
	}
}
