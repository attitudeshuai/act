package runner

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/container"
	"github.com/nektos/act/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func TestJobExecutor(t *testing.T) {
	tables := []TestJobFileInfo{
		{workdir, "uses-and-run-in-one-step", "push", "Invalid run/uses syntax for job:test step:Test", platforms, secrets},
		{workdir, "uses-github-empty", "push", "Expected format {org}/{repo}[/path]@ref", platforms, secrets},
		{workdir, "uses-github-noref", "push", "Expected format {org}/{repo}[/path]@ref", platforms, secrets},
		{workdir, "uses-github-root", "push", "", platforms, secrets},
		{workdir, "uses-github-path", "push", "", platforms, secrets},
		{workdir, "uses-docker-url", "push", "", platforms, secrets},
		{workdir, "uses-github-full-sha", "push", "", platforms, secrets},
		{workdir, "uses-github-short-sha", "push", "Unable to resolve action `actions/hello-world-docker-action@b136eb8`, the provided ref `b136eb8` is the shortened version of a commit SHA, which is not supported. Please use the full commit SHA `b136eb8894c5cb1dd5807da824be97ccdf9b5423` instead", platforms, secrets},
		{workdir, "job-nil-step", "push", "invalid Step 0: missing run or uses key", platforms, secrets},
	}
	// These tests are sufficient to only check syntax.
	ctx := common.WithDryrun(context.Background(), true)
	for _, table := range tables {
		t.Run(table.workflowPath, func(t *testing.T) {
			table.runTest(ctx, t, &Config{})
		})
	}
}

type jobInfoMock struct {
	mock.Mock
}

func (jim *jobInfoMock) matrix() map[string]interface{} {
	args := jim.Called()
	return args.Get(0).(map[string]interface{})
}

func (jim *jobInfoMock) steps() []*model.Step {
	args := jim.Called()

	return args.Get(0).([]*model.Step)
}

func (jim *jobInfoMock) startContainer() common.Executor {
	args := jim.Called()

	return args.Get(0).(func(context.Context) error)
}

func (jim *jobInfoMock) stopContainer() common.Executor {
	args := jim.Called()

	return args.Get(0).(func(context.Context) error)
}

func (jim *jobInfoMock) closeContainer() common.Executor {
	args := jim.Called()

	return args.Get(0).(func(context.Context) error)
}

func (jim *jobInfoMock) interpolateOutputs() common.Executor {
	args := jim.Called()

	return args.Get(0).(func(context.Context) error)
}

func (jim *jobInfoMock) result(result string) {
	jim.Called(result)
}

type jobContainerMock struct {
	container.Container
	container.LinuxContainerEnvironmentExtensions
}

func (jcm *jobContainerMock) ReplaceLogWriter(_, _ io.Writer) (io.Writer, io.Writer) {
	return nil, nil
}

type stepFactoryMock struct {
	mock.Mock
}

func (sfm *stepFactoryMock) newStep(model *model.Step, rc *RunContext) (step, error) {
	args := sfm.Called(model, rc)
	return args.Get(0).(step), args.Error(1)
}

