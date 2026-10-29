package panel

import (
	"testing"
	"time"

	traepkg "github.com/linguo2625469/workbuddy2api-panel/internal/upstream/trae"
)

// TestFindTraeSessionByTrace 校验回调反查 pending 的规则：
// 命中 loginTraceID / machine_id 优先；都没有但只有一个待处理会话时用它。
func TestFindTraeSessionByTrace(t *testing.T) {
	p := &Panel{traeLogins: map[string]*traeLoginSession{}}
	machine := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	device := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	sess := &traeLoginSession{
		machineID: machine,
		deviceID:  device,
		traceID:   traepkg.MachineTraceID(machine, device),
		created:   time.Now(),
	}
	p.traeLogins["pending-1"] = sess

	// 1) 命中 loginTraceID
	got := p.findTraeSessionByTrace(&traepkg.CallbackInfo{LoginTraceID: sess.traceID})
	if got != sess {
		t.Errorf("按 loginTraceID 未命中")
	}
	// 2) 命中 machine_id
	got = p.findTraeSessionByTrace(&traepkg.CallbackInfo{MachineID: machine})
	if got != sess {
		t.Errorf("按 machine_id 未命中")
	}
	// 3) 都没带 → 单会话语义，仍取到该会话
	got = p.findTraeSessionByTrace(&traepkg.CallbackInfo{})
	if got != sess {
		t.Errorf("单待处理会话时应回退到它")
	}
	// 4) 带了不匹配的 trace 且只有一个会话 → 仍回退（回调可能不带关联字段）
	got = p.findTraeSessionByTrace(&traepkg.CallbackInfo{LoginTraceID: "ffffffffffffffff"})
	if got != sess {
		t.Errorf("单会话时应回退，而不是判失败")
	}
}

// TestFindTraeSessionMultiplePending 校验多会话且无关联字段时**不**乱认。
func TestFindTraeSessionMultiplePending(t *testing.T) {
	p := &Panel{traeLogins: map[string]*traeLoginSession{}}
	now := time.Now()
	s1 := &traeLoginSession{machineID: "m1", deviceID: "d1", traceID: "trace-1", created: now}
	s2 := &traeLoginSession{machineID: "m2", deviceID: "d2", traceID: "trace-2", created: now}
	p.traeLogins["a"] = s1
	p.traeLogins["b"] = s2

	if got := p.findTraeSessionByTrace(&traepkg.CallbackInfo{}); got != nil {
		t.Errorf("多会话且无关联字段时不应乱认，得到 %+v", got)
	}
	if got := p.findTraeSessionByTrace(&traepkg.CallbackInfo{LoginTraceID: "trace-2"}); got != s2 {
		t.Errorf("应按 trace 精确命中 s2")
	}
}

// TestFindTraeSessionExpired 校验过期会话不参与匹配。
func TestFindTraeSessionExpired(t *testing.T) {
	p := &Panel{traeLogins: map[string]*traeLoginSession{}}
	p.traeLogins["old"] = &traeLoginSession{
		machineID: "m", deviceID: "d", traceID: "t",
		created: time.Now().Add(-2 * loginTTL),
	}
	if got := p.findTraeSessionByTrace(&traepkg.CallbackInfo{LoginTraceID: "t"}); got != nil {
		t.Errorf("过期会话不应命中")
	}
}

// TestTraeSessionTTL 校验 poll 取会话时过期即清。
func TestTraeSessionTTL(t *testing.T) {
	p := &Panel{traeLogins: map[string]*traeLoginSession{}}
	p.traeLogins["x"] = &traeLoginSession{created: time.Now()}
	if p.traeSession("x") == nil {
		t.Fatal("未过期会话应可取")
	}
	p.traeLogins["y"] = &traeLoginSession{created: time.Now().Add(-2 * loginTTL)}
	if p.traeSession("y") != nil {
		t.Errorf("过期会话不应可取")
	}
	if _, exists := p.traeLogins["y"]; exists {
		t.Errorf("过期会话应被清除")
	}
}
