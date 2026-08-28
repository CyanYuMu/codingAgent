package runtime

import (
	"os"
	"regexp"
	"strings"
)

// nonInteractiveEnv 返回非交互式环境变量（在进程环境基础上做硬化）。
// 禁用分页器/编辑器/凭据提示，让子进程不因等待交互而卡住。
func nonInteractiveEnv() []string {
	overrides := map[string]string{
		"PAGER":               "cat",
		"EDITOR":              "true",
		"GIT_EDITOR":          "true",
		"GIT_TERMINAL_PROMPT": "0",
		"TERM":                "dumb",
		"NO_COLOR":            "1",
		"CI":                  "true",
	}
	envMap := map[string]string{}
	for _, e := range os.Environ() {
		k, v, _ := strings.Cut(e, "=")
		envMap[k] = v
	}
	for k, v := range overrides {
		envMap[k] = v
	}
	out := make([]string, 0, len(envMap))
	for k, v := range envMap {
		out = append(out, k+"="+v)
	}
	return out
}

// secretEnvRe 密钥类环境变量（大小写不敏感）：bash 子进程拿不到 API key/token。
var secretEnvRe = regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password|passwd|credential|private[_-]?key|access[_-]?key)`)

// SanitizeEnv 过滤密钥类环境变量（保留其余全部）。
// 白名单会误伤真实工作流（GOPROXY/GIT_CONFIG_*/NPM_CONFIG_*），只剔密钥类与 Claude Code 行为一致。
func SanitizeEnv(base []string) []string {
	out := make([]string, 0, len(base))
	for _, e := range base {
		k, _, _ := strings.Cut(e, "=")
		if secretEnvRe.MatchString(k) {
			continue
		}
		out = append(out, e)
	}
	return out
}