func TestNewJobExecutor(t *testing.T) {
	table := []struct {
		name          string
		steps         []*model.Step
		preSteps      []bool
		postSteps     []bool
		executedSteps []string
		result        string
		hasError      bool
	}{
		{
			name:          "zeroSteps",
			steps:         []*model.Step{},
			preSteps:      []bool{},
			postSteps:     []bool{},
			executedSteps: []string{},
			result:        "success",
			hasError:      false,
		},
		{
			name: "stepWithoutPrePost",
			steps: []*model.Step{{
				ID: "1",
			}},
			preSteps:  []bool{false},
			postSteps: []bool{false},
			executedSteps: []string{
				"startContainer",
				"step1",
				"stopContainer",
				"interpolateOutputs",
				"closeContainer",
			},
			result:   "success",
			hasError: false,
		},
		{
			name: "stepWithFailure",
			steps: []*model.Step{{
				ID: "1",
			}},
			preSteps:  []bool{false},
			postSteps: []bool{false},
			executedSteps: []string{
				"startContainer",
				"step1",
				"interpolateOutputs",
				"closeContainer",
			},
			result:   "failure",
			hasError: true,
		},
		{
			name: "stepWithPre",
			steps: []*model.Step{{
				ID: "1",
			}},
			preSteps:  []bool{true},
			postSteps: []bool{false},
			executedSteps: []string{
				"startContainer",
				"pre1",
				"step1",
				"stopContainer",
				"interpolateOutputs",
				"closeContainer",
			},
			result:   "success",
			hasError: false,
		},
		{
			name: "stepWithPost",
			steps: []*model.Step{{
				ID: "1",
			}},
			preSteps:  []bool{false},
			postSteps: []bool{true},
			executedSteps: []string{
				"startContainer",
				"step1",
				"post1",
				"stopContainer",
				"interpolateOutputs",
				"closeContainer",
			},
			result:   "success",
			hasError: false,
		},
		{
			name: "stepWithPreAndPost",
			steps: []*model.Step{{
				ID: "1",
			}},
			preSteps:  []bool{true},
			postSteps: []bool{true},
			executedSteps: []string{
				"startContainer",
				"pre1",
				"step1",
				"post1",
				"stopContainer",
				"interpolateOutputs",
				"closeContainer",
			},
			result:   "success",
			hasError: false,
		},
		{
			name: "stepsWithPreAndPost",
			steps: []*model.Step{{
				ID: "1",
			}, {
				ID: "2",
			}, {
				ID: "3",
			}},
			preSteps:  []bool{true, false, true},
			postSteps: []bool{false, true, true},
			executedSteps: []string{
				"startContainer",
				"pre1",
				"pre3",
				"step1",
				"step2",
				"step3",
				"post3",
				"post2",
				"stopContainer",
				"interpolateOutputs",
				"closeContainer",
			},
			result:   "success",
			hasError: false,
		},
	}

	contains := func(needle string, haystack []string) bool {
		for _, item := range haystack {
			if item == needle {
				return true
			}
		}
		return false
	}

	for _, tt := range table {
		t.Run(tt.name, func(t *testing.T) {
			fmt.Printf("::group::%s\n", tt.name)

			ctx := common.WithJobErrorContainer(context.Background())
			jim := &jobInfoMock{}
			sfm := &stepFactoryMock{}
			rc := &RunContext{
				JobContainer: &jobContainerMock{},
				Run: &model.Run{
					JobID: "test",
					Workflow: &model.Workflow{
						Jobs: map[string]*model.Job{
							"test": {},
						},
					},
				},
				Config:           &Config{},
				nodeToolFullPath: "node",
			}
			rc.ExprEval = rc.NewExpressionEvaluator(ctx)
			executorOrder := make([]string, 0)

			jim.On("steps").Return(tt.steps)

			if len(tt.steps) > 0 {
				jim.On("startContainer").Return(func(_ context.Context) error {
					executorOrder = append(executorOrder, "startContainer")
					return nil
				})
			}

			for i, stepModel := range tt.steps {
				sm := &stepMock{}

				sfm.On("newStep", stepModel, rc).Return(sm, nil)

				sm.On("pre").Return(func(_ context.Context) error {
					if tt.preSteps[i] {
						executorOrder = append(executorOrder, "pre"+stepModel.ID)
					}
					return nil
				})

				sm.On("main").Return(func(_ context.Context) error {
					executorOrder = append(executorOrder, "step"+stepModel.ID)
					if tt.hasError {
						return fmt.Errorf("error")
					}
					return nil
				})

				sm.On("post").Return(func(_ context.Context) error {
					if tt.postSteps[i] {
						executorOrder = append(executorOrder, "post"+stepModel.ID)
					}
					return nil
				})

				defer sm.AssertExpectations(t)
			}

			if len(tt.steps) > 0 {
				jim.On("matrix").Return(map[string]interface{}{})

				jim.On("interpolateOutputs").Return(func(_ context.Context) error {
					executorOrder = append(executorOrder, "interpolateOutputs")
					return nil
				})

				if contains("stopContainer", tt.executedSteps) {
					jim.On("stopContainer").Return(func(_ context.Context) error {
						executorOrder = append(executorOrder, "stopContainer")
						return nil
					})
				}

				jim.On("result", tt.result)

				jim.On("closeContainer").Return(func(_ context.Context) error {
					executorOrder = append(executorOrder, "closeContainer")
					return nil
				})
			}

			executor := newJobExecutor(jim, sfm, rc)
			err := executor(ctx)
			assert.Nil(t, err)
			assert.Equal(t, tt.executedSteps, executorOrder)

			jim.AssertExpectations(t)
			sfm.AssertExpectations(t)

			fmt.Println("::endgroup::")
		})
	}
}

