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

package main

import (
	"context"
	"fmt"
	"log"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/compose"

	"github.com/cloudwego/eino-examples/adk/common/model"
	"github.com/cloudwego/eino-examples/adk/common/prints"
	"pcap_agent/internal/summary"
	"pcap_agent/pkg/logger"
)

func main() {
	ctx := context.Background()

	// Create middleware with custom configuration
	sumMW, err := conversationsummary.New(ctx, &conversationsummary.Config{
		Model:                      model.NewChatModel(),
		MaxTokensBeforeSummary:     10 * 1024, // Trigger at 10K tokens for demo
		MaxTokensForRecentMessages: 2 * 1024,  // Keep 2K tokens of recent messages
	})
	if err != nil {
		logger.Fatalf("create summarization middleware failed, err=%v", err)
	}

	// Note: This is a simplified example. In production, you would:
	// 1. Implement proper tool.BaseTool interface
	// 2. Or use existing tools from eino library
	// 3. Use the middleware as shown above

	// For now, we'll create a basic agent just to demonstrate the middleware
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        "summary_agent",
		Description: "An agent with conversation summarization",
		Instruction: `You are a helpful assistant.
Provide detailed, comprehensive answers to questions.
When appropriate, break down complex topics into multiple parts.`,
		Model:       model.NewChatModel(),
		Middlewares: []adk.AgentMiddleware{sumMW},
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				// Add your tools here - they will trigger context compression
				// when conversation history grows
			},
		},
		MaxIterations: 20,
	})
	if err != nil {
		logger.Fatalf("create agent failed, err=%v", err)
	}

	// Run the agent
	runner := adk.NewRunner(ctx, adk.RunnerConfig{
		EnableStreaming: true,
		Agent:           agent,
	})

	// Request long content to trigger summarization
	query := `Generate a detailed summary of machine learning algorithms.
Cover supervised learning, unsupervised learning, and reinforcement learning.
For each category, provide at least 3-4 detailed algorithms.`

	fmt.Println("=== Conversation Summarization Example ===")
	fmt.Println("Query:", query)
	fmt.Println()
	fmt.Println("Running agent with automatic context compression...")
	fmt.Println()

	iter := runner.Query(ctx, query)

	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			log.Fatal(event.Err)
		}

		prints.Event(event)
	}

	fmt.Println()
	fmt.Println("=== Agent Completed ===")
	fmt.Println("The conversation summarization middleware automatically compressed")
	fmt.Println("the conversation history when it exceeded the token threshold.")
}
