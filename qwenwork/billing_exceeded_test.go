package main

import "testing"

// TestBuildCreditsSummary_ExceededFlowsIntoSnapshot 验证上游 quota.exceeded
// 被写入额度快照。免费版账号没有额度包，total_used/total_size 恒为 0，
// 若解析层漏映射该信号，耗尽状态会被降级成「未知」，面板永远转圈。
func TestBuildCreditsSummary_ExceededFlowsIntoSnapshot(t *testing.T) {
	ctx := []byte(`{"code":"ok","data":{"quota":{"total":null,"used":null,"remaining":0,"exceeded":true}}}`)
	wal := []byte(`{"code":"ok","data":{"daily_credits":{"total_balance":0},` +
		`"monthly_credits":{"total_balance":0},"longterm_credits":{"total_balance":0},` +
		`"expiring_soon":{"count":0,"total_balance":0}}}`)

	cr := buildCreditsSummary(ctx, wal, true, true)
	if !cr.Exceeded {
		t.Fatal("quota.exceeded=true must be carried into the snapshot")
	}
	if cr.TotalRemain != 0 {
		t.Fatalf("exceeded snapshot must have zero remain, got %d", cr.TotalRemain)
	}
	if !isCreditsExhausted(cr) {
		t.Fatal("exceeded snapshot must be judged exhausted")
	}
}

// TestBuildCreditsSummary_HealthyAccountNotExceeded 验证正常账号不会被误判。
func TestBuildCreditsSummary_HealthyAccountNotExceeded(t *testing.T) {
	ctx := []byte(`{"code":"ok","data":{"quota":{"total":null,"used":null,"remaining":0,"exceeded":false}}}`)
	wal := []byte(`{"code":"ok","data":{"daily_credits":{"total_balance":0},` +
		`"monthly_credits":{"total_balance":0},"longterm_credits":{"total_balance":2098},` +
		`"expiring_soon":{"count":0,"total_balance":0}}}`)

	cr := buildCreditsSummary(ctx, wal, true, true)
	if cr.Exceeded {
		t.Fatal("healthy account must not be marked exceeded")
	}
	if cr.TotalRemain != 2098 {
		t.Fatalf("remain = %d, want 2098", cr.TotalRemain)
	}
	if isCreditsExhausted(cr) {
		t.Fatal("funded account must not be judged exhausted")
	}
}

// TestBuildCreditsSummary_WalletFailureKeepsQuotaFallback 验证钱包接口失败时
// 仍能从 account-context 的 remaining 推出额度，避免把失败当耗尽。
func TestBuildCreditsSummary_WalletFailureKeepsQuotaFallback(t *testing.T) {
	ctx := []byte(`{"code":"ok","data":{"quota":{"total":null,"used":null,"remaining":500,"exceeded":false}}}`)

	cr := buildCreditsSummary(ctx, nil, true, false)
	if cr.TotalRemain != 500 {
		t.Fatalf("remain = %d, want 500 from quota.remaining fallback", cr.TotalRemain)
	}
	if isCreditsExhausted(cr) {
		t.Fatal("funded account with failed wallet call must not be exhausted")
	}
}
