# PiG 原生 Claude Code 式上下文压缩：设计方案

状态：设计稿（未实现）
目标环境：pig 0.7.0+1.5.0（Pi 1.5.0 fork），扩展目录 `~/.pi/agent/extensions`

## 0. 一句话

用约 300 行 TypeScript 扩展，在 PiG 上复刻 Claude Code 压缩的**提示词、阈值、缓存复用**三件事：
`before_provider_request` 捕获上一次请求体 → `session_before_compact` 接管压缩 →
`ctx.modelRegistry.streamSimple` 配 `onPayload` 把捕获体原样重发（末尾追加一条摘要指令）→
解析 `<summary>` 后返回 `{compaction: {...}}` 交给 PiG。

## 1. 为什么不能用现成插件（实测）

| 包 | 假设 | 在 PiG 上的结果 |
| --- | --- | --- |
| `pi-cc-compaction` | 扩展与主进程同类，可 patch `SettingsManager.prototype.getCompactionSettings` 与 `ExtensionRunner.prototype.emitBeforeProviderRequest` | 阈值：Go 侧 `ShouldCompact` 计算，补丁无效（`override(model)`→33000 只在扩展进程内存里）。捕获：`emitBeforeProviderRequest` 整场会话调用 **0 次**（扩展跑在独立 Node 进程）。结果 `forkPlan()` 恒返回 `no request has been answered yet in this session`，每次都回退 |
| `pi-cached-compaction` | `before_provider_request` 传裸 payload；重建请求与 PiG 原请求可逐字段相等 | 能捕获、能尝试，但**8 次压缩全部 fallback**（trace `~/.pi/agent/cached-compaction.jsonl`）：3+2 次 `request field "model" differs`（`group/deepseek` vs `deepseek`，PiG 把带 `/` 的模型 id 拆成 provider/model）、2 次 overflow、1 次会话变更。summary 全部由 Pi 自己写，`cacheRead: 0` 全价 |

结论：Pi 生态的压缩插件都建立在"扩展与宿主同类、请求体裸传"的假设上。PiG 是 **Go 主机 + 独立 Node 扩展进程**，假设不成立。

## 2. PiG 的可扩展面（实测）

| 能力 | 实测结果 |
| --- | --- |
| `before_provider_request` | ✅ 会触发，但传**信封** `{type, payload}`，请求体在 `event.payload` |
| `session_before_compact` | ✅ 会触发；`event` = `{type, preparation, branchEntries, customInstructions, reason, willRetry, signal}` |
| `preparation` 字段 | `firstKeptEntryId`, `messagesToSummarize`, `turnPrefixMessages`, `isSplitTurn`, `tokensBefore`, `fileOps{read,written,edited}`, `settings{enabled,reserveTokens,keepRecentTokens}` |
| 扩展接管压缩 | ✅ 返回 `{compaction:{summary, firstKeptEntryId, tokensBefore, details?}}`，PiG 直接采用、不再调模型；`session_compact` 里 `fromExtension: true` |
| 取消压缩 | ✅ 返回 `{cancel:true}` |
| 扩展自己发模型请求 | ✅ `ctx.modelRegistry.streamSimple(model, {messages}, options)` |
| 覆盖最终请求体 | ✅ `options.onPayload(payload, model)` 返回替换对象即生效（pigsdk `model_stream_callbacks.go`: "may return the replacement payload"） |
| 主对话 system prompt | ✅ `ctx.getSystemPrompt()` / `ctx.getSystemPromptOptions()` |
| SDK 辅助函数 | ✅ `convertToLlm`、`buildSessionProjection`、`estimateTokens`、`sessionEntryToContextMessages`、`getLastAssistantUsage`、`getLatestCompactionEntry` 都在 |
| 阈值 | ⚠️ 只能走 settings `compaction.reserveTokens` / `modelOverrides`（Go 侧真读） |
| magpie 代理的 prompt cache | ✅ 实测单轮 `cacheRead 9216 / 9464`，跨请求可命中 |

## 3. 架构

```text
              ┌───────────────── Go 主机（真正干活） ─────────────────┐
用户 prompt ─▶│ 组装 payload ──▶ before_provider_request(event)      │
              │      │                    │ (信封 {type,payload})    │
              │      ▼                    ▼                        │
              │  发请求 ⇄ 缓存命中     Node 扩展进程（独立）           │
              │                           ├─ 捕获 event.payload      │
              │                           │  存 latestRequest[会话]   │
              │  阈值触发 ─────────────▶  │                          │
              │ (/compact、overflow、      │                          │
              │  context-cap 的 ctx.compact)                          │
              │      │                    │                          │
              │      ▼                    ▼                          │
              │ session_before_compact(preparation, branchEntries)   │
              │      ▲                    │                          │
              │      │                    ├─ 拼 payload + 1 条指令    │
              │      │                    ├─ streamSimple(onPayload)  │
              │      │                    └─ 解析 <summary>           │
              │      └──── {compaction:{summary,firstKeptEntryId,…}} │
              └──────────────────────────────────────────────────────┘
```

## 4. 组件设计

