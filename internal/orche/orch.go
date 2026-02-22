package orche

import (
	"context"
	"encoding/json"
	"fmt"
	"pcap_agent/internal/prompts"
	"pcap_agent/pkg/logger"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/prompt"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"

	"github.com/elastic/go-elasticsearch/v7"
)

// Step 代表单个执行步骤
type Step struct {
	StepID int    `json:"step_id"`
	Intent string `json:"intent"` // 这一步的意图，executor 按此开展工作
}

// Plan is the top-level structure returned by the Planner LLM.
type Plan struct {
	Thought     string `json:"thought"`      // LLM chain-of-thought reasoning
	TableSchema string `json:"table_schema"` // Verbatim output of pcapchu-scripts meta
	Steps       []Step `json:"steps"`        // Ordered execution steps
}

func ExtractJSON(input string) (string, error) {
	// 1. 处理 Markdown 代码块 (```json ... ```)
	if strings.Contains(input, "```") {
		start := strings.Index(input, "```")
		// 寻找代码块结尾
		end := strings.LastIndex(input, "```")
		if end > start {
			// 去掉 ```json 或者 ```xml 等标记
			content := input[start+3 : end]
			// 如果是以 json 开头（例如 ```json），去掉它
			if idx := strings.Index(content, "\n"); idx != -1 {
				// 简单的启发式：如果第一行包含 "json"，则跳过第一行
				if strings.Contains(strings.ToLower(content[:idx]), "json") {
					content = content[idx+1:]
				}
			}
			input = content
		}
	}

	// 2. 暴力定位：找到第一个 '{' 或 '[' 和最后一个 '}' 或 ']'
	// 这里为了简化演示，假设是 Object 类型。如果是 Array 需要判断 '['
	start := strings.Index(input, "{")
	end := strings.LastIndex(input, "}")

	if start == -1 || end == -1 || start > end {
		return "", fmt.Errorf("no valid json object found")
	}

	// 截取纯净的 JSON 字符串
	return input[start : end+1], nil
}

// truncateStr 截断字符串，超出 maxLen 时加省略号
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func MakePlanner(rAgent *react.Agent, es *elasticsearch.Client) (compose.Runnable[map[string]any, Plan], error) {
	ctx := context.Background()
	metrics := logger.NewMetrics(es)

	plannerPrompt, err := prompts.GetSinglePrompt("planner")
	if err != nil {
		logger.Fatalf("planner prompt not exsist", err)
	}
	tpl := prompt.FromMessages(schema.GoTemplate,
		schema.SystemMessage(plannerPrompt),
		schema.UserMessage("{{.user_input}}"))

	agentLambda, _ := compose.AnyLambda(rAgent.Generate, nil, nil, nil)
	_ = agentLambda

	// Planner ReAct with callback logging
	plannerReActLambda := compose.InvokableLambda(func(ctx context.Context, in []*schema.Message) (*schema.Message, error) {
		logger.Infof("[Planner-ReAct] input messages count: %d", len(in))
		cb := &logger.PrettyLoggerCallback{Es: es}
		timer := logger.NewTimer()
		out, err := rAgent.Generate(ctx, in,
			agent.WithComposeOptions(compose.WithCallbacks(cb)),
		)
		elapsed := timer.ElapsedMs()
		if err != nil {
			logger.Errorf("[Planner-ReAct] error after %dms: %v", elapsed, err)
			metrics.Emit(logger.MetricsEvent{
				LogType:    logger.LTPlannerError,
				Phase:      logger.PhasePlanner,
				Event:      logger.EventPhaseError,
				DurationMs: elapsed,
				Error:      err.Error(),
			})
			return nil, err
		}
		logger.Infof("[Planner-ReAct] output (%dms) callback_events=%d content=%s",
			elapsed, cb.Step, truncateStr(out.Content, 500))
		metrics.Emit(logger.MetricsEvent{
			LogType:    logger.LTPlannerRawOutput,
			Phase:      logger.PhasePlanner,
			Event:      "react_complete",
			DurationMs: elapsed,
			Output: map[string]any{
				"callback_steps": cb.Step,
				"content_length": len(out.Content),
			},
		})
		return out, nil
	})

	nodeOfPrompt := "prompt"
	nodeOfReAct := "ReAct"
	nodeOfParse := "parse"

	g := compose.NewGraph[map[string]any, Plan]()

	_ = g.AddChatTemplateNode(nodeOfPrompt, tpl)
	_ = g.AddLambdaNode(nodeOfReAct, plannerReActLambda)
	_ = g.AddLambdaNode(nodeOfParse, compose.InvokableLambda(func(ctx context.Context, input *schema.Message) (Plan, error) {
		var plan Plan
		str, err := ExtractJSON(input.Content)
		if err != nil {
			metrics.Emit(logger.MetricsEvent{
				LogType: logger.LTPlannerError,
				Phase:   logger.PhasePlanner,
				Event:   logger.EventPhaseError,
				Error:   fmt.Sprintf("extract json failed: %v", err),
				Input:   map[string]string{"raw_content": truncateStr(input.Content, 1000)},
			})
			return Plan{}, err
		}
		logger.Infof("[Planner] extracted JSON: %s", str)
		err = json.Unmarshal([]byte(str), &plan)
		if err != nil {
			metrics.Emit(logger.MetricsEvent{
				LogType: logger.LTPlannerError,
				Phase:   logger.PhasePlanner,
				Event:   logger.EventPhaseError,
				Error:   fmt.Sprintf("unmarshal plan failed: %v", err),
				Input:   map[string]string{"extracted_json": truncateStr(str, 1000)},
			})
			return Plan{}, fmt.Errorf("failed to unmarshal plan: %w | content: %s", err, input.Content)
		}
		metrics.Emit(logger.MetricsEvent{
			LogType:    logger.LTPlannerParsed,
			Phase:      logger.PhasePlanner,
			Event:      logger.EventPlanParsed,
			TotalSteps: len(plan.Steps),
			Output:     plan,
		})
		logger.Infof("[Planner] plan parsed: %d steps", len(plan.Steps))
		return plan, nil
	}))

	_ = g.AddEdge(compose.START, nodeOfPrompt)
	_ = g.AddEdge(nodeOfPrompt, nodeOfReAct)
	_ = g.AddEdge(nodeOfReAct, nodeOfParse)
	_ = g.AddEdge(nodeOfParse, compose.END)

	r, err := g.Compile(ctx)
	if err != nil {
		return nil, err
	}

	return r, nil
}

