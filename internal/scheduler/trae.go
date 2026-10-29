// TRAE 账号的签到与积分分支（scheduler）。
//
// 对齐参考实现 `trae2api-web` 的 scheduler 口径：签到走 UG 接口（Cloud-IDE-JWT +
// X-User-Region: CN + X-Device-Id），积分取 ide_user_ent_usage 的 credits_limit 汇总。
package scheduler

import (
	"log"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	traepkg "github.com/linguo2625469/workbuddy2api-panel/internal/upstream/trae"
)

// refreshTraeCredits 刷新单个 TRAE 账号的积分快照（周期余额刷新与手动刷新共用）。
//
// 数据源：POST {ug}/trae/api/v2/pay/ide_user_ent_usage，
// remain = Σ(credits_limit - credits_amount)，total = Σ credits_limit。
func (s *Scheduler) refreshTraeCredits(a *auth.Auth, uid string) {
	label := logfmt.Label(uid, a.Nickname)
	acct := traeSchedulerAccount(a)
	if acct.AccessToken == "" {
		return
	}
	remain, limit, _, packs, err := traepkg.NewClient(nil).EntUsage(acct)
	if err != nil {
		log.Printf("trae balance %s: %v", label, err)
		return
	}
	if packs == 0 {
		return // 无权益包：不覆盖既有快照（宁可留旧值，也不显示 0）
	}
	s.cfg.Pool.ReenableIfCredits(uid, remain, limit)
	s.cfg.Pool.SetCreditsSnapshot(uid, pool.CreditsSnapshot{
		Remain: remain,
		Total:  limit,
	})
	log.Printf("trae balance %s: %d/%d（%d 个权益包）", label, remain, limit, packs)
}

// runTraeCheckin 单个 TRAE 账号的签到 + 积分快照（RunCheckinNow 的 TRAE 分支）。
//
// 语义与 workbuddy/Qoder 分支对齐：签到幂等归一（今天已签不报错）、
// 积分进池的 CreditsSnapshot。
func (s *Scheduler) runTraeCheckin(a *auth.Auth, uid string) {
	label := logfmt.Label(uid, a.Nickname)
	acct := traeSchedulerAccount(a)
	if acct.AccessToken == "" {
		return
	}
	client := traepkg.NewClient(nil)

	// 先查状态，未签到才 claim（避免无谓 POST）。
	if checkedIn, credits, enable, err := client.CheckinStatus(acct); err != nil {
		log.Printf("trae checkin %s: status: %v", label, err)
	} else if checkedIn {
		s.cfg.Pool.NoteCheckinDone(uid)
		log.Printf("trae checkin %s: 今天已签到（%d 积分）", label, credits)
	} else if !enable {
		log.Printf("trae checkin %s: 签到未启用，跳过", label)
	} else if err := client.CheckinClaim(acct); err != nil {
		log.Printf("trae checkin %s: %v", label, err)
	} else {
		s.cfg.Pool.NoteCheckinDone(uid)
		log.Printf("trae checkin %s: 签到成功", label)
	}

	// 积分快照（credits_limit - credits_amount 汇总）。
	remain, limit, _, packs, err := client.EntUsage(acct)
	if err != nil {
		log.Printf("trae quota %s: %v", label, err)
		return
	}
	if packs == 0 {
		return
	}
	s.cfg.Pool.ReenableIfCredits(uid, remain, limit)
	s.cfg.Pool.SetCreditsSnapshot(uid, pool.CreditsSnapshot{
		Remain: remain,
		Total:  limit,
	})
	log.Printf("trae quota %s: %d/%d（%d 个权益包）", label, remain, limit, packs)
}

// traeSchedulerAccount auth.Auth → trae 出站账号视图。
func traeSchedulerAccount(a *auth.Auth) traepkg.Account {
	return traepkg.Account{
		UID:          a.UID,
		Nickname:     a.Nickname,
		AccessToken:  a.AccessTokenValue(),
		RefreshToken: a.RefreshTokenValue(),
		MachineID:    a.MachineID,
		DeviceID:     a.DeviceID,
		ExpiresAt:    a.ExpiresAt,
	}
}
