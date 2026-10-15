# 流可靠性加固（harden 分支）

本分支在原有网关之上加了一层"流可靠性"逻辑，目标只有一个：**把上游的空流、错流、挂死流在客户端可见之前处理掉**，并且不给正常运行的长流添乱。所有改动都可以通过配置关闭，关掉后行为与原版一致。

## 配置

```json
"hardening": {
  "hold_first_event_seconds": 120,
  "idle_event_seconds": 300,
  "keepalive_seconds": 10,
  "zero_output_retries": 1
}
```

| 字段 | 默认 | 作用 | 设为 0 |
| --- | --- | --- | --- |
| `hold_first_event_seconds` | 120 | 单次尝试最久等多久才拿到**第一个内容事件**；超时即放弃该节点并轮换 | 关闭首事件等待 |
| `idle_event_seconds` | 300 | 上游**完全不出字节**多久算挂死；提交前用于中止 hold，提交后用于中止回传 | 关闭空闲看门狗 |
| `keepalive_seconds` | 10 | 下游没有真实字节时，向下游补 `: keep-alive` 注释帧的间隔 | 关闭保温帧 |
| `zero_output_retries` | 1 | 非流式请求拿到"零输出文档"后，额外重派的轮数 | 不重派 |

`hold_first_event_seconds` 与 `idle_event_seconds` 同时为 0 时，整个 hold 阶段不启用（等价原版"拿到响应头就交给回传"）。

## 行为变化

### 1. 提交前 hold（对客户端不可见的重试）

上游返回 2xx 之后、任何字节发给客户端之前，网关会先把流头读出来判定：

- 出现**内容事件**（正文、思考、工具调用）→ 立即进入正常回传，已读字节原样重放，一个字节不丢。
- 流**结束/报错**但没有任何内容（含"只发 `[DONE]`"、"只发错误帧"、EOF）→ 视为该次尝试失败：冷却该节点、换下一个 key/代理重试。客户端完全看不到这次失败。
- **空闲超时**（`idle_event_seconds` 内没有任何字节）或 **hold 超时** → 同上，换节点重试。

关键约束：**最后一个可用尝试不启用 hold**。当重试预算只剩一次时，请求退回原版的直通行为——慢模型（首 token 很慢但活着）不会被 hold 砍掉，客户端也能尽早拿到响应头。

### 2. 提交后的空闲看门狗

流已经开始向客户端回传后，如果上游在 `idle_event_seconds` 内一个字节都不出，网关主动断开并向客户端发出协议正确的终止错误帧（Chat 为 error 帧 + `[DONE]`，Anthropic 为 `error` 事件，Responses 为 `response.failed`），请求日志记为 `idle_timeout`。这是"挂死"与"生成慢"的唯一区分手段——只有完全无字节才算挂死。

### 3. 下游保温帧

回传期间若 `keepalive_seconds` 内没有任何真实字节写出，网关会写 `: keep-alive` 注释帧。SSE 客户端会忽略注释帧，但它们的读超时会被刷新，长思考停顿不再触发客户端断连。写入由互斥锁串行化，不会与真实帧交错。

### 4. 非流式零输出重派

`stream:false` 的请求（含匿名通道的折叠路径）如果拿到 2xx 但文档没有任何输出（空 choices / 空 content / 空 output），在**尚未写任何响应**的前提下按 `zero_output_retries` 重派一次，换节点再试。

### 5. 轮转状态批量落盘

`rotation` 状态原先每次尝试都 marshal + fsync + rename；现在热路径只做脏标记，由运行时每 2 秒 flush 一次，进程退出前再 flush 一次。崩溃最多丢 2 秒尝试历史。`/api/rotation` 行为不变。

### 6. 小修

- 上游错误摘录按 UTF-8 边界截断（原先按字节截断可能切坏中文/emoji）。
- SSE 单行上限 16 MiB → 32 MiB。
- 单 key 池遇到 401/403 不再把重试预算耗在同一把 key 上（同一请求内每把 key 只试一次）。

### 7. 模型限额覆盖（models.limits）

上游能力目录对部分免费模型报的上下文偏保守：免费 `deepseek-v4.1-flash-free` 元数据写 200k，而匿名通道实测**接受到 2^20 = 1,048,576 token**（1,048,222 token 的请求 200 且能准确取回文档首尾信息，1,057,000 起被上游 400 拒绝）。客户端按 `/v1/models` 规划上下文，于是跟着错。用 `models.limits` 把网关**对外报的**数字改成实测值：

```json
"models": {
  "limits": {
    "deepseek-v4.1-flash-free": {
      "context_window": 1048576,
      "max_output": 32000
    }
  }
}
```

- 语义：覆盖 `MetadataForTier`（即 `/v1/models` 与 WebUI 展示的元数据），在能力目录与 models.dev 之后**最后**应用；`max_input`、`max_output` 同法可覆盖，未写的字段保持原值，`0` 视为非法。
- 边界：它只改**网关报出去的数字**，不改变上游实际强制的限额；写错只会让客户端规划失真，不会让网关"放行"超限请求。
- 该字段随 `models` 配置块在 WebUI 的 `PUT /api/config` 往返中保留，但当前没有 WebUI 编辑入口，直接改 `config.json` 即可（保存即热生效）。

## 可观测性

新的日志事件（`component`/`event`）：

| 事件 | 含义 |
| --- | --- |
| `attempt_empty` / `anonymous_attempt_empty` | 某次尝试在 hold 阶段被判定为零输出并轮换 |
| `zero_output_replay` | 非流式零输出文档触发重派 |
| `stream_idle_timeout` | 已提交的流被空闲看门狗中止 |
| `attempt_auth_exhausted` | 所有 key 都拒绝了本次请求，停止轮换 |

请求日志中的取值：`outcome` 可为 `empty_stream`、`idle_timeout`；`legs` 中某一段可能以 `empty`、`sse_error`、`hold_timeout`、`idle` 结尾。

## 测试

```shell
go test ./...
go test -race -count=1 ./...
```

新增覆盖：

- `internal/protocol/stream_transcode_test.go` —— 三协议转换、终止纪律、折叠、流首分类、hold 字节保真。
- `internal/gateway/hardening_test.go` —— 假上游集成：空流换 key、错误帧换 key、hold 空闲轮换、末次尝试不 hold、保温帧、空闲看门狗、非流式零输出重派/预算。
- `internal/gateway/pool_race_probe_test.go`、`internal/models/catalog_race_probe_test.go` —— 池管理面写 vs 请求面读、目录热替换 vs 路由读的 `-race` 探针。
- `internal/rotation/state_test.go` —— 批量落盘契约与失败重试。

## 与原版对齐的边界

- 未改动任何协议转换/指纹/池轮换逻辑，`go test -race` 全绿。
- `hardening` 字段在 WebUI 的 `PUT /api/config` 往返中保持不变（与 `rotation` 一样由服务端保留），当前不提供 WebUI 编辑入口。
- 原 `performance.first_event_timeout_seconds` 仍然生效（传输层等待）；新 hold 工作在网关层，两者互不冲突。
