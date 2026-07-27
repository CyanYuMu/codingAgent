package main

// aiTextChunkMsg: AI 流式输出的单个正文文本块。
//
// 由后台 OnAgentEvents 通过 program.Send 发给 TUI 主循环，
// TUI 的 Update 收到后追加到当前流式行。
// （思考过程 Reasoning 的展示留到阶段4，这里先只传正文。）
type aiTextChunkMsg struct {
	text string
}
