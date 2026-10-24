// cross_domain_fallback_config_test.go pool.cross_domain_fallback 配置测试：
// 默认 true（裸名跨域回退，配合 preferred_realm=global 国际版优先）/ 文件覆盖为 false。
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCrossDomainFallbackDefault 键缺席 → 默认 true（国际版优先 + 国内版兜底）。
func TestCrossDomainFallbackDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !c.Pool.CrossDomainFallback {
		t.Error("cross_domain_fallback 缺省必须为 true")
	}
}

// TestCrossDomainFallbackParsedFromFile 显式配置覆盖默认。
func TestCrossDomainFallbackParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"pool":{"cross_domain_fallback":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.CrossDomainFallback {
		t.Error("cross_domain_fallback=false 未生效")
	}
}

// TestPreferredRealmDefault 键缺席 → 默认 global（账号优先使用国际版）。
func TestPreferredRealmDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Pool.PreferredRealm != "global" {
		t.Errorf("preferred_realm 缺省必须为 global，got %q", c.Pool.PreferredRealm)
	}
}

// TestPreferredRealmExplicitCN 显式 "cn" 恢复国内版优先；非法值启动报错。
func TestPreferredRealmExplicitCN(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"pool":{"preferred_realm":"cn"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.PreferredRealm != "cn" {
		t.Errorf("preferred_realm=cn 未生效，got %q", c.Pool.PreferredRealm)
	}
	if err := os.WriteFile(fp, []byte(`{"pool":{"preferred_realm":"moon"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(fp); err == nil {
		t.Error("非法 preferred_realm 应启动报错")
	}
}
