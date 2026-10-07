# futuresearch-gateway

把 [FutureSearch](https://futuresearch.ai) 的**异步任务式研究 API** 反代成
**OpenAI 兼容端点 + 管理面板 + MCP 工具**。

一个二进制、一个配置文件、一个 data 目录 —— 这就是全部。

```
你的客户端 ──OpenAI 兼容──▶ futuresearch-gateway ──▶ FutureSearch v0 API
（dsh / Claude Code /            ├─ 号池：多 key 轮询 + 粘性 + 冷却
  Cursor / 任何 SDK）            ├─ 管理面板：账号 / 模型 / 用量 / 前置指令
                                 └─ MCP：/mcp 暴露 research / forecast / models
```

## 为什么需要它

FutureSearch 的上游**不是 chat completions**，是异步任务：

```
POST /operations/agent-map  {"input":[],"task":"…"}  →  {task_id}
GET  /tasks/{id}/status                              →  running | completed | failed
GET  /tasks/{id}/result                              →  {"data":[{"answer":…}]}
```

市面上的客户端都不会说这套。这个网关把它翻译成所有客户端都认的 OpenAI 协议，
并且处理掉那些**不显然的坑**（见下面「设计要点」）。

## 一键部署

### Docker（推荐）

```bash
docker run -d --name fsgw \
  -p 7868:7868 \
  -v $PWD/fsgw-data:/data \
  -e FSGW_LISTEN_HOST=0.0.0.0 \
  -e FSGW_API_KEY=sk-your-client-key \
  -e FSGW_ADMIN_PASSWORD=your-panel-password \
  ghcr.io/hey345437-boop/futuresearch-gateway:latest
```

打开 `http://127.0.0.1:7868/` → 面板 → 「账号」→ 粘一个 FutureSearch API key → 就能用了。

> **安全闸**：监听非环回地址时**必须**同时设 `FSGW_API_KEY` 与 `FSGW_ADMIN_PASSWORD`，
> 否则进程直接拒绝启动。想只给本机用就把 host 留 `127.0.0.1`（默认）。

### 裸二进制

```bash
go build -o fsgw ./cmd/gateway && ./fsgw          # 默认 http://127.0.0.1:7868
```

首次启动会生成 `config.json` 与 `data/`。面板上改配置，改完即生效（不用重启）。

### 当 MCP server 用（stdio）

本地 AI 客户端可以直接 spawn 这个二进制当 MCP server，**不需要 HTTP 服务**：

```bash
futuresearch-gateway --mcp-stdio
```

## 账号从哪来

FutureSearch 注册送 **$20**（不要信用卡），一次研究约 $0.2~5。注册要过 Cloudflare Turnstile，
所以得用**真浏览器 + 干净的出口 IP**。造号流程见仓库外的脚本，核心就三步：
协议注册 → 真浏览器过验证码 → 建 API key。**每个号 ≈30 秒**。

一个号用完就换下一个 —— 网关本身就是号池，多塞几个 key 即可。

## 客户端怎么接

### dsh / pi-ai（`~/.dsh/settings.yaml`）

```yaml
llm-pi-ai:
  providers:
    futuresearch:
      displayName: FutureSearch
      apiKeyEnv: FUTURESEARCH_KEY       # 密钥放 ~/.dsh/.credentials.yaml
      api: openai-completions
      baseURL: http://127.0.0.1:7868/v1
      models:
        - id: agent-medium
          name: "Agent"
          contextWindow: 200000
          maxTokens: 32000
          reasoningEfforts: {low: low, medium: medium, high: high}
        - id: claude-fable-5
          name: "Claude Fable-5"
          contextWindow: 200000
          maxTokens: 32000
          reasoningEfforts: {low: low, medium: medium, high: high}
```

### Claude Desktop / Cursor（MCP）

```json
{ "mcpServers": { "futuresearch": { "url": "http://127.0.0.1:7868/mcp" } } }
```

拿到三个工具：`research(question, effort?, model?)` / `forecast(question)` / `models()`。

### curl

```bash
curl http://127.0.0.1:7868/v1/chat/completions \
  -H "Authorization: Bearer sk-your-client-key" -H "Content-Type: application/json" \
  -d '{"model":"agent-medium","stream":false,"messages":[{"role":"user","content":"2+2=?"}]}'
```

## 模型怎么用

两种写法，都支持：

| 写法 | 例子 | 说明 |
|---|---|---|
| **预设档** | `agent-low` / `agent-medium` / `agent-high` | 平台自己挑模型，档位 = 研究深度（0/5/10 轮迭代） |
| | `multi-agent-*` | 多方向 agent 并行研究后综合 |
| | `forecast` | 二值概率预测（0–100 + 理由） |
| **模型家族 + 档位** | `claude-fable-5` + `reasoning_effort: max` | 你指定底层模型，档位单独选 |
| **完整枚举名** | `claude-fable-5-max` | 档位写在名字里 —— **过第三方网关时用这个** |

`/v1/models` 三种形态都列（共 185 个）。**挂到 one-api / new-api / sub2api 这类网关后面时，
用完整枚举名** —— 那些网关会把请求体重建成自己的 struct，`reasoning_effort` 可能被丢掉。

## 设计要点（这个项目真正值钱的地方）

### 1. 保活必须是「真事件」，空心跳没用

客户端的流空闲看门狗按**解析出来的事件**计时，不按字节数。空 delta 帧（`delta: {}`）
解析不出任何事件 —— 任务跑满 5 分钟，客户端照样报
`stream idle timeout after 300000ms`，而网关这边日志显示一直在写。

所以这里把**任务进度当 `reasoning_content` 发**：既是真实事件（重置看门狗），
也让等待期在客户端的「思考」面板里可见。节流到 45 秒一条。

### 2. 失败只发 error 帧，**绝不补 `[DONE]`**

`[DONE]` 是「正常结束」的标记。截断时补它 = 把失败伪装成成功 ——
客户端会拿着半截 tool_call arguments 去解析，而网关日志里什么都看不到。
上游 5xx 还会**连续容忍 20 次**才放弃（任务通常还在跑，判太早等于白花钱）。

### 3. 143 个枚举收敛成「家族 × 档位」

上游 `llm` 参数有 143 个枚举（33 个家族 × 推理档）。平铺进客户端的模型选择器太长，
所以提供「家族名 + `reasoning_effort`」这一层；完整枚举名同时保留，两条路都通。

### 4. 前置指令注入

上游 agent 自带一套输出风格（偏科普腔），而它的 API **没有 system 字段** ——
唯一能压过它的地方就是任务文本最前面。面板里可以按模型配注入指令：

```json
"prompts": { "*": "【最高优先级】不要科普腔，直接给结论。", "agent-high": "" }
```

`*` 是所有模型的兜底，具体模型名覆盖它，**空串 = 该模型不注入**。

### 5. usage 是**估算值**

上游按任务计费、不返回 token 用量。为了让下游网关/客户端有量可记，
这里发一帧估算的 usage（`2 字符 ≈ 1 token`）。
**够看相对用量，不能拿来按 token 计费** —— 想按钱管额度请用「按次计费」。

## 面板

七个页面：概览 / **接入** / 账号 / 租户 / 模型 / 预览 / 设置。

**接入**是三步流水：粘一个 key → 选客户端（dsh / Claude Desktop / Cursor / curl）→ 复制配置，
或者点「直接写入本机 dsh 配置」让它自己写（会先备份 `settings.yaml`，缩进按文件实际层级推断）。

**预览**把 `preview.dir` 挂到 `/preview/`：agent 生成的 HTML 写进去就能直接打开，
不用「吐一段代码让人自己存文件再打开」。

## 两组 MCP，别混

网关同时挂**两组** MCP（各自的 URL —— 让人一眼看清哪些工具会碰磁盘）：

| 端点 | server 名 | 工具 | 碰本地磁盘？ |
|---|---|---|---|
| `/mcp` | `futuresearch` | `research` / `forecast` / `models` | ❌ 纯云端研究 |
| `/mcp/local` | `localfs` | `list_dir` / `read_file` / `search`（+ 可选 `write_file` / `mkdir` / `run_command`） | ✅ |

`/mcp/local` 就是「让 AI 真的操作本地项目」的那一半 —— 因为上游 agent 够不着你的磁盘
（没有工具回调面），本地手脚必须由本地进程提供。

```json
{ "mcpServers": {
    "futuresearch": { "url": "http://127.0.0.1:7868/mcp" },
    "localfs":      { "url": "http://127.0.0.1:7868/mcp/local" } } }
```

### 本地工具的安全模型

```jsonc
"localfs": {
  "enabled": true,
  "root": "/Users/you/projects",   // 必填；所有路径都被限制在这里面
  "allow_write": true,             // 默认 false
  "allow_exec": false,             // 默认 false
  "max_read_kb": 256, "max_out_kb": 64, "timeout_sec": 30
}
```

- 路径先解析成绝对路径 + **解符号链接**再校验，root 里放软链也逃不出去
- 绝对路径不报错，但会被**解释成相对 root**（模型写 `/etc/passwd` 只落到 `<root>/etc/passwd`）
- `run_command` **不走 shell**：argv 逐项传入，`;` / `|` / `&&` 都不会被解释；环境变量清洗
- 没开的工具**不会出现在 tools/list 里**（免得模型反复试）
- 读有大小上限、命令有超时与输出上限、搜索有命中上限

## 限制

- **不支持工具调用**：上游 API 没有工具回调面，模型不会调你的本地工具。
  它是**研究子程序**，不是编码 agent。本地手脚靠上面的 `/mcp/local`。
- **不支持图片输入**：整个 OpenAPI 规范里 `png`/`jpeg`/`vision`/`multimodal`/`base64`
  出现 **0 次**；上传只有 CSV/JSON（`/artifacts/upload`、`/uploads/request`）。
  带图的请求会**明确报 `images_not_supported`**，而不是静默丢图给一个自信的胡答。
- **不支持多模态**：只吃文本。
- **没有真正的多轮记忆**：每轮都是新任务，网关只把历史折成一段上下文前置（尾部截断）。
- **首字节慢**：20~60 秒起步（`-max` 档更久）。任何前置网关的上游超时都要调到 300 秒以上。

## 配置

```jsonc
{
  "listen": { "host": "127.0.0.1", "port": 7868 },
  "api_key": "",            // 客户端 Bearer；空 = 不鉴权（仅环回允许）
  "admin_password": "",     // 面板密码；空 = 不鉴权（仅环回允许）
  "data_dir": "./data",     // keys.json 等
  "upstream": { "timeout_seconds": 120 },
  "pool": { "hard_cooldown": "12h", "soft_cooldown": "60s", "max_rotate": 3 },
  "prompts": {},
  "mcp": { "enabled": true, "path": "/mcp" }
}
```

环境变量覆盖（容器部署用）：`FSGW_LISTEN_HOST` / `FSGW_LISTEN_PORT` /
`FSGW_API_KEY` / `FSGW_ADMIN_PASSWORD` / `FSGW_DATA_DIR`。

## 开发

```bash
make build            # 本地构建
go test ./...         # 单元测试（协议翻译 / 档位 / 收尾语义 / 错误分类）

# 面板冒烟（需要 playwright）：会开一个真浏览器断言登录框不可见、页签都能渲染
pip install playwright && playwright install chromium
python3 tools/panel-smoke.py http://127.0.0.1:7868
```

> 面板那个「登录框永远显示」的 bug 是纯 CSS 的（`.mask` 与 `.hidden` 同为单类选择器、
> `.mask` 写在后面 → `display:flex` 胜出），Go 单测和 curl 都看不出来 ——
> 所以留了这个真浏览器冒烟脚本。

## 免责声明

本项目是对 FutureSearch 公开 API 的**非官方封装**，与 FutureSearch 无关。
请遵守上游服务条款与所在地区法律；账号封禁、服务中断等风险由使用者自负。
本项目只做「协议翻译 + 号池」，不提供、也不代管任何账号或额度。
