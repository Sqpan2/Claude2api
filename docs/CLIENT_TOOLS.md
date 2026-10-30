# Client tools: Claude2api -> magpie -> Claude Code

This `dev` branch implements client tool calls on `POST /v1/messages`.
Claude2api requests one web-model completion per API request, translates the
model's tool instructions into Anthropic `tool_use` blocks, then returns control
to the client. Claude Code runs the tools on the computer where Claude Code is
running, subject to its normal permissions. magpie forwards the API messages.

**Behavior change:** the proxy no longer runs Bash, Read, Write, Edit, Glob, Grep,
or any other client tool. There is no server-execution fallback. Clients that
previously relied on the server's internal tool loop must now execute tools and
send back their results.

## Protocol

1. Send `tools` with JSON `input_schema` definitions. The proxy supplies these
   definitions and the requested `tool_choice` to the web model as text.
2. The model emits `[TOOL_CALL]{"name":"Read","input":{"file_path":"..."}}[/TOOL_CALL]`.
   The proxy parses the JSON, checks the tool name and input schema, and assigns
   a unique `toolu_...` ID. It preserves text and parallel calls in their order.
3. The response contains `{"type":"tool_use","id":"toolu_...","name":"Read","input":{...}}`
   with `stop_reason: "tool_use"`. It contains no proxy-generated tool results.
4. Append that assistant response and a user message containing
   `{"type":"tool_result","tool_use_id":"toolu_...","content":"..."}`.
   Nested content blocks and `is_error: true` are preserved in the next prompt.
   Return all parallel results in the immediately following user message.
5. Repeat as needed. A text-only answer ends with `stop_reason: "end_turn"`.
   Final turns may omit the tool definitions while keeping the full history.

`tool_choice` supports `auto` (default), `none`, `any`, and
`{"type":"tool","name":"Read"}`. `disable_parallel_tool_use: true` restricts
the response to one call. At most 64 calls are accepted per response. Unknown
tools, invalid inputs, incomplete markers, or violations of `tool_choice` fail
the entire turn. JSON Schema local references work; external schema references
are rejected without network access.

With `stream: true`, every SSE frame has a matching `event:` and JSON `type`.
Tool requests send `message_start` immediately and `ping` while waiting. The
proxy buffers the model's entire tool turn for validation, then emits:

```text
event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_...","name":"Read","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":\"/local/project/a.txt\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0}}

event: message_stop
data: {"type":"message_stop"}
```

The tool input is empty in `content_block_start`; its arguments appear once in
`input_json_delta`. Upstream errors, truncated streams, and invalid tool output
produce HTTP 502 before streaming, or an SSE `error` after streaming starts,
without a successful `message_stop`. Invalid client tool definitions or result
IDs produce HTTP 400 before any upstream request. Tool schema regular expressions
use regexp2's ECMAScript mode, including lookaheads and backreferences, for both
schema compilation and tool argument validation. Each match has a 100 ms timeout;
a timeout rejects the value. External schema references cannot read network or
file resources. Ordinary text-only requests
continue to stream incremental text.

## magpie Configuration

Build this branch; upstream `latest` images do not contain this change:

```bash
git clone --branch dev https://github.com/Sqpan2/Claude2api.git
cd Claude2api
go build -o claude2api .
CLAUDE_SESSION_KEY="$CLAUDE_SESSION_KEY" PORT=8080 ./claude2api
```

Set `CLAUDE_SESSION_KEY` to your web session key in the service environment.
Select a model your account can actually use. `GET /v1/models` lists the web
catalog, which can also contain models unavailable to your plan.

For magpie on the same host, add a **custom Anthropic provider**, rather than
using magpie's built-in Claude subscription provider:

```bash
magpie provider add "Claude Web Dev" id=claude-web-dev \
  anthropic=http://127.0.0.1:8080 \
  key="$CLAUDE_SESSION_KEY" models="$CLAUDE_MODEL" catalog=anthropic
```

Set `CLAUDE_MODEL` to the actual web model ID before running this command. The
Anthropic base URL ends at the host/port; magpie appends `/v1/messages`.
For Docker, build with `docker build -t claude2api:dev .`, put the proxy and
magpie on the same private network, and use a reachable service address such as
`anthropic=http://claude2api-dev:8080`. `127.0.0.1` inside the magpie container
refers to that container.

