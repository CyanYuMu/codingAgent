package model

import "strings"

// 溢出错误的特征串（各 provider 的措辞不同，按子串匹配）。
var overflowMarkers = []string{
	"context_length_exceeded", "maximum context length", "context length", "prompt is too long",
	"too many tokens", "exceed context limit", "exceeds the context", "input length",
	"context window", "request too large", "input is too long",
}

// 可重试（瞬时）错误的特征串：限流 / 5xx / 网络。
var retryMarkers = []string{
	"429", "too many requests", "rate limit", "ratelimit", "500", "502", "503", "504",
	"overloaded", "server error", "internal error", "service unavailable", "timeout", "timed out",
	"connection reset", "connection refused", "broken pipe", "unexpected eof", "temporarily unavailable",
	"try again",
}

// IsContextOverflow 判断模型错误是否为上下文溢出（走压缩恢复通道，绝不重试）。
func IsContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, m := range overflowMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// IsRetryable 判断是否为可重试的瞬时错误（限流/5xx/网络）。溢出不可重试。
func IsRetryable(err error) bool {
	if err == nil || IsContextOverflow(err) {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, m := range retryMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
