# cnb2api

> 用 Go 实现的、把 CNB（cnb.cool）NPC 聊天接口封装成 OpenAI / Anthropic 兼容 API 的免登录反向代理网关。

零第三方依赖，编译产物为单个二进制。

## 功能特性

**三种客户端协议**，全部翻译到上游的 OpenAI Chat 格式：

| 协议 | 端点 |
|---|---|
| OpenAI Chat Completions | `/v1/chat/completions` |
| Anthropic Messages | `/v1/messages`（另兼容 `/anthropic/v1/messages`） |
| OpenAI Responses | `/v1/responses` |

所有业务端点**同时支持带 `/v1` 前缀与不带前缀**两种路径，方便各类 SDK 把 `base_url` 配成根路径或 `/v1`。

**图片输入（视觉）**：三种协议的图片都会被归一化并转发给上游，支持多图、多轮、任意消息角色。

**推理等级**：通过 `reasoning_effort` 控制思考深度，三个协议均支持（Anthropic 的 `thinking.budget_tokens`、Responses 的 `reasoning.effort` 会被自动映射）。默认注入最轻量的 `low`；传 `reasoning_effort: "off"` 可完全关闭思考。

**原生工具调用（function calling）**：三个协议都支持，网关自动处理上游的工具名前缀要求，客户端无感。

**凭证池**：自动从上游首页获取并轮换匿名凭证，带 TTL 与失效重试，无需任何账号或 key。`pool_max` 即并发上限。

**免鉴权存活探测**：根路径 `/` 返回 200 OK，可直接用于 Docker healthcheck。

