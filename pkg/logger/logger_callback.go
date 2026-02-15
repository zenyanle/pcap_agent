package logger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
	"github.com/elastic/go-elasticsearch/v7"
)

type LoggerCallback struct {
	Es *elasticsearch.Client
	//callbacks.HandlerBuilder // 可以用 callbacks.HandlerBuilder 来辅助实现 callback
}

func (cb *LoggerCallback) OnStart(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
	err := SendWrappedLog(cb.Es, "test_logs", "callback", input)
	if err != nil {
		Warnf("[OnStart] ES 日志写入失败: %v", err)
	}
	fmt.Println("==================")
	inputStr, _ := json.MarshalIndent(input, "", "  ")
	fmt.Printf("[OnStart] %s\n", string(inputStr))
	return ctx
}

func (cb *LoggerCallback) OnEnd(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
	err := SendWrappedLog(cb.Es, "test_logs", "callback", output)
	if err != nil {
		Warnf("[OnEnd] ES 日志写入失败: %v", err)
	}
	fmt.Println("=========[OnEnd]=========")
	outputStr, _ := json.MarshalIndent(output, "", "  ")
	fmt.Println(string(outputStr))
	return ctx
}

func (cb *LoggerCallback) OnError(ctx context.Context, info *callbacks.RunInfo, err error) context.Context {
	fmt.Println("=========[OnError]=========")
	fmt.Println(err)
	return ctx
}

// PrettyLoggerCallback 提供美观易读的日志输出
type PrettyLoggerCallback struct {
	Es   *elasticsearch.Client
	step int
}

func (cb *PrettyLoggerCallback) OnStart(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
	err := SendWrappedLog(cb.Es, "test_logs", "callback", input)
	if err != nil {
		Warnf("[OnStart] ES 日志写入失败: %v", err)
	}
	cb.step++
	fmt.Printf("\n╔════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║ 步骤 #%d - %s 开始\n", cb.step, info.Name)
	fmt.Printf("╠════════════════════════════════════════════════════════════╣\n")

	// 美化输入展示
	if msgs, ok := input.([]*schema.Message); ok {
		for i, msg := range msgs {
			fmt.Printf("║ 消息 %d [%s]:\n", i+1, msg.Role)
			content := msg.Content
			if len(content) > 100 {
				content = content[:100] + "..."
			}
			fmt.Printf("║   %s\n", content)
			if len(msg.ToolCalls) > 0 {
				fmt.Printf("║   工具调用: %d 个\n", len(msg.ToolCalls))
				for j, tc := range msg.ToolCalls {
					fmt.Printf("║     %d. %s\n", j+1, tc.Function.Name)
				}
			}
		}
	} else if msg, ok := input.(*schema.Message); ok {
		fmt.Printf("║ [%s]: %s\n", msg.Role, msg.Content)
	} else if toolMsgs, ok := input.([]*schema.Message); ok && len(toolMsgs) > 0 && toolMsgs[0].Role == schema.Tool {
		fmt.Printf("║ 工具响应: %d 个\n", len(toolMsgs))
		for i, tm := range toolMsgs {
			content := tm.Content
			if len(content) > 500 {
				content = content[:500] + "..."
			}
			fmt.Printf("║   %d. %s\n", i+1, content)
		}
	}
	fmt.Printf("╚════════════════════════════════════════════════════════════╝\n")
	return ctx
}

func (cb *PrettyLoggerCallback) OnEnd(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
	err := SendWrappedLog(cb.Es, "test_logs", "callback", output)
	if err != nil {
		Warnf("[OnEnd] ES 日志写入失败: %v", err)
	}
	fmt.Printf("\n┌────────────────────────────────────────────────────────────┐\n")
	fmt.Printf("│ %s 完成\n", info.Name)
	fmt.Printf("├────────────────────────────────────────────────────────────┤\n")

	// 美化输出展示
	if msg, ok := output.(*schema.Message); ok {
		fmt.Printf("│ 角色: %s\n", msg.Role)
		if msg.Content != "" {
			lines := splitLines(msg.Content, 56)
			for _, line := range lines {
				fmt.Printf("│ %s\n", line)
			}
		}
		if len(msg.ToolCalls) > 0 {
			fmt.Printf("│ \n")
			fmt.Printf("│ 🔧 工具调用:\n")
			for i, tc := range msg.ToolCalls {
				fmt.Printf("│   %d. %s(%s)\n", i+1, tc.Function.Name, tc.ID)
				if len(tc.Function.Arguments) > 0 && len(tc.Function.Arguments) < 100 {
					fmt.Printf("│      参数: %s\n", tc.Function.Arguments)
				}
			}
		}
	} else if msgs, ok := output.([]*schema.Message); ok {
		fmt.Printf("│ 消息数量: %d\n", len(msgs))
		for i, m := range msgs {
			fmt.Printf("│ [%d] %s: %s\n", i+1, m.Role, truncate(m.Content, 50))
		}
	}
	fmt.Printf("└────────────────────────────────────────────────────────────┘\n")
	return ctx
}

