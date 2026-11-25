package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/videotask"
	"github.com/stretchr/testify/require"
)

type videoTaskTestExecutor struct {
	videoTaskExecutor
	contentID string
}

func (e *videoTaskTestExecutor) Submit(_ context.Context, req VideoTaskRequest) (videotask.Task, error) {
	return videotask.Task{Model: req.Model}, nil
}

func (e *videoTaskTestExecutor) OpenContent(_ context.Context, task videotask.Task, _ string) (*http.Response, error) {
	e.contentID = task.ID
	return &http.Response{StatusCode: http.StatusOK}, nil
}

func TestVideoTaskDispatchUsesRegisteredAndPersistedProviders(t *testing.T) {
	ctx := context.Background()
	original, current := &videoTaskTestExecutor{}, &videoTaskTestExecutor{}
	s := &VideoTaskService{executors: map[string]videoTaskExecutor{"original": original, "current": current}}
	require.True(t, s.SupportsProvider("current"))
	require.False(t, s.SupportsProvider("unknown"))
	task, err := s.Submit(ctx, "current", VideoTaskRequest{Model: "alias"})
	require.NoError(t, err)
	require.Equal(t, "alias", task.Model)
	_, err = s.OpenContent(ctx, videotask.Task{ID: "vidtask_old", Provider: "original"}, "")
	require.NoError(t, err)
	require.Equal(t, "vidtask_old", original.contentID)
	require.Empty(t, current.contentID)
	_, err = s.OpenContent(ctx, videotask.Task{Provider: "unknown"}, "")
	require.ErrorIs(t, err, ErrVideoTaskTemporarilyUnavailable)
}
