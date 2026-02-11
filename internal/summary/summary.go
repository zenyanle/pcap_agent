package conversationsummary

import (
	"context"
	"errors"
	"fmt"
	"pcap_agent/pkg/logger"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/prompt"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

// New creates an AgentMiddleware that compacts long conversation history
// into a single summary message when the token threshold is exceeded.
// The summarizer chain is: ChatTemplate(SystemPrompt) -> ChatModel(Model).
// It applies defaults for token budgets and allows a custom Counter.
func New(ctx context.Context, cfg *Config) (*SummaryMiddleware, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	systemPrompt := cfg.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = PromptOfSummary
	}
	maxBefore := cfg.GetMaxTokensBeforeSummary()
	maxRecent := cfg.GetMaxTokensForRecentMessages()

	tpl := prompt.FromMessages(schema.FString,
		schema.SystemMessage(systemPrompt),
		schema.UserMessage("summarize 'older_messages': "))

	summarizer, err := compose.NewChain[map[string]any, *schema.Message]().
		AppendChatTemplate(tpl).
		AppendChatModel(cfg.Model).
		Compile(ctx, compose.WithGraphName("ConversationSummarizer"))
	if err != nil {
		return nil, fmt.Errorf("compile summarizer failed, err=%w", err)
	}

	counter := cfg.Counter
	if counter == nil {
		counter = defaultCounterToken
	}

	sm := &SummaryMiddleware{
		counter:    counter,
		maxBefore:  maxBefore,
		maxRecent:  maxRecent,
		summarizer: summarizer,
	}
	return sm, nil
}

const summaryMessageFlag = "_agent_middleware_summary_message"

type SummaryMiddleware struct {
	counter    TokenCounter
	maxBefore  int
	maxRecent  int
	summarizer compose.Runnable[map[string]any, *schema.Message]
}

func (s *SummaryMiddleware) MessageModifier(ctx context.Context, input []*schema.Message) []*schema.Message {

	messages := input
	msgsToken, err := s.counter(ctx, messages)
	if err != nil {
		logger.Fatalf("compress error")
		return input
	}
	if len(messages) != len(msgsToken) {
		logger.Fatalf("compress error")
		return input
	}

	var total int64
	for _, t := range msgsToken {
		total += t
	}
	// Trigger summarization only when exceeding threshold
	if total <= int64(s.maxBefore) {
		return input
	}

	// Build blocks with user-messages, summary-message, tool-call pairings
	type block struct {
		msgs   []*schema.Message
		tokens int64
	}
	idx := 0

	systemBlock := block{}
	if idx < len(messages) {
		m := messages[idx]
		if m != nil && m.Role == schema.System {
			systemBlock.msgs = append(systemBlock.msgs, m)
			systemBlock.tokens += msgsToken[idx]
			idx++
		}
	}
	userBlock := block{}
	for idx < len(messages) {
		m := messages[idx]
		if m == nil {
			idx++
			continue
		}
		if m.Role != schema.User {
			break
		}
		userBlock.msgs = append(userBlock.msgs, m)
		userBlock.tokens += msgsToken[idx]
		idx++
	}
	summaryBlock := block{}
	if idx < len(messages) {
		m := messages[idx]
		if m != nil && m.Role == schema.Assistant {
			if _, ok := m.Extra[summaryMessageFlag]; ok {
				summaryBlock.msgs = append(summaryBlock.msgs, m)
				summaryBlock.tokens += msgsToken[idx]
				idx++
			}
		}
	}

	toolBlocks := make([]block, 0)
	for i := idx; i < len(messages); i++ {
		m := messages[i]
		if m == nil {
			continue
		}
		if m.Role == schema.Assistant && len(m.ToolCalls) > 0 {
			b := block{msgs: []*schema.Message{m}, tokens: msgsToken[i]}
			// Collect subsequent tool messages matching any tool call id
			callIDs := make(map[string]struct{}, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				callIDs[tc.ID] = struct{}{}
			}
			j := i + 1
			for j < len(messages) {
				nm := messages[j]
				if nm == nil || nm.Role != schema.Tool {
					break
				}
				// Match by ToolCallID when available; if empty, include but keep boundary
				if nm.ToolCallID == "" {
					b.msgs = append(b.msgs, nm)
					b.tokens += msgsToken[j]
				} else {
					if _, ok := callIDs[nm.ToolCallID]; !ok {
						// Tool message not belonging to this assistant call -> end pairing
						break
					}
					b.msgs = append(b.msgs, nm)
					b.tokens += msgsToken[j]
				}
				j++
			}
			toolBlocks = append(toolBlocks, b)
			i = j - 1
			continue
		}
		toolBlocks = append(toolBlocks, block{msgs: []*schema.Message{m}, tokens: msgsToken[i]})
	}

	// Split into recent and older within token budget, from newest to oldest
	var recentBlocks []block
	var olderBlocks []block
	var recentTokens int64
	for i := len(toolBlocks) - 1; i >= 0; i-- {
		b := toolBlocks[i]
		if recentTokens+b.tokens > int64(s.maxRecent) {
			olderBlocks = append([]block{b}, olderBlocks...)
			continue
		}
		recentBlocks = append([]block{b}, recentBlocks...)
		recentTokens += b.tokens
	}

	joinBlocks := func(bs []block) string {
		var sb strings.Builder
		for _, b := range bs {
			for _, m := range b.msgs {
				sb.WriteString(renderMsg(m))
				sb.WriteString("\n")
			}
		}
		return sb.String()
	}

	olderText := joinBlocks(olderBlocks)
	recentText := joinBlocks(recentBlocks)

	msg, err := s.summarizer.Invoke(ctx, map[string]any{
		"system_prompt":    joinBlocks([]block{systemBlock}),
		"user_messages":    joinBlocks([]block{userBlock}),
		"previous_summary": joinBlocks([]block{summaryBlock}),
		"older_messages":   olderText,
		"recent_messages":  recentText,
	})
	if err != nil {
		logger.Fatalf("compress error")
		return input
	}

	summaryMsg := schema.AssistantMessage(msg.Content, nil)
	summaryMsg.Name = "summary"
	summaryMsg.Extra = map[string]any{
		summaryMessageFlag: true,
	}

	// Build new state: prepend summary message, keep recent messages
	newMessages := make([]*schema.Message, 0, len(messages))
	newMessages = append(newMessages, systemBlock.msgs...)
	newMessages = append(newMessages, userBlock.msgs...)
	newMessages = append(newMessages, summaryMsg)
	for _, b := range recentBlocks {
		newMessages = append(newMessages, b.msgs...)
	}

	return newMessages
}

