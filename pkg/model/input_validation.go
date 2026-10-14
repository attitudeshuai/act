package model

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Trigger names for which inputs can be declared.
const (
	WorkflowDispatchTrigger = "workflow_dispatch"
	WorkflowCallTrigger     = "workflow_call"
)

// InputIssueKind describes what kind of validation failure an InputIssue is.
type InputIssueKind string

const (
	// InputIssueUnknown is used by the runner when undeclared inputs are rejected.
	InputIssueUnknown         InputIssueKind = "unknown"
	InputIssueMissingRequired InputIssueKind = "required"
	InputIssueInvalidChoice   InputIssueKind = "choice"
	InputIssueInvalidNumber   InputIssueKind = "number"
	InputIssueInvalidBoolean  InputIssueKind = "boolean"
)

// InputIssue is a single input validation failure. All failures for one
// workflow are collected at once instead of failing on the first one.
type InputIssue struct {
	// Name is the declared input name.
	Name string
	// Kind classifies the validation failure.
	Kind InputIssueKind
	// Location points at the declaration, e.g. "workflow.yml#on.workflow_dispatch.inputs.foo".
	Location string
	// Message describes why the value is invalid.
	Message string
}

// UnknownInput is an input that was provided even though the workflow does
// not declare it.
type UnknownInput struct {
	// Name is the provided input name.
	Name string
	// Location points at the declaration section the workflow would expect it in.
	Location string
}

// InputValidationReport contains all validation findings for one workflow.
type InputValidationReport struct {
	// Trigger is either WorkflowDispatchTrigger or WorkflowCallTrigger.
	Trigger string
	// File is the workflow file the inputs are declared in.
	File string
	// Issues contains the hard validation failures.
	Issues []InputIssue
	// Unknown contains the provided inputs that are not declared.
	Unknown []UnknownInput
}

// HasIssues reports whether any hard validation failure was found.
func (r *InputValidationReport) HasIssues() bool {
	return r != nil && len(r.Issues) > 0
}

// Error renders all issues of the report as one error message.
func (r *InputValidationReport) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "input validation failed for workflow %q: %d invalid input(s):", r.File, len(r.Issues))
	for _, issue := range r.Issues {
		fmt.Fprintf(&b, "\n  - input %q (declared at %s): %s", issue.Name, issue.Location, issue.Message)
	}
	return b.String()
}

// WarnMessage renders the message used when undeclared inputs are ignored.
func (u UnknownInput) WarnMessage() string {
	return fmt.Sprintf("input %q is not declared in %s and will be ignored", u.Name, u.Location)
}

// ErrorMessage renders the message used when undeclared inputs are rejected.
func (u UnknownInput) ErrorMessage() string {
	return fmt.Sprintf("input %q is not declared in %s", u.Name, u.Location)
}

// declaredInput is the normalized view on either a WorkflowDispatchInput or a
// WorkflowCallInput so the validation logic only has to exist once.
type declaredInput struct {
	name       string
	required   bool
	typeName   string
	options    []string
	hasDefault bool
	defValue   interface{}
}

// ValidateDispatchInputs validates the inputs of a workflow_dispatch event.
//
// provided contains the inputs passed via the event (e.g. the command line or
// the event JSON). Missing map keys and empty strings both count as "not
// provided"; declared defaults are used in that case. Undeclared inputs are
// reported separately in InputValidationReport.Unknown.
func (w *Workflow) ValidateDispatchInputs(provided map[string]interface{}) *InputValidationReport {
	decls := make(map[string]declaredInput)

	if cfg := w.WorkflowDispatchConfig(); cfg != nil {
		for name, input := range cfg.Inputs {
			decls[name] = declaredInput{
				name:       name,
				required:   input.Required,
				typeName:   input.Type,
				options:    input.Options,
				hasDefault: input.Default != "",
				defValue:   input.Default,
			}
		}
	}

	return validateDeclaredInputs(w.File, WorkflowDispatchTrigger, decls, provided)
}

// ValidateCallInputs validates the inputs of a workflow_call event.
//
// provided contains the values passed by the calling job's `with` block (with
// expressions already evaluated). resolvedDefaults contains the decoded
// default values of the called workflow (the runner is responsible for
// decoding the yaml nodes and evaluating default expressions); it may be nil.
func (w *Workflow) ValidateCallInputs(provided, resolvedDefaults map[string]interface{}) *InputValidationReport {
	decls := make(map[string]declaredInput)

	if cfg := w.WorkflowCallConfig(); cfg != nil {
		for name, input := range cfg.Inputs {
			decl := declaredInput{
				name:     name,
				required: input.Required,
				typeName: input.Type,
			}
			if input.Default.Kind != 0 {
				decl.hasDefault = true
				if resolvedDefaults != nil {
					decl.defValue = resolvedDefaults[name]
				}
			}
			decls[name] = decl
		}
	}

	return validateDeclaredInputs(w.File, WorkflowCallTrigger, decls, provided)
}