/*func MakeExecutor(rAgent *react.Agent, plan Plan) {
	genStateFunc := func(ctx context.Context) *testState {
		return &testState{
			index:  0,
			length: len(plan.Steps),
		}
	}

	nodeOfL1 := "nodeOfL1"

	g := compose.NewGraph[Plan, string](compose.WithGenLocalState(genStateFunc))

	l1 := compose.InvokableLambda(func(ctx context.Context, in Plan) (out []Step, err error) {
		return in.Steps, nil
	})

	toAnyMaps := compose.InvokableLambda(func(ctx context.Context, in any) (out map[string]any, err error) {
		return map[string]any{
			"plans": in,
		}, nil
	})
	nodeOfAnymaps1 := "nodeOfAnymaps1"

	l1StateToOutput := func(ctx context.Context, out []Plan, state *testState) ([]Plan, error) {
		if state.index >= state.length {
			return make([]Plan, 0), nil
		}
		newOut := out[state.index]
		state.index++
		return []Plan{newOut}, nil
	}
	nodeOfAnymaps1State := func(ctx context.Context, out map[string]any, state *testState) (map[string]any, error) {
		out["passed_in"] = "you are first executor agent, no inputs"
		return out, nil
	}

	tpl := prompt.FromMessages(schema.GoTemplate,
		schema.SystemMessage(plannerPrompt),
		schema.UserMessage("{{.user_input}}"))
	tplBundle := struct {
		Tpl  *prompt.DefaultChatTemplate
		Name string
	}{
		Tpl:  tpl,
		Name: "019c7415-f4c2-7607-b792-1d67cf8cc018",
	}

	agentLambda, _ := compose.AnyLambda(rAgent.Generate, nil, nil, nil)
	agentLambdaBundle := struct {
		Runner *compose.Lambda
		Name   string
	}{
		Runner: agentLambda,
		Name:   "019c7416-499d-7e28-9994-e27e8a61e497",
	}

	_ = g.AddLambdaNode(nodeOfL1, l1,
		compose.WithStatePostHandler(l1StateToOutput))

	_ = g.AddLambdaNode(nodeOfAnymaps1, toAnyMaps,
		compose.WithStatePostHandler(nodeOfAnymaps1State))

	_ = g.AddChatTemplateNode(tplBundle.Name, tplBundle.Tpl)
	_ = g.AddLambdaNode(agentLambdaBundle.Name, agentLambdaBundle.Runner)
	_ = g.AddLambdaNode()

}*/

type BaseBundle struct {
	Runner *compose.Lambda
	Name   string
}

