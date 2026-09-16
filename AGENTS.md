# AGENTS.md

面向在本仓库工作的开发者与 AI Agent 的技术说明。

README 只讲「这个项目能干什么、怎么跑起来」；**本文记录「为什么这么写」**——
尤其是那些无法从代码本身读出来、只能靠实测得到的上游行为。改动协议转换、
错误映射、凭证池之前，请先读完对应章节。

---

## 1. 项目结构

```
cmd/server/          入口：配置加载（默认值 → JSON → 环境变量）与服务启动
internal/auth/       凭证池：从 cnb.cool 首页抓 csrfkey + csrftoken，维护池与 TTL
internal/upstream/   CNB 上游客户端：请求构造、SSE 读取、错误分类
internal/server/     HTTP 层：三个协议的 handler、协议转换、错误映射
internal/toolconv/   跨协议工具调用（function calling）统一层
```

数据流：客户端 → `internal/server`（按协议解析并归一化为 OpenAI Chat 格式）
→ `internal/upstream`（附凭证发往 CNB）→ SSE 流原路翻译回客户端协议。

**三种客户端协议（OpenAI Chat / Anthropic Messages / OpenAI Responses）在上游
只有一种落地格式**：CNB 的 OpenAI Chat 接口。所有协议差异都在
`internal/server` 里被吸收掉。

**无第三方依赖**（`go.mod` 仅 `module cnb2api` + `go 1.22`）。这是刻意维持的：
标准库足够，且 Nix 构建不需要 `vendorHash`。新增依赖前请先确认标准库确实做不到。

---

## 2. 上游协议事实（实测）

以下结论均来自对 `https://cnb.cool` 的实际请求，**不是猜测**。上游随时可能变更；
若代码注释里的结论与实际行为不符，以实测为准并更新注释。

### 2.1 端点与鉴权

| 项 | 值 |
|---|---|
| 端点 | `POST https://cnb.cool/ai/chat/completions` |
| `Csrftoken` 头 | 必需，取自首页 HTML 的 `window.csrftoken="..."` |
| `csrfkey` Cookie | 必需，与 token **配对**，取自同一会话的 `Set-Cookie` |
| `Origin` / `Referer` | 必须为 `https://cnb.cool` |
| 流式 | **强制**。上游拒绝非流式请求 |

凭证必须**同一次会话**取得（`auth.Fetch` 用全新 cookie jar 访问首页同时拿两者）。
跨会话混搭 token/cookie 会 403。

### 2.2 模型

**上游只有一个模型：`deepseek-v4.1-flash`**（类型 `deepthink`），来自任意页面的
SSR 数据 `systemConfigManage.ai`。

- `model` 请求字段被上游**完全忽略**：传 `deepseek-v4-flash`、`gpt-5`、`qwen3-max`、
  空串等一律返回 `"model":"deepseek-v4.1-flash"`。
- 前端 bundle 里那些旧模型名是**死代码**。
- **没有模型列表 API**。`/v1/models` 是本网关按配置白名单自行生成的。

因此本仓库把内置模型名统一为 `deepseek-v4.1-flash`，`models` 白名单默认只含它。
白名单的作用是**客户端侧**的（请求白名单外的名字会静默回退到默认 model），
不是上游要求。

### 2.3 上游不支持的东西

- **Anthropic Messages API**：不存在。`/v1/messages` 各种路径变体全部 404 或落到
  Next.js 的 HTML 兜底页。
- **OpenAI Responses API**：不存在（同上）。
- 两种 404 的形状不同，可据此判断一个路径是否被注册：
  - `/ai/*` 未注册 → **200 + HTML**（Next.js 兜底页）
  - `/v1/*`、`/responses` → **404 + JSON** `{"errcode":404}`
- **顶层 `system` 字段被静默忽略**：只有 `messages` 里的 `system` 角色生效。
  （用口令回显法验证过：`messages` 内的 system 指令被遵守，顶层 `system` 无效。）
- 响应恒为 OpenAI 形状（`"object":"chat.completion.chunk"`）。

> 结论：Anthropic / Responses 兼容是**本网关自己实现**的翻译层，
> 不要指望在上游找到对应端点。

