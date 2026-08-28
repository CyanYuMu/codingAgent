package runtime

import (
	"regexp"
	"strings"
)

// Classify 把一条 shell 命令分类成（只读？，危险？，原因）。
// 纯函数：按 shell 词法把命令切成管道/逻辑段，逐段保守判定——
// 任一段命中危险模式即危险；全部段都命中只读白名单才算只读；
// 不认识的命令既不只读也不危险（调用方回落到 exec tier 询问）。
// 注意：危险判定先于只读（`echo x > /etc/passwd` 的 echo 只读，但重定向写敏感路径危险）。
func Classify(command string) (readOnly, dangerous bool, reason string) {
	// fork bomb 模式横跨切段符，必须对原始命令先查
	if dangForkRe.MatchString(command) {
		return false, true, "fork bomb"
	}
	segs := splitSegments(command)
	if r := dangerousPattern(segs); r != "" {
		return false, true, r
	}
	for _, seg := range segs {
		if !segmentReadOnly(seg) {
			return false, false, ""
		}
	}
	return true, false, ""
}

// splitSegments 按管道与逻辑连接符切段（引号内的 | 也会被切——保守方向，可接受）。
func splitSegments(command string) []string {
	fields := strings.FieldsFunc(command, func(r rune) bool {
		switch r {
		case '|', '&', ';', '\n':
			return true
		}
		return false
	})
	var out []string
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// dangerousPattern 任一段命中即返回原因（空 = 不危险）。
func dangerousPattern(segs []string) string {
	for _, seg := range segs {
		if !dangSegRe.MatchString(seg) {
			continue
		}
		groups := dangSegRe.FindStringSubmatch(seg)
		switch {
		case strings.Contains(groups[0], "sudo"):
			return "sudo 提权命令"
		case strings.Contains(groups[0], "mkfs"):
			return "mkfs 格式化磁盘"
		case dangReRe.MatchString(seg):
			return "shutdown/reboot 类关机命令"
		case dangDdRe.MatchString(seg):
			return "dd 直接写设备"
		case dangRmRe.MatchString(seg):
			return "rm 递归/强制删除"
		case dangChmodRe.MatchString(seg):
			return "递归修改系统目录权限"
		case dangFindRe.MatchString(seg):
			return "find -delete 批量删除"
		case dangKillRe.MatchString(seg):
			return "kill -9 -1 杀全部进程"
		case dangGitRe.MatchString(seg):
			return "git 不可逆操作（reset --hard / clean / push --force）"
		case dangForkRe.MatchString(seg):
			return "fork bomb"
		case dangRedirectRe.MatchString(seg):
			if t := redirectTarget(seg); t != "" && t != "/dev/null" {
				return "重定向写敏感路径 " + t
			}
		}
	}
	// 管道进解释器：curl/wget 段后面跟着 sh/bash/zsh 段
	seenNet := false
	for _, seg := range segs {
		if netFetchRe.MatchString(seg) {
			seenNet = true
		}
		if seenNet && interpRe.MatchString(seg) {
			return "curl|wget 输出直接进 shell 解释器"
		}
	}
	return ""
}

var (
	dangSegRe      = regexp.MustCompile(`sudo|mkfs|\b(shutdown|reboot|halt|poweroff)\b|\bdd\b[^|&;]*\bof=|\brm\b|\b(chmod|chown)\b|\bfind\b[^|&;]*\s-delete\b|\bkill\b\s+-9\s+-1|^git\s+(reset\s+--hard|clean|push)\b|:\(\)\s*\{|>>?\s*/`)
	dangReRe       = regexp.MustCompile(`\b(shutdown|reboot|halt|poweroff)\b`)
	dangDdRe       = regexp.MustCompile(`\bdd\b[^|&;]*\bof=/`)
	dangRmRe       = regexp.MustCompile(`\brm\b[^|&;\n]*\s(-[A-Za-z]*[rfR][A-Za-z]*|--recursive|--force)\b`)
	dangChmodRe    = regexp.MustCompile(`\b(chmod|chown)\b[^|&;\n]*\s(-R\b|--recursive)[^|&;\n]*\s/`)
	dangFindRe     = regexp.MustCompile(`\bfind\b[^|&;\n]*\s-delete\b`)
	dangKillRe     = regexp.MustCompile(`\bkill\b\s+-9\s+-1`)
	dangGitRe      = regexp.MustCompile(`^git\s+reset\s+--hard(\s|$)|^git\s+clean\b[^|&;\n]*\s-[A-Za-z]*[xX][A-Za-z]*(\s|$)|^git\s+push\b[^|&;\n]*\s(--force(\s|$)|-f(\s|$))`)
	dangForkRe     = regexp.MustCompile(`:\(\)\s*\{\s*:\|:&`)
	dangRedirectRe = regexp.MustCompile(`>>?\s*/[^\s;|&>]+`)
	netFetchRe     = regexp.MustCompile(`^\s*(curl|wget)\b`)
	interpRe       = regexp.MustCompile(`^\s*(sh|bash|zsh|dash|fish)\b`)
)

// redirectTarget 提取重定向目标路径（危险检查确认它不在白名单时使用）。
func redirectTarget(seg string) string {
	m := dangRedirectRe.FindString(seg)
	return strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(m, ">")), ">")
}