func validateDeclaredInputs(file, trigger string, decls map[string]declaredInput, provided map[string]interface{}) *InputValidationReport {
	report := &InputValidationReport{
		Trigger: trigger,
		File:    file,
		Issues:  []InputIssue{},
		Unknown: []UnknownInput{},
	}

	// Workflows without declared inputs are not affected at all.
	if len(decls) == 0 {
		return report
	}

	section := fmt.Sprintf("%s#on.%s.inputs", file, trigger)

	names := make([]string, 0, len(decls))
	for name := range decls {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		decl := decls[name]
		location := fmt.Sprintf("%s.%s", section, name)

		value, wasProvided := provided[name]
		switch {
		case wasProvided && isProvidedValue(value):
			// the provided value wins
		case decl.hasDefault && isProvidedValue(decl.defValue):
			value = decl.defValue
		default:
			value = nil
		}

		if !isProvidedValue(value) {
			if decl.required {
				report.Issues = append(report.Issues, InputIssue{
					Name:     name,
					Kind:     InputIssueMissingRequired,
					Location: location,
					Message:  "required input was not provided (an empty string does not count as provided)",
				})
			}
			continue
		}

		switch decl.typeName {
		case "boolean":
			if !isValidBooleanInput(value) {
				report.Issues = append(report.Issues, InputIssue{
					Name:     name,
					Kind:     InputIssueInvalidBoolean,
					Location: location,
					Message: fmt.Sprintf(
						"value %s is not a recognizable boolean; declare it as true or false (allowed string values: %q, %q)",
						formatInputValue(value), "true", "false"),
				})
			}
		case "number":
			if _, err := parseNumberInput(value); err != nil {
				report.Issues = append(report.Issues, InputIssue{
					Name:     name,
					Kind:     InputIssueInvalidNumber,
					Location: location,
					Message: fmt.Sprintf(
						"value %s cannot be parsed as a number (declared type: number): %s",
						formatInputValue(value), err.Error()),
				})
			}
		case "choice":
			stringValue, isString := value.(string)
			if !isString || !containsString(decl.options, stringValue) {
				report.Issues = append(report.Issues, InputIssue{
					Name:     name,
					Kind:     InputIssueInvalidChoice,
					Location: location,
					Message: fmt.Sprintf(
						"value %s is not one of the declared choice options %s",
						formatInputValue(value), formatInputOptions(decl.options)),
				})
			}
		default:
			// "" (untyped), "string" and "environment" accept every value, the
			// runtime keeps coercing them the same way it did before.
		}
	}

	unknownNames := make([]string, 0)
	for name := range provided {
		if _, declared := decls[name]; !declared {
			unknownNames = append(unknownNames, name)
		}
	}
	sort.Strings(unknownNames)
	for _, name := range unknownNames {
		report.Unknown = append(report.Unknown, UnknownInput{
			Name:     name,
			Location: section,
		})
	}

	return report
}

// isProvidedValue reports whether a value actually counts as provided. nil and
// empty strings do not, matching the requirement that "" is not a value.
func isProvidedValue(value interface{}) bool {
	if value == nil {
		return false
	}
	if stringValue, isString := value.(string); isString && stringValue == "" {
		return false
	}
	return true
}

// isValidBooleanInput keeps the existing true/false conversion contract: only
// the strings "true" and "false" (and actual yaml/json booleans) are valid, so
// values like "yes" or "1" are reported instead of silently becoming false.
func isValidBooleanInput(value interface{}) bool {
	if _, isBool := value.(bool); isBool {
		return true
	}
	stringValue, isString := value.(string)
	return isString && (stringValue == "true" || stringValue == "false")
}

// parseNumberInput accepts actual numeric values and strings that parse as a
// finite floating point number. Everything else (including booleans) fails.
func parseNumberInput(value interface{}) (float64, error) {
	switch number := value.(type) {
	case int:
		return float64(number), nil
	case int32:
		return float64(number), nil
	case int64:
		return float64(number), nil
	case float32:
		return float64(number), nil
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return 0, fmt.Errorf("number must be finite")
		}
		return number, nil
	case string:
		parsed, err := strconv.ParseFloat(number, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a valid number", number)
		}
		if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return 0, fmt.Errorf("number must be finite")
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("expected a number, got %T", value)
	}
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func formatInputValue(value interface{}) string {
	if stringValue, isString := value.(string); isString {
		return strconv.Quote(stringValue)
	}
	return fmt.Sprintf("%v", value)
}

func formatInputOptions(options []string) string {
	quoted := make([]string, 0, len(options))
	for _, option := range options {
		quoted = append(quoted, strconv.Quote(option))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
