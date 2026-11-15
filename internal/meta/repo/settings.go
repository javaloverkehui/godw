package repo

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/godw/godw/internal/pkg/errs"
	"github.com/jackc/pgx/v5"
)

// 工作区级配置：目前只存建模规范（settings.modelingRules）。
// 用 JSONB 而不是给每层建表：层固定 5 个，但字段会继续长，拆表只会让读取多几次 JOIN。

// WorkspaceSettings 返回工作区的原始配置对象。
func (r *Repo) WorkspaceSettings(ctx context.Context, workspaceID string) (json.RawMessage, error) {
	var raw []byte
	err := r.DB.Pool.QueryRow(ctx, `SELECT settings FROM workspace WHERE id=$1`, workspaceID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errs.New(errs.CodeNotFound, "工作区不存在: "+workspaceID)
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// PatchWorkspaceSetting 写一个顶层键；value 为 nil 时删除该键（回到默认值）。
func (r *Repo) PatchWorkspaceSetting(ctx context.Context, workspaceID, key string, value any) error {
	if value == nil {
		_, err := r.DB.Pool.Exec(ctx,
			`UPDATE workspace SET settings = settings - $2::text WHERE id=$1`, workspaceID, key)
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = r.DB.Pool.Exec(ctx,
		`UPDATE workspace SET settings = jsonb_set(settings, ARRAY[$2::text], $3::jsonb, true) WHERE id=$1`,
		workspaceID, key, raw)
	return err
}