type BaseTplBundle struct {
	Name string
	Tpl  *prompt.DefaultChatTemplate
}

type RichBundle[I any, O any, S any] struct {
	Runner   *compose.Lambda
	Name     string
	PreHook  compose.StatePreHandler[I, *S]
	PostHook compose.StatePostHandler[O, *S]
}

type PlanState struct {
	Plan             Plan
	TableSchema      string   // Cached pcapchu-scripts meta output, injected into executor prompts
	CurrentStepIndex int      // 0-based index of the current step
	ResearchFindings string   // Cumulative research findings contributed by each executor
	OperationLog     []string // Append-only operation log, one entry per executor
	EndOutput        string
}

type normalOutput struct {
	Findings  flexString `json:"findings"`   // 本步骤的研究发现
	MyActions flexString `json:"my_actions"` // 本步骤执行的具体操作日志
}

// formatPlanOverview 将 Plan 格式化为可读的文本摘要
func formatPlanOverview(p Plan) string {
	var sb strings.Builder
	sb.WriteString("## Investigation Plan\n\n")
	sb.WriteString("**Planner Thought**: " + p.Thought + "\n\n")
	for _, s := range p.Steps {
		sb.WriteString(fmt.Sprintf("- **Step %d**: %s\n", s.StepID, s.Intent))
	}
	return sb.String()
}

// flexString 兼容 LLM 返回 string 或 array 的情况，统一转成 string
type flexString string

func (f *flexString) UnmarshalJSON(data []byte) error {
	// 尝试作为 string 解析
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*f = flexString(s)
		return nil
	}
	// 尝试作为 []string 解析，拼接成单个字符串
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		*f = flexString(strings.Join(arr, "\n"))
		return nil
	}
	// 兜底：直接用原始 JSON 文本
	*f = flexString(string(data))
	return nil
}

//////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

type NormalExecutor struct {
	tmplString string
	metrics    *logger.Metrics
}

// any, map[string]any
func (n *NormalExecutor) MakePrepareTmplMapBundle() RichBundle[any, map[string]any, PlanState] {
	PrepareTmplMap := compose.InvokableLambda(func(ctx context.Context, in any) (out map[string]any, err error) {
		return nil, nil
	})
	PrepareTmplMapPostHook := func(ctx context.Context, out map[string]any, state *PlanState) (map[string]any, error) {
		idx := state.CurrentStepIndex
		if idx >= len(state.Plan.Steps) {
			n.metrics.Emit(logger.MetricsEvent{
				LogType: logger.LTNormalPrepare,
				Phase:   logger.PhaseExecutor,
				Event:   logger.EventStepError,
				Error:   fmt.Sprintf("step index %d out of range (total %d)", idx, len(state.Plan.Steps)),
			})
			return nil, fmt.Errorf("step index %d out of range", idx)
		}
		step := state.Plan.Steps[idx]

		logger.Infof("[Executor] step %d start: %s", step.StepID, step.Intent)

		// 格式化全局计划概览
		planOverview := formatPlanOverview(state.Plan)

		// 格式化操作日志
		opLog := strings.Join(state.OperationLog, "\n---\n")
		if opLog == "" {
			opLog = "(No operations performed yet — you are the first executor)"
		}

		// 研究发现
		findings := state.ResearchFindings
		if findings == "" {
			findings = "(No research findings yet — you are the first executor)"
		}

		ret := map[string]any{
			"plan_overview":     planOverview,
			"research_findings": findings,
			"operation_log":     opLog,
			"current_step":      fmt.Sprintf("Step %d: %s", step.StepID, step.Intent),
			"table_schema":      state.TableSchema,
		}

		// 上报 step 开始 + 模板注入数据
		n.metrics.Emit(logger.MetricsEvent{
			LogType:    logger.LTNormalPrepare,
			Phase:      logger.PhaseExecutor,
			Event:      logger.EventStepStart,
			StepID:     step.StepID,
			StepIntent: step.Intent,
			TotalSteps: len(state.Plan.Steps),
			Input: map[string]string{
				"current_step":      ret["current_step"].(string),
				"research_findings": truncateStr(findings, 500),
				"operation_log":     truncateStr(opLog, 500),
			},
		})
		logger.Infof("[NormalExecutor] input to step %d:\n  current_step: %s",
			step.StepID, step.Intent)
		return ret, nil
	}
	return RichBundle[any, map[string]any, PlanState]{
		Runner:   PrepareTmplMap,
		PostHook: PrepareTmplMapPostHook,
		Name:     "PrepareTmplMap-019c746a-9645-73d5-879c-96dfb0ce881a",
	}
}

