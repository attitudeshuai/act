package runner

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/model"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const dispatchInputsWorkflowYml = `
name: dispatched
on:
  workflow_dispatch:
    inputs:
      name:
        required: true
        type: string
      mode:
        required: true
        type: choice
        options:
          - a
          - b
      retries:
        required: false
        type: number
        default: "1"
      enabled:
        required: false
        type: boolean
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`

const callInputsWorkflowYml = `
name: inner
on:
  workflow_call:
    inputs:
      required_string:
        required: true
        type: string
      required_bool:
        required: true
        type: boolean
      required_number:
        required: true
        type: number
      optional_number:
        required: false
        type: number
        default: 1
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`

func validationConfig(t *testing.T, eventName, policy string) *Config {
	return &Config{
		Workdir:       t.TempDir(),
		EventName:     eventName,
		Env:           map[string]string{},
		UnknownInputs: policy,
	}
}

func validationWorkflow(t *testing.T, file, yml string) *model.Workflow {
	t.Helper()
	workflow, err := model.ReadWorkflow(strings.NewReader(yml), false)
	require.NoError(t, err)
	workflow.File = file
	return workflow
}

func singleJobPlan(workflow *model.Workflow, jobID string) *model.Plan {
	return &model.Plan{
		Stages: []*model.Stage{
			{Runs: []*model.Run{{Workflow: workflow, JobID: jobID}}},
		},
	}
}

func loggingContext() (context.Context, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	logger := log.New()
	logger.SetOutput(buf)
	logger.SetLevel(log.DebugLevel)
	return common.WithLogger(context.Background(), logger), buf
}

func TestValidatePlanInputsDispatchSuccess(t *testing.T) {
	ctx, _ := loggingContext()
	cfg := validationConfig(t, model.WorkflowDispatchTrigger, UnknownInputsPolicyWarn)
	runner := &runnerImpl{
		config:    cfg,
		eventJSON: `{"inputs":{"name":"act","mode":"a","enabled":"true"}}`,
	}

	plan := singleJobPlan(validationWorkflow(t, "dispatch.yml", dispatchInputsWorkflowYml), "test")
	assert.NoError(t, runner.validatePlanInputs(ctx, plan))
}

func TestValidatePlanInputsDispatchReportsAllErrors(t *testing.T) {
	ctx, _ := loggingContext()
	cfg := validationConfig(t, model.WorkflowDispatchTrigger, UnknownInputsPolicyWarn)
	runner := &runnerImpl{
		config:    cfg,
		eventJSON: `{"inputs":{"mode":"c","retries":"x","enabled":"yes"}}`,
	}

	plan := singleJobPlan(validationWorkflow(t, "dispatch.yml", dispatchInputsWorkflowYml), "test")
	err := runner.validatePlanInputs(ctx, plan)
	require.Error(t, err)

	message := err.Error()
	assert.Contains(t, message, `input "name"`)
	assert.Contains(t, message, "required input was not provided")
	assert.Contains(t, message, `input "mode"`)
	assert.Contains(t, message, `"c"`)
	assert.Contains(t, message, `"a"`)
	assert.Contains(t, message, `"b"`)
	assert.Contains(t, message, `input "retries"`)
	assert.Contains(t, message, `"x"`)
	assert.Contains(t, message, `input "enabled"`)
	assert.Contains(t, message, `"yes"`)
	assert.Contains(t, message, "dispatch.yml#on.workflow_dispatch.inputs.name")
}

func TestValidatePlanInputsUnknownInputsWarnByDefault(t *testing.T) {
	ctx, logBuf := loggingContext()
	cfg := validationConfig(t, model.WorkflowDispatchTrigger, UnknownInputsPolicyWarn)
	runner := &runnerImpl{
		config:    cfg,
		eventJSON: `{"inputs":{"name":"act","mode":"a","bogus":"ignored"}}`,
	}

	plan := singleJobPlan(validationWorkflow(t, "dispatch.yml", dispatchInputsWorkflowYml), "test")
	assert.NoError(t, runner.validatePlanInputs(ctx, plan))
	assert.Contains(t, logBuf.String(), "bogus")
	assert.Contains(t, logBuf.String(), "is not declared")
	assert.Contains(t, logBuf.String(), "will be ignored")
}

func TestValidatePlanInputsUnknownInputsErrorPolicy(t *testing.T) {
	ctx, _ := loggingContext()
	cfg := validationConfig(t, model.WorkflowDispatchTrigger, UnknownInputsPolicyError)
	runner := &runnerImpl{
		config:    cfg,
		eventJSON: `{"inputs":{"name":"act","mode":"a","bogus":"rejected"}}`,
	}

	plan := singleJobPlan(validationWorkflow(t, "dispatch.yml", dispatchInputsWorkflowYml), "test")
	err := runner.validatePlanInputs(ctx, plan)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `input "bogus"`)
	assert.Contains(t, err.Error(), "is not declared by the workflow")
}

func TestValidatePlanInputsWithoutDeclarationsIsUnaffected(t *testing.T) {
	ctx, logBuf := loggingContext()
	cfg := validationConfig(t, model.WorkflowDispatchTrigger, UnknownInputsPolicyError)
	runner := &runnerImpl{
		config:    cfg,
		eventJSON: `{"inputs":{"anything":"value"}}`,
	}

	workflow := validationWorkflow(t, "plain.yml", "on: workflow_dispatch\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n")
	plan := singleJobPlan(workflow, "test")
	assert.NoError(t, runner.validatePlanInputs(ctx, plan))
	assert.NotContains(t, logBuf.String(), "not declared")
}

func TestValidatePlanInputsNonDispatchEventIsUnaffected(t *testing.T) {
	ctx, _ := loggingContext()
	cfg := validationConfig(t, "push", UnknownInputsPolicyError)
	runner := &runnerImpl{
		config:    cfg,
		eventJSON: `{"inputs":{"anything":"value"}}`,
	}

	workflow := validationWorkflow(t, "push.yml", "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n")
	plan := singleJobPlan(workflow, "test")
	assert.NoError(t, runner.validatePlanInputs(ctx, plan))
}

func TestValidatePlanInputsDirectWorkflowCall(t *testing.T) {
	ctx, _ := loggingContext()

	workflow := validationWorkflow(t, "call.yml", callInputsWorkflowYml)
	plan := singleJobPlan(workflow, "test")

	// all required inputs present, optional_number falls back to its default
	cfg := validationConfig(t, model.WorkflowCallTrigger, UnknownInputsPolicyWarn)
	runner := &runnerImpl{
		config:    cfg,
		eventJSON: `{"inputs":{"required_string":"hi","required_bool":"true","required_number":"2"}}`,
	}
	assert.NoError(t, runner.validatePlanInputs(ctx, plan))

	// missing required inputs fail before any job starts
	runner = &runnerImpl{
		config:    cfg,
		eventJSON: `{"inputs":{"required_string":"hi"}}`,
	}
	err := runner.validatePlanInputs(ctx, plan)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `input "required_bool"`)
	assert.Contains(t, err.Error(), `input "required_number"`)
	assert.Contains(t, err.Error(), "call.yml#on.workflow_call.inputs.required_bool")
}

const midCallerWorkflowYml = `
name: mid
on: push
jobs:
  call-inner:
    uses: ./inner.yml
    with:
      required_string: hello
      required_bool: ${{ true }}
      not_declared: oops
