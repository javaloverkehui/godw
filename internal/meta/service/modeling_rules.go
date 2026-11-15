package service

import (
	"context"
	"encoding/json"

	"github.com/godw/godw/internal/modelc"
	"github.com/godw/godw/internal/pkg/errs"
)

// modelingRulesKey 是 workspace.settings 里存规范的键名。
const modelingRulesKey = "modelingRules"

// ModelingRules 返回工作区当前生效的建模规范。
//
// 优先级：工作区自定义 > 进程级 Rules（测试注入） > 内置默认。
// 第二个返回值表示是否来自工作区自定义，用于设置页区分「用默认」与「已覆盖」。
// 分层规范会随客户/团队变化（前缀叫 dwd_ 还是 dw_），必须按工作区读，
// 不能像以前那样在 New() 里一次性写死。
func (s *Service) ModelingRules(ctx context.Context, workspaceID string) (modelc.Rules, bool, error) {
	raw, err := s.Repo.WorkspaceSettings(ctx, workspaceID)
	if err != nil {
		return modelc.Rules{}, false, err
	}
	var holder struct {
		ModelingRules *modelc.Rules `json:"modelingRules"`
	}
	if len(raw) > 0 && string(raw) != "{}" {
		if err := json.Unmarshal(raw, &holder); err != nil {
			return modelc.Rules{}, false, errs.Wrap(errs.CodeInternal, err, "工作区建模规范解析失败")
		}
	}
	if holder.ModelingRules != nil && len(holder.ModelingRules.Layers) > 0 {
		return *holder.ModelingRules, true, nil
	}
	if len(s.Rules.Layers) > 0 {
		return s.Rules, false, nil
	}
	return modelc.DefaultRules(), false, nil
}

// SaveModelingRules 保存工作区规范。只允许在默认规范之上收紧（见 modelc.Normalize）。
func (s *Service) SaveModelingRules(ctx context.Context, workspaceID, actor string, rules modelc.Rules) (modelc.Rules, error) {
	clean, err := rules.Normalize()
	if err != nil {
		return modelc.Rules{}, err
	}
	if err := s.Repo.PatchWorkspaceSetting(ctx, workspaceID, modelingRulesKey, clean); err != nil {
		return modelc.Rules{}, err
	}
	return clean, nil
}

// ResetModelingRules 删除自定义规范，回到内置默认。
func (s *Service) ResetModelingRules(ctx context.Context, workspaceID, actor string) error {
	return s.Repo.PatchWorkspaceSetting(ctx, workspaceID, modelingRulesKey, nil)
}
