package runtime

import (
	"os"
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
