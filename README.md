# pig-cc-compaction

PiG 的 Claude Code 式自动压缩扩展：Go 扩展负责与 PiG 宿主交互（捕获请求、调用模型、回传压缩结果），内嵌 moejs 运行 `plugin/compaction.js` 里的 JS 策略核心（决定是否接管、拼摘要提示词、解析九段 `<summary>`）。

实测环境：`pig 0.7.0+1.5.0`（Pi 1.5.0 fork）。本仓库只验证这个版本，不承诺其他 PiG/Pi 版本。

## 1. 机制

压缩发生时，扩展不重新拼一份“压缩专用请求”，而是把**本会话上一次 provider 请求体原样重发**，只在 `messages` 末尾追加一条摘要指令。前缀字节不变，provider 的 prompt cache 才能继续命中，整段历史不必按全价重算。

流程：

1. `before_provider_request`：宿主把即将发送的请求体交给扩展，扩展按 `sessionId` 保存一份（`Capture`）。
2. `session_before_compact`：PiG 要压缩时触发。JS 策略读取捕获、熔断状态和 `preparation`，决定 `takeover`（接管）还是 `pass`（让路）。
3. 接管时，driver 通过宿主的 `streamSimple` 重发捕获的 payload，并把摘要指令追加为最后一条 user 消息。
4. 回复解析出可用的 `<summary>` 后，返回：
   `{compaction: {summary, firstKeptEntryId, tokensBefore, details?}}`
   切点和压缩前 token 数直接取 PiG 的 `preparation`，扩展不自行决定保留范围。`summary` 末尾附上 PiG 自己也会写的 `<read-files>` / `<modified-files>` 清单，`details` 同步记录这两份清单。
5. 切点落在某一轮中间时（`preparation.isSplitTurn` 为真），再发一次请求，把这一轮被切开的前半段（`preparation.turnPrefixMessages`）按 PiG 自己的提示词总结成一段 checkpoint，接在摘要后面：

   ```text
   -----------
   **Turn Context (split turn):**

   ## Original Request
   ...
   ```

   这次请求复用捕获体的 system prompt 与采样参数、换掉全部 messages，所以只共享前缀开头那一小段缓存。没开 split turn 时不发。checkpoint 失败、为空或被截断时只丢这一段，摘要照旧写回，原因记进 trace。
6. 解析不出摘要、请求失败、没有捕获或熔断打开时，扩展返回空结果，PiG 按自己的方式压缩。

两条硬约束：

- **payload 必须整体替换，不能逐字段重建。** 宿主的 `onPayload` 必须返回捕获的那份对象。若让宿主重新拼请求，模型 id 会被拆成裸名（例如 `group/deepseek` 变成 `deepseek`），system prompt、参数顺序和 cache 标记也可能变化，缓存前缀立刻失效。trace 里表现为 `inputTokens` 很高而 `cacheReadTokens` 接近 0。
- **`tools` 必须原样保留。** 工具声明属于缓存前缀的一部分；删掉或重排会让前缀失配。摘要指令会要求模型不要调用工具，但请求体里的 `tools` 仍保持捕获时的样子。

## 2. 装配与构建

`go.mod` 钉住的是 pig 随包发布的 SDK 版本：

```go
require github.com/MichaelKinsy/PiG/extensions/sdk v0.7.0
```

这个版本能直接从 module cache 解析，所以裸 `go build ./...`、`go vet ./...` 和编辑器都能正常工作，不需要额外配置。真正的构建与测试走的是 pig 本机暂存的那份 SDK：pig 安装扩展时会用自己的 `replace` 替换它；`sh test.sh` 会扫描 `$HOME/.pig/state/pigsdk/sdk-*`，在 `.cache/go.local.mod` 里生成同一个 `replace`，复制 `go.sum` 后以 `-mod=readonly` 跑 `go vet` 与 `go test`，保证测的就是 pig 会用来构建的那份源码。`.cache/` 已被忽略，不会污染包根目录。

`go.mod` 还必须声明 moejs 自己的两个间接依赖：

```go
require (
	github.com/MoeForge/moego v0.1.0-alpha.1 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
)
```

原因：pig 打包 Go 扩展时，从一个只带 `go.mod` 声明的生成模块构建（packed-cell）。缺了这两个 `// indirect`，本地宽松构建可能通过，但安装/打包会报：

```text
updates to go.mod needed
```

`test.sh` 固定 `-mod=readonly`，就是为了让这类缺失在本地直接失败，而不是等到安装时才发现。补依赖时不要用 `-mod=mod` 把问题掩盖掉。

本地自检：

```bash
sh test.sh
```

