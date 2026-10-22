// failover_accounting.go bridges the failover state (accountFailover.go) and
// the anomaly pool (anomaly.go) with host auth records. noteAccountFailure is
// the single entry point the executor calls on upstream failures; it records
// the cooldown under the ID the caller passes and mirrors the failure to the
// canonical auth ID when the caller used an auth_index.
package main

import "strings"

// noteAccountFailure records an upstream failure for failover + anomaly
// accounting. Returns true when the failure was counted. The caller may pass
// either an auth ID (scheduler key) or an auth_index; the canonical ID is
// resolved and mirrored so the panel's failover snapshot stays consistent.
func noteAccountFailure(authID string, status int, body string) bool {
	return noteAccountFailureForModel(authID, "", status, body)
}

// noteAccountFailureForModel 是带模型维度的记账入口。
// 模型级失败只冷却该模型；账号级失败行为与历史一致。
// [参数] authID：账号唯一标识；model：已归一化的上游模型名，可为空；
// status：上游 HTTP 状态码；body：上游响应正文。
// [返回] true 表示失败已计入冷却阶梯。
// 最近修改时间：2026-10-19 01:20:00；改动原因：新增账号×模型双层冷却。
func noteAccountFailureForModel(authID, model string, status int, body string) bool {
	if !failoverActive() || strings.TrimSpace(authID) == "" {
		return false
	}
	if !recordAccountFailureForModel(authID, model, status, body) {
		return false
	}
	go func() {
		_, id := resolveAuthIndexAndID(authID)
		if id == "" || id == authID {
			return
		}
		recordAccountFailureForModel(id, model, status, body)
	}()
	return true
}

// noteForcedAccountFailure records a failure that bypasses isAccountFailure
// classification (see recordForcedFailure). Used by pseudo-completion
// detection, which must drive failover off an account even though the
// upstream response is a protocol-valid HTTP 200 + done.
func noteForcedAccountFailure(authID string, body string) bool {
	return noteForcedAccountFailureForModel(authID, "", body)
}

// noteForcedAccountFailureForModel 是带模型维度的强制记账入口。
// 伪完成归账号级冷却，但会把失败模型写入 lastModel，供禁用探活回放。
// [参数] authID：账号唯一标识；model：已归一化的上游模型名，可为空；body：失败原因文本。
// [返回] true 表示失败已计入。
// 最近修改时间：2026-10-19 01:20:00；改动原因：新增，让伪完成冷却也能被探活覆盖。
func noteForcedAccountFailureForModel(authID, model, body string) bool {
	if !failoverActive() || strings.TrimSpace(authID) == "" {
		return false
	}
	if !recordForcedFailureForModel(authID, model, body) {
		return false
	}
	go func() {
		_, id := resolveAuthIndexAndID(authID)
		if id == "" || id == authID {
			return
		}
		recordForcedFailureForModel(id, model, body)
	}()
	return true
}

// resolveAuthIndexAndID maps either an auth_index or an auth ID to the
// (authIndex, authID) pair the host understands. Returns empty strings when
// unresolvable. Used by anomaly freeze and failover mirroring.
func resolveAuthIndexAndID(authID string) (string, string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return "", ""
	}
	// Fast path: already an auth_index the host understands.
	if _, err := hostAuthGet(authID); err == nil {
		if files, err := hostAuthList(); err == nil {
			for _, f := range files {
				if f.AuthIndex == authID {
					return authID, f.ID
				}
			}
		}
		return authID, ""
	}
	files, err := hostAuthList()
	if err != nil {
		return "", ""
	}
	wantName := "traework-" + authID + ".json"
	for _, f := range files {
		if f.AuthIndex == authID || f.ID == authID || f.Name == authID {
			return f.AuthIndex, f.ID
		}
		if f.Name == wantName {
			return f.AuthIndex, f.ID
		}
	}
	return "", ""
}