func (s *SummaryMiddleware) BeforeModel(ctx context.Context, state *adk.ChatModelAgentState) (err error) {
	if state == nil || len(state.Messages) == 0 {
		return nil
	}

	messages := state.Messages
	msgsToken, err := s.counter(ctx, messages)
	if err != nil {
		return fmt.Errorf("count token failed, err=%w", err)
	}
	if len(messages) != len(msgsToken) {
		return fmt.Errorf("token count mismatch, msgNum=%d, tokenCountNum=%d", len(messages), len(msgsToken))
	}

	var total int64
	for _, t := range msgsToken {
		total += t
	}
	// Trigger summarization only when exceeding threshold
	if total <= int64(s.maxBefore) {
		return nil
	}

	// Build blocks with user-messages, summary-message, tool-call pairings
	type block struct {
		msgs   []*schema.Message
		tokens int64
	}
	idx := 0

	systemBlock := block{}
	if idx < len(messages) {
		m := messages[idx]
		if m != nil && m.Role == schema.System {
			systemBlock.msgs = append(systemBlock.msgs, m)
			systemBlock.tokens += msgsToken[idx]
			idx++
		}
	}
	userBlock := block{}
	for idx < len(messages) {
		m := messages[idx]
		if m == nil {
			idx++
			continue
		}
		if m.Role != schema.User {
			break
		}
		userBlock.msgs = append(userBlock.msgs, m)
		userBlock.tokens += msgsToken[idx]
		idx++
	}
	summaryBlock := block{}
	if idx < len(messages) {
		m := messages[idx]
		if m != nil && m.Role == schema.Assistant {
			if _, ok := m.Extra[summaryMessageFlag]; ok {
				summaryBlock.msgs = append(summaryBlock.msgs, m)
				summaryBlock.tokens += msgsToken[idx]
				idx++
			}
		}
	}

	toolBlocks := make([]block, 0)
	for i := idx; i < len(messages); i++ {
		m := messages[i]
		if m == nil {
			continue
		}
		if m.Role == schema.Assistant && len(m.ToolCalls) > 0 {
			b := block{msgs: []*schema.Message{m}, tokens: msgsToken[i]}
			// Collect subsequent tool messages matching any tool call id
			callIDs := make(map[string]struct{}, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				callIDs[tc.ID] = struct{}{}
			}
			j := i + 1
			for j < len(messages) {
				nm := messages[j]
				if nm == nil || nm.Role != schema.Tool {
					break
				}
				// Match by ToolCallID when available; if empty, include but keep boundary
				if nm.ToolCallID == "" {
					b.msgs = append(b.msgs, nm)
					b.tokens += msgsToken[j]
				} else {
					if _, ok := callIDs[nm.ToolCallID]; !ok {
						// Tool message not belonging to this assistant call -> end pairing
						break
					}
					b.msgs = append(b.msgs, nm)
					b.tokens += msgsToken[j]
				}
				j++
			}
			toolBlocks = append(toolBlocks, b)
			i = j - 1
			continue
		}
		toolBlocks = append(toolBlocks, block{msgs: []*schema.Message{m}, tokens: msgsToken[i]})
	}

	// Split into recent and older within token budget, from newest to oldest
	var recentBlocks []block
	var olderBlocks []block
	var recentTokens int64
	for i := len(toolBlocks) - 1; i >= 0; i-- {
		b := toolBlocks[i]
		if recentTokens+b.tokens > int64(s.maxRecent) {
			olderBlocks = append([]block{b}, olderBlocks...)
			continue
		}
		recentBlocks = append([]block{b}, recentBlocks...)
		recentTokens += b.tokens
	}

	joinBlocks := func(bs []block) string {
		var sb strings.Builder
		for _, b := range bs {
			for _, m := range b.msgs {
				sb.WriteString(renderMsg(m))
				sb.WriteString("\n")
			}
		}
		return sb.String()
	}

	olderText := joinBlocks(olderBlocks)
	recentText := joinBlocks(recentBlocks)

	msg, err := s.summarizer.Invoke(ctx, map[string]any{
		"system_prompt":    joinBlocks([]block{systemBlock}),
		"user_messages":    joinBlocks([]block{userBlock}),
		"previous_summary": joinBlocks([]block{summaryBlock}),
		"older_messages":   olderText,
		"recent_messages":  recentText,
	})
	if err != nil {
		return fmt.Errorf("summarize failed, err=%w", err)
	}

	summaryMsg := schema.AssistantMessage(msg.Content, nil)
	summaryMsg.Name = "summary"
	summaryMsg.Extra = map[string]any{
		summaryMessageFlag: true,
	}

	// Build new state: prepend summary message, keep recent messages
	newMessages := make([]*schema.Message, 0, len(messages))
	newMessages = append(newMessages, systemBlock.msgs...)
	newMessages = append(newMessages, userBlock.msgs...)
	newMessages = append(newMessages, summaryMsg)
	for _, b := range recentBlocks {
		newMessages = append(newMessages, b.msgs...)
	}

	state.Messages = newMessages
	return nil
}