安装时用 pig 的常规安装流程，例如：

```bash
pig install /path/to/pig-cc-compaction
pig install --validate-only --json /path/to/pig-cc-compaction
```

扩展的注册身份是 `pig-cc-compaction`，**目录名也要是 `pig-cc-compaction`**；直接安装一个叫 `root` 之类的目录会报 `name_mismatch`（见验证指南的排错清单）。

## 3. 前置条件

- 本扩展本身不需要 cgo，但 PiG SDK 会引入 `net/http`。本机没有 C 编译器时，Go 默认的 cgo 构建会失败；把 `CGO_ENABLED=0` 写进 Go 的环境文件即可：

  ```bash
  go env -w CGO_ENABLED=0
  ```

  这条命令写入 `~/.config/go/env`，对之后所有 Go 构建生效。它只影响本机构建，不需要写进仓库。

- 需要能正常调用 provider 的模型；验证 prompt cache 时，provider 必须在 usage 里返回 cache read 计数（否则 trace 的命中率无法判断）。
- 需要一个已安装并可加载的 `pig-cc-compaction` 扩展；安装后可用 `/cc-compact status` 确认它已注册。

## 4. 环境变量

| 变量 | 作用 | 默认 |
| --- | --- | --- |
| `PIG_CC_PLUGIN` | 指定外部 JS 文件，替代内嵌的 `plugin/compaction.js`。改提示词时不必重新编译 Go。文件必须导出 `planCompact` 和 `parseSummary`；读取或编译失败会显示在 `/cc-compact status` 的 `plugin:` 行，并以错误通知提示。 | 内嵌 `plugin/compaction.js` |
| `PIG_CC_TRACE` | 覆盖 trace 文件路径。 | `~/.pi/agent/pi-cc-trace.jsonl` |

示例：

```bash
PIG_CC_PLUGIN=/tmp/my-compaction.js \
PIG_CC_TRACE=/tmp/cc-trace.jsonl \
pig
```

## 5. 命令

| 命令 | 行为 |
| --- | --- |
| `/cc-compact` 或 `/cc-compact status` | 打印本会话状态：插件来源（`embedded` 或外部文件/加载错误）、trace 路径、是否已有捕获（模型、provider、字节数、年龄）、熔断计数（`fails`、`thrash`、`disabled`），以及真正生效的压缩阈值（`reserve`、`keepRecent`、window）和两条会让压缩静默失效的配置警告，见 §8。 |
| `/cc-compact reset` | 清掉本会话的捕获和熔断状态，同时允许同一失败通知再次出现；下一次压缩会像新会话一样先让路给 PiG。 |
| `/cc-compact now [指令]` | 立即请求一次压缩。`[指令]` 会作为 `customInstructions` 进入摘要提示词的 **Additional Instructions** 段；不带指令时该段整体省略。 |

不认识的参数会打印用法提示：`usage: /cc-compact [status|reset|now [instructions]]`。

## 6. 失败、熔断与抖动

| 情况 | 行为 |
| --- | --- |
| `reason` 是 `overflow` | 直接让路。捕获的请求本身已经超过上下文上限，重发只会再溢出一次，由 PiG 自己处理 overflow 恢复。 |
| 没有捕获（新会话、扩展重启、刚执行过 reset） | 直接让路。让新会话或切模型后的新模型正常跑一轮，捕获会刷新，下一次压缩自动恢复接管。 |
| 连续 3 次失败 | 本会话停用**自动**接管，并通知 `Autocompact is paused...`。成功一次会清零失败计数并解除失败熔断；显式的 `/cc-compact now` 仍可手动接管。 |
| 30 秒内再次成功累计，连续 3 次成功（相邻成功间隔都小于 30 秒） | 判为抖动（thrashing），停用自动接管并提示 `Autocompact is thrashing...; /cc-compact reset re-arms it`。`/cc-compact reset` 可复位。 |
| 回复里没有可用的 `<summary>` | 没有块、小节名少于 5 个、或 provider 报 `length` 截断，都算不可用：先写一条 `retry` trace 说明原因，再用更严格的口吻（明确要求九个小节名）问一次，第二次仍失败则回退 PiG，并提示摘要不可用的原因。 |
| 网络/5xx/超时 | driver 最多试两次（第一次失败后等 250ms）；仍失败则记为 `failed` 并回退 PiG，同时计入失败次数。 |
| split turn 的 checkpoint 失败、为空或被截断 | 只丢这一段，摘要照常写回，`compacted` 的 `detail` 写明原因。它是附加信息，不是压缩结果，所以不计失败次数、不回退。 |

