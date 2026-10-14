package model

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readValidationWorkflow(t *testing.T, yml string) *Workflow {
	t.Helper()
	workflow, err := ReadWorkflow(strings.NewReader(yml), false)
	require.NoError(t, err)
	return workflow
}

const dispatchValidationWorkflow = `
name: wf
on:
  workflow_dispatch:
    inputs:
      required_string:
        required: true
        type: string
      optional_string:
        required: false
        type: string
        default: hello
      pick:
        required: true
        type: choice
        options:
          - small
          - medium
      count:
        required: false
        type: number
        default: "3"
      enabled:
        required: false
        type: boolean
      env:
        required: false
        type: environment
      bad_choice_default:
        required: false
        type: choice
        options:
          - a
        default: b
      bad_number_default:
        required: false
        type: number
        default: not-a-number
      bad_boolean_default:
        required: false
        type: boolean
        default: yes
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo
`

func TestValidateDispatchInputsAllValid(t *testing.T) {
	workflow := readValidationWorkflow(t, dispatchValidationWorkflow)

	report := workflow.ValidateDispatchInputs(map[string]interface{}{
		"required_string":     "value",
		"pick":                "medium",
		"count":               "42",
		"enabled":             "true",
		"env":                 "production",
		"bad_choice_default":  "a",
		"bad_number_default":  "1",
		"bad_boolean_default": "true",
	})

	assert.False(t, report.HasIssues())
	assert.Empty(t, report.Unknown)
}

func TestValidateDispatchInputsDefaultsAreUsedAndValidated(t *testing.T) {
	workflow := readValidationWorkflow(t, dispatchValidationWorkflow)

	// only the required inputs are provided; defaults kick in for the rest -
	// but three of the declared defaults themselves violate the type/options
	report := workflow.ValidateDispatchInputs(map[string]interface{}{
		"required_string": "value",
		"pick":            "small",
	})

	names := issueNames(report)
	assert.ElementsMatch(t, []string{"bad_boolean_default", "bad_choice_default", "bad_number_default"}, names)
	assert.Equal(t, InputIssueInvalidChoice, issueByName(report, "bad_choice_default").Kind)
	assert.Equal(t, InputIssueInvalidNumber, issueByName(report, "bad_number_default").Kind)
	assert.Equal(t, InputIssueInvalidBoolean, issueByName(report, "bad_boolean_default").Kind)
}

func TestValidateDispatchInputsReportsAllFailuresAtOnce(t *testing.T) {
	workflow := readValidationWorkflow(t, dispatchValidationWorkflow)

	report := workflow.ValidateDispatchInputs(map[string]interface{}{
		"required_string":     "",      // empty string does not count as provided
		"pick":                "large", // not an option
		"count":               "abc",   // not a number
		"enabled":             "yes",   // not a recognizable boolean
		"extra_one":           "value", // undeclared
		"extra_two":           "value", // undeclared
		"bad_choice_default":  "a",     // makes the default irrelevant
		"bad_number_default":  "2",     // overrides invalid default
		"bad_boolean_default": "false", // overrides invalid default
	})

	// required_string is missing, pick/count/enabled have invalid values; the
	// bad defaults are masked by valid provided values
	kindsByName := map[string]InputIssueKind{}
	for _, issue := range report.Issues {
		kindsByName[issue.Name] = issue.Kind
	}
	assert.Equal(t, InputIssueMissingRequired, kindsByName["required_string"])
	assert.Equal(t, InputIssueInvalidChoice, kindsByName["pick"])
	assert.Equal(t, InputIssueInvalidNumber, kindsByName["count"])
	assert.Equal(t, InputIssueInvalidBoolean, kindsByName["enabled"])
	assert.Len(t, report.Issues, 4, "all issues are reported at once")

	// unknown inputs are reported separately, not as hard issues
	assert.ElementsMatch(t, []string{"extra_one", "extra_two"}, unknownNames(report))
}

func TestValidateDispatchInputsIssueMessagesCarryNameLocationAndReason(t *testing.T) {
	workflow := readValidationWorkflow(t, dispatchValidationWorkflow)

	report := workflow.ValidateDispatchInputs(map[string]interface{}{
		"required_string": "",
		"pick":            "large",
		"count":           "abc",
		"enabled":         "yes",
	})

	err := report.Error()
	assert.Contains(t, err, `input "required_string"`)
	assert.Contains(t, err, "input validation failed for workflow")

	choice := issueByName(report, "pick")
	assert.Contains(t, choice.Location, "on.workflow_dispatch.inputs.pick")
	assert.Contains(t, choice.Message, `"large"`)
	assert.Contains(t, choice.Message, `"small"`)
	assert.Contains(t, choice.Message, `"medium"`)

	number := issueByName(report, "count")
	assert.Contains(t, number.Message, `"abc"`)
	assert.Contains(t, number.Message, "number")

	boolean := issueByName(report, "enabled")
	assert.Contains(t, boolean.Message, `"yes"`)
	assert.Contains(t, boolean.Message, `"true"`)
	assert.Contains(t, boolean.Message, `"false"`)

	missing := issueByName(report, "required_string")
	assert.Contains(t, missing.Message, "required input was not provided")
	assert.Contains(t, missing.Location, "on.workflow_dispatch.inputs.required_string")
}

