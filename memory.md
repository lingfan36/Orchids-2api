# 项目记忆

## 环境
- 安装位置：`F:\ochids\orchids`
- 数据位置：`C:\Users\凌凡\AppData\Roaming\Orchids`
- 本地无 Go 环境，编译部署在 Railway

## Orchids 上游协议（WebSocket）

**连接 URL：**
```
wss://orchids-server.calmstone-6964e08a.westeurope.azurecontainerapps.io/agent/ws/coding?token={JWT}&orchids_api_version=5
```
- endpoint 是 `coding`（不是 `coding-agent`，后者只是 fallback）
- token 通过 URL query parameter 传递，不是 HTTP header
- API_VERSION = `"5"`

**消息流程：**
1. 建立 WebSocket 连接
2. 发 `client_hello`（isLocal=true 时）
3. 发 `user_request` 消息（通过 `sendWithAckRetry`）

**AgentRequest 关键字段：**
- `agentMode` = 模型名（如 `"claude-opus-4.6"`、`"claude-sonnet-4-6"`、`"auto"`）
- `mode` = `"agent"` / `"chat"` / `"plan"`
- 没有单独的 `model` 字段

**Orchids 支持的模型值：**
- `"claude-opus-4.6"`
- `"claude-sonnet-4-6"`（注意是连字符 `-`，不是点 `.`）
- `"auto"`

## 代码结构

| 包 | 职责 |
|---|---|
| `internal/client` | WebSocket 连接、JWT 刷新、帧编解码 |
| `internal/client/tls.go` | TLS 握手（`tlsDial`） |
| `internal/handler` | HTTP 路由、`mapModel` 模型映射 |
| `internal/store` | 账号存储，`UpdateClientCookie`、`UpdateSessionID` |
| `internal/loadbalancer` | 账号轮询 |
| `internal/clerk` | Clerk JWT 获取 |
| `internal/api` | API 路由注册 |
| `web/` | 静态文件，embed 进二进制 |

## client.go 关键实现

- 纯标准库，零外部依赖（RFC 6455 WebSocket 手写实现）
- `crypto/rand` 生成 WebSocket key 和帧 mask
- 每次请求调用 `refreshAndGetToken` 刷新 JWT，自动更新 rotating `__client` cookie
- `chatSessionID` 和 `requestId` 均生成 UUID v4
- ping/pong 处理，120 秒 read deadline

## Dockerfile 注意事项

- `web/` 已 embed 进二进制，无需 `COPY web`
- `data/` 运行时生成，无需 `COPY data`
- Railway 构建时自动执行 `go mod download`

## .gitignore

- `*.js`、`*.ps1` 已添加（过滤调试脚本）
