// realm_fallback.go 跨域回退（realm fallback）判定与切换辅助。
//
// 语义：请求 "cn:xxx" 时，若 cn 池遇到限额（ErrHardCredit）、限频（ErrSoftRate）、
// 账号禁用（ErrSessionDead/ErrAccountFault）或选不出可用账号，自动切到 "global:xxx"
// （同裸模型名、另一 realm 的账号）重试一轮，客户端无感；反向（global → cn）由配置
// 显式开启。开关口径见 cmd/server 的 realm_fallback 段（cn_to_global 缺省 true）。
package server

import (
	"errors"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// otherRealm 返回另一 realm（cn↔global）；其他值（含空串）原样返回——
// 非双域前缀的旧语义不得被回退逻辑改写。
func otherRealm(r string) string {
	switch r {
	case "cn":
		return "global"
	case "global":
		return "cn"
	}
	return r
}

// realmFallbackAllowed 报告 from realm 的跨域回退是否被放行。
//
// GlobalEnabled 是一票否决权（config global.enabled，缺省 true）：false（纯 CN
// 逃生门）时 global 域整体不可路由，任何回退都只会选到不该出站的账号。
// 其余按方向取键：cn → realm_fallback.cn_to_global（缺省 true，handler 缺省关——
// 由 main 显式接线）；global → realm_fallback.global_to_cn（缺省关）。
func (h *Handler) realmFallbackAllowed(from string) bool {
	if !h.cfg.GlobalEnabled {
		return false
	}
	switch from {
	case "cn":
		return h.cfg.RealmFallbackCnToGlobal
	case "global":
		return h.cfg.RealmFallbackGlobalToCn
	}
	return false
}

// realmFallbackEligible 报告错误是否属「另一域可能救活」的类别：余额耗尽
// （ErrHardCredit）、软限流（ErrSoftRate）、死 session（ErrSessionDead）、
// 账号级授权/配额故障（ErrAccountFault）。这四类都是**本域账号侧**的耗尽信号，
// 换域即换池，另一域的号不共享该状态。
//
// 其他类别不回退：请求级错误（11101/11115/图片无效）换域同样失败；传输层错误与
// 上游超时不是账号问题（回退只会放大请求量）；内容拦截同理。
// 非 *upstream.Error（本地调度类错误，如无可用账号）不在此判定——那条路径由
// 调用点按「本域是否真有账号」自行决定（选不出号 ≠ 错误类别可回退）。
func realmFallbackEligible(err error) bool {
	var ue *upstream.Error
	if !errors.As(err, &ue) {
		return false
	}
	switch ue.Kind {
	case upstream.ErrHardCredit, upstream.ErrSoftRate, upstream.ErrSessionDead, upstream.ErrAccountFault:
		return true
	}
	return false
}