func (cb *PrettyLoggerCallback) OnError(ctx context.Context, info *callbacks.RunInfo, err error) context.Context {
	fmt.Printf("\n╔════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║ ❌ 错误发生在: %s\n", info.Name)
	fmt.Printf("╠════════════════════════════════════════════════════════════╣\n")
	fmt.Printf("║ %s\n", err.Error())
	fmt.Printf("╚════════════════════════════════════════════════════════════╝\n")
	return ctx
}

// 辅助函数
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func splitLines(s string, maxLen int) []string {
	if len(s) == 0 {
		return []string{}
	}

	var lines []string
	words := []rune(s)

	for len(words) > 0 {
		if len(words) <= maxLen {
			lines = append(lines, string(words))
			break
		}

		// 查找合适的断点
		breakPoint := maxLen
		for i := maxLen; i > 0; i-- {
			if words[i] == ' ' || words[i] == '\n' {
				breakPoint = i
				break
			}
		}

		lines = append(lines, string(words[:breakPoint]))
		words = words[breakPoint:]

		// 跳过前导空格
		for len(words) > 0 && words[0] == ' ' {
			words = words[1:]
		}
	}

	return lines
}

func (cb *PrettyLoggerCallback) OnEndWithStreamOutput(ctx context.Context, info *callbacks.RunInfo,
	output *schema.StreamReader[callbacks.CallbackOutput]) context.Context {

	var graphInfoName = react.GraphName

	go func() {
		defer func() {
			if err := recover(); err != nil {
				fmt.Printf("\n⚠️  流式输出 panic: %v\n", err)
			}
		}()

		defer output.Close() // remember to close the stream in defer

		fmt.Printf("\n▶ 流式输出开始 [%s]\n", info.Name)
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

		for {
			frame, err := output.Recv()
			if errors.Is(err, io.EOF) {
				fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
				fmt.Printf("◼ 流式输出结束\n\n")
				break
			}
			if err != nil {
				fmt.Printf("⚠️  流式读取错误: %s\n", err)
				return
			}

			if info.Name == graphInfoName { // 仅打印 graph 的输出, 否则每个 stream 节点的输出都会打印一遍
				if msg, ok := frame.(*schema.Message); ok {
					if msg.Content != "" {
						fmt.Printf("│ %s", msg.Content)
					}
					if len(msg.ToolCalls) > 0 {
						fmt.Printf("\n│ 🔧 [工具调用] ")
						for i, tc := range msg.ToolCalls {
							if i > 0 {
								fmt.Print(", ")
							}
							fmt.Printf("%s", tc.Function.Name)
						}
						fmt.Println()
					}
				}
			}
		}

	}()
	return ctx
}

func (cb *PrettyLoggerCallback) OnStartWithStreamInput(ctx context.Context, info *callbacks.RunInfo,
	input *schema.StreamReader[callbacks.CallbackInput]) context.Context {
	defer input.Close()
	return ctx
}

func (cb *LoggerCallback) OnEndWithStreamOutput(ctx context.Context, info *callbacks.RunInfo,
	output *schema.StreamReader[callbacks.CallbackOutput]) context.Context {

	var graphInfoName = react.GraphName

	go func() {
		defer func() {
			if err := recover(); err != nil {
				fmt.Println("[OnEndStream] panic err:", err)
			}
		}()

		defer output.Close() // remember to close the stream in defer

		fmt.Println("=========[OnEndStream]=========")
		for {
			frame, err := output.Recv()
			if errors.Is(err, io.EOF) {
				// finish
				break
			}
			if err != nil {
				fmt.Printf("internal error: %s\n", err)
				return
			}

			s, err := json.Marshal(frame)
			if err != nil {
				fmt.Printf("internal error: %s\n", err)
				return
			}

			if info.Name == graphInfoName { // 仅打印 graph 的输出, 否则每个 stream 节点的输出都会打印一遍
				fmt.Printf("%s: %s\n", info.Name, string(s))
			}
		}

	}()
	return ctx
}

func (cb *LoggerCallback) OnStartWithStreamInput(ctx context.Context, info *callbacks.RunInfo,
	input *schema.StreamReader[callbacks.CallbackInput]) context.Context {
	defer input.Close()
	return ctx
}
