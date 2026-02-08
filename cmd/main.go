/*
 * Copyright 2024 CloudWeGo Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cloudwego/eino-ext/components/tool/commandline"
	"io"
	"log"
	"os"
	"time"

	logs "pcap_agent/pkg/logger"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino-ext/components/tool/commandline/sandbox"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
	"strings"
)

func main() {

	ctx := context.Background()

	op, err := sandbox.NewDockerSandbox(ctx, &sandbox.Config{Image: "net-analyzer:latest", VolumeBindings: map[string]string{
		"/home/hugo/ubuntu-mount": "/home/linuxbrew/pcaps",
	}})
	if err != nil {
		log.Fatal(err)
	}
	// you should ensure that docker has been started before create a docker container
	err = op.Create(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer op.Cleanup(ctx)

	/*	sre, err := commandline.NewStrReplaceEditor(ctx, &commandline.EditorConfig{Operator: op})
		if err != nil {
			log.Fatal(err)
		}*/

	arkApiKey := os.Getenv("ARK_API_KEY")
	arkModelName := os.Getenv("ARK_MODEL_NAME")
	arkBaseUrl := os.Getenv("ARK_BASE_URL")

	config := &openai.ChatModelConfig{
		APIKey:  arkApiKey,
		Model:   arkModelName,
		BaseURL: arkBaseUrl,
	}
	arkModel, err := openai.NewChatModel(ctx, config)
	if err != nil {
		logs.Errorf("failed to create chat model: %v", err)
		return
	}


	bash := NewBashTool(op)

	sre, err := commandline.NewStrReplaceEditor(ctx, &commandline.EditorConfig{Operator: op})
	if err != nil {
		log.Fatal(err)
	}

	content := `## 1. 环境概述 (System Context)
这是一个基于 **Ubuntu 24.04** 的 Docker 容器，专用于网络流量分析 (PCAP) 和取证。
- **系统架构**: Linux x86_64
- **当前用户**: linuxbrew (非 root，但有无密码 sudo 权限)
- **包管理器**: Homebrew (系统级), uv (Python 级)
- **工作目录**: /data (建议将 PCAP 文件挂载至此目录)
- **Python环境**: 虚拟环境已自动激活 (/home/linuxbrew/venv)

## 2. 常用工具指南 (Tool Usage)

### A. Tshark (Wireshark 命令行版)
用于精确提取包信息或进行包过滤。

* **基本读取**:
    tshark -r input.pcap
* **应用过滤器 (显示过滤器)**:
    tshark -r input.pcap -Y "http.request.method == POST"
* **提取特定字段 (CSV格式)**:
    tshark -r input.pcap -T fields -e frame.number -e ip.src -e ip.dst -e http.host
* **统计分析**:
    tshark -r input.pcap -q -z io,phs (协议分级统计)

### B. Zeek (原 Bro)
用于将 PCAP 文件转换为结构化的日志文件 (conn.log, http.log, dns.log 等)。

* **分析 PCAP 文件**:
    zeek -r input.pcap
    *(注意：这会在当前目录下生成大量 .log 文件)*
* **查看连接日志**:
    cat conn.log | zeek-cut id.orig_h id.resp_h service
* **指定脚本策略**:
    zeek -r input.pcap frameworks/files/extract-all-files (提取流量中的文件)

### C. Python 分析库 (已预装)
环境使用 uv 管理依赖，虚拟环境默认激活。直接运行 python 或 ipython 即可。

#### 1. Scapy (强大的包伪造与解析)
from scapy.all import *
# 读取 PCAP
packets = rdpcap("input.pcap")
# 查看摘要
packets.summary()
# 访问特定层 (例如提取 DNS 查询)
for pkt in packets:
    if DNS in pkt and pkt[DNS].qr == 0:
        print(pkt[DNS].qd.qname)

#### 2. PyShark (Tshark 的 Python 封装)

import pyshark
# 懒加载读取 (适合大文件)
cap = pyshark.FileCapture('input.pcap', display_filter='http')
for pkt in cap:
    print(pkt.http.host)

#### 3. Pandas (数据统计)

通常结合 CSV 使用。先用 Tshark 导出为 CSV，再用 Pandas 分析。
`

	rAgent, err := react.NewAgent(ctx, &react.AgentConfig{
		ToolCallingModel: arkModel,
		ToolsConfig: compose.ToolsNodeConfig{
			Tools: []tool.BaseTool{bash, sre},
		},
		MaxStep: 200, // 增加最大步数限制，默认通常是 10-15
		// StreamToolCallChecker: toolCallChecker, // uncomment it to replace the default tool call checker with custom one
	})
	if err != nil {
		logs.Errorf("failed to create agent: %v", err)
		return
	}

	// if you want ping/pong, use Generate
	// msg, err := agent.Generate(ctx, []*schema.Message{
	// 	{
	// 		Role:    schema.User,
	// 		Content: "我在北京，给我推荐一些菜，需要有口味辣一点的菜，至少推荐有 2 家餐厅",
	// 	},
	// }, react.WithCallbacks(&myCallback{}))
	// if err != nil {
	// 	log.Printf("failed to generate: %v\n", err)
	// 	return
	// }
	// fmt.Println(msg.String())

	// If you want to use ark caching in react, call ark.WithCache()
	//cacheOption := &ark.CacheOption{
	//	APIType: ark.ResponsesAPI,
	//	SessionCache: &ark.SessionCacheConfig{
	//		EnableCache: true,
	//		TTL:         3600,
	//	},
	//}

	opt := []agent.AgentOption{
		agent.WithComposeOptions(compose.WithCallbacks(&PrettyLoggerCallback{})), // 使用美观的 logger
		// agent.WithComposeOptions(compose.WithCallbacks(&LoggerCallback{})), // 原始 logger
		//react.WithChatModelOptions(ark.WithCache(cacheOption)),
	}

	/*	// Export graph and compile with mermaid (non-critical path)
		anyG	{
				anyG, opts := rAgent.ExportGraph()
				gen := visualize.NewMermaidGenerator("flow/agent/react")
				g := compose.NewGraph[[]*schema.Message, *schema.Message]()
				_ = g.AddGraphNode("react_agent", anyG, opts...)
				_ = g.AddEdge(compose.START, "react_agent")
				_ = g.AddEdge("react_agent", compose.END)
				_, _ = g.Compile(context.Background(), compose.WithGraphCompileCallbacks(gen))
			}*/

	// 使用 Generate 方法确保工具调用被正确执行（而不是流式处理）
	// 流式处理可能导致工具调用参数不完整
	msg, err := rAgent.Generate(ctx, []*schema.Message{
		{
			Role:    schema.System,
			Content: content,
		},
		{
			Role:    schema.User,
			Content: "分析/home/linuxbrew/pcaps/ 目录下的文件，告诉我tcp udp流量的数量及其元数据",
		},
	}, opt...)
	if err != nil {
		logs.Errorf("failed to generate: %v", err)
		return
	}

	logs.Infof("\n\n===== result =====\n\n")
	logs.Infof("%s\n", msg.Content)
	time.Sleep(2 * time.Second)
}

