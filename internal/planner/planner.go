package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"pcap_agent/internal/common"
	"pcap_agent/internal/events"
	"pcap_agent/internal/prompts"
	"pcap_agent/pkg/logger"
	"strings"

	"github.com/cloudwego/eino/components/prompt"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
)

// PlannerInput is the input to a planner invocation.
type PlannerInput struct {
	UserQuery string
	PcapPath  string                 // container-side path to the target PCAP
	History   *common.SessionHistory // nil on first round
}

// Planner wraps a compiled eino graph that produces a Plan from a user query.
type Planner struct {
	graph   compose.Runnable[map[string]any, common.Plan]
	emitter events.Emitter
}

// NewPlanner builds the planner graph: prompt → react → parse.
// rAgent is the shared ReAct agent (with tools for pcapchu-scripts meta etc.).
func NewPlanner(ctx context.Context, rAgent *react.Agent, emitter events.Emitter) (*Planner, error) {
	plannerPrompt, err := prompts.GetSinglePrompt("planner")
	if err != nil {
		return nil, fmt.Errorf("load planner prompt: %w", err)
	}

	tpl := prompt.FromMessages(schema.GoTemplate,
		schema.SystemMessage(plannerPrompt),
		schema.UserMessage("{{.user_input}}"),
	)

	// Planner ReAct with callback-based logging
	plannerReActLambda := compose.InvokableLambda(func(ctx context.Context, in []*schema.Message) (*schema.Message, error) {
		logger.Infof("[Planner-ReAct] input messages count: %d", len(in))
		cb := &logger.PrettyLoggerCallback{}
		timer := logger.NewTimer()
		out, err := rAgent.Generate(ctx, in,
			agent.WithComposeOptions(compose.WithCallbacks(cb)),
		)
		elapsed := timer.ElapsedMs()
		if err != nil {
			logger.Errorf("[Planner-ReAct] error after %dms: %v", elapsed, err)
			return nil, err
		}
		logger.Infof("[Planner-ReAct] output (%dms) callback_events=%d content=%s",
			elapsed, cb.Step, common.TruncateStr(out.Content, 500))
		return out, nil
	})

	// Parse LLM output → Plan
	parseLambda := compose.InvokableLambda(func(ctx context.Context, input *schema.Message) (common.Plan, error) {
		var plan common.Plan
		str, err := common.ExtractJSON(input.Content)
		if err != nil {
			return common.Plan{}, fmt.Errorf("extract json from planner output: %w", err)
		}
		logger.Infof("[Planner] extracted JSON: %s", common.TruncateStr(str, 500))
		if err := json.Unmarshal([]byte(str), &plan); err != nil {
			return common.Plan{}, fmt.Errorf("unmarshal plan: %w | content: %s", err, common.TruncateStr(input.Content, 1000))
		}
		logger.Infof("[Planner] plan parsed: %d steps", len(plan.Steps))
		return plan, nil
	})

	g := compose.NewGraph[map[string]any, common.Plan]()
	_ = g.AddChatTemplateNode("planner-prompt", tpl)
	_ = g.AddLambdaNode("planner-react", plannerReActLambda)
	_ = g.AddLambdaNode("planner-parse", parseLambda)
	_ = g.AddEdge(compose.START, "planner-prompt")
	_ = g.AddEdge("planner-prompt", "planner-react")
	_ = g.AddEdge("planner-react", "planner-parse")
	_ = g.AddEdge("planner-parse", compose.END)

	compiled, err := g.Compile(ctx)
	if err != nil {
		return nil, fmt.Errorf("compile planner graph: %w", err)
	}

	return &Planner{graph: compiled, emitter: emitter}, nil
}

// Run executes the planner graph and returns a Plan.
// If input.History is non-nil, session history is injected as a user message before the query.
func (p *Planner) Run(ctx context.Context, input PlannerInput) (common.Plan, error) {
	templateVars := map[string]any{
		"user_input": input.UserQuery,
		"pcap_path":  input.PcapPath,
	}

	// If we have session history, prepend it to the user input
	if input.History != nil {
		historySection := buildHistorySection(input.History)
		if historySection != "" {
			templateVars["user_input"] = historySection + "\n\n---\n\n**Current Query:**\n" + input.UserQuery
		}
	}

	plan, err := p.graph.Invoke(ctx, templateVars)
	if err != nil {
		p.emitter.Emit(events.NewEvent(events.TypePlanError, "", events.ErrorData{
			Phase:   "planner",
			Message: err.Error(),
		}))
		return common.Plan{}, fmt.Errorf("planner invoke: %w", err)
	}

	// Emit plan created event
	steps := make([]events.StepInfo, len(plan.Steps))
	for i, s := range plan.Steps {
		steps[i] = events.StepInfo{StepID: s.StepID, Intent: s.Intent}
	}
	p.emitter.Emit(events.NewEvent(events.TypePlanCreated, "", events.PlanCreatedData{
		Thought:    plan.Thought,
		TotalSteps: len(plan.Steps),
		Steps:      steps,
	}))

	return plan, nil
}

// buildHistorySection formats session history into a markdown section for the planner prompt.
func buildHistorySection(h *common.SessionHistory) string {
	if h == nil {
		return ""
	}

	var parts []string

	if h.Findings != "" {
		parts = append(parts, "## Previous Research Findings\n\n"+h.Findings)
	}
	if h.OperationLog != "" {
		parts = append(parts, "## Previous Operation Log\n\n"+h.OperationLog)
	}
	if h.PreviousReport != "" {
		parts = append(parts, "## Most Recent Report\n\n"+h.PreviousReport)
	}

	if len(parts) == 0 {
		return ""
	}

	return "# Context From Previous Rounds\n\n" + strings.Join(parts, "\n\n---\n\n")
}