### 2.4 `api.cnb.cool` 的坑

`api.cnb.cool` 是 **CNB OPENAPI Playground**（Swagger UI），与 `/ai/*` 无关。
它返回的 `401 {"errcode":16,...}` 来自**全局鉴权中间件**，
对任何路径都一样——**不能用它判断端点是否存在**。
要查真实端点清单请看 `https://api.cnb.cool/swagger.json`（权威 spec，194 条路径）。

### 2.5 `reasoning_effort` 白名单

上游白名单（**大小写敏感、精确匹配，非法值直接 400**）：

```
""  |  none  |  minimal  |  low  |  medium  |  high  |  xhigh  |  max
```

实测拒绝的行为：

| 输入 | 结果 |
|---|---|
| `"off"` | 400 `{"code":11150,"msg":"the reasoning effort value is not supported by the current model"}` |
| `"banana"` / `"MINIMAL"` / `" medium"`（带空格） | 同上 400 11150 |
| 省略字段 | 上游自行决定（本网关默认注入 `low`，见下） |
| `enable_thinking` | **双向无效**——传 true / false 都不影响是否思考 |

要点：

- **空串 `""` 是唯一真正「不思考」的取值**。字面量 `"off"` 反而会被拒。
  所以本网关把客户端传来的 `off`（大小写不敏感）归一化为 `""`
  （见 `internal/server/reasoning.go` 的 `normalizeReasoningEffort`）。
- **本网关默认注入 `low`**（`reasoningDefault`）。原因：上游 `enable_thinking`
  单独不触发思考链，必须靠 `reasoning_effort`；不注入的话回答质量明显下降。
  三个 handler 行为一致。客户端要完全不思考需显式传 `off`。
- **思考深度不是可靠的单调解**。同一批硬任务实测
  `minimal 1470 < low 2045 < medium 2639 < high 3318 ≈ xhigh 3404`（思考字符数），
  但重复跑会反转（`high` 掉到 1550）。**不要把 effort 当成精确的算力旋钮。**
- **网关不硬编码档位列表**：除 `off` 外一律原样透传给上游裁决。
  这样上游新增档位时网关自动支持，收紧时也能拿到上游的真实报错。
  这是本项目的通用原则，见 §4。

### 2.6 图片输入（视觉）

上游**支持**视觉输入，但格式很窄：

**唯一接受的形态**是 OpenAI 的 `image_url` 块，且 URL 必须是 **data URL（base64 内联）**：

```json
{"type":"image_url","image_url":{"url":"data:image/png;base64,<...>"}}
```

实测结论：

| 情况 | 结果 |
|---|---|
| data URL | ✅ 三种协议的图片都归一化成这个形态转发 |
| 远程 `http(s)://` 地址 | ❌ 400 `code 11133` |
| Anthropic 原生 `{"type":"image","source":{...}}` 直接透传 | ❌ 400 `code 11101` unsupported |
| 裸 base64 当普通文本发 | ⚠️ 200，但模型**幻觉**（把 base64 当正文读） |

mime 白名单（上游只看**声明的字符串**，不校验字节内容）：

- 接受：`image/png`、`image/jpeg`、`image/jpg`、`image/gif`、`image/webp`
  （大小写、`;charset=...` 后缀、首尾空格均容忍）
- 拒绝：`image/bmp`、`image/svg+xml`、不带 mime

> 实测细节：把**非法 JPEG 字节**配上 `image/jpeg` 的 mime 声明，上游照样接受——
> 说明它只匹配 mime 字符串。所以「换 mime 能否通过」的测试要用**同一份字节**，
> 否则测的是字节合法性而不是 mime。

其他已验证行为：

- 图片可出现在 `user` / `system` / `assistant` / `tool` **任一角色**
- 单条消息可带**多张图**，多条消息各带图也可以
- **纯图片无文本**的消息可用
- `max_tokens` 要给足：图片会显著拉长思考链，额度太小时思考会把配额吃光，
  表现为 `content` 为空——**这是截断，不是图片没传过去**。