type LoggerCallback struct {
	callbacks.HandlerBuilder // 可以用 callbacks.HandlerBuilder 来辅助实现 callback
}

func (cb *LoggerCallback) OnStart(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
	fmt.Println("==================")
	inputStr, _ := json.MarshalIndent(input, "", "  ")
	fmt.Printf("[OnStart] %s\n", string(inputStr))
	return ctx
}

func (cb *LoggerCallback) OnEnd(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
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
	callbacks.HandlerBuilder
	step int
}

func (cb *PrettyLoggerCallback) OnStart(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
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
			if len(content) > 80 {
				content = content[:80] + "..."
			}
			fmt.Printf("║   %d. %s\n", i+1, content)
		}
	}
	fmt.Printf("╚════════════════════════════════════════════════════════════╝\n")
	return ctx
}

func (cb *PrettyLoggerCallback) OnEnd(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
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


/*
 * Copyright 2025 CloudWeGo Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */



var (
	bashToolInfo = &schema.ToolInfo{
		Name: "bash",
		Desc: `Run commands in a bash shell
* When invoking this tool, the contents of the \"command\" parameter does NOT need to be XML-escaped.
* You don't have access to the internet via this tool.
* You do have access to a mirror of common linux and python packages via apt and pip.
* State is persistent across command calls and discussions with the user.
* To inspect a particular line range of a file, e.g. lines 10-25, try 'sed -n 10,25p /path/to/the/file'.
* Please avoid commands that may produce a very large amount of output.
* Please run long lived commands in the background, e.g. 'sleep 10 &' or start a server in the background.`,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"command": {
				Type:     "string",
				Desc:     "The command to execute",
				Required: true,
			},
		}),
	}
)

func NewBashTool(op commandline.Operator) tool.InvokableTool {
	return &bashTool{op: op}
}

type bashTool struct {
	op commandline.Operator
}

func (b *bashTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return bashToolInfo, nil
}

type shellInput struct {
	Command string `json:"command"`
}

func (b *bashTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	input := &shellInput{}
	err := json.Unmarshal([]byte(argumentsInJSON), input)
	if err != nil {
		return "", err
	}
	if len(input.Command) == 0 {
		return "command cannot be empty", nil
	}
	o := tool.GetImplSpecificOptions(&options{b.op}, opts...)
	cmd, err := o.op.RunCommand(ctx, []string{"bash", "-c", input.Command})
	if err != nil {
		if strings.HasPrefix(err.Error(), "internal error") {
			return err.Error(), nil
		}
		return "", err
	}
	return FormatCommandOutput(cmd), nil
}

type options struct {
	op commandline.Operator
}

func FormatCommandOutput(output *commandline.CommandOutput) string {
	return fmt.Sprintf("---\nstdout:%v\n---\nstderr:%v\n---", output.Stdout, output.Stderr)
}