Authentication in the existing proxy uses web credentials, not an independent
gateway key. An explicit Bearer token selects that web session and overrides
the server's configured Cookie/account pool. **Do not use a dummy provider key.**
The example above uses the real session key. For full browser Cookie mode, also
set the custom provider header `X-Claude-Cookie` to the full Cookie; it takes
precedence over Bearer. Keep these credentials private, and keep Claude2api on
a trusted internal network. Use magpie's own gateway authentication for clients.

On your computer, point Claude Code to magpie's gateway and select
`claude-web-dev/<web-model-id>`. For example, with a gateway key created in magpie:

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:3425 \
ANTHROPIC_API_KEY="$MAGPIE_GATEWAY_KEY" \
claude --model "claude-web-dev/$CLAUDE_MODEL"
```

For a remote gateway, use its configured HTTPS endpoint or a local SSH tunnel.
Run Claude Code in your local project directory. The model and proxy may be
remote; Read/Write/Bash run where that Claude Code process runs.

## Limits And Verification

- This is prompt-based tool emulation over the web interface. Schema validation
  catches invalid requests but cannot guarantee that the model chooses the
  right tool or produces usable instructions on every turn.
- This change does not add OpenAI `tool_calls`, Responses function calls, server
  tools, signed thinking blocks, native prompt caching, image/file uploads, or
  the full Anthropic API feature set. Structured tool-result content is kept as
  JSON text; embedded images are not uploaded as vision inputs.
- Tool streams are buffered per turn. `usage` is approximate, without fabricated
  cache hits. Web-side generation limits still apply; `max_tokens`, sampling,
  stop sequences and thinking budgets are not native API guarantees.
- Free-account login, plan quotas, model permissions, Cookie expiry and
  Cloudflare challenges remain upstream constraints. Protocol compatibility does
  not prove that a particular Free session can generate successfully.

```bash
go test ./...
go test -race ./...
go vet ./...
```

The handler tests use a local web-upstream fixture and exercise streaming and
JSON responses, parallel calls, multiple client tool rounds, local permission
errors, schema validation, cancellation, and truncated upstream streams. They
also verify that suggested Write and Bash calls never execute on the proxy.

On 2026-10-29, a separate integration check used the built proxy and a simulated
web model on an OCI instance, magpie v0.1.1729 in an isolated container, and
Claude Code v2.1.311 on macOS through an SSH tunnel. Claude Code executed a Bash
call to create a temporary local file, returned the result, executed a Read call
on the next turn, and received `LOCAL_TOOL_ROUNDTRIP_OK`. The file existed only
on the client; the same path was absent on the instance. The temporary services
were removed after the check. This verifies the protocol and execution location,
not authenticated Free-account generation.

## 中文接入说明

此分支的执行顺序为：网页模型输出工具指令 -> Claude2api 校验并转换为标准
`tool_use` -> magpie 转发 -> **你电脑上的 Claude Code 执行** -> 将标准
`tool_result.tool_use_id` 回传 -> 下一轮模型回复。代理不再执行任何客户端工具，
不会在云端容器里代替本机写文件或运行 Bash。

请使用自己构建的 `dev` 版本，在 magpie 添加自定义 **Anthropic** 提供商，
地址填写代理服务根地址，选择账号实际可用的模型。magpie 的内置 Claude 订阅
接入与这个自定义提供商是两条不同的认证路径。

提供商 key 使用真实的网页 sessionKey；不能随便填写占位 key，因为显式 Bearer
会覆盖服务器环境里的 Cookie/账号池。需要完整浏览器 Cookie 时，在提供商的
自定义 Header 中配置 `X-Claude-Cookie`。本机到 magpie 使用 magpie 自己的
网关密钥，代理到 claude.ai 才使用网页凭据。

工具流会等待整轮生成结束并校验后返回，不是逐字符工具参数流。此改造只实现
`/v1/messages` 的客户端工具协议；不会解除 Free 额度、模型权限或 Cloudflare
限制。正式使用仍需用有效的 Free 会话凭据验证真实生成。
