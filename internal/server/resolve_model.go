package server

import "strings"

// resolveModelDetail 解析模型名协议（PLAN D6），并区分「realm 是否由调用方显式写出」：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为 "cn"/"global"/"relay" 时按枚举剥离；其余带 ":" 的前缀原样
// 保留（prefix=原串、bare=原串），由调用方（handler）按中转站昵称匹配「中转站名:model」；
// 无 ":" 视为裸名，realm=cn。bare 即出站/选号/账本使用的裸模型名。
//
// 第三个返回值 realmExplicit 供跨域回退（issue #135）判定：只有裸名（false）才允许
// 在首选域选不到号时改用另一域；显式 cn:/global:/relay: 前缀永不被回退改写。
func resolveModelDetail(model string) (realm, bare string, realmExplicit bool) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", model, false
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" && prefix != "relay" {
		// 未知前缀：不当作裸名回落 cn（那会把 "站名:model" 整串当模型名打上游），
		// 保留原串交由 handler 按中转站昵称二次匹配；匹配不到时报 model_unavailable。
		return prefix, model, true
	}
	return prefix, model[idx+1:], true
}

// resolveModel 是 resolveModelDetail 的两返回值形态（既有调用方语义零改动）。
func resolveModel(model string) (realm, bare string) {
	realm, bare, _ = resolveModelDetail(model)
	return realm, bare
}

// stripModelPrefix 剥掉指定前缀（如 "trae:"）返回裸模型名。
//
// resolveModelDetail 对未知前缀**刻意不剥离**（保留整串供中转站昵称二次匹配），
// 故按域直通的分支（trae）必须自己剥：上游只认裸模型名，
// 带前缀会被当成模型不存在（trae 报 4023）。
func stripModelPrefix(model, prefix string) string {
	head := prefix + ":"
	if strings.HasPrefix(model, head) {
		return model[len(head):]
	}
	return model
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }

// otherRealm 返回另一个选号域（"cn" ↔ "global"），供跨域回退使用。
func otherRealm(realm string) string {
	if realm == "global" {
		return "cn"
	}
	return "global"
}
