package server

import "testing"

// TestStripModelPrefix 锁住「按域直通分支必须自己剥前缀」这条约定。
//
// resolveModelDetail 对未知前缀刻意不剥离（保留整串供中转站昵称二次匹配），
// 若 trae 分支忘了剥，上游会收到 "trae:glm-5.2" 这种整串而报模型不存在。
func TestStripModelPrefix(t *testing.T) {
	cases := []struct {
		model  string
		prefix string
		want   string
	}{
		{"trae:glm-5.2", "trae", "glm-5.2"},
		// 无前缀（裸名）原样返回
		{"glm-5.2", "trae", "glm-5.2"},
		// 别家前缀不误剥
		{"qoder:Qwen3.8-Flash", "trae", "qoder:Qwen3.8-Flash"},
		// 模型名里带冒号但前缀不匹配时不切
		{"sn:weird:name", "relay", "sn:weird:name"},
	}
	for _, c := range cases {
		if got := stripModelPrefix(c.model, c.prefix); got != c.want {
			t.Errorf("stripModelPrefix(%q, %q) = %q，期望 %q", c.model, c.prefix, got, c.want)
		}
	}
}

// TestResolveModelDetailKeepsUnknownPrefix 确认底层解析对未知前缀的行为没变
// （trae 分支依赖它把 realm 原样带回）。
func TestResolveModelDetailKeepsUnknownPrefix(t *testing.T) {
	realm, bare, explicit := resolveModelDetail("trae:glm-5.2")
	if realm != "trae" || bare != "trae:glm-5.2" || !explicit {
		t.Errorf("得到 realm=%q bare=%q explicit=%v", realm, bare, explicit)
	}
}