`

const outerCallerWorkflowYml = `
name: outer
on: push
jobs:
  call-mid:
    uses: ./mid.yml
`

func newValidationRunContext(t *testing.T, cfg *Config, workflow *model.Workflow, jobID string) *RunContext {
	t.Helper()
	runner := &runnerImpl{config: cfg}
	run := &model.Run{Workflow: workflow, JobID: jobID}
	return runner.newRunContext(context.Background(), run, nil)
}

func TestValidatePlanInputsReusableWorkflowCall(t *testing.T) {
	ctx, logBuf := loggingContext()
	cfg := validationConfig(t, "push", UnknownInputsPolicyWarn)

	inner := validationWorkflow(t, "inner.yml", callInputsWorkflowYml)
	mid := validationWorkflow(t, "mid.yml", midCallerWorkflowYml)

	midRC := newValidationRunContext(t, cfg, mid, "call-inner")

	calleeRunner := &runnerImpl{
		config: cfg,
		caller: &caller{runContext: midRC},
	}
	plan := singleJobPlan(inner, "test")

	err := calleeRunner.validatePlanInputs(ctx, plan)
	require.Error(t, err)
	message := err.Error()

	// required_number is missing in the caller's `with` block
	assert.Contains(t, message, `input "required_number"`)
	assert.Contains(t, message, "required input was not provided")
	assert.Contains(t, message, "inner.yml#on.workflow_call.inputs.required_number")

	// the error carries the call chain
	assert.Contains(t, message, "mid (mid.yml)#call-inner -> inner (inner.yml)")

	// undeclared `with` inputs are warned and ignored by default
	assert.Contains(t, logBuf.String(), "not_declared")
	assert.Contains(t, logBuf.String(), "is not declared")
	assert.Contains(t, logBuf.String(), "will be ignored")

	// expression values are evaluated with the calling context: ${{ true }} is a valid bool
	assert.NotContains(t, message, `input "required_bool"`)
}

func TestValidatePlanInputsReusableWorkflowCallSuccess(t *testing.T) {
	ctx, _ := loggingContext()
	cfg := validationConfig(t, "push", UnknownInputsPolicyWarn)

	inner := validationWorkflow(t, "inner.yml", callInputsWorkflowYml)
	mid := validationWorkflow(t, "mid.yml", strings.Replace(midCallerWorkflowYml,
		"      not_declared: oops\n",
		"      required_number: 1\n", 1))

	midRC := newValidationRunContext(t, cfg, mid, "call-inner")
	calleeRunner := &runnerImpl{
		config: cfg,
		caller: &caller{runContext: midRC},
	}
	plan := singleJobPlan(inner, "test")

	assert.NoError(t, calleeRunner.validatePlanInputs(ctx, plan))
}

func TestValidatePlanInputsReusableWorkflowCallStrictUnknown(t *testing.T) {
	ctx, _ := loggingContext()
	cfg := validationConfig(t, "push", UnknownInputsPolicyError)

	inner := validationWorkflow(t, "inner.yml", callInputsWorkflowYml)
	mid := validationWorkflow(t, "mid.yml", strings.Replace(midCallerWorkflowYml,
		"      not_declared: oops\n",
		"      required_number: 1\n      not_declared: oops\n", 1))

	midRC := newValidationRunContext(t, cfg, mid, "call-inner")
	calleeRunner := &runnerImpl{
		config: cfg,
		caller: &caller{runContext: midRC},
	}
	plan := singleJobPlan(inner, "test")

	err := calleeRunner.validatePlanInputs(ctx, plan)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `input "not_declared"`)
	assert.Contains(t, err.Error(), "is not declared by the workflow")
}

func TestValidatePlanInputsNestedCallChain(t *testing.T) {
	ctx, _ := loggingContext()
	cfg := validationConfig(t, "push", UnknownInputsPolicyWarn)

	inner := validationWorkflow(t, "inner.yml", callInputsWorkflowYml)
	mid := validationWorkflow(t, "mid.yml", midCallerWorkflowYml)
	outer := validationWorkflow(t, "outer.yml", outerCallerWorkflowYml)

	midRC := newValidationRunContext(t, cfg, mid, "call-inner")
	outerRC := newValidationRunContext(t, cfg, outer, "call-mid")
	midRC.caller = &caller{runContext: outerRC}

	calleeRunner := &runnerImpl{
		config: cfg,
		caller: &caller{runContext: midRC},
	}
	plan := singleJobPlan(inner, "test")

	err := calleeRunner.validatePlanInputs(ctx, plan)
	require.Error(t, err)
	assert.Contains(t, err.Error(),
		"outer (outer.yml)#call-mid -> mid (mid.yml)#call-inner -> inner (inner.yml)")
}
