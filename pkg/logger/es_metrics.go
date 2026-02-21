package logger

import (
	"time"

	"github.com/elastic/go-elasticsearch/v7"
)

const (
	MetricsIndex = "test_logs" // 与 callback 使用同一个 index，确保 ES/OpenObserve 可写入

	// 阶段类型
	PhasePlanner  = "planner"
	PhaseExecutor = "executor"

	// LogType 常量 — 用于 ES 过滤，标识上报来源
	// Planner 阶段
	LTPlannerRawOutput = "planner.react_output" // Planner ReAct 原始 LLM 输出
	LTPlannerParsed    = "planner.plan_parsed"  // Planner 解析出的 Plan 结构
	LTPlannerError     = "planner.error"        // Planner 解析/执行错误

	// Executor 阶段 — 全局
	LTExecutorStart     = "executor.start"      // Executor 整体开始
	LTExecutorEnd       = "executor.end"        // Executor 整体结束
	LTExecutorError     = "executor.error"      // Executor 整体错误
	LTExecutorLoopCheck = "executor.loop_check" // 循环分支判断

	// NormalExecutor 阶段
	LTNormalPrepare     = "executor.normal.prepare"      // Normal step 模板注入数据
	LTNormalReactOutput = "executor.normal.react_output" // Normal ReAct 输出
	LTNormalReactError  = "executor.normal.react_error"  // Normal ReAct 错误
	LTNormalParsed      = "executor.normal.parsed"       // Normal step 解析后的 findings/actions
	LTNormalParseError  = "executor.normal.parse_error"  // Normal step JSON 解析失败

	// FinalExecutor 阶段
	LTFinalPrepare     = "executor.final.prepare"      // Final step 模板注入数据
	LTFinalReactOutput = "executor.final.react_output" // Final ReAct 输出
	LTFinalReactError  = "executor.final.react_error"  // Final ReAct 错误
	LTFinalOutput      = "executor.final.output"       // Final step 最终输出

	// Legacy event types (kept for backward compat)
	EventPhaseStart  = "phase_start"
	EventPhaseEnd    = "phase_end"
	EventPhaseError  = "phase_error"
	EventStepStart   = "step_start"
	EventStepEnd     = "step_end"
	EventStepError   = "step_error"
	EventPlanParsed  = "plan_parsed"
	EventLoopEnter   = "loop_enter"
	EventFinalOutput = "final_output"
)

// MetricsEvent 是写入 ES 的打点事件结构
type MetricsEvent struct {
	Timestamp  time.Time   `json:"@timestamp"`
	LogType    string      `json:"log_type"`              // 上报来源标识，用于 ES 过滤（如 "executor.normal.parsed"）
	Phase      string      `json:"phase"`                 // planner / executor
	Event      string      `json:"event"`                 // 事件子类型
	StepID     int         `json:"step_id,omitempty"`     // 当前执行的 step 编号
	StepIntent string      `json:"step_intent,omitempty"` // step 的意图描述
	TotalSteps int         `json:"total_steps,omitempty"` // plan 总步数
	DurationMs int64       `json:"duration_ms,omitempty"` // 耗时毫秒
	Error      string      `json:"error,omitempty"`       // 错误信息
	Input      interface{} `json:"input,omitempty"`       // 节点输入数据
	Output     interface{} `json:"output,omitempty"`      // 节点输出数据
	Detail     interface{} `json:"detail,omitempty"`      // 自定义附加数据
}

// Metrics 是 ES 打点上报器
type Metrics struct {
	es    *elasticsearch.Client
	index string
}

// NewMetrics 创建打点上报器；es 为 nil 时所有上报静默跳过（不影响主流程）
func NewMetrics(es *elasticsearch.Client) *Metrics {
	return &Metrics{es: es, index: MetricsIndex}
}

// Emit 上报一条打点事件；失败仅打 warn 日志，不阻断业务
func (m *Metrics) Emit(evt MetricsEvent) {
	if m == nil || m.es == nil {
		return
	}
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now()
	}
	logType := evt.LogType
	if logType == "" {
		logType = evt.Phase + "." + evt.Event // fallback: e.g. "planner.phase_error"
	}
	if err := SendWrappedLog(m.es, m.index, logType, evt); err != nil {
		Warnf("[Metrics] ES 写入失败 (log_type=%s): %v", logType, err)
	} else {
		Infof("[Metrics] ES 写入成功: log_type=%s", logType)
	}
}

// Timer 用于方便地测量耗时
type Timer struct {
	start time.Time
}

func NewTimer() *Timer {
	return &Timer{start: time.Now()}
}

func (t *Timer) ElapsedMs() int64 {
	return time.Since(t.start).Milliseconds()
}
