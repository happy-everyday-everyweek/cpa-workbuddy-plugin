package main

import (
	"testing"
	"time"
)

// TestModelCooldownOnlyBlocksSameModel 锁定账号×模型维度的核心语义：
// 限流类失败只冷却出问题的模型，该账号在其它模型上仍可正常路由。
func TestModelCooldownOnlyBlocksSameModel(t *testing.T) {
	const authID = "zcode-model-scope"
	resetAccountFailover(authID)
	t.Cleanup(func() { resetAccountFailover(authID) })

	const badModel = "GLM-5.3"
	const goodModel = "GLM-4.6"

	// 429 属账号×模型级。
	noteAccountFailureForModel(authID, badModel, 429, "rate limit exceeded")

	if !isAuthModelCoolingDown(authID, badModel) {
		t.Fatal("失败模型应处于冷却")
	}
	if isAuthModelCoolingDown(authID, goodModel) {
		t.Fatal("其它模型不应被账号×模型级冷却挡住")
	}
	// 账号级判定必须为假：模型维度不得把整个账号摘掉。
	if isAccountCoolingDown(authID) {
		t.Fatal("账号×模型级冷却不应污染账号维度")
	}

	count, until, ok := authModelFailoverSnapshot(authID, badModel)
	if !ok || count != 1 {
		t.Fatalf("模型维度计数 = %d, ok = %v; want 1", count, ok)
	}
	if remain := time.Until(until); remain <= 55*time.Second || remain > 65*time.Second {
		t.Fatalf("模型维度冷却剩余 = %v, want ~60s", remain)
	}
}

// TestAccountFailureStillBlocksAllModels 锁定账号级失败语义不变：
// 401/403/传输错误等仍会让账号在所有模型上让位。
func TestAccountFailureStillBlocksAllModels(t *testing.T) {
	const authID = "zcode-account-scope"
	resetAccountFailover(authID)
	t.Cleanup(func() { resetAccountFailover(authID) })

	noteAccountFailureForModel(authID, "GLM-5.3", 403, "forbidden")
	if !isAccountCoolingDown(authID) {
		t.Fatal("账号级失败必须让账号整体冷却")
	}
	if !isAuthModelCoolingDown(authID, "GLM-5.3") {
		t.Fatal("账号级冷却应挡住原模型")
	}
	if !isAuthModelCoolingDown(authID, "other-model") {
		t.Fatal("账号级冷却应挡住其它模型")
	}
}

// TestModelCooldownLadderIncrements 验证模型维度同样走阶梯，且与账号层计数独立。
func TestModelCooldownLadderIncrements(t *testing.T) {
	const authID = "zcode-model-ladder"
	const model = "GLM-5.3"
	resetAccountFailover(authID)
	t.Cleanup(func() { resetAccountFailover(authID) })

	wants := []time.Duration{60 * time.Second, 120 * time.Second, 180 * time.Second}
	for i, want := range wants {
		noteAccountFailureForModel(authID, model, 429, "rate limit")
		count, until, ok := authModelFailoverSnapshot(authID, model)
		if !ok || count != i+1 {
			t.Fatalf("第 %d 次失败后计数 = %d, ok = %v; want %d", i+1, count, ok, i+1)
		}
		remain := time.Until(until)
		if remain <= want-5*time.Second || remain > want+5*time.Second {
			t.Fatalf("第 %d 次失败后冷却剩余 = %v, want ~%v", i+1, remain, want)
		}
		failoverMu.RLock()
		accountCount := failoverRecords[authID].failCount
		failoverMu.RUnlock()
		if accountCount != 0 {
			t.Fatalf("模型维度失败不应推动账号层计数，got %d", accountCount)
		}
	}
}

// TestModelCooldownClearedOnSuccess 验证成功请求会清掉模型维度残留。
func TestModelCooldownClearedOnSuccess(t *testing.T) {
	const authID = "zcode-model-reset"
	const model = "GLM-5.3"
	resetAccountFailover(authID)
	t.Cleanup(func() { resetAccountFailover(authID) })

	noteAccountFailureForModel(authID, model, 429, "rate limit")
	if !isAuthModelCoolingDown(authID, model) {
		t.Fatal("前置条件不成立：应处于模型冷却")
	}

	resetAccountFailover(authID)

	if isAuthModelCoolingDown(authID, model) {
		t.Fatal("成功请求后模型冷却应被清除")
	}
	if _, _, ok := authModelFailoverSnapshot(authID, model); ok {
		t.Fatal("成功请求后模型维度记录应被清除")
	}
}

// TestResetAuthModelFailoverKeepsAccountState 验证精确清除只影响指定模型。
func TestResetAuthModelFailoverKeepsAccountState(t *testing.T) {
	const authID = "zcode-model-precise"
	resetAccountFailover(authID)
	t.Cleanup(func() { resetAccountFailover(authID) })

	noteAccountFailureForModel(authID, "model-a", 429, "rate limit")
	noteAccountFailureForModel(authID, "model-b", 429, "rate limit")

	resetAuthModelFailover(authID, "model-a")

	if isAuthModelCoolingDown(authID, "model-a") {
		t.Fatal("model-a 应已被精确清除")
	}
	if !isAuthModelCoolingDown(authID, "model-b") {
		t.Fatal("model-b 的冷却不应被误清")
	}
}

// TestLastFailoverModelRecorded 验证最后失败模型被记录，供冷却前探活回放。
func TestLastFailoverModelRecorded(t *testing.T) {
	const authID = "zcode-last-model"
	resetAccountFailover(authID)
	t.Cleanup(func() { resetAccountFailover(authID) })

	noteAccountFailureForModel(authID, "model-x", 429, "rate limit")
	if got := lastFailoverModel(authID); got != "model-x" {
		t.Fatalf("lastFailoverModel = %q, want %q", got, "model-x")
	}

	noteAccountFailureForModel(authID, "model-y", 403, "forbidden")
	if got := lastFailoverModel(authID); got != "model-y" {
		t.Fatalf("账号级失败后 lastFailoverModel = %q, want %q", got, "model-y")
	}

	resetAccountFailover(authID)
	if got := lastFailoverModel(authID); got != "" {
		t.Fatalf("成功后 lastFailoverModel 应清空，got %q", got)
	}
}

// TestModelCooldownEmptyModelFallsBackToAccount 验证模型名为空时退化为账号级。
func TestModelCooldownEmptyModelFallsBackToAccount(t *testing.T) {
	const authID = "zcode-empty-model"
	resetAccountFailover(authID)
	t.Cleanup(func() { resetAccountFailover(authID) })

	noteAccountFailureForModel(authID, "", 429, "rate limit")
	if !isAccountCoolingDown(authID) {
		t.Fatal("无模型维度可用时应退化为账号级冷却")
	}
}

// TestModelCooldownIsolatedAcrossAccounts 验证模型冷却按账号隔离。
func TestModelCooldownIsolatedAcrossAccounts(t *testing.T) {
	resetAccountFailover("zcode-a")
	resetAccountFailover("zcode-b")
	t.Cleanup(func() {
		resetAccountFailover("zcode-a")
		resetAccountFailover("zcode-b")
	})

	noteAccountFailureForModel("zcode-a", "model-x", 429, "rate limit")

	if !isAuthModelCoolingDown("zcode-a", "model-x") {
		t.Fatal("zcode-a 应处于 model-x 冷却")
	}
	if isAuthModelCoolingDown("zcode-b", "model-x") {
		t.Fatal("zcode-b 不应受 zcode-a 的模型冷却影响")
	}
}
