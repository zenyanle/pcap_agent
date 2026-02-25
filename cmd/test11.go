package main

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/compose"
)

// 1. 定义状态和迭代结果（与之前一致）
type LoopState struct {
	Queue   []string
	Results []string
}

type IteratorResult struct {
	Done bool
	Data string
}

// 2. 升级您的封装结构，加入 State 的泛型以完美契合 Eino LocalState
type BundleWithHook[I any, O any, S any] struct {
	Name     string
	Runner   compose.InvokableRunnable[I, O]
	PreHook  func(ctx context.Context, in I, state *S) (I, error)
	PostHook func(ctx context.Context, out O, state *S) (O, error)
}

// ==========================================
// 节点定义区 (采用您的封装方式)
// ==========================================

// PopNodeBundleFunc: 负责从队列中取出下一个元素
func PopNodeBundleFunc() BundleWithHook[any, IteratorResult, LoopState] {
	runner := compose.InvokableLambda(func(ctx context.Context, in any) (IteratorResult, error) {
		// Runner 保持纯粹，只负责透传 Hook 处理好的数据
		return in.(IteratorResult), nil
	})

	preHook := func(ctx context.Context, in any, state *LoopState) (any, error) {
		if len(state.Queue) == 0 {
			return IteratorResult{Done: true}, nil
		}
		item := state.Queue[0]
		state.Queue = state.Queue[1:]
		return IteratorResult{Done: false, Data: item}, nil
	}

	return BundleWithHook[any, IteratorResult, LoopState]{
		Runner:  runner,
		PreHook: preHook, // 这里我们用 PreHook 来操纵状态和输入
		Name:    "PopNode-Iterator",
	}
}

// ProcessNodeBundleFunc: 真正的业务处理节点
// 注意：为了演示方便，这里 I 和 O 都是 string
func ProcessNodeBundleFunc() BundleWithHook[string, string, LoopState] {
	runner := compose.InvokableLambda(func(ctx context.Context, in string) (out string, err error) {
		if in == "" {
			return "", fmt.Errorf("input is empty")
		}
		// 您的核心自定义逻辑
		return in + "_processed", nil
	})

	postHook := func(ctx context.Context, out string, state *LoopState) (string, error) {
		// 业务处理完后，将结果收集到 State 中
		state.Results = append(state.Results, out)
		return out, nil
	}

	return BundleWithHook[string, string, LoopState]{
		Runner:   runner,
		PostHook: postHook,
		Name:     "ProcessNode-BusinessLogic",
	}
}

// ==========================================
// 图编排区 (主流程变得极其清爽)
// ==========================================

func BuildGraphWithBundles() (compose.Runnable[[]string, []string], error) {
	g := compose.NewGraph[[]string, []string](compose.WithGenLocalState(func(ctx context.Context) *LoopState {
		return &LoopState{Queue: make([]string, 0), Results: make([]string, 0)}
	}))

	// 1. 实例化 Bundles
	popBundle := PopNodeBundleFunc()
	processBundle := ProcessNodeBundleFunc()

	// 2. 注册节点 (提取 Bundle 中的内容)
	// 注册 PopNode
	optsPop := []compose.GraphNodeOption{compose.WithNodeName(popBundle.Name)}
	if popBundle.PreHook != nil {
		optsPop = append(optsPop, compose.WithStatePreHandler(popBundle.PreHook))
	}
	g.AddLambdaNode(popBundle.Name, popBundle.Runner, optsPop...)

	// 注册 ProcessNode
	optsProcess := []compose.GraphNodeOption{compose.WithNodeName(processBundle.Name)}
	if processBundle.PostHook != nil {
		optsProcess = append(optsProcess, compose.WithStatePostHandler(processBundle.PostHook))
	}
	g.AddLambdaNode(processBundle.Name, processBundle.Runner, optsProcess...)

	// ... 其他节点注册与 AddEdge/AddBranch 连线逻辑同上一回答 ...
	// g.AddEdge(popBundle.Name, processBundle.Name) 等

	return g.Compile(context.Background())
}
