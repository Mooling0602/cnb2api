# cnb2api

> 用 Go 实现的、把 CNB（cnb.cool）NPC 聊天接口封装成 OpenAI / Anthropic 兼容 API 的免登录反向代理网关。

## 使用方法

构建：

```bash
git clone https://github.com/Dreamwxz/cnb2api.git
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
CNB2API_LISTEN=:7863 CNB2API_MODEL=deepseek-v4-flash \
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
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"你好"}]}'

# 对话（流式）
curl -N http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"数到3"}]}'

# 凭证池状态
curl -s http://localhost:7863/pool -H "Authorization: Bearer your-api-key"
```

鉴权说明：除根路径 `/`（免鉴权存活探测，返回 200 OK）外，其余所有端点统一鉴权（同时支持 OpenAI `Authorization: Bearer` 与 Anthropic `x-api-key`）；业务接口同时支持 /v1/... 与 /... 两种路径。Docker healthcheck 探测 `/` 即可，无需携带 key。

## 配置说明

唯一命令行参数是 -config <json 路径>。配置优先级：默认值 → JSON 文件 → 环境变量覆盖。

```jsonc
// config.json（示例）
{
  "listen": ":7863",                                  // 监听地址
  "api_key": "cnb-sk-...",                            // API 鉴权 key；留空 "" = 不鉴权
  "model": "deepseek-v4-flash",                       // 默认模型
  "models": ["deepseek-v4-flash", "deepseek-v4-pro"], // 支持的模型白名单（仅 JSON 可配）
  "pool_min": 2,                                      // 凭证池最少常驻凭证数
  "pool_max": 8,                                      // 凭证池最大凭证数（≈并发上限）
  "ttl_minutes": 30,                                  // 凭证有效期（分钟）
  "upstream": "https://cnb.cool"                      // 上游地址（凭证也从该页获取，可指向代理）
}
```

除 models 外，每个字段都有对应的环境变量，环境变量会覆盖 JSON 中的值（数值型解析失败会被忽略）：

- listen → CNB2API_LISTEN，默认 :7863
- api_key → CNB2API_API_KEY，默认空（= 不鉴权）
- model → CNB2API_MODEL，默认 deepseek-v4-flash
- pool_min → CNB2API_POOL_MIN，默认 2
- pool_max → CNB2API_POOL_MAX，默认 8
- ttl_minutes → CNB2API_TTL_MINUTES，默认 30
- upstream → CNB2API_UPSTREAM，默认 https://cnb.cool
- models 无环境变量，只能写在 JSON 里

> 若请求的模型不在 models 白名单内，会静默回退到默认的 model。
