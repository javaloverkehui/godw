package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/quality"
)

// QualityRules 返回某模型当前生效的规则集。
//
// 同时满足 node.Resolver：管道执行质量节点时按模型名反查规则，
// 这样规则改动不必重发管道。
func (s *Service) QualityRules(ctx context.Context, workspaceID, modelName string) ([]model.QualityRule, error) {
	if strings.TrimSpace(modelName) == "" {
		return nil, errs.New(errs.CodeInvalidArgument, "model 不能为空")
	}
	return s.Repo.ListQualityRules(ctx, workspaceID, modelName)
}

// SaveQualityRules 整套替换一个模型的规则集。
//
// 语义是「这次提交的就是全部」：页面上一行行编辑，保存时整表提交，
// 未出现的规则视为删除。增量接口会让「删了一条」需要额外的 DELETE 调用，
// 前端漏调就出现幽灵规则。
func (s *Service) SaveQualityRules(ctx context.Context, workspaceID, actor, modelName string, rules []model.QualityRule) ([]model.QualityRule, error) {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return nil, errs.New(errs.CodeInvalidArgument, "model 不能为空")
	}
	if err := s.Repo.AssertModelExists(ctx, workspaceID, modelName); err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(rules))
	clean := make([]model.QualityRule, 0, len(rules))
	for i, q := range rules {
		q.Name = strings.TrimSpace(q.Name)
		q.ModelName = modelName
		q.WorkspaceID = workspaceID
		if seen[q.Name] {
			return nil, errs.New(errs.CodeInvalidArgument,
				fmt.Sprintf("rules[%d] 与前面的规则重名: %s", i, q.Name))
		}
		seen[q.Name] = true
		if q.Severity == "" {
			q.Severity = "error"
		}
		// 空配置按空对象存：nil 与 {} 在 JSONB 里是两种值，
		// 前者读回来是 null，后续遍历 config 的地方要多一处判空。
		if q.Config == nil {
			q.Config = map[string]any{}
		}
		if err := quality.ValidateDef(q); err != nil {
			return nil, err
		}
		clean = append(clean, q)
	}
	// 稳定排序：规则集是给人看的，保存顺序不应取决于前端拖动。
	sort.SliceStable(clean, func(i, j int) bool { return clean[i].Name < clean[j].Name })

	if err := s.Repo.ReplaceQualityRules(ctx, workspaceID, modelName, clean); err != nil {
		return nil, err
	}
	return s.Repo.ListQualityRules(ctx, workspaceID, modelName)
}
