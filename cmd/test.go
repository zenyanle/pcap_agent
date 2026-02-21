package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"pcap_agent/internal/orche"
	conversationsummary "pcap_agent/internal/summary"
	"pcap_agent/internal/tools"
	"pcap_agent/internal/virtual_env"
	"pcap_agent/pkg/logger"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino-ext/components/tool/commandline"
	"github.com/cloudwego/eino-ext/components/tool/commandline/sandbox"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/elastic/go-elasticsearch/v7"
)

func main() {
	ctx := context.Background()

	// ES client
	esCfg := elasticsearch.Config{
		Addresses: []string{"http://localhost:5080/api/default"},
		Username:  "root@example.com",
		Password:  "Complexpass#123",
	}
	esClient, err := elasticsearch.NewClient(esCfg)
	if err != nil {
		log.Fatalf("创建 ES 客户端失败: %s", err)
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

	/*	out, err := op.RunCommand(ctx, []string{"bash", "-c", "pcapchu-scripts init /home/linuxbrew/pcaps/test.pcapng"})
		if err != nil {
			logger.Fatalf("init error", err)
		}
		if out.ExitCode != 0 {
			logger.Warnf("init error", out.Stdout, out.ExitCode)
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
		logger.Errorf("failed to create chat model: %v", err)
		return
	}

	bash := tools.NewBashTool(op)

	sre, err := commandline.NewStrReplaceEditor(ctx, &commandline.EditorConfig{Operator: op})
	if err != nil {
		log.Fatal(err)
	}

	sumMW, err := conversationsummary.New(ctx, &conversationsummary.Config{
		Model:                      arkModel,
		MaxTokensBeforeSummary:     32 * 1024, // 32K tokens 时触发摘要压缩，防止内存膨胀
		MaxTokensForRecentMessages: 4 * 1024,  // 保留最近 4K tokens 的原始消息
	})
	if err != nil {
		logger.Fatalf("create summarization middleware failed, err=%v", err)
	}

	rAgent, err := react.NewAgent(ctx, &react.AgentConfig{
		MessageRewriter:  sumMW.MessageModifier,
		ToolCallingModel: arkModel,
		ToolsConfig: compose.ToolsNodeConfig{
			Tools: []tool.BaseTool{bash, sre},
		},
		MaxStep: 200, // 复杂分析任务需要较多工具调用轮次
		// StreamToolCallChecker: toolCallChecker, // uncomment it to replace the default tool call checker with custom one
	})
	if err != nil {
		logger.Errorf("failed to create agent: %v", err)
		return
	}

	r, err := orche.MakePlanner(rAgent, esClient)
	if err != nil {
		panic(err)
	}
	i, err := r.Invoke(ctx, map[string]any{
		"user_input": "分析capture.pcapng，这个用户在学习什么内容",
		//"thought": "这个用户访问了哪些网站，他的意图是什么",
	})
	if err != nil {
		//logger.Fatalf("invoke_1111", err)
		panic(err)
	}

	ret, err := orche.MakeExecutor(rAgent, i, esClient)
	if err != nil {
		panic(err)
	}
	fmt.Println(ret)

}