func (n *NormalExecutor) IntroduceTmplBundle() BaseTplBundle {
	tpl := prompt.FromMessages(schema.GoTemplate,
		schema.SystemMessage(n.tmplString))

	return BaseTplBundle{
		Name: "IntroduceTmplBundle-019c79ca-9ebb-7864-8df5-2ff9f00de87b",
		Tpl:  tpl,
	}
}

func (n *NormalExecutor) ParseToExecutorContextBundle() RichBundle[*schema.Message, any, PlanState] {
	ParseToExecutorContext := compose.InvokableLambda(func(ctx context.Context, in *schema.Message) (out any, err error) {
		return nil, nil
	})

	ParseToExecutorContextPreHook := func(ctx context.Context, in *schema.Message, state *PlanState) (*schema.Message, error) {
		idx := state.CurrentStepIndex
		step := state.Plan.Steps[idx]

		str, err := ExtractJSON(in.Content)
		if err != nil {
			n.metrics.Emit(logger.MetricsEvent{
				LogType:    logger.LTNormalParseError,
				Phase:      logger.PhaseExecutor,
				Event:      "extract_json_failed",
				StepID:     step.StepID,
				StepIntent: step.Intent,
				Error:      err.Error(),
				Input:      map[string]string{"raw_content": truncateStr(in.Content, 1000)},
			})
			return nil, fmt.Errorf("extract JSON from executor output failed: %w", err)
		}
		var parsed normalOutput
		err = json.Unmarshal([]byte(str), &parsed)
		if err != nil {
			n.metrics.Emit(logger.MetricsEvent{
				LogType:    logger.LTNormalParseError,
				Phase:      logger.PhaseExecutor,
				Event:      "unmarshal_failed",
				StepID:     step.StepID,
				StepIntent: step.Intent,
				Error:      err.Error(),
				Input:      map[string]string{"extracted_json": truncateStr(str, 1000)},
			})
			return nil, fmt.Errorf("unmarshal executor output failed: %w", err)
		}

		// 追加研究发现到累积报告
		if string(parsed.Findings) != "" {
			state.ResearchFindings += fmt.Sprintf("\n\n### Step %d: %s\n%s", step.StepID, step.Intent, string(parsed.Findings))
		}

		// 追加本步骤的操作日志
		if string(parsed.MyActions) != "" {
			state.OperationLog = append(state.OperationLog, fmt.Sprintf("[Step %d - %s]\n%s", step.StepID, step.Intent, string(parsed.MyActions)))
		}

		// 上报解析后的输出数据
		n.metrics.Emit(logger.MetricsEvent{
			LogType:    logger.LTNormalParsed,
			Phase:      logger.PhaseExecutor,
			Event:      "step_parsed",
			StepID:     step.StepID,
			StepIntent: step.Intent,
			Output: map[string]string{
				"findings":   truncateStr(string(parsed.Findings), 1000),
				"my_actions": truncateStr(string(parsed.MyActions), 1000),
			},
		})

		// 推进步骤索引
		state.CurrentStepIndex++

		logger.Infof("[NormalExecutor] step %d done, parsed output:\n  findings: %s\n  my_actions: %s",
			step.StepID, truncateStr(string(parsed.Findings), 300), truncateStr(string(parsed.MyActions), 300))
		logger.Infof("[Executor] step %d completed, advancing to step index %d", step.StepID, state.CurrentStepIndex)
		return nil, nil
	}

	return RichBundle[*schema.Message, any, PlanState]{
		PreHook: ParseToExecutorContextPreHook,
		Runner:  ParseToExecutorContext,
		Name:    "ParseToExecutorContext-019c7a0e-e202-7696-b79e-7fdf43c4e9ea",
	}
}

//////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

type FinalExecutor struct {
	tmpString string
	metrics   *logger.Metrics
}

