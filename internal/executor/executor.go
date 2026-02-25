package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"pcap_agent/internal/common"
	"pcap_agent/internal/events"
	"pcap_agent/internal/prompts"
	"pcap_agent/pkg/logger"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/prompt"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
)

// Graph node name constants.
const (
	nodeIsLast         = "is-last"
	nodeNormalPrepare  = "normal-prepare"
	nodeNormalTemplate = "normal-template"
	nodeNormalReact    = "normal-react"
	nodeNormalParse    = "normal-parse"
	nodeFinalPrepare   = "final-prepare"
	nodeFinalTemplate  = "final-template"
	nodeFinalReact     = "final-react"
	nodeFinalParse     = "final-parse"
)

// Result is the output of a full executor pipeline run.
type Result struct {
	Report       string
	Findings     string
	OperationLog string
}

// Executor wraps a compiled eino graph that executes all plan steps.
type Executor struct {
	rAgent  *react.Agent
	emitter events.Emitter
}

// NewExecutor creates a new Executor. The graph is built on each Run() call
// because it captures per-run closure state.
func NewExecutor(rAgent *react.Agent, emitter events.Emitter) *Executor {
	return &Executor{rAgent: rAgent, emitter: emitter}
}

// Run executes all steps in the plan and returns the final report plus captured state.
// userQuery is the original user question, injected into executor prompts for context.
// pcapPath is the container-side path to the target PCAP file.
func (e *Executor) Run(ctx context.Context, plan common.Plan, userQuery string, pcapPath string) (*Result, error) {
	if len(plan.Steps) == 0 {
		return nil, fmt.Errorf("plan has no steps")
	}

	// --- Closure state for capturing findings/oplog after Invoke ---
	var (
		capturedFindings string
		capturedOpLog    []string
		captureMu        sync.Mutex
	)

	// --- Load prompt templates ---
	pMaps, err := prompts.GetPrompts()
	if err != nil {
		return nil, fmt.Errorf("load prompts: %w", err)
	}

	// --- State initializer ---
	prepareStateFunc := func(ctx context.Context) *common.PlanState {
		tableSchema := plan.TableSchema
		if tableSchema == "" {
			tableSchema = "(Table schema not available - run `pcapchu-scripts meta` if needed)"
		}
		return &common.PlanState{
			Plan:             plan,
			TableSchema:      tableSchema,
			CurrentStepIndex: 0,
			ResearchFindings: "",
			OperationLog:     []string{},
			EndOutput:        "",
		}
	}

	// --- ReAct wrapper with callback logging ---
	reactWithLog := func(label string) *compose.Lambda {
		return compose.InvokableLambda(func(ctx context.Context, in []*schema.Message) (*schema.Message, error) {
			logger.Infof("[%s] input messages count: %d", label, len(in))
			cb := &logger.PrettyLoggerCallback{}
			timer := logger.NewTimer()
			out, err := e.rAgent.Generate(ctx, in,
				agent.WithComposeOptions(compose.WithCallbacks(cb)),
			)
			elapsed := timer.ElapsedMs()
			if err != nil {
				logger.Errorf("[%s] error after %dms: %v", label, elapsed, err)
				return nil, err
			}
			logger.Infof("[%s] output (%dms) callback_events=%d content=%s",
				label, elapsed, cb.Step, common.TruncateStr(out.Content, 500))
			return out, nil
		})
	}

	// ===================== NORMAL EXECUTOR NODES =====================

	// normal-prepare: inject template variables from PlanState
	normalPrepareLambda := compose.InvokableLambda(func(ctx context.Context, in any) (map[string]any, error) {
		return nil, nil
	})
	normalPreparePostHook := func(ctx context.Context, out map[string]any, state *common.PlanState) (map[string]any, error) {
		idx := state.CurrentStepIndex
		if idx >= len(state.Plan.Steps) {
			return nil, fmt.Errorf("step index %d out of range (total %d)", idx, len(state.Plan.Steps))
		}
		step := state.Plan.Steps[idx]
		logger.Infof("[Executor] step %d start: %s", step.StepID, step.Intent)

		e.emitter.Emit(events.NewEvent(events.TypeStepStarted, "", events.StepStartedData{
			StepID:     step.StepID,
			Intent:     step.Intent,
			TotalSteps: len(state.Plan.Steps),
		}))

		planOverview := common.FormatPlanOverview(state.Plan)
		opLog := strings.Join(state.OperationLog, "\n---\n")
		if opLog == "" {
			opLog = "(No operations performed yet - you are the first executor)"
		}
		findings := state.ResearchFindings
		if findings == "" {
			findings = "(No research findings yet - you are the first executor)"
		}

		return map[string]any{
			"user_query":        userQuery,
			"pcap_path":         pcapPath,
			"plan_overview":     planOverview,
			"research_findings": findings,
			"operation_log":     opLog,
			"current_step":      fmt.Sprintf("Step %d: %s", step.StepID, step.Intent),
			"table_schema":      state.TableSchema,
		}, nil
	}

	// normal-template
	normalTpl := prompt.FromMessages(schema.GoTemplate,
		schema.SystemMessage(pMaps["normal_execulator"]),
	)

	// normal-parse: extract JSON, update state, capture for Result
	normalParseLambda := compose.InvokableLambda(func(ctx context.Context, in *schema.Message) (any, error) {
		return nil, nil
	})
	normalParsePreHook := func(ctx context.Context, in *schema.Message, state *common.PlanState) (*schema.Message, error) {
		idx := state.CurrentStepIndex
		step := state.Plan.Steps[idx]

		str, err := common.ExtractJSON(in.Content)
		if err != nil {
			e.emitter.Emit(events.NewEvent(events.TypeStepError, "", events.ErrorData{
				Phase:   "executor",
				Message: fmt.Sprintf("extract json failed for step %d: %v", step.StepID, err),
				StepID:  step.StepID,
			}))
			return nil, fmt.Errorf("extract JSON from executor output: %w", err)
		}

		var parsed common.NormalOutput
		if err := json.Unmarshal([]byte(str), &parsed); err != nil {
			e.emitter.Emit(events.NewEvent(events.TypeStepError, "", events.ErrorData{
				Phase:   "executor",
				Message: fmt.Sprintf("unmarshal failed for step %d: %v", step.StepID, err),
				StepID:  step.StepID,
			}))
			return nil, fmt.Errorf("unmarshal executor output: %w", err)
		}

		// Append findings
		if parsed.Findings.String() != "" {
			state.ResearchFindings += fmt.Sprintf("\n\n### Step %d: %s\n%s", step.StepID, step.Intent, parsed.Findings)
		}

		// Append operation log
		if parsed.MyActions.String() != "" {
			state.OperationLog = append(state.OperationLog, fmt.Sprintf("[Step %d - %s]\n%s", step.StepID, step.Intent, parsed.MyActions))
		}

		// Emit step findings event
		e.emitter.Emit(events.NewEvent(events.TypeStepFindings, "", events.StepFindingsData{
			StepID:   step.StepID,
			Intent:   step.Intent,
			Findings: common.TruncateStr(parsed.Findings.String(), 2000),
			Actions:  common.TruncateStr(parsed.MyActions.String(), 2000),
		}))

		// --- Capture state via closure for Result ---
		captureMu.Lock()
		capturedFindings = state.ResearchFindings
		capturedOpLog = make([]string, len(state.OperationLog))
		copy(capturedOpLog, state.OperationLog)
		captureMu.Unlock()

		// Advance step index
		state.CurrentStepIndex++
		logger.Infof("[Executor] step %d completed, advancing to index %d", step.StepID, state.CurrentStepIndex)
		return nil, nil
	}

	// ===================== FINAL EXECUTOR NODES =====================

	// final-prepare: inject template variables for the final step
	finalPrepareLambda := compose.InvokableLambda(func(ctx context.Context, in any) (map[string]any, error) {
		return nil, nil
	})
	finalPreparePostHook := func(ctx context.Context, out map[string]any, state *common.PlanState) (map[string]any, error) {
		idx := state.CurrentStepIndex
		if idx >= len(state.Plan.Steps) {
			return nil, fmt.Errorf("step index %d out of range in FinalExecutor (total %d)", idx, len(state.Plan.Steps))
		}
		step := state.Plan.Steps[idx]
		logger.Infof("[Executor] final step %d start: %s", step.StepID, step.Intent)

		e.emitter.Emit(events.NewEvent(events.TypeStepStarted, "", events.StepStartedData{
			StepID:     step.StepID,
			Intent:     step.Intent,
			TotalSteps: len(state.Plan.Steps),
		}))

		planOverview := common.FormatPlanOverview(state.Plan)
		opLog := strings.Join(state.OperationLog, "\n---\n")
		if opLog == "" {
			opLog = "(No operations recorded)"
		}
		findings := state.ResearchFindings
		if findings == "" {
			findings = "(No research findings accumulated)"
		}

		return map[string]any{
			"user_query":        userQuery,
			"pcap_path":         pcapPath,
			"plan_overview":     planOverview,
			"research_findings": findings,
			"operation_log":     opLog,
			"table_schema":      state.TableSchema,
		}, nil
	}

	// final-template
	finalTpl := prompt.FromMessages(schema.GoTemplate,
		schema.SystemMessage(pMaps["final_execulator"]),
	)

	// final-parse: extract the final report content
	finalParseLambda := compose.InvokableLambda(func(ctx context.Context, in *schema.Message) (string, error) {
		logger.Infof("[FinalExecutor] output (length=%d):\n%s", len(in.Content), common.TruncateStr(in.Content, 1000))
		return in.Content, nil
	})

	// ===================== IS-LAST NODE =====================

	isLastLambda := compose.InvokableLambda(func(ctx context.Context, in any) (bool, error) {
		return false, nil
	})
	isLastPostHook := func(ctx context.Context, out bool, state *common.PlanState) (bool, error) {
		remaining := len(state.Plan.Steps) - state.CurrentStepIndex
		isLast := remaining == 1
		logger.Infof("[Executor] loop check: currentIndex=%d, remaining=%d, isLast=%v",
			state.CurrentStepIndex, remaining, isLast)
		return isLast, nil
	}

	// ===================== BUILD GRAPH =====================

	g := compose.NewGraph[any, string](compose.WithGenLocalState(prepareStateFunc))

	// Add nodes
	_ = g.AddLambdaNode(nodeIsLast, isLastLambda, compose.WithStatePostHandler(isLastPostHook))

	_ = g.AddLambdaNode(nodeNormalPrepare, normalPrepareLambda, compose.WithStatePostHandler(normalPreparePostHook))
	_ = g.AddChatTemplateNode(nodeNormalTemplate, normalTpl)
	_ = g.AddLambdaNode(nodeNormalReact, reactWithLog("ReAct-NormalExecutor"))
	_ = g.AddLambdaNode(nodeNormalParse, normalParseLambda, compose.WithStatePreHandler(normalParsePreHook))

	_ = g.AddLambdaNode(nodeFinalPrepare, finalPrepareLambda, compose.WithStatePostHandler(finalPreparePostHook))
	_ = g.AddChatTemplateNode(nodeFinalTemplate, finalTpl)
	_ = g.AddLambdaNode(nodeFinalReact, reactWithLog("ReAct-FinalExecutor"))
	_ = g.AddLambdaNode(nodeFinalParse, finalParseLambda)

	// Branch: is-last decides normal vs final path
	condition := func(ctx context.Context, in bool) (string, error) {
		if in {
			return nodeFinalPrepare, nil
		}
		return nodeNormalPrepare, nil
	}
	branch := compose.NewGraphBranch(condition, map[string]bool{
		nodeNormalPrepare: true,
		nodeFinalPrepare:  true,
	})

	// Edges
	_ = g.AddEdge(compose.START, nodeIsLast)
	_ = g.AddBranch(nodeIsLast, branch)

	_ = g.AddEdge(nodeNormalPrepare, nodeNormalTemplate)
	_ = g.AddEdge(nodeNormalTemplate, nodeNormalReact)
	_ = g.AddEdge(nodeNormalReact, nodeNormalParse)
	_ = g.AddEdge(nodeNormalParse, nodeIsLast) // loop back

	_ = g.AddEdge(nodeFinalPrepare, nodeFinalTemplate)
	_ = g.AddEdge(nodeFinalTemplate, nodeFinalReact)
	_ = g.AddEdge(nodeFinalReact, nodeFinalParse)
	_ = g.AddEdge(nodeFinalParse, compose.END)

	// Compile with generous step limit (5 nodes per loop × max 20 steps)
	compiled, err := g.Compile(ctx, compose.WithMaxRunSteps(100))
	if err != nil {
		return nil, fmt.Errorf("compile executor graph: %w", err)
	}

	// ===================== INVOKE =====================

	timer := logger.NewTimer()
	report, err := compiled.Invoke(ctx, struct{}{})
	elapsed := timer.ElapsedMs()

	if err != nil {
		e.emitter.Emit(events.NewEvent(events.TypeError, "", events.ErrorData{
			Phase:   "executor",
			Message: err.Error(),
		}))
		return nil, fmt.Errorf("executor invoke: %w (after %dms)", err, elapsed)
	}

	// Emit report event
	e.emitter.Emit(events.NewEvent(events.TypeReportGenerated, "", events.ReportData{
		Report:     common.TruncateStr(report, 5000),
		ContentLen: len(report),
		TotalSteps: len(plan.Steps),
		DurationMs: elapsed,
	}))

	// Build result from captured closure state
	captureMu.Lock()
	result := &Result{
		Report:       report,
		Findings:     capturedFindings,
		OperationLog: strings.Join(capturedOpLog, "\n---\n"),
	}
	captureMu.Unlock()

	logger.Infof("[Executor] completed in %dms, report length=%d", elapsed, len(report))
	return result, nil
}
