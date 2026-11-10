# 端到端验证指南

目标：在真实 PiG 会话里确认本扩展确实接管了压缩、重发请求命中了 prompt cache、摘要格式正确，并且边界情况会安全回退给 PiG。

验证环境：`pig 0.7.0+1.5.0`（Pi 1.5.0 fork），Linux。下面命令默认在一次性目录 `/tmp/cctest` 里跑，不会修改仓库。

## 0. 准备

1. 本机 Go 构建可用。扩展本身不需要 cgo，但 SDK 会引入 `net/http`；没有 C 编译器时先执行 `go env -w CGO_ENABLED=0`（写入 `~/.config/go/env`），否则构建会失败。
2. 扩展已安装，并能在 `/cc-compact status` 里看到 `pig-cc-compaction status`。如果还没安装：

   ```bash
   repo=/absolute/path/to/pig-cc-compaction
   PIG_USE_PI_DIRS=1 pig install "$repo"
   ```

   注意 `repo` 的最后一级目录必须是 `pig-cc-compaction`；直接安装一个叫 `root` 的目录会得到 `name_mismatch`。如果不用 `PIG_USE_PI_DIRS=1`，把下文的 `~/.pi` 换成 `~/.pig`，项目设置目录也从 `.pi` 换成 `.pig`。

3. 已经登录至少一个支持 prompt cache、且 usage 里返回 cache read token 的 provider；`pig` 的默认模型可用。
4. 仓库根执行 `sh test.sh` 全绿。单元测试覆盖 JS 策略、driver、捕获表和命令解析；本指南只验证真实宿主链路。

## 1. 跑一次手动压缩

`/tmp/cctest/.pi/settings.json` 把 `keepRecentTokens` 调到 100，让手动压缩一定有可压缩内容：

```bash
set -eu
repo=/absolute/path/to/pig-cc-compaction
rm -rf /tmp/cctest
mkdir -p /tmp/cctest/.pi
cp "$repo/README.md" "$repo/pig-cc-compaction.md" /tmp/cctest/
printf '%s\n' '{"compaction":{"keepRecentTokens":100}}' > /tmp/cctest/.pi/settings.json
cd /tmp/cctest

{
  printf '%s\n' '{"id":"p1","type":"prompt","message":"Read README.md and pig-cc-compaction.md, then reply exactly: done"}'
  sleep 60
  printf '%s\n' '{"id":"c1","type":"compact"}'
  sleep 60
} | PIG_USE_PI_DIRS=1 pig --mode rpc --approve --no-lens --no-lsp > rpc.jsonl 2> pig.err
```

说明：

- `--mode rpc` 让 PiG 用 JSON Lines 通信；`--approve` 信任项目设置；`--no-lens --no-lsp` 关掉诊断扩展，避免额外请求干扰 cache 观察。
- `{"id":"c1","type":"compact"}` 是 RPC 的手动压缩命令，等价于用户在会话里执行 `/compact`。扩展会收到 `reason: "manual"` 的 `session_before_compact`。
- 第一条 prompt 必须先完成一次 provider 请求，扩展才有可捕获的请求体。等待时间按你的模型调整。
- 命令结束后检查有没有压缩事件：

  ```bash
  grep -a '"type":"compaction_end"' rpc.jsonl
  grep -a '"command":"compact"' rpc.jsonl
  ```

## 2. 五条断言

### 断言 1：`session_compact` 的 `fromExtension` 为 `true`

扩展接管后，PiG 发出的 `session_compact` 事件里 `fromExtension` 必须是 `true`，而不是 PiG 自己生成的摘要。RPC 客户端只转发 agent 事件，不转发扩展事件，所以脚本用持久化条目做等价检查：compaction 条目的 `fromHook` 就是事件里的 `fromExtension`。