// any, map[string]any
func (f *FinalExecutor) MakePrepareTmplMapBundle() RichBundle[any, map[string]any, PlanState] {
	PrepareTmplMap := compose.InvokableLambda(func(ctx context.Context, in any) (out map[string]any, err error) {
		return nil, nil
	})
	PrepareTmplMapPostHook := func(ctx context.Context, out map[string]any, state *PlanState) (map[string]any, error) {
		idx := state.CurrentStepIndex
		if idx >= len(state.Plan.Steps) {
			f.metrics.Emit(logger.MetricsEvent{
				LogType: logger.LTFinalPrepare,
				Phase:   logger.PhaseExecutor,
				Event:   logger.EventStepError,
				Error:   fmt.Sprintf("step index %d out of range in FinalExecutor (total %d)", idx, len(state.Plan.Steps)),
			})
			return nil, fmt.Errorf("step index %d out of range in FinalExecutor", idx)
		}
		step := state.Plan.Steps[idx]

		logger.Infof("[Executor] final step %d start: %s", step.StepID, step.Intent)

		// 格式化全局计划概览
		planOverview := formatPlanOverview(state.Plan)

		// 格式化操作日志
		opLog := strings.Join(state.OperationLog, "\n---\n")
		if opLog == "" {
			opLog = "(No operations recorded)"
		}

		// 研究发现
		findings := state.ResearchFindings
		if findings == "" {
			findings = "(No research findings accumulated)"
		}

		ret := map[string]any{
			"plan_overview":     planOverview,
			"research_findings": findings,
			"operation_log":     opLog,
			"table_schema":      state.TableSchema,
		}

		// 上报 final step 开始 + 输入数据
		f.metrics.Emit(logger.MetricsEvent{
			LogType:    logger.LTFinalPrepare,
			Phase:      logger.PhaseExecutor,
			Event:      logger.EventStepStart,
			StepID:     step.StepID,
			StepIntent: step.Intent,
			TotalSteps: len(state.Plan.Steps),
			Input: map[string]string{
				"research_findings": truncateStr(findings, 500),
				"operation_log":     truncateStr(opLog, 500),
			},
		})
		logger.Infof("[FinalExecutor] input to final step %d:\n  findings: %s",
			step.StepID, truncateStr(findings, 300))
		return ret, nil
	}
	return RichBundle[any, map[string]any, PlanState]{
		Runner:   PrepareTmplMap,
		PostHook: PrepareTmplMapPostHook,
		Name:     "PrepareTmplMap-019c7ba4-846f-77fa-86b1-018f13a0fd30",
	}
}

func (f *FinalExecutor) IntroduceTmplBundle() BaseTplBundle {
	tpl := prompt.FromMessages(schema.GoTemplate,
		schema.SystemMessage(f.tmpString))

	return BaseTplBundle{
		Name: "IntroduceTmplBundle-019c7ba5-a5bf-7030-bc82-8cd6cbbb472e",
		Tpl:  tpl,
	}
}

// 在这里解析出final output
func (f *FinalExecutor) ParseToFinalBundle() BaseBundle {
	ParseToFinal := compose.InvokableLambda(func(ctx context.Context, in *schema.Message) (out string, err error) {
		f.metrics.Emit(logger.MetricsEvent{
			LogType: logger.LTFinalOutput,
			Phase:   logger.PhaseExecutor,
			Event:   logger.EventFinalOutput,
			Output: map[string]any{
				"content_length": len(in.Content),
				"content":        truncateStr(in.Content, 2000),
			},
		})
		logger.Infof("[FinalExecutor] output (length=%d):\n%s", len(in.Content), truncateStr(in.Content, 1000))
		return in.Content, nil
	})

	return BaseBundle{
		Runner: ParseToFinal,
		Name:   "ParseToFinal-019c7a13-2bb2-7051-a9a6-a1ee4bc44564",
	}
}

//////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

/*func MakeExecutorReAct(rAgent *react.Agent) BaseBundle {
	agentLambda, _ := compose.AnyLambda(rAgent.Generate, nil, nil, nil)
	return BaseBundle{
		Runner: agentLambda,
		Name:   "ExecutorReAct-019c79ce-5c46-7600-86d8-7a9cd967a015",
	}
}*/

func MakeIsLast(metrics *logger.Metrics) RichBundle[any, bool, PlanState] {
	IsLast := compose.InvokableLambda(func(ctx context.Context, in any) (out bool, err error) {
		return false, nil
	})
	IsLastPostHook := func(ctx context.Context, out bool, state *PlanState) (bool, error) {
		remaining := len(state.Plan.Steps) - state.CurrentStepIndex
		isLast := remaining == 1
		metrics.Emit(logger.MetricsEvent{
			LogType: logger.LTExecutorLoopCheck,
			Phase:   logger.PhaseExecutor,
			Event:   logger.EventLoopEnter,
			Detail: map[string]any{
				"current_step_index": state.CurrentStepIndex,
				"remaining_steps":    remaining,
				"is_last":            isLast,
			},
		})
		logger.Infof("[Executor] loop check: currentIndex=%d, remaining=%d, isLast=%v", state.CurrentStepIndex, remaining, isLast)
		return isLast, nil
	}
	return RichBundle[any, bool, PlanState]{
		Runner:   IsLast,
		PostHook: IsLastPostHook,
		Name:     "IsLast-019c79fe-2cd3-772f-93f6-ecc2c556bd6a",
	}
}

