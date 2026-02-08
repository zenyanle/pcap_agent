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
	"log"
	"os"
	"pcap_agent/internal/prompts"
	"pcap_agent/internal/tools"
	"pcap_agent/internal/virtual_env"
	"time"

	"github.com/cloudwego/eino-ext/components/tool/commandline"
	"github.com/cloudwego/eino-ext/components/tool/commandline/sandbox"

	"pcap_agent/pkg/logger"

	"github.com/cloudwego/eino-ext/components/model/openai"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
)

func main() {

	ctx := context.Background()

	// 加载 prompts
	promptMap, err := prompts.GetPrompts()
	if err != nil {
		logger.Fatalf("failed to load prompts: %v", err)
	}

	op, err := virtual_env.GetOperator(ctx)
	if err != nil {
		logger.Fatalf("op create errot")
	}
	defer func() {
		if entity, ok := op.(*sandbox.DockerSandbox); ok {
			entity.Cleanup(ctx)
		}
	}()

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
		logger.Errorf("failed to create chat model: %v", err)
		return
	}

	bash := tools.NewBashTool(op)

	sre, err := commandline.NewStrReplaceEditor(ctx, &commandline.EditorConfig{Operator: op})
	if err != nil {
		log.Fatal(err)
	}

	rAgent, err := react.NewAgent(ctx, &react.AgentConfig{
		ToolCallingModel: arkModel,
		ToolsConfig: compose.ToolsNodeConfig{
			Tools: []tool.BaseTool{bash, sre},
		},
		MaxStep: 200, // 增加最大步数限制，默认通常是 10-15
		// StreamToolCallChecker: toolCallChecker, // uncomment it to replace the default tool call checker with custom one
	})
	if err != nil {
		logger.Errorf("failed to create agent: %v", err)
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

	opt := []agent.AgentOption{
		//agent.WithComposeOptions(compose.WithCallbacks(&logger.PrettyLoggerCallback{})), // 使用美观的 logger
		agent.WithComposeOptions(compose.WithCallbacks(&logger.LoggerCallback{})), // 原始 logger
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
			Content: promptMap["analyzer_introduction"],
		},
		{
			Role:    schema.User,
			Content: "分析/home/linuxbrew/pcaps/ 目录下的文件，告诉我tcp udp流量的数量及其元数据",
		},
	}, opt...)
	if err != nil {
		logger.Errorf("failed to generate: %v", err)
		return
	}

	logger.Infof("\n\n===== result =====\n\n")
	logger.Infof("%s\n", msg.Content)
	time.Sleep(2 * time.Second)
}