实现位置：`internal/server/image.go`（三个 `extract*Images`）+
`internal/upstream/client.go` 的 `ChatMessage.MarshalJSON`（有 `Parts` 时把
`content` 输出成数组，否则保持字符串）。**网关不校验 mime 与 base64 合法性**，
一律透传交上游裁决。

### 2.7 请求体 1 MiB 硬上限（重要）

上游**外层网关**对请求体有硬上限 **1 MiB**。实测二分定位：

- `1048112` 字节 → 通过
- `1048688` 字节 → 拒绝

拒绝时上游返回（注意是 `errcode/errmsg` 格式，说明**请求根本没到模型**）：

```json
{"errcode":413,"errmsg":"[BODY_TOO_LARGE]Request body too large"}
```

容易踩的点：

- **限制的是整个请求体**，不是图片大小。纯文本堆到 1 MiB 一样 413。
- **base64 膨胀约 33%**（4/3）。所以单张图的原始体积天花板约 **750 KB**，
  稳妥取 **700 KB**；多图合计同理。
- **每一轮都会重发整个历史（含图片）**。于是会出现这个致命的连锁反应：

  | 轮次 | 请求 | 结果 |
  |---|---|---|
  | 1 | 1 张图（0.52 MiB） | ✅ |
  | 2 | 再加 1 张（合计 1.04 MiB） | ❌ 413 |
  | 3 | 只发文本 | ❌ 413 |
  | 4 | 极短文本 | ❌ 413 |
  | 5 | 从历史中移除图片后（211 B） | ✅ |

  即**会话会永久卡死**——图片仍在历史里，体积一直超限。
  用户报的 bug 正是这个（dsh 侧靠自动压缩会话才恢复）。
- **这不是模型上下文损坏**，分叉回发图之前的会话即可恢复。
- 重试或换凭证**无效**（请求没到模型）。上游**不支持** `Content-Encoding: gzip`
  绕过（实测返回 500）。
- **网关刻意不做自动裁剪历史**：静默丢弃用户提供的图片会改变对话语义，
  该由客户端决策。网关只负责把错误说清楚。

**因此网关把这种情况明确映射为 HTTP 413**（而不是含糊的 502），
并在响应里给出实际体积、上限与处理建议：

```json
{"error":{"type":"request_too_large","code":"body_too_large","message":"..."},
 "body_bytes":1090831,"limit_bytes":1048576}
```

`body_bytes` / `limit_bytes` 是给程序化处理用的结构化字段。
实现见 `internal/server/errors.go` 与 `internal/upstream/client.go` 的
`ErrBodyTooLarge`。

`MaxBodyBytes = 1 << 20` 常量**只用于诊断文案与体积提示，不作为拦截依据**：
是否超限始终由上游裁决，避免上游放宽后网关仍按旧值拒绝（同 §4 原则）。

### 2.8 工具调用（function calling）

- 工具名**必须以 `cnb_` 前缀**，否则 403。
  网关在 `internal/toolconv` 里自动加前缀、响应时还原，客户端无感。
- `tool_choice` 字段**本身触发 403**
  （`[FORBIDDEN]Agent calls are not allowed in this scenario`，即使工具名已带前缀）。
  网关统一丢弃该字段；不带它时上游会自动选工具，行为等价于 `auto`。
- 工具历史必须 `tool_call_id` 配对（`assistant.tool_calls` ↔ `tool.tool_call_id`）。

### 2.9 内容清洗

台湾国旗 emoji（`U+1F1F9 U+1F1FC`）会让上游返回 **500**
（实测 `user` / `system` / `tool` 任一角色都触发，属上游内容审核 bug）。
网关在最后一公里做等价替换（该 emoji → 字面量 `tw`，见 `sanitizeUpstreamBody`）。
**这是规避上游 bug，不是内容审查**，不要扩展成通用过滤。

---

## 3. 网关实现要点

### 3.1 路由与鉴权

除根路径 `/`（免鉴权存活探测，返回 200 OK，供 Docker healthcheck 用）外，
**所有已注册端点统一鉴权**，同时支持 OpenAI 的 `Authorization: Bearer`
与 Anthropic 的 `x-api-key`。

