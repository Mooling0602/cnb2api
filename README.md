# cnb2api

> 用 Go 实现的、把 CNB（cnb.cool）NPC 聊天接口封装成 OpenAI / Anthropic 兼容 API 的免登录反向代理网关。

## 使用方法

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

- **在原生 aarch64 机器上**（ARM 服务器、树莓派、Asahi 等），`nix build .#default` 直接可用 —— nixpkgs 有预编译的 Go 工具链，`CGO_ENABLED=0` 产出静态二进制。
- **从 x86_64 交叉构建 aarch64** 则需要在目标平台侧配置 `boot.binfmt.emulatedSystems = [ "aarch64-linux" ]`（或加远程 builder）。本仓库**没有**单独提供 `pkgsCross` 输出：交叉路径下 nixpkgs 需要从源码为本机编译 aarch64 的 glibc，没有 binfmt 会直接失败，而给一个"看起来能用其实装不上"的输出反而更坑。

服务端代码本身无平台相关实现（无 build tag、无 cgo、无汇编、无 `syscall`/`unsafe`），aarch64 上不存在需要改代码的地方。

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

# 凭证池状态
curl -s http://localhost:7863/pool -H "Authorization: Bearer your-api-key"
```

### 图片输入（视觉）

上游模型支持图片理解。三种协议的图片都会被转换成上游接受的 `image_url` 块转发：

| 协议 | 客户端传法 |
|---|---|
| OpenAI Chat | `content: [{type:"text",...},{type:"image_url",image_url:{url:"data:image/png;base64,..."}}]` |
| Anthropic | `content: [{type:"text",...},{type:"image",source:{type:"base64",media_type:"image/png",data:"..."}}]` |
| OpenAI Responses | `content: [{type:"input_text",...},{type:"input_image",image_url:"data:image/png;base64,..."}]` |

cURL 示例（Chat 协议，`$B64` 为图片的 base64）：

```bash
curl -N http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4.1-flash","stream":true,"max_tokens":2000,"messages":[
        {"role":"user","content":[
          {"type":"text","text":"这张图是什么颜色？"},
          {"type":"image_url","image_url":{"url":"data:image/png;base64,'"$B64"'"}}]}]}'
```

注意事项：

- **图片必须内联为 data URL**（base64）。远程 `http(s)` 地址会被上游以 400 拒绝（`code 11133`），网关不做代下载 —— 那会引入 SSRF 与体积膨胀风险。
- 图片可出现在 `user` / `system` / `assistant` / `tool` 任一角色，单条消息可带多张图。
- 上游接受的 mime：`image/png`、`image/jpeg`、`image/jpg`、`image/gif`、`image/webp`（大小写、`;charset=...` 后缀、首尾空格均容忍）；`image/bmp`、`image/svg+xml` 会被拒。网关不预校验 mime，由上游裁决，避免上游放宽后网关结论过期。
- **给足 `max_tokens`**：图片会显著拉长思考链。若额度太小，思考过程会把配额耗尽，导致 `content` 为空 —— 这是输出被截断，不是图片没传过去。需要立刻拿到答案可显式传 `reasoning_effort: "off"`。

鉴权说明：除根路径 `/`（免鉴权存活探测，返回 200 OK）外，其余所有端点统一鉴权（同时支持 OpenAI `Authorization: Bearer` 与 Anthropic `x-api-key`）；业务接口同时支持 /v1/... 与 /... 两种路径。Docker healthcheck 探测 `/` 即可，无需携带 key。

## 配置说明

唯一命令行参数是 -config <json 路径>。配置优先级：默认值 → JSON 文件 → 环境变量覆盖。

```jsonc
// config.json（示例）
{
  "listen": ":7863",                                  // 监听地址
  "api_key": "cnb-sk-...",                            // API 鉴权 key；留空 "" = 不鉴权
  "model": "deepseek-v4.1-flash",                       // 默认模型
  "models": ["deepseek-v4.1-flash"],                    // 支持的模型白名单（仅 JSON 可配）
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

## 原项目与许可说明

原项目位于 https://github.com/lwjlwjlwjlwj/cnb2api

本仓库增加了推理等级、图片输入支持，并将内置的模型 ID 同步到上游已更新的 deepseek-v4.1-flash。

协议使用 MIT，与原项目相同。

**注意**：本项目仅供学习参考使用，请勿在生产环境下部署，请勿滥用于违法违规活动！上游接口可能随时进行调整和变动，本项目不对兼容和支持这些变更做任何保证。
