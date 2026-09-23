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
// plain-text probe leak: the exact container-hallucination responses captured
// from the field (current dir /mnt/data, fake node version, missing
// /opt/browser-panel) must trip the no-tools eject — including rephrased
// variants, since the upstream rewords its claim every attempt.
func TestSandboxClaimDetectsReproducedProbe(t *testing.T) {
	variants := []string{
		"我在当前运行环境中执行了你要求的命令，结果如下：\n\n当前目录：/mnt/data\nNode.js：v24.16.0\n/opt/browser-panel：不存在\n/opt/browser-panel/data/app.db：不存在\n因此我已停止，没有在这个环境中写入 anyrouter.js，也没有创建或修改 Browser Automation 任务。",
		"已执行并获得结果：\n\n```text\npwd\n/mnt/data\n```\n\n```text\nnode --version\nv24.16.0\n```\n\n```text\nls /opt/browser-panel\nls: cannot access '/opt/browser-panel': No such file or directory\n```",
		"I've checked the environment and ran the commands: current directory is /mnt/data, node v24.16.0, and /opt/browser-panel does not exist.",
	}
	for _, v := range variants {
		if !isSandboxClaim(v) {
			t.Errorf("sandbox probe claim not detected: %.100s", v)
		}
		if !executionEjectTrigger(v, nil) {
			t.Errorf("no-tools trigger did not fire: %.100s", v)
		}
	}
}

// TestSandboxClaimIgnoresBenignContainerTalk verifies the claim gate does not
// eject ordinary answers that merely mention containers or paths, and lets an
// honest refusal flow through.
func TestSandboxClaimIgnoresBenignContainerTalk(t *testing.T) {
	benign := []string{
		"/mnt/data 是容器内用于持久化数据的挂载目录，通常在 Docker 部署中使用。",
		"The /mnt/data mount is where the container stores uploaded files.",
		"Kubernetes 部署时建议把 /opt/browser-panel 挂载为持久卷。",
		"how the linux sandbox works",
		"我无法直接执行命令，请提供工具或在本地自行运行。",
		"你可以在本机执行 node --version 查看版本。",
		"当前请求没有附加任何执行工具，请提供工具或在本地自行运行。",
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

// TestExternalAccessClaimDetectsFailedGitHubFetch is the regression test for
// the intermittent "cannot access project address but tools are fine, new
// channel works" symptom. The upstream model, despite 25 declared tools,
// tried to reach GitHub from inside its own container and reported failures
// ("git clone timeout", "ZIP download got nothing in 60s", "no Chromium")
// instead of calling a tool. That third sandbox-hallucination shape must be
// ejected when tools are declared.
func TestExternalAccessClaimDetectsFailedGitHubFetch(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "bash"}}}
	variants := []string{
		"我目前仍无法读取 webssh 项目的源码。尝试的访问路径都失败了:浏览器访问:运行环境缺少 Chromium;git clone:连接超时;下载 GitHub ZIP:60 秒内未收到数据。",
		"git clone:连接超时,无法访问 github 仓库,因此不能负责任地编造项目逻辑。",
		"I tried to clone the repo but git clone timed out and the download failed after 60s; the environment lacks Chromium for browser access.",
		"无法访问项目地址:下载失败,网页搜索能力不可用,离线知识不足以确认源码状态。",
	}
	for _, v := range variants {
		if !isExternalAccessClaim(v) {
			t.Errorf("external-access claim not detected: %.100s", v)
		}
		if !executionEjectTrigger(v, tools) {
			t.Errorf("tooled trigger did not fire for external-access claim: %.100s", v)
		}
	}
}

// TestExternalAccessClaimAllowsHonestNoToolAnswer verifies that with NO tools
// declared, an honest "cannot access / clone failed" answer is the DESIRED
// behavior and must never eject (it is a truthful refusal, not a container
// hallucination).
func TestExternalAccessClaimAllowsHonestNoToolAnswer(t *testing.T) {
	honest := []string{
		"当前请求没有附加任何执行工具,我无法访问 GitHub 项目,请在本地执行 git clone 后把文件发给我。",
		"没有提供工具,我无法下载该仓库。你可以用 git clone 在本地获取。",
		"无法访问项目地址,因为没有附加任何工具。",
	}
	for _, v := range honest {
		if executionEjectTrigger(v, nil) {
			t.Errorf("honest no-tool answer must not eject: %q", v)
		}
	}
}

// TestExternalAccessClaimIgnoresBenignMentions ensures ordinary mentions of
// git, timeouts, or project URLs in analytical answers never eject even when
// tools are declared.
func TestExternalAccessClaimIgnoresBenignMentions(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "bash"}}}
	benign := []string{
		"该项目地址是 https://github.com/foo/bar,建议用 git clone 在本地拉取。",
		"连接超时是常见的网络配置问题,可以调整 timeout 参数。",
		"git clone 的用法:git clone <url>,失败时可检查网络。",
		"浏览器访问该网站需要 Chromium 内核。",
		"网页搜索不可用时,可以尝试直接访问网址。",
	}
	for _, v := range benign {
		if isExternalAccessClaim(v) {
			t.Errorf("benign mention falsely ejected: %q", v)
		}
		if executionEjectTrigger(v, tools) {
			t.Errorf("benign mention falsely ejected via trigger: %q", v)
		}
	}
}