```bash
agent_dir=$HOME/.pi/agent          # 未设置 PIG_USE_PI_DIRS=1 时是 ~/.pig/agent
session_file=$(find "$agent_dir/sessions/--tmp-cctest--" -name '*.jsonl' -type f | sort | tail -n 1)
jq -e 'select(.type == "compaction") | .fromHook == true' "$session_file" >/dev/null
jq -r 'select(.type == "compaction") | {fromHook, firstKeptEntryId, tokensBefore}' "$session_file" | tail -n 1
```

判据：输出里 `fromHook` 为 `true`。如果你另装了一个订阅 `session_compact` 的扩展，直接断言 `event.fromExtension === true` 即可；若它是 `false`，说明这次摘要是 PiG 自己写的，检查 trace 的 `outcome` 和 `detail`。

### 断言 2：trace 的 cache read 比例大于 0.9

```bash
trace=${PIG_CC_TRACE:-$HOME/.pi/agent/pi-cc-trace.jsonl}
tail -n 5 "$trace"
jq -e -s '
  map(select(.outcome == "compacted")) | last |
  ((.inputTokens + .cacheReadTokens) > 0) and
  ((.cacheReadTokens / (.inputTokens + .cacheReadTokens)) > 0.9)
' "$trace"
```

判据：上面的 `jq -e` 退出码为 0。人读时看最近一条：

```sh
input=1200 cache=48000
cache / (input + cache) = 48000 / 49200 ≈ 0.976
```

大于 0.9 才说明捕获的旧前缀被 provider 复用了。只有 `cacheReadTokens` 大于 0 还不够；如果 `cacheReadTokens` 为 0、`inputTokens` 接近整段上下文，说明 payload 被重建或前缀变了。分母为 0 时 provider 没有返回 usage，不能判为命中。

### 断言 3：摘要包含九段标题，且正文没有 `<analysis>`

从 RPC 的 `compaction_end.result.summary` 取出摘要：

```bash
jq -r 'select(.type == "compaction_end" and .result.summary != null) | .result.summary' rpc.jsonl > summary.txt
test -s summary.txt
# 摘要正文可能引用 <analysis> 这个词（它在描述指令本身），只拒绝以分析块开头的回复。
! head -n 1 summary.txt | grep -q '^<analysis>'
```

九段标题逐条检查：

```bash
while IFS= read -r title; do
  grep -qF "$title" summary.txt || { echo "missing: $title"; exit 1; }
done <<'EOF'
1. Primary Request and Intent
2. Key Technical Concepts
3. Files and Code Sections
4. Errors and fixes
5. Problem Solving
6. All user messages
7. Pending Tasks
8. Current Work
9. Optional Next Step
EOF
sed -n '1,40p' summary.txt
```

判据：九条 `grep` 全过（标题带序号或加粗都算，例如 `1. **Primary Request and Intent**:`）；`summary.txt` 的第一行不是 `<analysis>`。正文里出现 `<analysis>` 字样不算失败：摘要可以引用它正在描述的标签。

解析器在回复里枚举每个 `<summary>...</summary>` 候选，按"独占一行的九节标题数"择优，所以模型把指令原样引用回来时，引用里那个字面 `</summary>` 不会把摘要截断，引用本身也拿不到分。候选标题数少于 5 个、或 provider 的 `stopReason` 是 `length`，都判为不可用：重试一次后回退 PiG，而不是把残段写进会话。

摘要末尾还应带 PiG 自己也会写的那段文件清单：

```bash
grep -qE '^<read-files>$|^<modified-files>$' summary.txt && echo 'file tail present'
```

判据：这轮会话读过或改过文件时，`summary.txt` 末尾出现 `<read-files>` / `<modified-files>` 块，路径一行一个且已排序，同一个文件不会同时出现在两个块里（改过的文件算 modified）。扩展会把上一次压缩留下的清单并进来，所以连续压缩后更早的路径不会消失；PiG 自己的压缩路径只在条目 `fromHook` 为 `false` 时才继承清单，接管路径靠摘要正文继承。

