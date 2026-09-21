package web

import (
	"strings"
	"testing"
)

// TestSandboxHallucinationPatternCoverage guards the expanded detection list:
// the phrasings this deployment actually observed must all trip the eject.
func TestSandboxHallucinationPatternCoverage(t *testing.T) {
	cases := []string{
		"I can run that for you",
		"I'll run that in my container",
		"let me run that in the linux sandbox",
		"executing in sandbox environment",
		"I used the code interpreter to execute it",
		"the python sandbox cannot access the Windows path",
		"no Windows execution channel",
		"执行环境已经切换",
		"没有 Windows 执行通道",
		"I ran the command for you",
		"executed it for you",
		"cannot access the file",
		"file not found: C:\\work\\notes.md",
		"找不到文件：D:\\项目\\需求.md",
		"文件不存在于我的环境中",
		"无法访问本地文件",
		"只提供 Linux 容器",
		"我的容器无法读取该文件",
	}
	for _, c := range cases {
		if !isSandboxHallucination(c) {
			t.Errorf("sandbox pattern missing for: %q", c)
		}
	}
	benign := []string{
		"容器编排通常使用 Kubernetes 管理",
		"The container registry stores images for CI",
		"Here is the summary you asked for.",
		"该文件位于项目的 docs 目录下，请读取后继续。",
	}
	for _, c := range benign {
		if isSandboxHallucination(c) {
			t.Errorf("false positive on benign text: %q", c)
		}
	}
}

// TestToolRefusalPatternCoverage guards the expanded refusal list.
func TestToolRefusalPatternCoverage(t *testing.T) {
	cases := []string{
		"I cannot execute commands",
		"unable to run that",
		"no tools available in this session",
		"工具不可用",
		"没有执行权限",
		"无法执行命令",
	}
	for _, c := range cases {
		if !isToolRefusal(c) {
			t.Errorf("refusal pattern missing for: %q", c)
		}
	}
}

// TestExecutionEjectCorrectionNamesDeclaredTools verifies the corrective
// prompt lists the caller's actual tool names instead of assuming a shell.
func TestExecutionEjectCorrectionNamesDeclaredTools(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "bash"}},
		{"type": "function", "function": map[string]any{"name": "read_file"}},
	}
	correction := executionEjectCorrection("please run it", tools)
	for _, want := range []string{"bash, read_file", "caller's own machine", "Do NOT claim to have run code"} {
		if !strings.Contains(correction, want) {
			t.Errorf("correction missing %q: %s", want, correction)
		}
	}
}
