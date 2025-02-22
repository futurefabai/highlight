package store

import (
	"context"
	"fmt"
	"github.com/highlight-run/highlight/backend/model"
	"github.com/highlight-run/highlight/backend/redis"
	"time"
)

func (store *Store) GetAllWorkspaceSettings(ctx context.Context, workspaceID int) (*model.AllWorkspaceSettings, error) {
	return redis.CachedEval(ctx, store.redis, fmt.Sprintf("all-workspace-settings-workspace-%d", workspaceID), 250*time.Millisecond, 5*time.Second, func() (*model.AllWorkspaceSettings, error) {
		var workspaceSettings model.AllWorkspaceSettings
		if err := store.db.Where(model.AllWorkspaceSettings{WorkspaceID: workspaceID}).FirstOrCreate(&workspaceSettings).Error; err != nil {
			return nil, err
		}
		return &workspaceSettings, nil
	})
}

func (store *Store) GetAllWorkspaceSettingsByProject(ctx context.Context, projectID int) (*model.AllWorkspaceSettings, error) {
	return redis.CachedEval(ctx, store.redis, fmt.Sprintf("all-workspace-settings-project-%d", projectID), 250*time.Millisecond, 5*time.Second, func() (*model.AllWorkspaceSettings, error) {
		var workspaceSettings model.AllWorkspaceSettings
		if err := store.db.Model(&model.AllWorkspaceSettings{}).Joins("INNER JOIN projects ON projects.workspace_id = all_workspace_settings.workspace_id").Where("projects.id = ?", projectID).Take(&workspaceSettings).Error; err != nil {
			return nil, err
		}
		return &workspaceSettings, nil
	})
}