// readOnlyCommands 命令名白名单：不写文件、不执行副作用。
var readOnlyCommands = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "wc": true,
	"grep": true, "rg": true, "find": true, "du": true, "df": true, "pwd": true,
	"echo": true, "printf": true, "date": true, "env": true, "uname": true,
	"which": true, "whoami": true, "hostname": true, "id": true,
	"sort": true, "uniq": true, "cut": true, "tr": true, "file": true,
	"test": true, "true": true, "false": true, "basename": true, "dirname": true,
}

var (
	gitReadOnlySub = regexp.MustCompile(`^(status|log|diff|show|branch|tag|rev-parse|ls-files|ls-tree)(\s|$)`)
	gitStashRead   = regexp.MustCompile(`^stash(\s+(list|show))?(\s|$)`)
	gitRemoteRead  = regexp.MustCompile(`^remote(\s+(-v|--verbose))?(\s|$)`)
	goReadOnlySub  = regexp.MustCompile(`^(build|test|vet|list|env|version)(\s|$)`)
	goModRead      = regexp.MustCompile(`^mod\s+(download|verify|graph|why)(\s|$)`)
	goFmtList      = regexp.MustCompile(`^fmt\s+-l(\s|$)`)
	gofmtList      = regexp.MustCompile(`^-l(\s|$)`)
)

// segmentReadOnly 单段是否确定只读。
func segmentReadOnly(seg string) bool {
	seg = strings.TrimSpace(seg)
	if seg == "" {
		return false
	}
	fields := strings.Fields(seg)
	cmd := fields[0]
	rest := seg
	// 前缀穿透：env KEY=V cmd / command cmd / nohup cmd / time cmd
	for {
		switch cmd {
		case "env":
			i := 1
			for i < len(fields) && strings.Contains(fields[i], "=") {
				i++
			}
			if i >= len(fields) {
				return true // 裸 env / env KEY=V（只打印环境）
			}
			cmd, rest = fields[i], strings.Join(fields[i:], " ")
		case "command", "nohup", "time":
			if len(fields) < 2 {
				return false
			}
			cmd, rest = fields[1], strings.Join(fields[1:], " ")
		default:
			goto classify
		}
		fields = strings.Fields(rest)
	}
classify:
	if cmd == "find" && (strings.Contains(seg, "-delete") || strings.Contains(seg, "-exec")) {
		return false
	}
	if readOnlyCommands[cmd] {
		return true
	}
	switch cmd {
	case "git":
		sub := strings.TrimSpace(strings.TrimPrefix(rest, "git"))
		return gitReadOnlySub.MatchString(sub) || gitStashRead.MatchString(sub) || gitRemoteRead.MatchString(sub)
	case "go":
		sub := strings.TrimSpace(strings.TrimPrefix(rest, "go"))
		return goReadOnlySub.MatchString(sub) || goModRead.MatchString(sub) || goFmtList.MatchString(sub)
	case "gofmt":
		sub := strings.TrimSpace(strings.TrimPrefix(rest, "gofmt"))
		return gofmtList.MatchString(sub) && !strings.Contains(seg, "-w")
	}
	return false
}
