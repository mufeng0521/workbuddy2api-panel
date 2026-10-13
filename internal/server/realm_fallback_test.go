// realm_fallback_test.go 跨域回退（cn: ↔ global:）端到端测试。
//
// 覆盖：cn 池限额（402）回退、预算耗尽回退、本域选不出号回退、开关关闭不回退、
// 反向缺省关（附显式打开的正例）、另一域无号时保持原错误语义。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// realmFakeUpstream 构造两域都指向 fake 的 upstream client。
//
// newFakeUpstream 只设了 CN base，global 号的出站 base 会回落到内置默认
// https://www.workbuddy.ai（ChatBaseGlobal 空时）——测试必须把 global base 也钉到
// fake 域名，任何路径都不许碰真实上游。
func realmFakeUpstream(t *testing.T, behavior func(authz string) (int, string, bool)) *upstream.Client {
	t.Helper()
	up := newFakeUpstream(t, behavior)
	up.ChatBaseGlobal = "https://fake.example"
	up.BillingBaseGlobal = "https://fake.example"
	return up
}

// enableGlobalRealm 确保 auth 侧 global 开关处于缺省开启态：Realm() 才会把
// workbuddy.ai 后缀的号判为 global。显式设置 + 恢复，防别的测试留下关闭态。
func enableGlobalRealm(t *testing.T) {
	t.Helper()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
}

// fastRotation 把轮转退避基数临时置 0：本文件的用例大多要把 MaxRotate 轮转打满
// （真实退避 500ms·2^i 封顶 8s 会让单个用例排队十几秒），backoff.go 已声明
// "测试可置 0 跳过等待"。d<=0 时 rotateBackoff 直接返回 ctx.Err()==nil，
// 轮转分支走向与 sleeps 后一致。t.Cleanup 恢复原值；本包测试无 t.Parallel。
func fastRotation(t *testing.T) {
	t.Helper()
	old := rotateBackoffBase
	rotateBackoffBase = 0
	t.Cleanup(func() { rotateBackoffBase = old })
}

// cnAcct / globalAcct 构造两个域的账号（realm 判定口径见 auth.Realm()）。
func cnAcct(uid, token string) *auth.Auth {
	return &auth.Auth{UID: uid, AccessToken: token, ExpiresAt: 9999999999}
}

func globalAcct(uid, token string) *auth.Auth {
	return &auth.Auth{UID: uid, AccessToken: token, ExpiresAt: 9999999999, Domain: "workbuddy.ai"}
}

