package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pcap_agent/internal/events"
	"pcap_agent/internal/executor"
	"pcap_agent/internal/planner"
	"pcap_agent/internal/session"
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
)

func main() {
	// --- Flags ---
	pcapFlag := flag.String("pcap", "", "Local PCAP file path (required for new session)")
	sessionID := flag.String("session", "", "Resume an existing session by ID")
	dbPath := flag.String("db", "pcap_agent.db", "SQLite database path")
	flag.Parse()

	ctx := context.Background()

	// --- SQLite store ---
	store, err := session.OpenStore(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// --- Event system ---
	emitter := events.NewChannelEmitter(512)
	defer emitter.Close()

	// Event printer goroutine (CLI consumer)
	go func() {
		ch := emitter.Subscribe()
		for ev := range ch {
			printEvent(ev)
		}
	}()

	// --- Docker sandbox ---
	op, err := virtual_env.GetOperator(ctx)
	if err != nil {
		log.Fatalf("create sandbox operator: %v", err)
	}
	defer func() {
		if entity, ok := op.(*sandbox.DockerSandbox); ok {
			entity.Cleanup(ctx)
		}
	}()

	// --- Copy PCAP into container (if starting new session) ---
	var containerPcapPath string
	if *pcapFlag != "" {
		dockerSandbox, ok := op.(*sandbox.DockerSandbox)
		if !ok {
			log.Fatalf("operator is not DockerSandbox, cannot copy PCAP")
		}
		containerPcapPath = "/home/linuxbrew/pcaps/" + filepath.Base(*pcapFlag)
		if err := virtual_env.CopyFileToContainer(ctx, dockerSandbox, *pcapFlag, containerPcapPath); err != nil {
			log.Fatalf("copy pcap to container: %v", err)
		}
		fmt.Printf("Copied %s → container:%s\n", *pcapFlag, containerPcapPath)
	}

	// --- LLM ---
	arkApiKey := os.Getenv("ARK_API_KEY")
	arkModelName := os.Getenv("ARK_MODEL_NAME")
	arkBaseUrl := os.Getenv("ARK_BASE_URL")

	arkModel, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		APIKey:  arkApiKey,
		Model:   arkModelName,
		BaseURL: arkBaseUrl,
	})
	if err != nil {
		log.Fatalf("create chat model: %v", err)
	}

	// --- Tools ---
	bash := tools.NewBashTool(op)
	sre, err := commandline.NewStrReplaceEditor(ctx, &commandline.EditorConfig{Operator: op})
	if err != nil {
		log.Fatalf("create str_replace_editor: %v", err)
	}

	// --- Summarization middleware ---
	sumMW, err := conversationsummary.New(ctx, &conversationsummary.Config{
		Model:                      arkModel,
		MaxTokensBeforeSummary:     64 * 1024,
		MaxTokensForRecentMessages: 20 * 1024,
	})
	if err != nil {
		log.Fatalf("create summarization middleware: %v", err)
	}

	// --- ReAct agent ---
	rAgent, err := react.NewAgent(ctx, &react.AgentConfig{
		MessageRewriter:  sumMW.MessageModifier,
		ToolCallingModel: arkModel,
		ToolsConfig: compose.ToolsNodeConfig{
			Tools: []tool.BaseTool{bash, tools.WrapToolSafe(sre)},
		},
		MaxStep: 200,
	})
	if err != nil {
		log.Fatalf("create react agent: %v", err)
	}

	// --- Planner & Executor ---
	p, err := planner.NewPlanner(ctx, rAgent, emitter)
	if err != nil {
		log.Fatalf("create planner: %v", err)
	}
	exec := executor.NewExecutor(rAgent, emitter)

	// --- Session ---
	var sess *session.Session
	if *sessionID != "" {
		sess, err = session.ResumeSession(store, emitter, *sessionID)
		if err != nil {
			log.Fatalf("resume session %s: %v", *sessionID, err)
		}
		containerPcapPath = sess.PcapPath // use the stored container path
		fmt.Printf("Resumed session %s (pcap: %s, round: %d)\n", sess.ID, sess.PcapPath, sess.RoundNum)
	} else {
		if *pcapFlag == "" {
			log.Fatalf("--pcap is required for new sessions")
		}
		sess, err = session.NewSession(store, emitter, containerPcapPath)
		if err != nil {
			log.Fatalf("create session: %v", err)
		}
		fmt.Printf("New session %s (pcap: %s)\n", sess.ID, sess.PcapPath)
	}

	// --- REPL ---
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	fmt.Println("\nPCAP Analysis Agent - Multi-turn REPL")
	fmt.Println("Type your query and press Enter. Type 'quit' or 'exit' to stop.\n")

	for {
		fmt.Printf("[round %d] > ", sess.RoundNum+1)
		if !scanner.Scan() {
			break
		}
		query := strings.TrimSpace(scanner.Text())
		if query == "" {
			continue
		}
		if query == "quit" || query == "exit" {
			fmt.Println("Goodbye.")
			break
		}

		// Load session history for multi-round context
		history, err := sess.History()
		if err != nil {
			logger.Errorf("load session history: %v", err)
		}

		// --- Plan ---
		fmt.Println("\n--- Planning ---")
		plan, err := p.Run(ctx, planner.PlannerInput{
			UserQuery: query,
			PcapPath:  containerPcapPath,
			History:   history,
		})
		if err != nil {
			logger.Errorf("planner failed: %v", err)
			fmt.Printf("Planner error: %v\n\n", err)
			continue
		}

		fmt.Printf("\nPlan: %d steps\n", len(plan.Steps))
		for _, s := range plan.Steps {
			fmt.Printf("  Step %d: %s\n", s.StepID, s.Intent)
		}

		// --- Execute ---
		fmt.Println("\n--- Executing ---")
		result, err := exec.Run(ctx, plan, query, containerPcapPath)
		if err != nil {
			logger.Errorf("executor failed: %v", err)
			fmt.Printf("Executor error: %v\n\n", err)
			continue
		}

		// --- Save round ---
		if err := sess.SaveRound(query, plan, result.Report, result.Findings, result.OperationLog); err != nil {
			logger.Errorf("save round: %v", err)
		}

		// --- Print report ---
		fmt.Println("\n===== REPORT =====")
		fmt.Println(result.Report)
		fmt.Println("==================\n")
	}

	// Allow events to flush
	time.Sleep(500 * time.Millisecond)
}

// printEvent formats and prints an event to the terminal.
func printEvent(ev events.Event) {
	switch ev.Type {
	case events.TypePlanCreated:
		fmt.Printf("[EVENT] Plan created\n")
	case events.TypeStepStarted:
		var d events.StepStartedData
		_ = json.Unmarshal(ev.Data, &d)
		fmt.Printf("[EVENT] Step %d/%d started: %s\n", d.StepID, d.TotalSteps, d.Intent)
	case events.TypeStepFindings:
		fmt.Printf("[EVENT] Step findings captured\n")
	case events.TypeStepError:
		var d events.ErrorData
		_ = json.Unmarshal(ev.Data, &d)
		fmt.Printf("[EVENT] Step error: %s\n", d.Message)
	case events.TypeReportGenerated:
		fmt.Printf("[EVENT] Report generated\n")
	case events.TypeError:
		var d events.ErrorData
		_ = json.Unmarshal(ev.Data, &d)
		fmt.Printf("[EVENT] Error (%s): %s\n", d.Phase, d.Message)
	}
}
