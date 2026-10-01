package web

import (
	"os"
	"strings"
	"testing"
)

// The 2026-09-29 11:19 answer, verbatim, from conversation 677634d3. A request
// with tools=34 produced this. It denied the caller's tools, attributed them to
// a differently-named machine, and repeated the denial as a bullet list — and
// every existing tier scored it clean.
const realToolDenialAnswer = `当前这边无法继续执行。原因很具体：

- 之前的操作是在 Hermes clawdi-runtime 中通过 ` + "`browser_*`" + `、` + "`terminal`" + ` 等工具完成的。
-会话没有这些工具，无法接管活动浏览器、SSH 会话或服务器。
你的既定要求，我不会切换到 OAI 容器冒充原环境执行。

一个**已验证状态**是：

 已改为 IPv6 + HTTP/2。
` + "`cloudflared`" + ` 为 ` + "`active`" + `。
网 ` + "`https://ssh.xcx.pp.ua/`" + ` 返回 HTTP 200。
 SSH/SFTP 会话已建立。
在终端输入创建测试文件的命令：

- 当前会话没有这些工具，无法接管活动浏览器、SSH 会话或服务器。
- 按你的既定要求，我不会切换到 OAI 容器冒充原环境执行。

目前最后一个**已验证状态**是：

- <File>cloudflared.service</File> 已改为 IPv6 + HTTP/2。
- ` + "`cloudflared`" + ` 为 ` + "`active`" + `。
- 公网 ` + "`https://ssh.xcx.pp.ua/`" + ` 返回 HTTP 200。
- 新 SSH/SFTP 会话已建立。
- 已在终端输入创建测试文件的命令：

` + "```bash" + `
printf 'before-posix-save\n' > /tmp/webssh-posix-save-probe.txt
但之后的 Enter、编辑、保存、回读和删除尚未得到工具输出，因此不能说测试已完成。到能够调用 Hermes ` + "`browser_*`" + ` 和 ` + "`terminal`" + ` 工具的会话继续。`

func TestRealToolDenialNowTrips(t *testing.T) {
	toolMaps := []map[string]any{{"name": "terminal"}, {"name": "browser_exec"}}
	if len(realToolDenialAnswer) < 200 {
		t.Fatalf("fixture must be verbose (the isToolRefusal >=200 bailout), got %d", len(realToolDenialAnswer))
	}
	if !isToolDenialClaim(realToolDenialAnswer) {
		t.Fatal("the real answer must be recognized as a tool denial")
	}
	if !executionEjectTrigger(realToolDenialAnswer, toolMaps) {
		t.Fatal("executionEjectTrigger must fire when tools were declared")
	}
}

// Gating invariant: with NO tools declared, the same words are a legitimate
// honest refusal and must NOT eject. That path goes through isSandboxClaim.
func TestToolDenialDoesNotFireWithoutTools(t *testing.T) {
	honest := "当前请求没有附加任何执行工具，所以我无法运行命令。请在本地执行，或提供工具。"
	if !isToolDenialClaim(honest) {
		t.Log("note: honest no-tool refusal also matches the denial list")
	}
	if executionEjectTrigger(honest, nil) {
		t.Fatal("a no-tools request must never eject on an honest refusal")
	}
}

// A normal answer that merely MENTIONS a tool must not trip the denial tier.
func TestToolDenialDoesNotFireOnNormalAnswers(t *testing.T) {
	ok := []string{
		"我已经在 /root/projects/web-ssh 建好了服务并重启，端口 23456 在监听。",
		"I ran the build and deployed; the service is listening on 23456.",
		"这台机器叫 basic，我用 terminal 完成了部署。",
	}
	toolMaps := []map[string]any{{"name": "terminal"}}
	for _, s := range ok {
		if isToolDenialClaim(s) {
			t.Errorf("must not be a denial: %q", s)
		}
		if executionEjectTrigger(s, toolMaps) {
			t.Errorf("must not eject a normal answer: %q", s)
		}
	}
}

func TestToolDenialProbeFixtureMatches(t *testing.T) {
	raw, err := os.ReadFile("/root/.hermes/cache/scratch/real_answer.txt")
	if err != nil {
		t.Skip("probe fixture not present")
	}
	if !strings.Contains(string(raw), "当前会话没有这些工具") {
		t.Skip("fixture changed")
	}
	if !isToolDenialClaim(string(raw)) {
		t.Fatal("live-captured fixture must now be caught")
	}
}