func TestEvaluateJobTimeoutMinutes(t *testing.T) {
	newRC := func(raw string, matrix map[string]interface{}) *RunContext {
		rc := &RunContext{
			JobName: "test-job",
			Matrix:  matrix,
			Run: &model.Run{
				JobID: "test",
				Workflow: &model.Workflow{
					Jobs: map[string]*model.Job{
						"test": {TimeoutMinutes: raw},
					},
				},
			},
			Config:      &Config{},
			StepResults: map[string]*model.StepResult{},
		}
		rc.ExprEval = rc.NewExpressionEvaluator(context.Background())
		return rc
	}

	table := []struct {
		name        string
		raw         string
		matrix      map[string]interface{}
		wantMinutes int64
		wantSet     bool
		errContains string
	}{
		{name: "unset", raw: "", wantSet: false},
		{name: "plain", raw: "10", wantMinutes: 10, wantSet: true},
		{name: "whitespace", raw: "  3 ", wantMinutes: 3, wantSet: true},
		{name: "matrixExpression", raw: "${{ matrix.timeout }}", matrix: map[string]interface{}{"timeout": 7}, wantMinutes: 7, wantSet: true},
		{name: "expressionInteger", raw: "${{ 5 }}", wantMinutes: 5, wantSet: true},
		{name: "emptyExpression", raw: "${{ null }}", wantSet: false},
		{name: "emptyStringExpression", raw: "${{ '' }}", wantSet: false},
		{name: "zero", raw: "0", errContains: "greater than zero"},
		{name: "negative", raw: "-2", errContains: "greater than zero"},
		{name: "fraction", raw: "1.5", errContains: "positive integer"},
		{name: "notANumber", raw: "abc", errContains: "positive integer"},
		{name: "expressionFailure", raw: "${{ , }}", errContains: "unable to evaluate"},
	}

	for _, tt := range table {
		t.Run(tt.name, func(t *testing.T) {
			rc := newRC(tt.raw, tt.matrix)
			minutes, hasTimeout, err := evaluateJobTimeoutMinutes(context.Background(), rc)
			if tt.errContains != "" {
				assert.Error(t, err)
				assert.False(t, hasTimeout)
				assert.Contains(t, err.Error(), tt.errContains)
				assert.Contains(t, err.Error(), "test-job")
				assert.Contains(t, err.Error(), tt.raw)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.wantSet, hasTimeout)
			assert.Equal(t, tt.wantMinutes, minutes)
		})
	}
}

func newTimeoutJobExecutorMocks(t *testing.T, timeoutMinutes string, stepMains ...func(context.Context) error) (*jobInfoMock, *stepFactoryMock, *RunContext, *[]string) {
	t.Helper()

	ctx := context.Background()
	stepModels := make([]*model.Step, 0, len(stepMains))
	for i := range stepMains {
		stepModels = append(stepModels, &model.Step{ID: fmt.Sprintf("%d", i), Name: fmt.Sprintf("step %d", i)})
	}

	jim := &jobInfoMock{}
	sfm := &stepFactoryMock{}
	rc := &RunContext{
		JobName: "timed-out-job",
		Run: &model.Run{
			JobID: "test",
			Workflow: &model.Workflow{
				Jobs: map[string]*model.Job{
					"test": {TimeoutMinutes: timeoutMinutes, Steps: stepModels},
				},
			},
		},
		Config:           &Config{},
		StepResults:      map[string]*model.StepResult{},
		JobContainer:     &jobContainerMock{},
		nodeToolFullPath: "node",
	}
	rc.ExprEval = rc.NewExpressionEvaluator(ctx)

	executorOrder := make([]string, 0)

	jim.On("steps").Return(stepModels)
	jim.On("matrix").Return(map[string]interface{}{})

	for i, stepModel := range stepModels {
		sm := &stepMock{}
		sfm.On("newStep", stepModel, rc).Return(sm, nil)
		main := stepMains[i]
		idx := i
		sm.On("pre").Return(func(_ context.Context) error { return nil })
		sm.On("main").Return(func(ctx context.Context) error {
			executorOrder = append(executorOrder, fmt.Sprintf("step%d", idx))
			return main(ctx)
		})
		sm.On("post").Return(func(_ context.Context) error { return nil })
	}

	return jim, sfm, rc, &executorOrder
}

