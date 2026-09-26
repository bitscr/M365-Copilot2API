# M365-Copilot2API

将 Microsoft 365 的 Copilot 能力封装成 **OpenAI 兼容 API** 的网关服务。单二进制部署,自带 Web 控制台,支持多账户轮换、会话稳定续接、工具调用路由(router 模式)、Cloudflare 前置部署、自动清理云对话。

## 特性

- **OpenAI 兼容**:`POST /v1/chat/completions`(兼容流式 SSE 与工具调用 tool_calls)
- **Web 控制台**:模型管理、账户管理、会话/对话管理、用量统计、设置
- **会话稳定续接**:同一逻辑对话固定到同一云对话 + 同一账户,不漂移、不孤儿化
- **工具路由**:router 模式让网关询问上游模型做工具决策,兼容标准 OpenAI 工具协议
- **多账户轮询**:健康检查、限流冷却、空完成重试与故障转移
- **Cloudflare 友好**:正确处理 `X-Forwarded-For`,支持稳定的 UA 指纹

## 目录

- [环境变量](#环境变量)
- [部署方式一:源码编译部署](#部署方式一源码编译部署)
- [部署方式二:下载二进制直接运行(占位)](#部署方式二下载二进制直接运行占位)
- [License](#license)

---

## 环境变量

> 完整清单(全部变量 + 默认值 + 详细说明)见 [.env.example](.env.example)。

以下为核心变量快速说明。除标注"必配"外均有安全默认值。

| 变量 | 必配 | 说明 |
|---|---|---|
| `M365_LISTEN` | 否 | HTTP 监听地址(默认 `0.0.0.0:4141`) |
| `M365_DATA_DIR` | 否 | 数据目录:token / 账户 / 会话 / 对话 / 用量(默认 `$HOME/.config/m365-copilot2api`) |
| `M365_API_KEYS` | **是** | 逗号分隔的 API Key,调用 `/v1/*` 时凭此鉴权 |
| `M365_ADMIN_PASSWORD` | **是** | Web 控制台与管理 API 的管理员密码(绝不要提交真实值) |
| `M365_TRUSTED_PROXIES` | CF 前置**必配** | `cloudflare` 或逗号分隔 CIDR 列表;只信任这些来源的 `X-Forwarded-For` |
| `M365_FINGERPRINT_MODE` | CF 前置**必配** | `ua`(推荐,CF 后稳定)/ `ip_ua`(默认)/ `off` |
| `M365_TOOL_PLANNING_MODE` | 否 | `router`(默认,推荐)/ `native`(上游透传,不推荐) |
| `M365_CONTEXT_WINDOW` | 否 | 上下文窗口。**建议 `200000`**(实测 199K→398K tokens 零退化;上游 1M 窗口真实可靠) |
| `M365_MAX_OUTPUT_TOKENS` | 否 | 最大输出 token(建议 `8192` 与上述窗口搭配) |
| `M365_AUTO_CLEANUP_*` | 否 | 云对话自动清理:闲置窗口与数量上限 |
| `M365_OUTBOUND_PROXY` / `M365_PROXY_POOL` | 否 | 上游调用走代理(WARP/SOCKS/HTTP) |

**注意**:启动时 `<M365_DATA_DIR>/settings.json` 中的持久化设置会**覆盖**环境变量。要让环境变量反向压过控制台设置,设 `M365_SETTINGS_ENV_WINS=true`(默认不要设)。

---

## 部署方式一:源码编译部署

### 前置

- Go 1.23+
- Linux / macOS / Windows 均可

### 步骤

```bash
# 1. 克隆仓库
git clone https://github.com/bitscr/M365-Copilot2API.git
cd M365-Copilot2API

# 2. 编译(静态链接,单文件)
CGO_ENABLED=0 go build -ldflags="-s -w" -o m365-copilot2api ./cmd/server

# 3. 准备配置
mkdir -p /var/lib/m365-copilot2api
cp .env.example .env        # 按需修改,至少填 M365_API_KEYS 与 M365_ADMIN_PASSWORD

# 4. 直接运行(或看下方 systemd 方式)
./m365-copilot2api
```

启动后浏览器打开 `http://127.0.0.1:4141` 登录控制台,添加 Microsoft 365 账户即可开始使用。

### 以 systemd 服务运行(推荐)

```bash
sudo cp m365-copilot2api /opt/m365-copilot2api/
sudo cp .env /opt/m365-copilot2api/.env
```

```ini
# /etc/systemd/system/m365-copilot2api.service
[Unit]
Description=M365 Copilot2API Gateway
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/m365-copilot2api
EnvironmentFile=/opt/m365-copilot2api/.env
ExecStart=/opt/m365-copilot2api/m365-copilot2api
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now m365-copilot2api
```

---

## 部署方式二:下载二进制直接运行(占位)

> **⚠️ 占位说明**:多平台二进制尚未发布。待作者手动触发 GitHub Actions 的 Release workflow 后,下方表格会自动填充可下载链接(release.yml 会在 push tag 时自动更新本表)。在那之前,请使用[源码部署](#部署方式一源码编译部署)。

从 GitHub Releases 下载对应平台的最新二进制:

<!-- DOWNLOAD_TABLE_START -->
| 平台 | 架构 | 下载 |
|---|---|---|
| Linux | amd64 | [m365-copilot2api-linux-amd64](../../releases/download/v0.1.0/m365-copilot2api-linux-amd64) |
| Linux | arm64 | [m365-copilot2api-linux-arm64](../../releases/download/v0.1.0/m365-copilot2api-linux-arm64) |
| Linux | 386 | [m365-copilot2api-linux-386](../../releases/download/v0.1.0/m365-copilot2api-linux-386) |
| Darwin | amd64 | [m365-copilot2api-darwin-amd64](../../releases/download/v0.1.0/m365-copilot2api-darwin-amd64) |
| Darwin | arm64 | [m365-copilot2api-darwin-arm64](../../releases/download/v0.1.0/m365-copilot2api-darwin-arm64) |
| Windows | amd64 | [m365-copilot2api-windows-amd64.exe](../../releases/download/v0.1.0/m365-copilot2api-windows-amd64.exe) |
<!-- DOWNLOAD_TABLE_END -->

示例(以 Linux amd64 为例):

```bash
# 1. 下载并赋予执行权限
wget -O m365-copilot2api \
  https://github.com/bitscr/M365-Copilot2API/releases/download/v0.1.0/m365-copilot2api-linux-amd64
chmod +x m365-copilot2api

# 2. 准备配置(同源码部署)
mkdir -p /var/lib/m365-copilot2api
cp .env.example .env        # 至少填 M365_API_KEYS 与 M365_ADMIN_PASSWORD

# 3. 运行
./m365-copilot2api
```

Windows 用户直接运行 `m365-copilot2api-windows-amd64.exe` 即可(控制台窗口保持开启)。

> 手动触发说明:在 GitHub Actions 页面选择 **Release** workflow → **Run workflow** → 填写 tag(如 `v0.1.0`),即可在各平台构建二进制并创建 GitHub Release。若你手动填写的 tag 与表中版本不一致,发布后手动更新本表的版本号即可。

---

## License

[MIT](LICENSE)