让路（`pass`）本身不是错误：溢出、没有捕获、熔断打开都走这条路；只有真正尝试接管后失败才计入熔断。

## 7. trace 与命中判据

每次压缩尝试向 trace 文件追加一行 JSON。字段：

| 字段 | 含义 |
| --- | --- |
| `ts` | 记录时间（RFC 3339）。 |
| `sessionId` | PiG 会话 ID。 |
| `outcome` | 压缩结果：`compacted`、`cancelled`、`fallback`、`failed`。另有两条不是压缩结果：`retry` 记一次被策略拒绝、但还有重试机会的回复（`detail` 写明是标题不足还是输出截断），`evicted` 记捕获表淘汰了会话（见 §8）。 |
| `detail` | 让路/失败原因；成功时通常省略，唯一例外是 split turn 的 checkpoint 被跳过，那时 `compacted` 也带一行 `split turn: ...`。 |
| `inputTokens` | 本次压缩请求的输入 token（非缓存部分）。 |
| `cacheReadTokens` | 从 provider cache 读取的 token。 |
| `outputTokens` | 模型输出 token。 |
| `durationMs` | 本次尝试耗时（毫秒）。 |

`outcome` 的完整取值集合是 `compacted|cancelled|fallback|failed`。当前 driver 会写 `compacted`、`fallback`、`failed`；`cancelled` 是保留值，当前没有主动写出的分支。

判断前缀是否真的命中：

```text
cacheReadTokens / (inputTokens + cacheReadTokens) > 0.9
```

简写就是 `cacheRead/(input+cacheRead) > 0.9`。只看 `cacheReadTokens > 0` 不够：比例接近全量才说明捕获的旧前缀被复用。若分母为 0，说明 provider 没有返回 usage，不能判为命中。典型失配是 `compacted` 但 `cacheReadTokens` 为 0、`inputTokens` 接近整段上下文。

## 8. 已知限制

- **失败原因只看得到回复开头。** 每次被拒的回复都留一条 `retry` 行，`detail` 带 `stopReason`、`outputTokens`、判定原因（标题不足还是输出截断）和回复开头 240 字符，provider 拒绝时再补 `errorMessage`。回复全文不入 trace，也没有结尾片段，所以模型写到一半跑偏时只能从开头判断。
- **摘要门槛偏严，这是有意的取舍。** 摘要必须包含九个小节名中的至少 5 个，且每个节名独占一行；provider 报 `length` 时一律不收。换语言改写标题、或把九节写成散文的模型会被判为不可用，重试一次后回退 PiG。宁可回退，也不把截断的摘要写进会话。
- **切点由 PiG 决定。** 扩展使用 `preparation.firstKeptEntryId`，保留范围由 `keepRecentTokens` 控制；Claude Code 只保留最后一轮。想更接近 CC，把 `compaction.keepRecentTokens` 调小（例如 100 用于测试，8000 用于日常）。
- **跨会话状态只在本进程内共享。** 捕获和熔断表以会话为键，进程重启即丢失；内存表最多 32 个会话，超出按 LRU 淘汰并写一条 `evicted` trace。被淘汰的会话下一次压缩回退一次，熔断计数清零。宿主是每会话一个 runner，正常只会有一条记录，这个上限只在多会话共享进程时才起作用。
- **捕获是完整请求体的副本。** 大会话会占用相应内存；每个会话只保留最近一次请求，不保留历史。
- **阈值设错会让压缩静默失效。** `compaction.keepRecentTokens` 不小于会话现有 token 时，PiG 判定切点覆盖整个会话，直接返回 `Nothing to compact (session too small)`，连 `session_before_compact` 都不发，扩展既不运行也不留 trace。`keepRecentTokens` 超过窗口一半同样是坏配置，切点几乎总落在某一轮中间，每次压缩都保留几乎全部上下文。`/cc-compact status` 会就这两种情况给出警告，并打印实际生效的数值。
- **checkpoint 是第二次模型调用，只共享很短的缓存前缀。** 它换掉了全部 messages，只有 system prompt 与采样参数沿用捕获体，所以这一小段输入按全价计费。split turn 打开时一次压缩就是两次请求。
- **checkpoint 不走九段门槛。** 它有自己的小节结构（`## Original Request` 等），只检查回复是否正常结束，不数标题。模型写得很简略时，这一段就只有一两句。
- **`/cc-compact now` 依赖宿主提供 `session_before_compact`。** 若 PiG 的压缩被设置关闭，或当前模型没有上下文窗口信息，扩展不会被调用。

更细的端到端验证和排错步骤见 [`docs/verify.md`](docs/verify.md)。