func mockCleanupExecutors(jim *jobInfoMock, order *[]string, stopContainer bool) {
	if stopContainer {
		jim.On("stopContainer").Return(func(_ context.Context) error {
			*order = append(*order, "stopContainer")
			return nil
		})
	}
	jim.On("interpolateOutputs").Return(func(_ context.Context) error {
		*order = append(*order, "interpolateOutputs")
		return nil
	})
	jim.On("closeContainer").Return(func(_ context.Context) error {
		*order = append(*order, "closeContainer")
		return nil
	})
}

func TestNewJobExecutorTimesOutRunningStep(t *testing.T) {
	oldJobTimeoutDuration := jobTimeoutDuration
	jobTimeoutDuration = func(_ int64) time.Duration { return 100 * time.Millisecond }
	defer func() { jobTimeoutDuration = oldJobTimeoutDuration }()

	step2Ran := false
	jim, sfm, rc, order := newTimeoutJobExecutorMocks(t, "1",
		func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
		func(_ context.Context) error {
			step2Ran = true
			return nil
		},
	)

	jim.On("startContainer").Return(func(_ context.Context) error {
		*order = append(*order, "startContainer")
		return nil
	})
	mockCleanupExecutors(jim, order, false)
	jim.On("result", jobResultTimedOut)

	// simulate runStepExecutor having marked the running step cancelled
	rc.StepResults["0"] = &model.StepResult{
		Outcome:    model.StepStatusCancelled,
		Conclusion: model.StepStatusCancelled,
		Outputs:    map[string]string{},
	}

	ctx := common.WithJobErrorContainer(context.Background())
	err := newJobExecutor(jim, sfm, rc)(ctx)

	// the job itself completes with a timed_out conclusion, just like a
	// failure it does not abort the plan; handleFailure turns the conclusion
	// into a failed run after sibling and downstream jobs are done
	assert.NoError(t, err)
	assert.True(t, rc.TimedOut)
	jim.AssertCalled(t, "result", jobResultTimedOut)
	assert.False(t, step2Ran, "steps after the timeout must not run")

	// running step cancelled, not started steps marked skipped
	assert.Equal(t, model.StepStatusCancelled, rc.StepResults["0"].Conclusion)
	assert.Equal(t, model.StepStatusSkipped, rc.StepResults["1"].Conclusion)

	assert.Contains(t, *order, "startContainer")
	assert.Contains(t, *order, "step0")
	assert.NotContains(t, *order, "step1")
	assert.Contains(t, *order, "interpolateOutputs")
	assert.Contains(t, *order, "closeContainer")
	jim.AssertExpectations(t)
}

func TestNewJobExecutorCompletesWithinTimeout(t *testing.T) {
	oldJobTimeoutDuration := jobTimeoutDuration
	jobTimeoutDuration = func(_ int64) time.Duration { return 10 * time.Second }
	defer func() { jobTimeoutDuration = oldJobTimeoutDuration }()

	jim, sfm, rc, order := newTimeoutJobExecutorMocks(t, "1",
		func(_ context.Context) error { return nil },
		func(_ context.Context) error { return nil },
	)

	jim.On("startContainer").Return(func(_ context.Context) error {
		*order = append(*order, "startContainer")
		return nil
	})
	mockCleanupExecutors(jim, order, true)
	jim.On("result", "success")

	ctx := common.WithJobErrorContainer(context.Background())
	err := newJobExecutor(jim, sfm, rc)(ctx)

	assert.NoError(t, err)
	assert.False(t, rc.TimedOut)
	assert.Contains(t, *order, "step0")
	assert.Contains(t, *order, "step1")
	jim.AssertCalled(t, "result", "success")
	jim.AssertExpectations(t)
}

