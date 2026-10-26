// Qoder 账号的签到与额度刷新（scheduler 分支）：qoder 包的 sash 接口接线。
package scheduler

import (
	"context"
	"log"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	qoderpkg "github.com/linguo2625469/workbuddy2api-panel/internal/upstream/qoder"
)

// runQoderCheckin 单个 Qoder 账号的签到 + 额度快照（RunCheckinNow 的 Qoder 分支）。
// 语义与 workbuddy 分支对齐：签到幂等归一、余额进池的 CreditsSnapshot（plan 名入 Note）。
func (s *Scheduler) runQoderCheckin(a *auth.Auth, uid string) {
	realm := qoderpkg.RealmFor(qoderRealmOfAuth(a))
	token := a.AccessTokenValue()
	label := logfmt.Label(uid, a.Nickname)

	// 签到（先查状态，CLAIMABLE 才 claim，避免无谓 POST）
	ctx := context.Background()
	client := http.DefaultClient
	if st, err := qoderpkg.FetchCheckinStatus(ctx, client, realm, token); err != nil {
		log.Printf("qoder checkin %s: status: %v", label, err)
	} else if st.Available && st.Active && !st.TodayCheckedIn {
		res, err := qoderpkg.DailyCheckin(ctx, client, realm, token)
		switch {
		case err != nil:
			log.Printf("qoder checkin %s: %v", label, err)
		case res.Already:
			s.cfg.Pool.NoteCheckinDone(uid)
			log.Printf("qoder checkin %s: 今天已签到（幂等）", label)
		case res.OK:
			s.cfg.Pool.NoteCheckinDone(uid)
			log.Printf("qoder checkin %s: %s", label, res.Message)
		default:
			log.Printf("qoder checkin %s: %s", label, res.Message)
		}
	} else if st.Available && st.TodayCheckedIn {
		s.cfg.Pool.NoteCheckinDone(uid)
		log.Printf("qoder checkin %s: 今天已签到（连签 %d 天）", label, st.StreakDays)
	}
	// 区域无 sash 签到接口（intl 404）→ 静默跳过，不刷日志。

	// 额度快照：quota/usage 汇总（remain/used/size/套餐名）。
	q, err := qoderpkg.FetchQuota(ctx, client, realm, token)
	if err != nil {
		log.Printf("qoder quota %s: %v", label, err)
		return
	}
	s.cfg.Pool.ReenableIfCredits(uid, q.Remain, q.Size)
	s.cfg.Pool.SetCreditsSnapshot(uid, pool.CreditsSnapshot{
		Remain: q.Remain,
		Total:  q.Size,
	})
	if q.PlanTier != "" {
		log.Printf("qoder quota %s: %d/%d（套餐 %s）", label, q.Remain, q.Size, q.PlanTier)
	}
}

// qoderRealmOfAuth auth realm（"qoder"/"qoder_global"）→ qoder 包区域名。
func qoderRealmOfAuth(a *auth.Auth) string {
	if a.Realm() == "qoder_global" {
		return "global"
	}
	return "cn"
}
