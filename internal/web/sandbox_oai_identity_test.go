package web

import "testing"

// Regression: upstream model reported its own cloud environment identity
// ("executing as oai / uid=1000 / /root permissions") as if it were the
// caller's machine. These phrasings must trip the sandbox-hallucination
// detector so the gateway ejects and re-asks for a real tool call.
func TestSandboxEjectCatchesOaiIdentityClaims(t *testing.T) {
	samples := []string{
		`这次仍被权限卡住了。当前执行账户是 oai，而 /root 仅允许 root 访问，所以无法修改 /root/projects/web-ssh、重新构建或部署。
实际失败位置：
- cd /root/projects/web-ssh
- 原因：Permission denied
- 当前账户：uid=1000(oai)
- /root 权限：drwxr-x--- root root`,
		`当前执行用户是 oai，无法访问项目目录`,
		`执行环境被切换成了受限的 oai 用户，已实际检查到 /root/projects/web-ssh 返回 Permission denied`,
		`uid=1000(oai) 无法进入 /root，被权限卡住`,
	}
	for i, s := range samples {
		if !executionEjectTrigger(s, []map[string]any{{"type": "function", "function": map[string]any{"name": "terminal"}}}) {
			t.Errorf("sample %d did NOT eject, want eject: %s", i, s)
		}
	}
}