切点落在某一轮中间时，摘要里还会多一段 PiG 自己的 turn 上下文标记：

```bash
grep -n '^\*\*Turn Context (split turn):\*\*$' summary.txt
```

判据：`compaction.keepRecentTokens` 比当前会话小很多时（例如 1500），切点通常落在轮中，这里应命中，且后面紧跟 `## Original Request` / `## Progress So Far` / `## Context Needed to Continue` 三个小节，位置在九段摘要之后、`<read-files>` 之前。切点正好落在某一轮开头时不该出现，那时 `compacted` 的 `detail` 为空。

若 `detail` 写着 `split turn: the preparation carried no turn prefix messages`，说明宿主给的 `preparation.turnPrefixMessages` 是空的，这一轮没有可总结的前半段；写着 `split turn: the prefix request failed (...)` 则是那次附加请求失败，摘要本身仍然完整。

### 断言 4：`/cc-compact now 只保留文件名` 的 Additional Instructions 生效

再开一次 RPC，用扩展命令而不是 RPC 的 `compact`：

```bash
cd /tmp/cctest
{
  printf '%s\n' '{"id":"p2","type":"prompt","message":"Read README.md and pig-cc-compaction.md, then reply exactly: done"}'
  sleep 60
  printf '%s\n' '{"id":"cmd1","type":"prompt","message":"/cc-compact now 只保留文件名"}'
  sleep 60
} | PIG_USE_PI_DIRS=1 pig --mode rpc --approve --no-lens --no-lsp > now.jsonl 2> now.err

jq -r 'select(.type == "compaction_end" and .result.summary != null) | .result.summary' now.jsonl > now-summary.txt
test -s now-summary.txt
awk '/3\. Files and Code Sections/,/4\. Errors and fixes/' now-summary.txt
```

判据：这次摘要在 `Files and Code Sections` 里只列文件名/路径，不再贴大段代码片段。说明 `customInstructions` 已经作为 **Additional Instructions** 进入摘要提示词。

本地还可先跑确定性的单元测试，确认指令拼接本身没问题：

```bash
GOFLAGS=-mod=readonly go test -modfile=.cache/go.local.mod -run 'TestPlanCustomInstructions|TestCompactCommandNowCarriesInstructions|TestParseSummaryGatesIncompleteReplies|TestBeforeCompactAppendsFileOperations|TestBeforeCompactAppendsTurnContext|TestSummarizeTurnPrefixPromptKeepsPisShape|TestDecodeCompactEventKeepsTheTurnPrefix|TestSettingsReport'
```

（该命令需要先在仓库根跑过一次 `sh test.sh` 生成 `.cache/go.local.mod`。）

### 断言 5：边界情况回退 PiG，并在 trace 里留下证据

**新会话 / reset 后没有捕获。** 在已跑过一轮的会话里执行 reset，再手动压缩：

```bash
trace=${PIG_CC_TRACE:-$HOME/.pi/agent/pi-cc-trace.jsonl}
{
  printf '%s\n' '{"id":"p3","type":"prompt","message":"Say done"}'
  sleep 30
  printf '%s\n' '{"id":"r1","type":"prompt","message":"/cc-compact reset"}'
  sleep 1
  printf '%s\n' '{"id":"c2","type":"compact"}'
  sleep 30
} | PIG_USE_PI_DIRS=1 pig --mode rpc --approve --no-lens --no-lsp > edge.jsonl 2> edge.err

jq -e -s 'last | .outcome == "fallback" and (.detail | contains("no captured request"))' "$trace"
```

新会话第一次压缩、扩展重启后第一次压缩也是同一路径：没有捕获时策略直接 `pass`，trace 记 `fallback`，`detail` 以 `no captured request` 开头，PiG 自己完成压缩。

**切模型。** 用 RPC 的 `set_model` 切到一个新模型，然后立即压缩：

