# Issue #19：未知 CLI 参数与 pidfile 接管实施报告

## 最终状态

通过。计划范围内的 CLI 拒绝路径与 pidfile-only 接管已落地，并有包测试与一次本地 `go run` 证据。

生成时间：2026-09-14

## 依据与授权

- 规格：`docs/specs/2026-09-14-cli-unknown-command-pidfile-daemon-design.md`
- 计划：`docs/plans/2026-09-14-cli-unknown-command-pidfile-daemon-implementation.md`
- 外部依据：[Issue #19](https://github.com/opentokenz/mcpx/issues/19)
- 实现授权：用户回复「批准并开始实现」。
- 本轮未执行 commit、push、关闭远程 Issue。

## 执行范围

只改 `cmd/mcpx-server` 的启动分发与后台停止。未改 MCP 工具协议、配置、Windows 进程树终止（PR #15）、#20 / #12 / #21。

## 计划步骤对照

### 1. 未知位置参数 fail-closed

- 实际实现：`isUnknownPositionalArg`；未知位置参数调用 `unknownCommandStatus()`（打印用法，返回 2），发生在 `flag.Parse` 与 `stopExistingBackground` 之前。`-` 开头仍走服务 flag。
- 验证证据：`TestIsUnknownPositionalArg`、`TestUnknownCommandStatusPrintsUsageAndExitsTwo`；`go run ./cmd/mcpx-server skills --help` 打印 Usage 且进程为 `exit status 2`，无 `stopped previous background daemon`。
- 状态：已实现，已验证。
- 差异：无。

### 2. pidfile-only 接管

- 实际实现：`stopPreviousBackground` 只读 pidfile，用记录中的 `Executable` 调用 `terminateBackgroundProcess`，然后删除 pidfile。删除 Unix 扫描与 Windows 空桩 `discoverBackgroundProcesses`。
- 验证证据：原有 `TestStopPreviousBackgroundRemovesInvalidState`、`TestStopPreviousBackgroundDiscardsReusedPID`；新增 `TestStopPreviousBackgroundWithoutPidfileIsNoop`、`TestStopPreviousBackgroundCorruptStateReturnsError`。
- 状态：已实现，已验证。
- 差异：函数签名去掉未再使用的 `executable` 参数；匹配对象改为 pidfile 内记录的路径。不影响 `-d` 互相替换。

### 3. 验收命令

- `rtk go test ./cmd/mcpx-server -count=1`：28 passed
- `rtk gofmt -w ./cmd/mcpx-server` 与 `rtk go vet ./cmd/mcpx-server`：通过
- 状态：已验证
- 未执行：`go test ./...` 全量、真实 systemd 误杀复现、Windows 真机。

## 实际修改文件

- `cmd/mcpx-server/main.go`
- `cmd/mcpx-server/main_test.go`
- `cmd/mcpx-server/background.go`
- `cmd/mcpx-server/background_unix.go`
- `cmd/mcpx-server/background_unix_test.go`
- `cmd/mcpx-server/background_windows.go`
- `cmd/mcpx-server/background_windows_test.go`
- `docs/specs/2026-09-14-cli-unknown-command-pidfile-daemon-design.md`
- `docs/plans/2026-09-14-cli-unknown-command-pidfile-daemon-implementation.md`
- `docs/results/2026-09-14-cli-unknown-command-pidfile-daemon-implementation-report.md`

## 未验证与剩余风险

- 未在 Linux systemd `Restart=on-failure` 下复现「误杀后不拉起」再验证修复。
- 未在 Windows 上跑本包测试（本机为 macOS）。
- `go run` 对非零子进程自身退出码为 1，但日志含 `exit status 2`，产品退出码仍为 2。

## 后续动作

- 用户决定是否 commit / push / 关闭 Issue #19。
- 下一份规格：#20 `last_active_at`。