### 4.1 阈值：不写代码，改 settings

Go 侧判定（`pigsdk/compaction_usage.go`）：

```go
var DefaultCompactionSettings = CompactionSettings{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 20000}
return contextTokens > contextWindow - settings.ReserveTokens
```

CC 的公式是 `窗口 − min(最大输出, 20K) − 13K`，也就是 reserve = `min(maxOut, 20000) + 13000`。
绝大多数模型 maxOut ≥ 20K ⇒ reserve 恒为 33000，所以：

```json
{ "compaction": { "reserveTokens": 33000 } }
```

1M 窗口 ⇒ 1,015,576 触发（Pi 默认 1,032,192）。需要差别对待时用 `compaction.modelOverrides`
按 `provider/modelId` 覆盖。**不要**去 patch `SettingsManager`（第 1 节已证无效）。

### 4.2 捕获：`before_provider_request`

```ts
pi.on("before_provider_request", async (event, ctx) => {
  const payload = (event as any).payload ?? event;   // PiG 是信封，Pi 是裸 payload
  if (!holdsConversation(payload)) return;
  if (isWarmReplay(payload)) return;                 // max_tokens===1 的保温重放不算
  pending.set(sessionId(ctx), { payload, at: Date.now(), modelKey: keyOf(ctx.model) });
});
```

要点：

1. **只提交成功请求**。`before_provider_request` 在发送前触发，失败的请求（overflow、5xx）不能当分叉起点。
   在 `turn_end` / `message_end` 看到非 error 的 assistant 消息后，才把 `pending` 提升为 `latest`。
   （pi-cached-compaction 就是这里踩空：它保留了 overflow 前的请求，白跑两次。）
2. **按引用保存要验证**。Pi 的定义是"请求对象发送后不再修改"，但 PiG 的 `payload` 来自桥接层，
   建议首次实现就 `JSON.stringify` 算前缀 hash（只算一次，别每轮都算）。
3. **只留一份，按 sessionId 分**；`session_shutdown` 释放。

### 4.3 压缩：`session_before_compact`

```ts
pi.on("session_before_compact", async (event, ctx) => {
  const cap = latest.get(sessionId(ctx));
  if (!cap) return undefined;                        // 没有前缀 ⇒ 交回 PiG
  if (event.reason === "overflow") return undefined; // 前缀本身已超限，重发必失败
  if (event.reason === "branch") return undefined;   // 分支摘要另说

  const instr = buildInstruction(event.customInstructions);   // §4.4
  const msgs = [...cap.payload.messages, { role: "user", content: [{ type: "text", text: instr }] }];

  const res = await ctx.modelRegistry.streamSimple(ctx.model, { messages: convertToLlm(msgs) }, {
    onPayload: () => ({ ...cap.payload, messages: msgs }),      // ★ 整体替换，逐字节等于缓存前缀
    maxTokens: cap.payload.max_tokens ?? cap.payload.max_completion_tokens,
    signal: event.signal,
    toolChoice: "none",
  });

  const summary = parseSummary(res);                            // §4.4
  if (!summary) return undefined;                               // 让 PiG 自己压
  return { compaction: {
    summary,
    firstKeptEntryId: event.preparation.firstKeptEntryId,
    tokensBefore: event.preparation.tokensBefore,
  }};
});
```

**为什么必须用 `onPayload` 整体替换**：不替换的话，`streamSimple` 会自己重建 payload，
PiG 会把模型 id `group/deepseek` 拆成 `deepseek` 再发（实测：pi-cached-compaction 的 3 次
`request field "model" differs`），前缀立刻不一致、缓存全废。整体替换同时解决了
system prompt、tools 顺序、cache 标记、`model` 字符串这四类差异——它们本来也不该由我们逐字段对齐。

**工具**：保持 `tools` 原样（缓存需要），在指令里禁止调用工具，并只取文本、丢弃工具调用块。

### 4.4 提示词与解析：照 CC 2.1.316

外层指令（末尾追加的那条 user message）：

```text
Your task is to create a detailed summary of the conversation so far, paying close
attention to the user's explicit requests and your previous actions. This summary
should be thorough in capturing technical details, code patterns, and architectural
decisions that would be essential for continuing development work without losing context.

Before providing your final summary, wrap your analysis in <analysis> tags ...
[CC 原文]

Additional Instructions: <步骤 12 的 /compact 参数，没有则整段省略>

Your response must contain only the <analysis> and <summary> blocks.
```

`<summary>` 九段：

1. Primary Request and Intent
2. Key Technical Concepts
3. Files and Code Sections
4. Errors and fixes
5. Problem Solving
6. All user messages
7. Pending Tasks
8. Current Work
9. Optional Next Step

解析照 CC：先丢弃第一块 `<analysis>`（它可能有截断的重复块），再取 `<summary>` 正文。
没有可用 `<summary>` 时，追加一条更严格的口吻重问一次；仍不行则回退 PiG。

### 4.5 返回契约

```ts
{ compaction: { summary, firstKeptEntryId, tokensBefore } }
```