// TestChatRealmFallbackCNHardCreditToGlobal cn 号 402（ErrHardCredit，余额耗尽）：
// 换号到 cn 池打干后自动回退 global 的同名裸模型，客户端拿到 200 无感。
func TestChatRealmFallbackCNHardCreditToGlobal(t *testing.T) {
	enableGlobalRealm(t)
	fastRotation(t)
	calls := map[string]int{}
	up := realmFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if strings.Contains(authz, "at-cn") {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		cnAcct("cn1", "at-cn1"),
		cnAcct("cn2", "at-cn2"),
		globalAcct("g1", "at-g1"),
	)
	h := NewHandler(Config{
		Pool: p, Upstream: up,
		RealmFallbackCnToGlobal: true, GlobalEnabled: true,
		SoftCooldown: time.Minute,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s（cn 池余额耗尽应回退 global 并成功）", rec.Code, rec.Body)
	}
	if calls["Bearer at-cn1"] != 1 || calls["Bearer at-cn2"] != 1 {
		t.Errorf("cn 号调用次数=%v，want 各 1 次", calls)
	}
	if calls["Bearer at-g1"] != 1 {
		t.Errorf("global 号未被调用（回退未发生）：%v", calls)
	}
	if total := calls["Bearer at-cn1"] + calls["Bearer at-cn2"] + calls["Bearer at-g1"]; total > 2*h.cfg.MaxRotate {
		t.Errorf("总上游调用 %d 次 > 2×MaxRotate(%d)", total, 2*h.cfg.MaxRotate)
	}
}

// TestChatRealmFallbackBudgetExhausted cn 号数 > MaxRotate 且全 429（ErrSoftRate）：
// 轮转预算耗尽时（而非池子无号时）触发回退，另一域另有完整一轮预算。
func TestChatRealmFallbackBudgetExhausted(t *testing.T) {
	enableGlobalRealm(t)
	fastRotation(t)
	calls := map[string]int{}
	up := realmFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if strings.Contains(authz, "at-cn") {
			return 429, `{"code":1,"msg":"too many requests, please slow down"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		cnAcct("cn1", "at-cn1"),
		cnAcct("cn2", "at-cn2"),
		cnAcct("cn3", "at-cn3"),
		globalAcct("g1", "at-g1"),
	)
	const maxRotate = 2
	h := NewHandler(Config{
		Pool: p, Upstream: up, MaxRotate: maxRotate,
		RealmFallbackCnToGlobal: true, GlobalEnabled: true,
		SoftCooldown: time.Minute,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s（轮转预算耗尽应切域，而非直接报错）", rec.Code, rec.Body)
	}
	if calls["Bearer at-cn1"] != 1 || calls["Bearer at-cn2"] != 1 {
		t.Errorf("预算内应按序换号：%v", calls)
	}
	if calls["Bearer at-cn3"] != 0 {
		t.Errorf("第 3 个 cn 号不应被调用（MaxRotate=%d 预算耗尽即切域）：%v", maxRotate, calls)
	}
	if calls["Bearer at-g1"] != 1 {
		t.Errorf("global 号未被调用（回退未发生）：%v", calls)
	}
	if total := calls["Bearer at-cn1"] + calls["Bearer at-cn2"] + calls["Bearer at-g1"]; total > 2*maxRotate {
		t.Errorf("总上游调用 %d 次 > 2×MaxRotate(%d)", total, 2*maxRotate)
	}
}

// TestChatRealmFallbackNoServableAccount cn 号全禁用（选号返回 nil）：
// 本域有号但一个都不可选 → 回退 global，而不是把 503 直接甩给客户端。
func TestChatRealmFallbackNoServableAccount(t *testing.T) {
	enableGlobalRealm(t)
	fastRotation(t)
	calls := map[string]int{}
	up := realmFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 200, sseOK, true
	})
	p := testPoolWith(
		cnAcct("cn1", "at-cn1"),
		cnAcct("cn2", "at-cn2"),
		globalAcct("g1", "at-g1"),
	)
	p.Disable("cn1", "test: disabled")
	p.Disable("cn2", "test: disabled")
	h := NewHandler(Config{
		Pool: p, Upstream: up,
		RealmFallbackCnToGlobal: true, GlobalEnabled: true,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s（cn 池全禁用应回退 global）", rec.Code, rec.Body)
	}
	if calls["Bearer at-g1"] != 1 || len(calls) != 1 {
		t.Errorf("应只调用 global 号：%v", calls)
	}
}

// TestChatRealmFallbackDisabledStaysOriginal 开关关（RealmFallbackCnToGlobal 缺省
// 零值）：维持原错误语义（429 限流透传），不回退、不额外打上游。
func TestChatRealmFallbackDisabledStaysOriginal(t *testing.T) {
	enableGlobalRealm(t)
	fastRotation(t)
	calls := map[string]int{}
	up := realmFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if strings.Contains(authz, "at-cn") {
			return 429, `{"code":1,"msg":"too many requests, please slow down"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		cnAcct("cn1", "at-cn1"),
		globalAcct("g1", "at-g1"),
	)
	h := NewHandler(Config{
		Pool: p, Upstream: up, GlobalEnabled: true, // 回退开关不设 = 关
		SoftCooldown: time.Minute,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s（开关关时保持 429）", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "rate_limit_exceeded") {
		t.Errorf("响应应保留 rate_limit_exceeded 语义：%s", rec.Body)
	}
	if calls["Bearer at-g1"] != 0 {
		t.Errorf("开关关时不应回退 global：%v", calls)
	}
}

// TestChatRealmFallbackGlobalToCNDefaultOff "global:m" 全 429：
// 缺省（GlobalToCn=false）不回切 cn；显式打开时回切 cn 并成功（反向链路正例）。
func TestChatRealmFallbackGlobalToCNDefaultOff(t *testing.T) {
	enableGlobalRealm(t)
	fastRotation(t)
	behavior := func(authz string) (int, string, bool) {
		if strings.Contains(authz, "at-g") {
			return 429, `{"code":1,"msg":"too many requests, please slow down"}`, false
		}
		return 200, sseOK, true
	}
	const req = `{"model":"global:glm-5.2","messages":[]}`

	// 缺省：global 池耗尽不回切 cn。
	calls := map[string]int{}
	up := realmFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return behavior(authz)
	})
	h := NewHandler(Config{
		Pool: testPoolWith(
			globalAcct("g1", "at-g1"),
			cnAcct("cn1", "at-cn1"),
		),
		Upstream: up, GlobalEnabled: true, SoftCooldown: time.Minute,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(req)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s（GlobalToCn 缺省关应保持 429）", rec.Code, rec.Body)
	}
	if calls["Bearer at-cn1"] != 0 {
		t.Errorf("GlobalToCn 缺省关，不应回切 cn：%v", calls)
	}

	// 正例对照：显式打开 → 回切 cn 成功（证明缺口只在开关，不在链路）。
	calls2 := map[string]int{}
	up2 := realmFakeUpstream(t, func(authz string) (int, string, bool) {
		calls2[authz]++
		return behavior(authz)
	})
	h2 := NewHandler(Config{
		Pool: testPoolWith(
			globalAcct("g1", "at-g1"),
			cnAcct("cn1", "at-cn1"),
		),
		Upstream: up2, GlobalEnabled: true,
		RealmFallbackGlobalToCn: true,
		SoftCooldown:            time.Minute,
	})
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(req)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s（GlobalToCn 打开应回切 cn 成功）", rec2.Code, rec2.Body)
	}
	if calls2["Bearer at-cn1"] != 1 || calls2["Bearer at-g1"] != 1 {
		t.Errorf("回切调用分布不对：%v", calls2)
	}
}

// TestChatRealmFallbackNoGlobalAccountKeepsRateLimit cn 全 429 且池里没有 global 号：
// 回退尝试落空后，响应仍是 429 rate_limit_exceeded（不能把限流语义改写成
// no_healthy_account——两者的客户端处置完全不同）。
func TestChatRealmFallbackNoGlobalAccountKeepsRateLimit(t *testing.T) {
	enableGlobalRealm(t)
	fastRotation(t)
	calls := map[string]int{}
	up := realmFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 429, `{"code":1,"msg":"too many requests, please slow down"}`, false
	})
	p := testPoolWith(cnAcct("cn1", "at-cn1")) // 无 global 号
	h := NewHandler(Config{
		Pool: p, Upstream: up,
		RealmFallbackCnToGlobal: true, GlobalEnabled: true,
		SoftCooldown: time.Minute,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s（无 global 号可回退时应保持 429）", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "rate_limit_exceeded") {
		t.Errorf("应保留 rate_limit_exceeded：%s", body)
	}
	if strings.Contains(body, "no_healthy_account") {
		t.Errorf("限流语义不应被改写成 no_healthy_account：%s", body)
	}
	if calls["Bearer at-cn1"] != 1 || len(calls) != 1 {
		t.Errorf("只应调用 cn 号一次：%v", calls)
	}
}