func TestNewJobExecutorInvalidTimeoutFailsBeforeJobStarts(t *testing.T) {
	for _, raw := range []string{"0", "-3", "1.5", "abc"} {
		t.Run(raw, func(t *testing.T) {
			jim, sfm, rc, order := newTimeoutJobExecutorMocks(t, raw,
				func(_ context.Context) error { return nil },
			)
			// these executors are constructed while building the job pipeline,
			// but an invalid timeout-minutes must abort before any of them runs
			jim.On("startContainer").Return(func(_ context.Context) error {
				*order = append(*order, "startContainer")
				return nil
			})
			mockCleanupExecutors(jim, order, true)

			ctx := common.WithJobErrorContainer(context.Background())
			err := newJobExecutor(jim, sfm, rc)(ctx)

			assert.Error(t, err)
			assert.Contains(t, err.Error(), "timed-out-job")
			assert.Contains(t, err.Error(), raw)
			assert.Empty(t, *order, "job must not start before the timeout declaration is validated")
			jim.AssertNotCalled(t, "result", mock.Anything)
		})
	}
}

func TestExecuteJobWithTimeoutExternalCancellationWins(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	rc := &RunContext{
		JobName: "cancelled-job",
		Run: &model.Run{
			JobID:    "test",
			Workflow: &model.Workflow{Jobs: map[string]*model.Job{"test": {}}},
		},
		Config:      &Config{},
		StepResults: map[string]*model.StepResult{},
	}

	ran := make(chan struct{})
	pipeline := func(ctx context.Context) error {
		<-ctx.Done()
		close(ran)
		return ctx.Err()
	}

	// an external signal cancels the parent context before the deadline fires
	cancelParent()
	err := executeJobWithTimeout(parent, rc, time.Hour, pipeline)

	<-ran
	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, rc.TimedOut)
}

func TestSetJobResultTimedOutAggregation(t *testing.T) {
	type instanceResult struct {
		success  bool
		timedOut bool
	}

	newSetup := func(matrix map[string]interface{}) (*jobInfoMock, *RunContext) {
		jim := &jobInfoMock{}
		rc := &RunContext{
			Run: &model.Run{
				JobID:    "test",
				Workflow: &model.Workflow{Jobs: map[string]*model.Job{"test": {}}},
			},
			Config: &Config{},
		}
		jim.On("matrix").Return(matrix)
		jim.On("result", mock.Anything).Return().Run(func(args mock.Arguments) {
			rc.Run.Job().Result = args.Get(0).(string)
		})
		return jim, rc
	}

	table := []struct {
		name    string
		matrix  map[string]interface{}
		results []instanceResult
		want    string
	}{
		{name: "nonMatrixSuccess", matrix: map[string]interface{}{}, results: []instanceResult{{true, false}}, want: "success"},
		{name: "nonMatrixFailure", matrix: map[string]interface{}{}, results: []instanceResult{{false, false}}, want: "failure"},
		{name: "nonMatrixTimeout", matrix: map[string]interface{}{}, results: []instanceResult{{false, true}}, want: jobResultTimedOut},
		{name: "matrixSuccessThenTimeout", matrix: map[string]interface{}{"i": 1}, results: []instanceResult{{true, false}, {false, true}}, want: jobResultTimedOut},
		{name: "matrixTimeoutThenSuccess", matrix: map[string]interface{}{"i": 1}, results: []instanceResult{{false, true}, {true, false}}, want: jobResultTimedOut},
		{name: "matrixFailureThenTimeout", matrix: map[string]interface{}{"i": 1}, results: []instanceResult{{false, false}, {false, true}}, want: "failure"},
		{name: "matrixTimeoutThenFailure", matrix: map[string]interface{}{"i": 1}, results: []instanceResult{{false, true}, {false, false}}, want: jobResultTimedOut},
	}

	for _, tt := range table {
		t.Run(tt.name, func(t *testing.T) {
			jim, rc := newSetup(tt.matrix)
			for _, r := range tt.results {
				setJobResult(context.Background(), jim, rc, r.success, r.timedOut)
			}
			assert.Equal(t, tt.want, rc.Run.Job().Result)
		})
	}
}