func MakeExecutor(rAgent *react.Agent, plan Plan, es *elasticsearch.Client) (string, error) {
	ctx := context.Background()
	metrics := logger.NewMetrics(es)

	metrics.Emit(logger.MetricsEvent{
		LogType:    logger.LTExecutorStart,
		Phase:      logger.PhaseExecutor,
		Event:      logger.EventPhaseStart,
		TotalSteps: len(plan.Steps),
		Input:      plan,
	})
	executorTimer := logger.NewTimer()

	if len(plan.Steps) == 0 {
		metrics.Emit(logger.MetricsEvent{
			LogType: logger.LTExecutorError,
			Phase:   logger.PhaseExecutor,
			Event:   logger.EventPhaseError,
			Error:   "plan steps is empty",
		})
		return "", fmt.Errorf("plan steps is empty")
	}
	prepareStateFunc := func(ctx context.Context) *PlanState {
		tableSchema := plan.TableSchema
		if tableSchema == "" {
			tableSchema = "(Table schema not available — run `pcapchu-scripts meta` if needed)"
		}
		return &PlanState{
			Plan:             plan,
			TableSchema:      tableSchema,
			CurrentStepIndex: 0,
			ResearchFindings: "",
			OperationLog:     []string{},
			EndOutput:        "",
		}
	}

	pMaps, err := prompts.GetPrompts()
	if err != nil {
		return "", err
	}

	agentLambda, _ := compose.AnyLambda(rAgent.Generate, nil, nil, nil)
	_ = agentLambda

	// 每个 step 的 ReAct 回调事件计数（用于估算轮数）
	type stepRoundInfo struct {
		Label        string `json:"label"`
		CallbackStep int    `json:"callback_steps"` // OnStart 回调次数
		DurationMs   int64  `json:"duration_ms"`
	}
	var (
		roundRecords []stepRoundInfo
		roundMu      sync.Mutex
	)

	// 包装 ReAct agent，添加输入/输出日志 + WithCallbacks（模仿 main.go 的上报模式）
	reactWithLog := func(label string, logTypeOutput string, logTypeError string) *compose.Lambda {
		wrapped := compose.InvokableLambda(func(ctx context.Context, in []*schema.Message) (*schema.Message, error) {
			logger.Infof("[%s] input messages count: %d", label, len(in))
			for i, msg := range in {
				logger.Infof("[%s]   msg[%d] role=%s content=%s", label, i, msg.Role, truncateStr(msg.Content, 300))
			}
			cb := &logger.PrettyLoggerCallback{Es: es}
			timer := logger.NewTimer()
			out, err := rAgent.Generate(ctx, in,
				agent.WithComposeOptions(compose.WithCallbacks(cb)),
			)
			elapsed := timer.ElapsedMs()

			// 记录本次 ReAct 调用的回调事件数
			roundMu.Lock()
			roundRecords = append(roundRecords, stepRoundInfo{
				Label:        label,
				CallbackStep: cb.Step,
				DurationMs:   elapsed,
			})
			roundMu.Unlock()
			logger.Infof("[%s] callback events: %d (duration %dms)", label, cb.Step, elapsed)

			if err != nil {
				logger.Errorf("[%s] error after %dms: %v", label, elapsed, err)
				metrics.Emit(logger.MetricsEvent{
					LogType:    logTypeError,
					Phase:      logger.PhaseExecutor,
					Event:      logger.EventStepError,
					DurationMs: elapsed,
					Error:      err.Error(),
					Detail:     map[string]string{"node": label},
				})
				return nil, err
			}
			logger.Infof("[%s] output (%dms) role=%s content=%s", label, elapsed, out.Role, truncateStr(out.Content, 500))
			metrics.Emit(logger.MetricsEvent{
				LogType:    logTypeOutput,
				Phase:      logger.PhaseExecutor,
				Event:      logger.EventStepEnd,
				DurationMs: elapsed,
				Output: map[string]any{
					"node":           label,
					"content_length": len(out.Content),
					"content":        truncateStr(out.Content, 2000),
					"callback_steps": cb.Step,
				},
			})
			return out, nil
		})
		return wrapped
	}

	ReAct1 := "ReAct1"
	ReAct2 := "ReAct2"
	react1Lambda := reactWithLog("ReAct-NormalExecutor", logger.LTNormalReactOutput, logger.LTNormalReactError)
	react2Lambda := reactWithLog("ReAct-FinalExecutor", logger.LTFinalReactOutput, logger.LTFinalReactError)

	g := compose.NewGraph[any, string](compose.WithGenLocalState(prepareStateFunc))

	isLast := MakeIsLast(metrics)
	g.AddLambdaNode(isLast.Name, isLast.Runner, compose.WithStatePostHandler(isLast.PostHook))

	normalExecutor := NormalExecutor{tmplString: pMaps["normal_execulator"], metrics: metrics}
	prepareTmplMapBundleNormal := normalExecutor.MakePrepareTmplMapBundle()
	g.AddLambdaNode(prepareTmplMapBundleNormal.Name, prepareTmplMapBundleNormal.Runner, compose.WithStatePostHandler(prepareTmplMapBundleNormal.PostHook))
	introduceTmplBundleNoraml := normalExecutor.IntroduceTmplBundle()
	g.AddChatTemplateNode(introduceTmplBundleNoraml.Name, introduceTmplBundleNoraml.Tpl)
	g.AddLambdaNode(ReAct1, react1Lambda)
	parseToExecutorContextBundle := normalExecutor.ParseToExecutorContextBundle()
	g.AddLambdaNode(parseToExecutorContextBundle.Name, parseToExecutorContextBundle.Runner, compose.WithStatePreHandler(parseToExecutorContextBundle.PreHook))

	finalExecutor := FinalExecutor{tmpString: pMaps["final_execulator"], metrics: metrics}
	prepareTmplMapBundleFinal := finalExecutor.MakePrepareTmplMapBundle()
	g.AddLambdaNode(prepareTmplMapBundleFinal.Name, prepareTmplMapBundleFinal.Runner, compose.WithStatePostHandler(prepareTmplMapBundleFinal.PostHook))
	introduceTmplBundleFinal := finalExecutor.IntroduceTmplBundle()
	g.AddChatTemplateNode(introduceTmplBundleFinal.Name, introduceTmplBundleFinal.Tpl)
	g.AddLambdaNode(ReAct2, react2Lambda)
	parseToFinalBundle := finalExecutor.ParseToFinalBundle()
	g.AddLambdaNode(parseToFinalBundle.Name, parseToFinalBundle.Runner)

	condition := func(ctx context.Context, in bool) (string, error) {
		if in {
			return prepareTmplMapBundleFinal.Name, nil
		}
		return prepareTmplMapBundleNormal.Name, nil
	}
	branch := compose.NewGraphBranch(condition, map[string]bool{
		prepareTmplMapBundleNormal.Name: true,
		prepareTmplMapBundleFinal.Name:  true,
	})

	g.AddEdge(compose.START, isLast.Name)
	g.AddBranch(isLast.Name, branch)

	g.AddEdge(prepareTmplMapBundleNormal.Name, introduceTmplBundleNoraml.Name)
	g.AddEdge(introduceTmplBundleNoraml.Name, ReAct1)
	g.AddEdge(ReAct1, parseToExecutorContextBundle.Name)
	g.AddEdge(parseToExecutorContextBundle.Name, isLast.Name)

	g.AddEdge(prepareTmplMapBundleFinal.Name, introduceTmplBundleFinal.Name)
	g.AddEdge(introduceTmplBundleFinal.Name, ReAct2)
	g.AddEdge(ReAct2, parseToFinalBundle.Name)
	g.AddEdge(parseToFinalBundle.Name, compose.END)

	// 每轮循环约5个节点，支持最多20个step的plan
	r, err := g.Compile(ctx, compose.WithMaxRunSteps(100))
	if err != nil {
		return "", err
	}
	res, err := r.Invoke(ctx, struct{}{})
	if err != nil {
		metrics.Emit(logger.MetricsEvent{
			LogType:    logger.LTExecutorError,
			Phase:      logger.PhaseExecutor,
			Event:      logger.EventPhaseError,
			DurationMs: executorTimer.ElapsedMs(),
			Error:      err.Error(),
		})
		return "", err
	}

	// 上报每个 step 的 ReAct 轮数统计
	roundMu.Lock()
	roundSummary := make([]stepRoundInfo, len(roundRecords))
	copy(roundSummary, roundRecords)
	roundMu.Unlock()

	metrics.Emit(logger.MetricsEvent{
		LogType:    logger.LTExecutorEnd,
		Phase:      logger.PhaseExecutor,
		Event:      logger.EventPhaseEnd,
		DurationMs: executorTimer.ElapsedMs(),
		TotalSteps: len(plan.Steps),
		Output: map[string]any{
			"step_react_rounds": roundSummary,
		},
	})
	logger.Infof("[Executor] completed in %dms, react round summary:", executorTimer.ElapsedMs())
	for i, r := range roundSummary {
		logger.Infof("  [%d] %s: %d callback events, %dms", i, r.Label, r.CallbackStep, r.DurationMs)
	}

	return res, nil

}

