package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/model"
	"gopkg.in/yaml.v3"
)

// Policies that control what happens when an input is provided that the
// workflow does not declare.
const (
	// UnknownInputsPolicyWarn logs every undeclared input and continues.
	// This is the default.
	UnknownInputsPolicyWarn = "warn"
	// UnknownInputsPolicyError fails the run before any job starts when an
	// undeclared input is provided.
	UnknownInputsPolicyError = "error"
)

func (runner *runnerImpl) unknownInputsPolicy() string {
	if strings.EqualFold(runner.config.UnknownInputs, UnknownInputsPolicyError) {
		return UnknownInputsPolicyError
	}
	return UnknownInputsPolicyWarn
}

// validatePlanInputs runs one uniform input validation pass over every
// workflow in the plan. It executes after the workflows have been loaded and
// before the first job of the plan is started. Both manual workflow_dispatch
// runs and reusable workflow_call invocations (including nested ones) are
// covered.
func (runner *runnerImpl) validatePlanInputs(ctx context.Context, plan *model.Plan) error {
	type workflowRef struct {
		workflow *model.Workflow
		run      *model.Run
	}

	seen := make(map[*model.Workflow]bool)
	workflows := make([]workflowRef, 0)
	for _, stage := range plan.Stages {
		for _, run := range stage.Runs {
			if run.Workflow == nil || seen[run.Workflow] {
				continue
			}
			seen[run.Workflow] = true
			workflows = append(workflows, workflowRef{workflow: run.Workflow, run: run})
		}
	}
	sort.Slice(workflows, func(i, j int) bool {
		return workflows[i].workflow.File < workflows[j].workflow.File
	})

	logger := common.Logger(ctx)
	policy := runner.unknownInputsPolicy()

	failures := make([]string, 0)
	for _, ref := range workflows {
		report := runner.validateWorkflowInputs(ctx, ref.run)
		if report == nil {
			continue
		}

		for _, unknown := range report.Unknown {
			if policy == UnknownInputsPolicyError {
				report.Issues = append(report.Issues, model.InputIssue{
					Name:     unknown.Name,
					Kind:     model.InputIssueUnknown,
					Location: unknown.Location,
					Message:  "is not declared by the workflow and undeclared inputs are rejected (unknown-inputs policy: error)",
				})
			} else {
				logger.Warnf("%s", unknown.WarnMessage())
			}
		}

		if report.HasIssues() {
			failure := report.Error()
			if chain := runner.inputCallChain(ref.workflow); chain != "" {
				failure += "\n  call chain: " + chain
			}
			failures = append(failures, failure)
		}
	}

	if len(failures) > 0 {
		return fmt.Errorf("%s", strings.Join(failures, "\n"))
	}
	return nil
}

// validateWorkflowInputs validates the inputs of one workflow according to the
// position of this runner in a possible reusable workflow call chain.
func (runner *runnerImpl) validateWorkflowInputs(ctx context.Context, repRun *model.Run) *model.InputValidationReport {
	workflow := repRun.Workflow

	// This runner executes a called reusable workflow: validate the caller's
	// `with` block against the callee's workflow_call declarations.
	if runner.caller != nil {
		cfg := workflow.WorkflowCallConfig()
		if cfg == nil || cfg.Inputs == nil {
			return nil
		}

		provided := map[string]interface{}{}
		if callerRC := runner.caller.runContext; callerRC != nil && callerRC.Run != nil {
			if job := callerRC.Run.Job(); job != nil {
				for name, value := range job.With {
					provided[name] = evaluateCallInputValue(ctx, value, callerRC.ExprEval)
				}
			}
		}

		calleeRC := runner.newRunContextForCallDefaults(ctx, repRun, cfg)
		defaults := resolveCallInputDefaults(ctx, calleeRC, cfg)
		return workflow.ValidateCallInputs(provided, defaults)
	}

	// Manual run of the top-level workflow.
	switch runner.config.EventName {
	case model.WorkflowDispatchTrigger:
		cfg := workflow.WorkflowDispatchConfig()
		if cfg == nil || cfg.Inputs == nil {
			return nil
		}
		return workflow.ValidateDispatchInputs(parseEventInputs(runner.eventJSON))
	case model.WorkflowCallTrigger:
		// A workflow_call workflow invoked directly (e.g. `act workflow_call`
		// with an event JSON) is validated against its own declarations.
		cfg := workflow.WorkflowCallConfig()
		if cfg == nil || cfg.Inputs == nil {
			return nil
		}
		calleeRC := runner.newRunContextForCallDefaults(ctx, repRun, cfg)
		defaults := resolveCallInputDefaults(ctx, calleeRC, cfg)
		return workflow.ValidateCallInputs(parseEventInputs(runner.eventJSON), defaults)
	default:
		return nil
	}
}

