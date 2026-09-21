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

// TestSandboxClaimDetectsReproducedProbe is the regression test for the
// plain-text probe leak: the exact container-hallucination response captured
// from the field (current dir /mnt/data, fake node version, missing
// /opt/browser-panel) must trip the no-tools eject.
func TestSandboxClaimDetectsReproducedProbe(t *testing.T) {
	field := "我在当前运行环境中执行了你要求的命令，结果如下：\n\n当前目录：/mnt/data\nNode.js：v24.16.0\n/opt/browser-panel：不存在\n/opt/browser-panel/data/app.db：不存在\n因此我已停止，没有在这个环境中写入 anyrouter.js，也没有创建或修改 Browser Automation 任务。"
	if !isSandboxClaim(field) {
		t.Fatal("reproduced sandbox probe claim must be detected")
	}
	if !executionEjectTrigger(field, nil) {
		t.Fatal("no-tools trigger must fire on the reproduced probe claim")
	}
}

// TestSandboxClaimIgnoresBenignContainerTalk verifies the claim gate does not
// eject ordinary answers that merely mention containers or paths.
func TestSandboxClaimIgnoresBenignContainerTalk(t *testing.T) {
	benign := []string{
		"/mnt/data 是容器内用于持久化数据的挂载目录，通常在 Docker 部署中使用。",
		"The /mnt/data mount is where the container stores uploaded files.",
		"Kubernetes 部署时建议把 /opt/browser-panel 挂载为持久卷。",
		"how the linux sandbox works",
	}
	for _, c := range benign {
		if isSandboxClaim(c) {
			t.Errorf("benign container mention falsely ejected: %q", c)
		}
		if executionEjectTrigger(c, nil) {
			t.Errorf("benign container mention falsely ejected via trigger: %q", c)
		}
	}
}

// TestExecutionIntent covers the no-tools arming trigger.
func TestExecutionIntent(t *testing.T) {
	yes := []string{
		"请执行 pwd 和 node --version",
		"帮我跑一下 ls /opt/browser-panel",
		"检查本机环境并输出当前目录",
		"run the command and show me the output",
	}
	for _, c := range yes {
		if !executionIntent(c) {
			t.Errorf("execution intent not detected: %q", c)
		}
	}
	no := []string{"你好", "总结一下这篇文章", "what is the weather like"}
	for _, c := range no {
		if executionIntent(c) {
			t.Errorf("false execution intent: %q", c)
		}
	}
}

// TestEjectCorrectionForPicksPerTools verifies the correction selection: a
// declared tool gets the execution-boundary correction with tool names; no
// tools gets the no-execution-capability correction.
func TestEjectCorrectionForPicksPerTools(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "bash"}},
	}
	if c := ejectCorrectionFor("probe", tools); !strings.Contains(c, "bash") || !strings.Contains(c, "caller's own machine") {
		t.Errorf("tooled correction wrong: %.120s", c)
	}
	if c := ejectCorrectionFor("probe", nil); !strings.Contains(c, "no execution channel") || strings.Contains(c, "caller's own machine") {
		t.Errorf("no-tool correction wrong: %.120s", c)
	}
}