// TestChatRealmFallbackGlobalDisabledVeto GlobalEnabled=false（纯 CN 逃生门，
// config global.enabled=false）是一票否决：即便回退开关打开，也不许把请求切到
// global 域（那里选出的号本不该出站）。
func TestChatRealmFallbackGlobalDisabledVeto(t *testing.T) {
	enableGlobalRealm(t)
	fastRotation(t)
	calls := map[string]int{}
	up := realmFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if strings.Contains(authz, "at-cn") {
			return 429, `{"code":1,"msg":"too many requests, please slow down"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		cnAcct("cn1", "at-cn1"),
		globalAcct("g1", "at-g1"),
	)
	h := NewHandler(Config{
		Pool: p, Upstream: up,
		RealmFallbackCnToGlobal: true, GlobalEnabled: false,
		SoftCooldown: time.Minute,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s（GlobalEnabled=false 必须保持原错误语义）", rec.Code, rec.Body)
	}
	if calls["Bearer at-g1"] != 0 {
		t.Errorf("GlobalEnabled=false 时 global 号必须零调用：%v", calls)
	}
	if calls["Bearer at-cn1"] != 1 || len(calls) != 1 {
		t.Errorf("只应调用 cn 号一次：%v", calls)
	}
}

// TestChatRealmFallbackBidirectionalSingleSwitch 双向开关都打开且两域全部 429：
// 整个请求只允许切域一次——cn 一轮（MaxRotate 次）+ 回退后 global 一轮，
// 上游调用总数恰为 2×MaxRotate；不出现两域互踢的死循环（realmSwitched 单次闸）。
func TestChatRealmFallbackBidirectionalSingleSwitch(t *testing.T) {
	enableGlobalRealm(t)
	fastRotation(t)
	calls := map[string]int{}
	up := realmFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 429, `{"code":1,"msg":"too many requests, please slow down"}`, false
	})
	p := testPoolWith(
		cnAcct("cn1", "at-cn1"), cnAcct("cn2", "at-cn2"),
		globalAcct("g1", "at-g1"), globalAcct("g2", "at-g2"),
	)
	const maxRotate = 2
	h := NewHandler(Config{
		Pool: p, Upstream: up, MaxRotate: maxRotate,
		RealmFallbackCnToGlobal: true, RealmFallbackGlobalToCn: true, GlobalEnabled: true,
		SoftCooldown: time.Minute,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s（两域全限流应回 429，不得死循环/500）", rec.Code, rec.Body)
	}
	total := 0
	for _, n := range calls {
		total += n
	}
	// 精确上界：只切一次域 → 两轮各打满 MaxRotate，总数恰 2×MaxRotate
	//（若允许二次切域，这里会翻到 3×MaxRotate 以上）。
	if total != 2*maxRotate {
		t.Errorf("上游调用总数=%d want %d（只切一次域，无两域互踢）：%v", total, 2*maxRotate, calls)
	}
	if calls["Bearer at-cn1"] != 1 || calls["Bearer at-cn2"] != 1 ||
		calls["Bearer at-g1"] != 1 || calls["Bearer at-g2"] != 1 {
		t.Errorf("各号应恰被调一次：%v", calls)
	}
}

// TestChatRealmFallbackStickyBindsOnlySameRealm 跨域回退成功后不得把会话粘性绑到
// 另一域账号（否则客户端后续仍发 cn: 前缀时会「命中粘性 → 域不符解绑 → 重选」白抖
// 一轮）；同域（含同域轮转换号）的粘性跟随语义必须原样保留。
func TestChatRealmFallbackStickyBindsOnlySameRealm(t *testing.T) {
	enableGlobalRealm(t)
	fastRotation(t)
	const req = `{"model":"cn:glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`

	// 场景一：cn 号 402 → 回退 global 成功 → 不绑（sticky 域检查）。
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"cn1", "g1"} },
	})
	up := realmFakeUpstream(t, func(authz string) (int, string, bool) {
		if strings.Contains(authz, "at-cn") {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool: testPoolWith(
			cnAcct("cn1", "at-cn1"),
			globalAcct("g1", "at-g1"),
		),
		Upstream: up, Session: sess,
		RealmFallbackCnToGlobal: true, GlobalEnabled: true,
		SoftCooldown: time.Minute,
	})
	sess.Bind("conv-1", "cn1") // 模拟历史粘性：会话此前绑在 cn 号上
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(req)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s（cn 402 应回退 global 并成功）", rec.Code, rec.Body)
	}
	if uid, ok := st.lastUID("conv-1"); ok {
		t.Errorf("跨域回退成功号不应绑粘性，got %q（下一轮 cn: 请求会域不符解绑，白抖一轮）", uid)
	}

	// 场景二：同域轮转换号（cn1 402 → cn2 200）→ 粘性必须跟随到 cn2（既有语义）。
	st2 := newBindStore()
	sess2 := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st2,
		Available: func() []string { return []string{"cn1", "cn2", "g1"} },
	})
	up2 := realmFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-cn1" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	h2 := NewHandler(Config{
		Pool: testPoolWith(
			cnAcct("cn1", "at-cn1"),
			cnAcct("cn2", "at-cn2"),
			globalAcct("g1", "at-g1"),
		),
		Upstream: up2, Session: sess2,
		RealmFallbackCnToGlobal: true, GlobalEnabled: true,
		SoftCooldown: time.Minute,
	})
	sess2.Bind("conv-1", "cn1")
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(req)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s（同域换号应成功）", rec2.Code, rec2.Body)
	}
	if uid, ok := st2.lastUID("conv-1"); !ok || uid != "cn2" {
		t.Errorf("同域成功应照常绑粘性到 cn2，got %q ok=%v（域检查不得破坏既有语义）", uid, ok)
	}
}