```json
{"id":"m1","type":"set_model","provider":"<provider>","modelId":"<modelId>"}
{"id":"c3","type":"compact"}
```

判据：这次压缩应回退给 PiG。若策略在旧捕获上让路，trace 是 `fallback`；若请求已经发出后才失败，trace 是 `failed`，同样把压缩交回 PiG。切模型后让新模型正常跑一轮，捕获刷新，下一次压缩应重新出现 `compacted`。

**overflow。** 用超大 prompt 或小上下文模型触发 provider 的上下文溢出。PiG 会带 `reason: "overflow"` 触发压缩；扩展必须直接让路：

```bash
jq -e -s 'last | .outcome == "fallback" and (.detail | startswith("overflow:"))' "$trace"
```

overflow 时不能重发捕获，否则只会二次溢出；trace 的 `detail` 会说明这一点。

## 3. 排错清单

| 现象 | 说明与处理 |
| --- | --- |
| packed-cell 构建报 `updates to go.mod needed` | `go.mod` 少了 moejs 的间接依赖。补上 `github.com/MoeForge/moego` 和 `go.yaml.in/yaml/v3` 两个 `// indirect`，然后在仓库根重跑 `sh test.sh`（它固定 `-mod=readonly`，能在本地复现）。 |
| `pig install --validate-only --json` 返回 `"registered": false` | 看同一 JSON 里的 `stderrLog` 路径，`cat` 它看扩展子进程的启动/注册 stderr。常见原因是插件编译失败、SDK 构建失败或身份不匹配。 |
| 错误码 `name_mismatch` | 直接以目录方式安装时，pig 拿目录名当扩展身份；本扩展注册名是 `pig-cc-compaction`，目录也必须叫这个名字。重命名目录，或通过 Package/Piglet 声明同名 member。 |
| `/cc-compact status` 显示 `capture: none` | 当前会话还没有成功发出过 provider 请求（新会话、刚 reset、扩展重启）。先正常发一轮 prompt。 |
| `/cc-compact status` 的 `plugin:` 是加载错误 | `PIG_CC_PLUGIN` 指向的文件不存在、不能编译，或没有导出 `planCompact` / `parseSummary`。清掉该变量可回到内嵌插件。 |
| trace 没有新行 | 确认扩展真的注册并收到了 `session_before_compact`；检查 `PIG_CC_TRACE` 是否指向可写路径。`/cc-compact status` 的 `trace:` 行会打印实际路径。 |
| trace 有 `compacted`，但 cache 比例很低 | 说明前缀没命中。检查是否整体替换了 payload、`tools` 是否原样保留、provider 是否真的返回 cache usage；同模型但模型 id 被拆成裸名也会导致全价。 |
| `session_compact.fromExtension` 为 `false` | 这次摘要是 PiG 自己写的。先看 trace：`fallback` 表示策略让路（无捕获/overflow/熔断），`failed` 表示请求或模型解析失败。 |
| `/cc-compact status` 出现 `Nothing to compact (session too small)` 警告 | `compaction.keepRecentTokens` 不小于会话现有 token，PiG 会拒绝压缩且根本不发 `session_before_compact`，扩展不会被调用。把它调到日常值（8000 左右）。 |
| 摘要里没有 `**Turn Context (split turn):**` | 切点落在某一轮开头时本来就没有。若 `keepRecentTokens` 很小却一直没有，先看 `compacted` 行的 `detail` 写了什么跳过原因。 |
| 自动压缩不再接管，但手动 `/cc-compact now` 还行 | 连续失败或抖动把自动接管熔断了。看 `/cc-compact status` 的 `breaker:` 行；用 `/cc-compact reset` 复位。 |

## 4. 清理

```bash
rm -rf /tmp/cctest
rm -f "$HOME/.pi/agent/pi-cc-trace.jsonl"
```

如果验证期间设置过 `PIG_CC_TRACE` / `PIG_CC_PLUGIN`，在退出 shell 前 unset 即可。
