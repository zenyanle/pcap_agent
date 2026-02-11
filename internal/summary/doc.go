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

// Package conversationsummary provides a middleware that automatically compresses long conversation histories
// in multi-turn LLM agents using intelligent summarization.
//
// Overview:
//
// When a multi-turn conversation accumulates too many messages, it can exceed the model's token limit,
// causing performance degradation or errors. This middleware automatically detects when the conversation
// history exceeds a configurable token threshold and summarizes the older messages while preserving
// recent interactions.
//
// Key Features:
//
//   - Token-based summarization triggering: Monitors total token count of the conversation
//   - Intelligent message grouping: Keeps tool calls and responses paired together
//   - Configurable retention: Reserves token budget for recent messages to maintain context
//   - LLM-driven compression: Uses a language model to generate semantic summaries
//   - Customizable token counter: Support for different tokenization strategies
//
// Basic Usage:
//
//	ctx := context.Background()
//	model := openai.NewChatModel(ctx, conf)
//
//	// Create middleware with defaults
//	middleware, err := conversationsummary.NewWithDefaults(ctx, model)
//	if err != nil {
//		log.Fatal(err)
//	}
//
//	// Use in agent
//	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
//		Middlewares: []adk.AgentMiddleware{middleware},
//		// ... other config
//	})
//
// Advanced Configuration:
//
//	middleware, err := conversationsummary.New(ctx, &conversationsummary.Config{
//		Model:                      model,
//		MaxTokensBeforeSummary:     100 * 1024,  // Trigger at 100K tokens
//		MaxTokensForRecentMessages: 20 * 1024,   // Keep 20K tokens of recent messages
//		Counter:                    customCounter, // Optional: custom token counter
//		SystemPrompt:               customPrompt, // Optional: custom summarization prompt
//	})
//
// How It Works:
//
// 1. Message Monitoring: The middleware intercepts messages before they're sent to the model
// 2. Token Counting: Calculates total token count of all conversation messages
// 3. Threshold Check: If total tokens exceed MaxTokensBeforeSummary, triggers summarization
// 4. Message Partitioning: Organizes messages into logical blocks (system, user, tool calls)
// 5. Budget Allocation: From newest to oldest, allocates token budget for recent messages
// 6. LLM Summarization: Older messages are summarized using a language model
// 7. Context Compression: Replaces older messages with a single summary while keeping recent context
//
// Message Structure After Summarization:
//
//	[System Prompt] + [User Instructions] + [Summary] + [Recent Messages]
//
// This ensures the model has access to:
//   - System context (unchanged)
//   - User intent (unchanged)
//   - Compressed history (summary)
//   - Recent interactions (full detail)
//
// Token Counting:
//
// By default, uses the cl100k_base encoding (OpenAI's encoding) via tiktoken-go.
// You can provide a custom TokenCounter function to support other tokenization schemes
// or integrate with different model providers.
//
// Limitations:
//
//   - Summarization itself incurs token cost (cost of calling the summarizer model)
//   - May lose fine-grained details in very long conversations
//   - Performance depends on summarizer model quality
//   - Works best with structured conversations (e.g., agents with clear turn boundaries)
package conversationsummary
