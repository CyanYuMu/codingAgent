package main

// aiTextChunkMsg: AI 流式输出的单个正文文本块(Markdown 原文)。
// 由后台 OnAgentEvents 通过 program.Send 发给 TUI 主循环。
type aiTextChunkMsg struct {
	text string
}

// aiThinkingChunkMsg: AI 流式思考过程(Reasoning)的单个文本块。
// 到达后追加到当前思考缓冲，View 时渲染成灰色块。
type aiThinkingChunkMsg struct {
	text string
}