/*func MakeExecutor(rAgent *react.Agent, plan Plan) {
	genStateFunc := func(ctx context.Context) *testState {
		ec := make([]ExecutorContext, len(plan.Steps))
		ec[0] = ExecutorContext{
			MainIdea:         "You are first executor, so theres no main idea for you",
			OperationHistory: "You are first executor, so theres no operation history for you",
		}
		return &testState{
			index:            0,
			length:           len(plan.Steps),
			ExecutorContexts: ec,
		}
	}

	g := compose.NewGraph[Plan, string](compose.WithGenLocalState(genStateFunc))

	// 按照index提取出一个step
	// in Plan out []step
	ToSingleStepBundleFunc := func() BundleWithHook[any, []Step] {

		ToSingleStep := compose.InvokableLambda(func(ctx context.Context, in Plan) (out []Step, err error) {
			return in.Steps, nil
		})
		ToSingleStepPostHook := func(ctx context.Context, out []Step, state *testState) ([]Step, error) {
			if state.index >= state.length {
				return make([]Step, 0), nil
			}
			newOut := out[state.index]
			state.index++
			return []Step{newOut}, nil
		}
		return BundleWithHook[any, []Step]{
			Runner:   ToSingleStep,
			PostHook: ToSingleStepPostHook,
			Name:     "ToSingleStep-019c746a-9645-73d5-879c-96dfb0ce881a",
		}
	}

	// 按照index注入ExecutorContext
	// in []Step out ExecutorContext
	EnrichToExecutorContextBundleFunc := func() BundleWithHook[any, ExecutorContext] {

		EnrichToExecutorContext := compose.InvokableLambda(func(ctx context.Context, in []Step) (out ExecutorContext, err error) {
			if len(in) == 0 {
				return ExecutorContext{}, fmt.Errorf("[]step is empty")
			}
			return ExecutorContext{
				Step: in[0],
			}, nil
		})
		EnrichToExecutorContextPostHook := func(ctx context.Context, out ExecutorContext, state *testState) (ExecutorContext, error) {
			ec := state.ExecutorContexts[state.index]
			ec.Step = out.Step
			return ec, nil
		}
		return BundleWithHook[any, ExecutorContext]{
			Runner:   EnrichToExecutorContext,
			PostHook: EnrichToExecutorContextPostHook,
			Name:     "EnrichToExecutorContext-019c7946-d3f1-780c-83c6-6096e7c59f03",
		}
	}

	// 组装成executor templ map
	//

	// 执行器模板
	// in map[string]any out []*schema.Message
	ExecutorTmplBundleFunc := func() BaseTpl {
		tpl := prompt.FromMessages(schema.GoTemplate,
			schema.SystemMessage("plannerPrompt"),
			schema.UserMessage("{{.user_input}}"))
		return BaseTpl{
			Name: "ExecutorTmpl-019c7481-f9a0-7237-96e2-97202471f3c1",
			Tpl:  tpl,
		}
	}

	// 执行器react agent
	// in []*schema.Message out *schema.Message
	ExecutorReActAgentBundleFunc := func() BaseBundle {
		agentLambda, _ := compose.AnyLambda(rAgent.Generate, nil, nil, nil)
		return BaseBundle{
			Runner: agentLambda,
			Name:   "ExecutorReActAgent-019c7486-5e7e-7340-96f7-e0e50769424a",
		}
	}

	ToSingleStepBundle := ToSingleStepBundleFunc()
	_ = g.AddLambdaNode(ToSingleStepBundle.Name, ToSingleStepBundle.Runner,
		compose.WithStatePostHandler(ToSingleStepBundle.PostHook))

	ExecutorTmplBundle := ExecutorTmplBundleFunc()
	_ = g.AddChatTemplateNode(ExecutorTmplBundle.Name, ExecutorTmplBundle.Tpl)

	ExecutorReActAgentBundle := ExecutorReActAgentBundleFunc()
	_ = g.AddLambdaNode(ExecutorTmplBundle.Name, ExecutorReActAgentBundle.Runner)

}*/