业务端点**同时注册 `/v1/xxx` 与 `/xxx` 两种路径**
（`register` 辅助函数），以兼容 base_url 配成根路径或 `/v1` 的各类 SDK。

`/` 在 `ServeMux` 里是兜底模式：只有确切的 `/` 返回 200，其他未知路径返回 404。
**鉴权不套在兜底上**，否则未知路径会被拦成 401 而不是 404。

### 3.2 凭证池的 `inUse` 计数

`Pool.Acquire` 会把 `inUse++`；**每一条退出路径都必须归还**——要么
`Report(cs, ok)`，要么 `Release`。`Report` 同时负责 `inUse--` 与错误计数。

漏掉一次 `Report` 会让 `inUse` **永久泄漏**：`Acquire` 倾向于挑 `inUse` 最小的
凭证，泄漏后池会随着失败次数不断膨胀（实测连发 6 次 413：池 1→6）。

- 413 与「其他非 2xx」分支都调 `Report(cs, true)`：**凭证本身有效**，
  错误来自客户端请求内容，不该消耗凭证的错误计数。
- 401/403 且响应体是 CSRF 相关错误才算凭证失效（`Report(cs, false)`）并重试；
  业务性 403（如工具名不合法）**不重试**，凭证仍标记为有效。
- `reportCloser` 在 `resp.Body` 关闭时上报成功结果，覆盖流式读取路径。

回归测试见 `internal/upstream/body_limit_test.go`
（`TestNoCredentialLeakOn413` 等，用 `httptest` 假上游 + `auth.SetBaseURL`）。
**改动 `Chat` 的重试循环后务必跑这一组测试。**

### 3.3 请求体只序列化一次

`Chat` 在重试循环**之前**完成 `json.Marshal` + `sanitizeUpstreamBody`，
循环内复用同一份 `[]byte`。图片内联后体积可观，放在循环里会在每次换凭证重试时
重复序列化整份 body。`TestChatRequestBodySerializedOnce` 守着这条。

---

## 4. 通用原则：不硬编码上游的取值列表

凡是「上游接受哪些值」这类**只有上游才知道**的判定，网关一律**透传 + 交上游裁决**，
只在错误路径上把上游的报错翻译清楚：

| 场景 | 做法 |
|---|---|
| `reasoning_effort` | 除 `off` 归一化外原样透传 |
| 图片 mime / base64 | 不预校验，透传 |
| 请求体大小 | 不预拦截，等上游 413 |

理由：硬编码的列表会**过期**。上游放宽后网关仍按旧值拒绝（假阴性），
上游收紧后网关的结论则是错的（假阳性）。**唯一的例外是必须做等价变换的地方**
（`off` → `""`、`cnb_` 前缀、emoji 替换），这些不是「判定」而是「改写」。

---

## 5. Nix / 构建

### 5.1 `nix run .` 会不会自动重编译？

**会，但前提是源码已进入 Git 索引。**

flake 的 `src = self` 取的是 **Git 树**（不是工作目录）。因此：

- **新增文件必须先 `git add`（甚至 `git add -N` 就够，不需要 commit）**，
  否则构建看不到它——表现为「明明改了源码却还是旧行为」或
  `undefined: xxx` 编译错误。
- 修改**已跟踪**的文件会被正常感知。
- 源码未变时命中二进制缓存（约 0.3s）；有变化时重新编译（约 13s）。
- 构建时那句 `warning: Git tree ... is dirty` 是**正常的**，不代表构建失败。

### 5.2 常用命令

```bash
nix run .                              # 编译并运行（走环境变量）
nix run . -- -config ~/cnb2api.json    # 带配置文件
nix build .#default                    # 产物在 ./result/bin/cnb2api（server 的同义软链）
nix flake check                        # 跑 go test ./...（checks.tests）
nix flake check --all-systems          # 需 binfmt 才能跑非本机架构
nix develop                            # go / gopls / golangci-lint / nixfmt
nix fmt                                # nixfmt
```

`checks.tests` 通过 `overrideAttrs` 放开 `subPackages` 并开 `doCheck`，
确保 `internal/**` 的测试也真的被执行（`buildGoModule` 默认只构建 `cmd/server`）。
**新增包后检查测试是否被覆盖到。**