// Render messages into strings
func renderMsg(m *schema.Message) string {
	if m == nil {
		return ""
	}
	var sb strings.Builder
	if m.Role == schema.Tool {
		if m.ToolName != "" {
			sb.WriteString("[tool:")
			sb.WriteString(m.ToolName)
			sb.WriteString("]\n")
		} else {
			sb.WriteString("[tool]\n")
		}
	} else {
		sb.WriteString("[")
		sb.WriteString(string(m.Role))
		sb.WriteString("]\n")
	}
	if m.Content != "" {
		sb.WriteString(m.Content)
		sb.WriteString("\n")
	}
	if m.Role == schema.Assistant && len(m.ToolCalls) > 0 {
		for _, tc := range m.ToolCalls {
			if tc.Function.Name != "" {
				sb.WriteString("tool_call: ")
				sb.WriteString(tc.Function.Name)
				sb.WriteString("\n")
			}
			if tc.Function.Arguments != "" {
				sb.WriteString("args: ")
				sb.WriteString(tc.Function.Arguments)
				sb.WriteString("\n")
			}
		}
	}
	for _, part := range m.UserInputMultiContent {
		if part.Type == schema.ChatMessagePartTypeText && part.Text != "" {
			sb.WriteString(part.Text)
			sb.WriteString("\n")
		}
	}
	for _, part := range m.AssistantGenMultiContent {
		if part.Type == schema.ChatMessagePartTypeText && part.Text != "" {
			sb.WriteString(part.Text)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

var (
	// ErrConfigNil is returned when the config is nil.
	ErrConfigNil = errors.New("config is nil")

	// ErrModelRequired is returned when the model is not provided in config.
	ErrModelRequired = errors.New("model is required in config")

	// ErrTokenCountMismatch is returned when token counts don't match message count.
	ErrTokenCountMismatch = errors.New("token count mismatch with message count")
)