> ⚠️ 上游对**请求体有 1 MiB 硬上限**，而 base64 图片会膨胀约 33%、且每轮都会重发整个历史。
> 单张图建议不超过 ~700 KB。超限时会返回明确的 **HTTP 413**（含 `body_bytes` / `limit_bytes`）。
> 详见 [AGENTS.md](AGENTS.md#27-请求体-1-mib-硬上限重要)。

## 快速开始

构建：

```bash
git clone https://github.com/Mooling0602/cnb2api.git
cd cnb2api
go build -o cnb2api ./cmd/server
```

准备配置：

```bash
cp config.example.json config.json
# 编辑 config.json（字段说明见下方「配置说明」；api_key 留空 = 不鉴权）
```

启动：

```bash
./cnb2api -config config.json
```

不写配置文件、仅用环境变量也能启动：

```bash
CNB2API_LISTEN=:7863 CNB2API_MODEL=deepseek-v4.1-flash \
CNB2API_UPSTREAM=https://cnb.cool ./cnb2api
```

Docker 部署（docker-compose.yml 已含健康检查）：

```bash
docker compose up -d --build   # 宿主机 7863 -> 容器 7863
```

验证是否可用：

```bash
# 存活探测（免鉴权，返回 200 OK）
curl -s http://localhost:7863/

# 模型列表
curl -s http://localhost:7863/v1/models -H "Authorization: Bearer your-api-key"

# 对话（非流式）
curl -s http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"你好"}]}'

# 对话（流式）
curl -N http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"数到3"}]}'

# 图片输入（$B64 为图片的 base64）
curl -N http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4.1-flash","stream":true,"max_tokens":2000,"messages":[
        {"role":"user","content":[
          {"type":"text","text":"这张图是什么颜色？"},
          {"type":"image_url","image_url":{"url":"data:image/png;base64,'"$B64"'"}}]}]}'

# 凭证池状态
curl -s http://localhost:7863/pool -H "Authorization: Bearer your-api-key"
```

> 图片必须**内联为 data URL（base64）**，远程 `http(s)` 地址会被上游拒绝。
> 给图片请求留足 `max_tokens`：图片会显著拉长思考链，额度太小会导致 `content` 为空（输出被截断）。

## Nix / NixOS

仓库自带 `flake.nix`（已启用 flakes），无需本机安装 Go：

```bash
# 直接运行（不传 -config 时走环境变量）
CNB2API_API_KEY=your-key nix run .

# 用配置文件运行
nix run . -- -config ~/cnb2api.json

# 构建二进制到 ./result/bin/cnb2api（cnb2api 是 server 的同义软链）
nix build .#default
./result/bin/cnb2api -config ~/cnb2api.json

# 开发环境（go / gopls / golangci-lint）
nix develop

# 跑单元测试
nix flake check
```

`nix run .` 会在源码变化时自动重新编译；注意 flake 取的是 **Git 树**，新增文件需要先 `git add`（不必 commit）。

NixOS 上也可以声明式部署：

```nix
{
  inputs.cnb2api.url = "github:Mooling0602/cnb2api";

  # 在 NixOS 配置里
  imports = [ inputs.cnb2api.nixosModules.default ];

  services.cnb2api = {
    enable = true;
    listen = ":7863";
    apiKeyFile = "/run/secrets/cnb2api-key"; # 纯文本 key 文件；不设 = 不鉴权
    openFirewall = true;
  };
}
```

`apiKeyFile` 通过 systemd `LoadCredential` 注入，key 不会出现在 systemd 的 `Environment` 里。服务以 `DynamicUser` 运行，工作目录为 `/var/lib/cnb2api`。

配合 sops-nix 时：

```nix
sops.secrets.cnb2api_key = { };              # 默认落在 /run/secrets/cnb2api_key
systemd.services.cnb2api = {
  after = [ "sops-install-secrets.service" ];
  requires = [ "sops-install-secrets.service" ];
};
services.cnb2api.apiKeyFile = config.sops.secrets.cnb2api_key.path;
```

因为模块启用了 `ProtectHome`，key 文件不能放在 `/home` 或 `/root` 下（systemd 读不到），`/run/secrets` 这类位置没问题。

### 支持的架构

flake 声明支持 `x86_64-linux`、`aarch64-linux`、`aarch64-darwin`：

- **在原生 aarch64 机器上**（ARM 服务器、树莓派、Asahi 等），`nix build .#default` 直接可用。服务端代码本身无平台相关实现，aarch64 上不存在需要改代码的地方。
- **从 x86_64 交叉构建 aarch64** 需要在目标平台侧配置 `boot.binfmt.emulatedSystems = [ "aarch64-linux" ]`（或加远程 builder）。本仓库**没有**单独提供 `pkgsCross` 输出，原因见 [AGENTS.md](AGENTS.md#53-架构支持)。

## 鉴权说明

除根路径 `/`（免鉴权存活探测，返回 200 OK）外，其余所有端点统一鉴权，同时支持 OpenAI 的 `Authorization: Bearer` 与 Anthropic 的 `x-api-key`。Docker healthcheck 探测 `/` 即可，无需携带 key。

## 端点一览

| 路径 | 说明 |
|---|---|
| `GET /` | 免鉴权存活探测 |
| `GET /v1/models` | 模型列表（按配置白名单生成） |
| `POST /v1/chat/completions` | OpenAI Chat 协议 |
| `POST /v1/messages` | Anthropic Messages 协议 |
| `POST /v1/messages/count_tokens` | Anthropic token 计数 |
| `POST /v1/responses` | OpenAI Responses 协议 |
| `GET /v1/pool` | 凭证池状态 |

以上业务端点均同时支持不带 `/v1` 前缀的形式。

## 配置说明

唯一命令行参数是 -config <json 路径>。配置优先级：默认值 → JSON 文件 → 环境变量覆盖。

```jsonc
// config.json（示例）
{
  "listen": ":7863",                                  // 监听地址
  "api_key": "cnb-sk-...",                            // API 鉴权 key；留空 "" = 不鉴权
  "model": "deepseek-v4.1-flash",                     // 默认模型
  "models": ["deepseek-v4.1-flash"],                  // 支持的模型白名单（仅 JSON 可配）
  "pool_min": 2,                                      // 凭证池最少常驻凭证数
  "pool_max": 8,                                      // 凭证池最大凭证数（≈并发上限）
  "ttl_minutes": 30,                                  // 凭证有效期（分钟）
  "upstream": "https://cnb.cool"                      // 上游地址（凭证也从该页获取，可指向代理）
}
```

除 models 外，每个字段都有对应的环境变量，环境变量会覆盖 JSON 中的值（数值型解析失败会被忽略）：

- listen → CNB2API_LISTEN，默认 :7863
- api_key → CNB2API_API_KEY，默认空（= 不鉴权）
- model → CNB2API_MODEL，默认 deepseek-v4.1-flash
- pool_min → CNB2API_POOL_MIN，默认 2
- pool_max → CNB2API_POOL_MAX，默认 8
- ttl_minutes → CNB2API_TTL_MINUTES，默认 30
- upstream → CNB2API_UPSTREAM，默认 https://cnb.cool
- models 无环境变量，只能写在 JSON 里

> 若请求的模型不在 models 白名单内，会静默回退到默认的 model。

## 开发

```bash
go build ./... && go vet ./... && go test ./...
nix flake check     # 含 go test ./...
```

面向贡献者与 AI Agent 的技术说明见 **[AGENTS.md](AGENTS.md)**：上游协议的实测结论、
图片与体积上限细节、Nix 构建注意事项、以及提交信息与注释规范。

## 原项目与许可说明

原项目位于 https://github.com/lwjlwjlwjlwj/cnb2api

本仓库增加了推理等级、图片输入支持，并将内置的模型 ID 同步到上游已更新的 deepseek-v4.1-flash。

协议使用 MIT，与原项目相同。

**注意**：本项目仅供学习参考使用，请勿在生产环境下部署，请勿滥用于违法违规活动！上游接口可能随时进行调整和变动，本项目不对兼容和支持这些变更做任何保证。
