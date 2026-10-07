package server

import "github.com/hey345437-boop/futuresearch-gateway/internal/fsapi"

// setupModel 写 dsh 配置需要的模型信息。
type setupModel struct {
	ID      string
	Name    string
	Efforts map[string]string
}

// staticModelsForSetup 预设档 + 模型家族（不含 143 个完整枚举 —— 那会让 dsh 的选择器爆掉）。
func staticModelsForSetup() []setupModel {
	out := []setupModel{}
	for _, m := range fsapi.StaticModels() {
		if m.ID == "" {
			continue
		}
		// 完整枚举名带档位后缀（`-max`/`-low`…）—— 跳过后半段，只留预设档 + 家族
		if fsapi.IsTieredSlug(m.ID) {
			continue
		}
		out = append(out, setupModel{ID: m.ID, Name: m.Name, Efforts: fsapi.EffortsFor(m.ID)})
	}
	return out
}