func TestValidateDispatchInputsBooleanAndNumberAcceptance(t *testing.T) {
	workflow := readValidationWorkflow(t, dispatchValidationWorkflow)

	validOverrides := map[string]interface{}{
		"required_string":     "value",
		"pick":                "small",
		"bad_choice_default":  "a",
		"bad_number_default":  "1",
		"bad_boolean_default": "true",
	}

	provided := cloneInputs(validOverrides)
	provided["count"] = 7       // actual number from yaml/json
	provided["enabled"] = false // actual boolean from yaml/json
	report := workflow.ValidateDispatchInputs(provided)
	assert.False(t, report.HasIssues())

	provided = cloneInputs(validOverrides)
	provided["count"] = "1.5"
	provided["enabled"] = "false"
	report = workflow.ValidateDispatchInputs(provided)
	assert.False(t, report.HasIssues())

	// "1" must stay invalid as boolean: the runtime only treats "true" as true
	provided = cloneInputs(validOverrides)
	provided["enabled"] = "1"
	report = workflow.ValidateDispatchInputs(provided)
	assert.Equal(t, InputIssueInvalidBoolean, issueByName(report, "enabled").Kind)

	// a boolean value is not a number
	provided = cloneInputs(validOverrides)
	provided["count"] = true
	report = workflow.ValidateDispatchInputs(provided)
	assert.Equal(t, InputIssueInvalidNumber, issueByName(report, "count").Kind)
}

func TestValidateDispatchInputsWithoutDeclarationsIsEmpty(t *testing.T) {
	scalarWorkflow := readValidationWorkflow(t, "on: workflow_dispatch\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n")
	report := scalarWorkflow.ValidateDispatchInputs(map[string]interface{}{
		"anything": "value",
	})
	assert.False(t, report.HasIssues())
	assert.Empty(t, report.Unknown, "workflows without declared inputs are not affected")
}

const callValidationWorkflow = `
name: reusable
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
      optional_bool:
        required: false
        type: boolean
        default: true
      optional_number:
        required: false
        type: number
        default: 1
      optional_string:
        required: false
        type: string
        default: dflt
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo
`

func TestValidateCallInputsAllValid(t *testing.T) {
	workflow := readValidationWorkflow(t, callValidationWorkflow)

	defaults := map[string]interface{}{
		"optional_bool":   true,
		"optional_number": 1,
		"optional_string": "dflt",
	}

	report := workflow.ValidateCallInputs(map[string]interface{}{
		"required_string": "value",
		"required_bool":   true,
		"required_number": 42,
	}, defaults)
	assert.False(t, report.HasIssues())
	assert.Empty(t, report.Unknown)

	// string forms are valid as well, keeping the existing conversion contract
	report = workflow.ValidateCallInputs(map[string]interface{}{
		"required_string": "value",
		"required_bool":   "false",
		"required_number": "3.14",
	}, defaults)
	assert.False(t, report.HasIssues())
}

func TestValidateCallInputsFailures(t *testing.T) {
	workflow := readValidationWorkflow(t, callValidationWorkflow)

	report := workflow.ValidateCallInputs(map[string]interface{}{
		"required_string": "value",
		// required_bool and required_number missing
		"optional_number": "nope",
		"optional_bool":   "maybe",
		"unexpected":      "x",
	}, map[string]interface{}{
		"optional_bool":   true,
		"optional_number": 1,
		"optional_string": "dflt",
	})

	kindsByName := map[string]InputIssueKind{}
	for _, issue := range report.Issues {
		kindsByName[issue.Name] = issue.Kind
	}
	assert.Equal(t, InputIssueMissingRequired, kindsByName["required_bool"])
	assert.Equal(t, InputIssueMissingRequired, kindsByName["required_number"])
	assert.Equal(t, InputIssueInvalidNumber, kindsByName["optional_number"])
	assert.Equal(t, InputIssueInvalidBoolean, kindsByName["optional_bool"])

	assert.Contains(t, issueByName(report, "required_bool").Location, "on.workflow_call.inputs.required_bool")
	assert.ElementsMatch(t, []string{"unexpected"}, unknownNames(report))
}

func TestValidateCallInputsWithoutDeclarationsIsEmpty(t *testing.T) {
	workflow := readValidationWorkflow(t, "on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n")
	report := workflow.ValidateCallInputs(map[string]interface{}{"anything": "value"}, nil)
	assert.False(t, report.HasIssues())
	assert.Empty(t, report.Unknown)
}

func TestUnknownInputMessages(t *testing.T) {
	unknown := UnknownInput{Name: "foo", Location: "wf.yml#on.workflow_dispatch.inputs"}
	assert.Contains(t, unknown.WarnMessage(), "will be ignored")
	assert.Contains(t, unknown.ErrorMessage(), `input "foo" is not declared`)
}

func cloneInputs(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func issueNames(report *InputValidationReport) []string {
	names := make([]string, 0, len(report.Issues))
	for _, issue := range report.Issues {
		names = append(names, issue.Name)
	}
	return names
}

func unknownNames(report *InputValidationReport) []string {
	names := make([]string, 0, len(report.Unknown))
	for _, unknown := range report.Unknown {
		names = append(names, unknown.Name)
	}
	return names
}

func issueByName(report *InputValidationReport, name string) InputIssue {
	for _, issue := range report.Issues {
		if issue.Name == name {
			return issue
		}
	}
	return InputIssue{}
}