// newRunContextForCallDefaults creates a callee RunContext only when at least
// one declared input has a default (which may contain an expression that must
// be evaluated with the called context). Plain defaults can be decoded from
// their yaml nodes without any RunContext.
func (runner *runnerImpl) newRunContextForCallDefaults(ctx context.Context, repRun *model.Run, cfg *model.WorkflowCall) *RunContext {
	for _, input := range cfg.Inputs {
		if input.Default.Kind != 0 {
			return runner.newRunContext(ctx, repRun, nil)
		}
	}
	return nil
}

// evaluateCallInputValue resolves one value of a caller's `with` block. It
// mirrors the value resolution performed at runtime in setupWorkflowInputs:
// the value is evaluated with the CALLING RunContext's evaluator.
func evaluateCallInputValue(ctx context.Context, value interface{}, eval ExpressionEvaluator) interface{} {
	if value == nil {
		return nil
	}

	node := yaml.Node{}
	if err := node.Encode(value); err != nil {
		return value
	}
	if eval != nil {
		_ = eval.EvaluateYamlNode(ctx, &node)
	}
	if err := node.Decode(&value); err != nil {
		return nil
	}
	return value
}

// resolveCallInputDefaults decodes the workflow_call input defaults. Default
// expressions are evaluated with the CALLED RunContext's evaluator, the same
// way the runtime does it in setupWorkflowInputs.
func resolveCallInputDefaults(ctx context.Context, rc *RunContext, cfg *model.WorkflowCall) map[string]interface{} {
	defaults := map[string]interface{}{}

	for name, input := range cfg.Inputs {
		if input.Default.Kind == 0 {
			continue
		}

		def := input.Default
		if rc != nil && rc.ExprEval != nil {
			if err := rc.ExprEval.EvaluateYamlNode(ctx, &def); err != nil {
				// Fall back to the raw default node; never block validation on
				// an expression that the runtime itself would ignore.
				def = input.Default
			}
		}

		var value interface{}
		if err := def.Decode(&value); err == nil && value != nil {
			defaults[name] = value
		}
	}

	return defaults
}

// parseEventInputs extracts the "inputs" object from an event JSON payload.
func parseEventInputs(eventJSON string) map[string]interface{} {
	if eventJSON == "" {
		return nil
	}

	event := map[string]interface{}{}
	if err := json.Unmarshal([]byte(eventJSON), &event); err != nil {
		return nil
	}

	inputs, _ := event["inputs"].(map[string]interface{})
	return inputs
}

// inputCallLink describes one hop from a calling job to a called workflow.
type inputCallLink struct {
	workflowName string
	file         string
	jobID        string
}

// inputCallChain renders the chain of reusable workflow calls from the
// outermost caller down to the validated workflow, e.g.
// "ci (push.yml)#call -> reusable (reusable.yml)#nested -> inner (inner.yml)".
func (runner *runnerImpl) inputCallChain(workflow *model.Workflow) string {
	if runner.caller == nil {
		return ""
	}

	links := make([]inputCallLink, 0)
	for c := runner.caller; c != nil && c.runContext != nil; c = c.runContext.caller {
		calleeCaller := c.runContext
		if calleeCaller.Run == nil || calleeCaller.Run.Workflow == nil {
			continue
		}
		links = append(links, inputCallLink{
			workflowName: calleeCaller.Run.Workflow.Name,
			file:         calleeCaller.Run.Workflow.File,
			jobID:        calleeCaller.Run.JobID,
		})
	}

	segments := make([]string, 0, len(links)+1)
	for i := len(links) - 1; i >= 0; i-- {
		link := links[i]
		segments = append(segments, fmt.Sprintf("%s (%s)#%s", link.workflowName, link.file, link.jobID))
	}
	segments = append(segments, fmt.Sprintf("%s (%s)", workflow.Name, workflow.File))

	return strings.Join(segments, " -> ")
}
