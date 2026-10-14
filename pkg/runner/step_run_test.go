package runner

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/container"
	"github.com/nektos/act/pkg/model"
)

func TestStepRun(t *testing.T) {
	cm := &containerMock{}
	fileEntry := &container.FileEntry{
		Name: "workflow/1.sh",
		Mode: 0o755,
		Body: "\ncmd\n",
	}

	sr := &stepRun{
		RunContext: &RunContext{
			StepResults: map[string]*model.StepResult{},
			ExprEval:    &expressionEvaluator{},
			Config:      &Config{},
			Run: &model.Run{
				JobID: "1",
				Workflow: &model.Workflow{
					Jobs: map[string]*model.Job{
						"1": {
							Defaults: model.Defaults{
								Run: model.RunDefaults{
									Shell: "bash",
								},
							},
						},
					},
				},
			},
			JobContainer: cm,
		},
		Step: &model.Step{
			ID:               "1",
			Run:              "cmd",
			WorkingDirectory: "workdir",
		},
	}

	cm.On("Copy", "/var/run/act", []*container.FileEntry{fileEntry}).Return(func(_ context.Context) error {
		return nil
	})
	cm.On("Exec", []string{"bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "/var/run/act/workflow/1.sh"}, mock.AnythingOfType("map[string]string"), "", "workdir").Return(func(_ context.Context) error {
		return nil
	})

	cm.On("Copy", "/var/run/act", mock.AnythingOfType("[]*container.FileEntry")).Return(func(_ context.Context) error {
		return nil
	})

	cm.On("UpdateFromEnv", "/var/run/act/workflow/envs.txt", mock.AnythingOfType("*map[string]string")).Return(func(_ context.Context) error {
		return nil
	})

	cm.On("UpdateFromEnv", "/var/run/act/workflow/statecmd.txt", mock.AnythingOfType("*map[string]string")).Return(func(_ context.Context) error {
		return nil
	})

	cm.On("UpdateFromEnv", "/var/run/act/workflow/outputcmd.txt", mock.AnythingOfType("*map[string]string")).Return(func(_ context.Context) error {
		return nil
	})

	ctx := context.Background()

	cm.On("GetContainerArchive", ctx, "/var/run/act/workflow/SUMMARY.md").Return(io.NopCloser(&bytes.Buffer{}), nil)
	cm.On("GetContainerArchive", ctx, "/var/run/act/workflow/pathcmd.txt").Return(io.NopCloser(&bytes.Buffer{}), nil)

	err := sr.main()(ctx)
	assert.Nil(t, err)

	cm.AssertExpectations(t)
}

func TestStepRunPrePost(t *testing.T) {
	ctx := context.Background()
	sr := &stepRun{}

	err := sr.pre()(ctx)
	assert.Nil(t, err)

	err = sr.post()(ctx)
	assert.Nil(t, err)
}

func TestStepRunJobTimeoutMarksStepCancelled(t *testing.T) {
	cm := &containerMock{}
	fileEntry := &container.FileEntry{
		Name: "workflow/1.sh",
		Mode: 0o755,
		Body: "\ncmd\n",
	}

	sr := &stepRun{
		RunContext: &RunContext{
			StepResults: map[string]*model.StepResult{},
			ExprEval:    &expressionEvaluator{},
			Config:      &Config{},
			Run: &model.Run{
				JobID: "1",
				Workflow: &model.Workflow{
					Jobs: map[string]*model.Job{
						"1": {
							Defaults: model.Defaults{
								Run: model.RunDefaults{
									Shell: "bash",
								},
							},
						},
					},
				},
			},
			JobContainer: cm,
		},
		Step: &model.Step{
			ID:               "1",
			Run:              "cmd",
			WorkingDirectory: "workdir",
		},
	}

	cm.On("Copy", "/var/run/act", []*container.FileEntry{fileEntry}).Return(func(_ context.Context) error {
		return nil
	})
	cm.On("Copy", "/var/run/act", mock.AnythingOfType("[]*container.FileEntry")).Return(func(_ context.Context) error {
		return nil
	})
	// the job-level timeout fires while the step is executing: the timeout
	// marker is signaled first and the job context cancelled right after, as
	// executeJobWithTimeout does it
	parent, cancelParent := context.WithCancel(context.Background())
	timeoutSignal, markTimedOut := context.WithCancel(context.Background())
	ctx := common.WithJobTimeoutContext(parent, timeoutSignal)

	cm.On("Exec", []string{"bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "/var/run/act/workflow/1.sh"}, mock.AnythingOfType("map[string]string"), "", "workdir").Return(func(ctx context.Context) error {
		markTimedOut()
		cancelParent()
		<-ctx.Done()
		return ctx.Err()
	})

	err := sr.main()(ctx)
	assert.ErrorIs(t, err, context.Canceled)

	result := sr.RunContext.StepResults["1"]
	if assert.NotNil(t, result) {
		assert.Equal(t, model.StepStatusCancelled, result.Outcome)
		assert.Equal(t, model.StepStatusCancelled, result.Conclusion)
	}
	assert.True(t, runningStepEquals(sr.RunContext, "1"))
	cm.AssertExpectations(t)
}

func runningStepEquals(rc *RunContext, id string) bool {
	pointer := rc.runningStep.Load()
	return pointer != nil && *pointer == id
}