PiG 记为 `compaction` 条目，`session_compact` 事件带 `fromExtension: true`。
`firstKeptEntryId` 直接用 `preparation` 给的切点：保留范围由 PiG 决定，摘要描述其之前的部分。
注意这跟 CC 略有出入：CC 的摘要覆盖全量上下文、只保留最后一轮；PiG 的切点由
`keepRecentTokens` 决定。想更贴近 CC 就把 `keepRecentTokens` 调小（如 8000）。

### 4.6 失败、熔断、抖动

| 情况 | 处理 |
|---|---|
| 网络/5xx/超时 | 按 `retry.*` 退避重试，仍失败 → 返回 `undefined`，提示 `Error during compaction: …` |
| 响应无 `<summary>` | 换更严格口吻重问一次；仍无 → 回退 PiG，提示写原因（stopReason、输出 token、片段首尾） |
| 前缀缺失（新会话/刚切换模型） | 返回 `undefined`，跑一轮后自动恢复 |
| 连续 3 次失败 | 本会话停用自动接管（`/compact` 仍可用）；任何一次成功解除 |
| 压缩后 3 轮内再次触发 | 抖动计数，3 次后停用并提示 `Autocompact is thrashing` |
| overflow 触发 | 不回退不行：前缀已超限，重发只会再失败；直接交回 PiG |

所有分支写一行 JSON 到 `~/.pi/agent/pi-cc-trace.jsonl`（`compacted|cancelled|fallback|failed` + 原因 +
`input/cacheRead/output` + 耗时），这是后面验证缓存是否真命中的唯一依据。

### 4.7 与现有扩展的分工

| 触发 | 谁负责 |
|---|---|
| 阈值自动触发 | settings `compaction.reserveTokens`（§4.1），Go 侧 |
| 工具循环中途背压 | `pi-context-cap`：`turn_end` 时 `ctx.compact()`，仍保留（CC 没有这个，因为 CC 每轮都查） |
| `/compact [指令]` | 本扩展（`reason: "manual"`，`customInstructions` 进步骤 12） |
| overflow 恢复 | 本扩展直接让路，PiG 自己重试 |

注意 `pi-context-cap` 触发时 `ctx.compact()` 会 abort 当前 run 并发一条续跑 prompt；接管后要保持这个行为不变。

## 5. 验证计划

```bash
# 1) 大上下文会话 + 手动压缩
mkdir -p /tmp/cctest/.pi
printf '{"compaction":{"keepRecentTokens":100}}\n' > /tmp/cctest/.pi/settings.json
cd /tmp/cctest && {
  echo '{"id":"p1","type":"prompt","message":"read <两个大文档>, then reply exactly: done"}'; sleep 45
  echo '{"id":"c1","type":"compact"}'; sleep 45
} | pig --mode rpc --approve --no-lens --no-lsp | grep -a '"command":"compact"'
```

断言：

1. `session_compact.fromExtension === true`（不是 Pi 自己写的）
2. trace 里 `cacheRead / (input + cacheRead) > 0.9` —— 前缀确实命中
3. 摘要含九段标题，且 `<analysis>` 不在正文里
4. `/compact 只保留文件名` → Additional Instructions 生效
5. 边界：新会话首次压缩、切换模型后、overflow 触发、扩展关闭时四条路径都能优雅回退

## 6. 风险

1. **payload 引用可变**：PiG 的桥接层可能复用对象。对策：捕获后立刻取前缀 hash，发现变化就重捕。
2. **序列化顺序**：Go map 序列化不保序时，即便内容相同前缀也可能不同。对策：`cacheRead` 断言 + 失败自动降级。
3. **magpie 缓存策略未知**：TTL、按 key 还是按前缀、是否有 5 分钟下限，都未测。对策：trace 里记录 `cacheRead`，用真实数据说话。
4. **大 payload 成本**：1M 上下文 ≈ 数 MB JSON，每次捕获做 hash 有代价。对策：只在 `turn_end` 后算一次，且可关闭。
5. **与 `pi-context-cap` 叠加**：两者都触发压缩时以 `reason` 区分，避免重复摘要。
6. **PiG 版本漂移**：`onPayload` 在 `model_stream_callbacks.go`，`preparation` 字段来自 Node 兼容层，两者都可能变。对策：启动时做一次能力自检，缺字段直接不接管并提示。

## 7. 工作量

| 模块 | 约 |
|---|---|
| 捕获 + 提交 + hash + trace | 80 行 |
| 压缩 + onPayload + 解析 + 重问 | 140 行 |
| 失败/熔断/抖动/`/cc-compact` 命令 | 80 行 |
| RPC 测试脚本 | 60 行 |
| 合计 | ≈ 360 行，半天可跑通，一天可稳定 |

## 8. 备选方案

- **Go 扩展**（PiG 原生）：能碰到压缩内部，但仍无法改变"摘要请求必须自带前缀"这一事实，且开发成本高一个量级。不划算。
- **只调阈值**：`compaction.reserveTokens: 33000` 一行。拿不到 CC 提示词与缓存复用，但阈值行为立刻等于 CC。