### 5.3 架构支持

flake 声明 `x86_64-linux`、`aarch64-linux`、`aarch64-darwin`：

- **原生 aarch64 机器上直接可用**：nixpkgs 有预编译 Go 工具链，
  `CGO_ENABLED=0` 产出静态二进制。
- **从 x86_64 交叉构建 aarch64** 需要目标侧 `boot.binfmt.emulatedSystems`
  或远程 builder。仓库**刻意不提供** `pkgsCross` 输出：交叉路径下 nixpkgs 要从源码
  为本机编译 aarch64 的 glibc，没有 binfmt 必然失败，给一个「看起来能用其实装不上」
  的输出更坑。
- 服务端代码无平台相关实现（无 build tag、无 cgo、无汇编、无 `syscall`/`unsafe`）。

### 5.4 NixOS 模块

`nixosModules.default` 提供 `services.cnb2api`。几个非显然的点：

- `apiKeyFile` 通过 systemd `LoadCredential` 注入，由 wrapper 读进环境变量，
  **key 不出现在 systemd 的 `Environment` 里**，也不进 Nix store。
- 服务跑在 `DynamicUser` + `ProtectHome` 下，所以 **key 文件不能放 `/home` 或
  `/root`**（systemd 读不到），`/run/secrets` 可以。
- wrapper 用 `writeShellScriptBin` 而**不是** `writeShellApplication`：
  后者会引入 ShellCheck，进而拖入整个 GHC/Haskell 工具链，构建代价过大。
- `openFirewall` 从 `listen` 里取最后一段端口，兼容 `":7863"` / `"0.0.0.0:7863"` /
  `"[::]:7863"`。

---

## 6. 开发与测试

```bash
# 非 Nix 环境
go build ./... && go vet ./... && go test ./...

# 全量（含格式检查）
gofmt -l .        # 应无输出
nix flake check
```

- 测试全部是**表驱动 + 标准库 `testing`**，不引入断言库。
- 上游相关的测试用 `httptest` 起假上游，配合 `auth.SetBaseURL` 重定向
  （见 `internal/upstream/body_limit_test.go` 的 `newTestClient`），
  **不要把测试打到真实的 cnb.cool**。
- 测试函数名用英文，失败信息用中文（与代码注释一致）。

---

## 7. 编码规范

### 7.1 提交信息

**使用语义化（Conventional Commits）格式，用英文书写。**

```
<type>(<scope>): <description>

[optional body]
```

- `type`：`feat` / `fix` / `refactor` / `docs` / `test` / `chore` / `perf` / `build` / `ci`
- `scope`：受影响的模块，如 `server` / `upstream` / `auth` / `toolconv` / `nix`
- `description` 用祈使句、小写开头、不加句号
- **破坏性变更**用 `!` 标记（如 `chore!: change submodule point`）并在正文说明

示例（取自本仓库历史）：

```
feat: add image input support
fix(responses): honor max_output_tokens in streaming events
refactor: root liveness probe & unified per-endpoint auth
test: add regression tests for credential leak on 413
```

### 7.2 注释与文档

**代码注释与文档一律使用中文。**

- 注释解释 **「为什么」**，不是「做了什么」——代码本身已经说明后者。
- 涉及上游行为的注释**必须写明实测结论**（附现象或错误码），
  例如「实测 1048112 字节通过、1048688 字节被拒」，而不是「据说上限是 1 MiB」。
- 被刻意否决的做法要留下理由，避免后人「顺手优化」回去
  （例：网关不做历史自动裁剪、`tool_choice` 必须丢弃、wrapper 不用
  `writeShellApplication`）。
- 注释中引用上游错误时保留原始错误码（`code 11150`、`errcode 413` 等），
  便于日后对照上游变更。

### 7.3 代码风格

- `gofmt` 必须干净（`nix flake check` 会跑）。
- 不引入第三方依赖；标准库能做的事就用标准库。
- 协议转换的差异吸收在 `internal/server`，`internal/upstream` 只认 OpenAI Chat 格式。
- 新增「上游接受哪些值」的判定前，先读 §4。
