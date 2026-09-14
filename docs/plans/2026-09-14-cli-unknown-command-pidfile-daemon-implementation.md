# Issue #19：未知 CLI 参数与 pidfile 接管实施计划

- 状态：**已确认**（用户于 2026-09-14 批准规格并授权实现）
- 规格：[docs/specs/2026-09-14-cli-unknown-command-pidfile-daemon-design.md](../specs/2026-09-14-cli-unknown-command-pidfile-daemon-design.md)
- 依据：[Issue #19](https://github.com/opentokenz/mcpx/issues/19)

## 目标

未知位置参数在任何副作用前以退出码 2 拒绝；`mcpx` / `mcpx -d` / `mcpx stop` 只 terminate pidfile 中且命令行仍匹配的 `-d` daemon。

## 范围

- `cmd/mcpx-server/main.go`
- `cmd/mcpx-server/background.go`
- `cmd/mcpx-server/background_unix.go`（删除扫描）
- `cmd/mcpx-server/background_windows.go`（删除空扫描桩）
- `cmd/mcpx-server/main_test.go`
- `cmd/mcpx-server/background_unix_test.go`

## 非目标

- 不改 MCP 工具协议、配置、环境变量。
- 不改 PR #15 Windows 对已知 daemon PID 的进程树终止。
- 不提交、不推送、不关远程 Issue。
- 不实现 #20 / #12 / #21。

## 步骤

### 1. 未知位置参数 fail-closed

前置：规格已批准。

改动：第一个参数不是已知子命令、且不是 `-` 开头的 flag 时，调用 `printUsage()` 并以退出码 2 结束，发生在 `flag.Parse` 与 `stopExistingBackground` 之前。抽出可单测的判定函数。

预期：`skills`、`skills --help` 判定为未知；`-addr`、`observe`、`--help` 不判定为未知。

验证：包测试覆盖判定与退出码 2；stderr 含 `Usage:`。

失败：只改 CLI 分发，不碰 daemon 停止逻辑。

### 2. pidfile-only 接管

前置：步骤 1 不阻塞本步。

改动：`stopPreviousBackground` 去掉 `discoverBackgroundProcesses`。无 pidfile 时直接返回空列表。删除 Unix `discoverBackgroundProcesses` / `parseDetachedBackgroundProcesses` 及对应测试。删除 Windows 空实现 `discoverBackgroundProcesses`。保留 `backgroundCommandMatches` 与对已知 PID 的 `terminateBackgroundProcess`。

预期：无 pidfile 不扫进程；陈旧/复用 PID 行为与现测一致。

验证：现有 `TestStopPreviousBackground*` 仍过；新增无 pidfile 为 noop 的测试。

失败：还原扫描调用。

### 3. 验收

验证命令：

```bash
go test ./cmd/mcpx-server -count=1
gofmt -l ./cmd/mcpx-server
go vet ./cmd/mcpx-server
```

里程碑：步骤 1+2 代码落地后跑上述命令。不启动真实 MCP 服务。

回滚：还原 `main.go` default 与 `stopPreviousBackground` 扫描。
