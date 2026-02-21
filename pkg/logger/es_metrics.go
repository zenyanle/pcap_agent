package logger

import (
	"time"

	"github.com/elastic/go-elasticsearch/v7"
)

const (
	MetricsIndex = "pcap_agent_metrics"

	// 阶段类型
	PhasePlanner  = "planner"
	PhaseExecutor = "executor"

	// 事件类型
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
	Phase      string      `json:"phase"`                 // planner / executor
	Event      string      `json:"event"`                 // phase_start / step_end / ...
	StepID     int         `json:"step_id,omitempty"`     // 当前执行的 step 编号
	StepIntent string      `json:"step_intent,omitempty"` // step 的意图描述
	TotalSteps int         `json:"total_steps,omitempty"` // plan 总步数
	DurationMs int64       `json:"duration_ms,omitempty"` // 耗时毫秒
	Error      string      `json:"error,omitempty"`       // 错误信息
	Detail     interface{} `json:"detail,omitempty"`      // 自定义附加数据
}

// Metrics 是 ES 打点上报器
type Metrics struct {
	es    *elasticsearch.Client
	index string
}

// NewMetrics 创建打点上报器；es 为 nil 时所有上报静默跳过（不影响主流程）
func NewMetrics(es *elasticsearch.Client) *Metrics {
	idx := MetricsIndex
	return &Metrics{es: es, index: idx}
}

// Emit 上报一条打点事件；失败仅打 warn 日志，不阻断业务
func (m *Metrics) Emit(evt MetricsEvent) {
	if m == nil || m.es == nil {
		return
	}
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now()
	}
	if err := SendWrappedLog(m.es, m.index, evt.Event, evt); err != nil {
		Warnf("[Metrics] ES 写入失败 (event=%s): %v", evt.Event, err)
	} else {
		Infof("[Metrics] ES 写入成功: phase=%s event=%s", evt.Phase, evt.Event)
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
