# MCPX：未知 CLI 参数与 pidfile 接管

- 记录日期：2026-09-14
- 状态：书面规格已批准；实现已授权（2026-09-14）
- 依据：[Issue #19](https://github.com/opentokenz/mcpx/issues/19)
- 方案：未知位置参数 fail-closed；启动与 `stop` 只 terminate pidfile 中的 `-d` daemon
- 宿主 / 运维入口：本机 CLI（`mcpx`）；与 MCP 工具协议无关

## 1. 背景、问题与目标

### 1.1 问题

当前 `cmd/mcpx-server/main.go` 的子命令 `switch` 没有 `default`。第一个位置参数若不是已知子命令，会落到普通服务启动。`flag.Parse()` 在第一个位置参数处停止，因此 `mcpx skills --help` 既不会被当成帮助，也不会被当成未知命令。

启动路径在加载配置、绑定端口之前调用 `stopExistingBackground()`。该函数除 pidfile 外，还会扫描命令行匹配当前可执行文件的 session leader，并对其发送 `SIGTERM`。于是：

1. 未知参数会先杀掉已在跑的实例；
2. 新进程随后才读配置、绑端口，失败则无人再监听。

Issue 报告于 v0.9.9（`3f81d08`），Linux，systemd `Restart=on-failure`。`SIGTERM` 被 systemd 视为干净退出，`on-failure` 不会拉起。

### 1.2 目标

1. 未知位置参数在任何副作用之前以非零状态退出，并打印用法。
2. `mcpx`、`mcpx -d`、`mcpx stop` 只 terminate `~/.mcpx/mcpx-daemon.json` 记录、且命令行仍匹配的 `-d` daemon。
3. 未写入该 pidfile 的前台实例、systemd 实例，不会被误敲的 CLI 或另一次 `mcpx` 启动杀掉。

### 1.3 为什么现在做

Issue #19 会把正在服务 MCP 的进程打掉且无法自动恢复，属于可用性事故。与 #20 / #12 / #21 分属不同子系统，本规格只覆盖 #19。

## 2. 范围与非目标

### 2.1 范围

- `cmd/mcpx-server/main.go`：已知子命令之外的第一个位置参数走 `default`，调用 `printUsage()`，在 `flag.Parse` 与 `stopExistingBackground` 之前退出，退出码 2。
- `cmd/mcpx-server/background.go`：`stopPreviousBackground` 只处理 pidfile 中的 PID；去掉对 `discoverBackgroundProcesses` 的调用。
- Unix / Windows 共用 `stopPreviousBackground`，两侧都改为 pidfile-only。
- 对**已确认的 pidfile PID** 调用现有 `terminateBackgroundProcess`；Windows 上 PR #15 的进程树终止逻辑保留。
- 若 `discoverBackgroundProcesses` 及其解析辅助函数在去掉调用后无引用，则删除，不留兼容扫描。
- 补充 `cmd/mcpx-server` 测试。

已知子命令（保持不变）：`observe`、`workspace`、`oauth-register`、`update`、`stop`、`desktop`、`help`、`-h`、`--help`。无位置参数的 `mcpx [flags]` 仍表示启动 Streamable HTTP 服务。

### 2.2 非目标

- 不改 MCP 工具协议、公开 schema、确认流。
- 不增加配置项或环境变量（包括 `MCPX_NO_TAKEOVER`）。
- 不改 PR #15 对已知 daemon PID 的 Windows 进程树杀法。
- 不添加 systemd unit，不写服务部署专章。
- 不再按可执行路径扫描并杀掉前台或服务管理实例。
- 不处理子命令内部的未知 flag（各子命令仍自管 flag）。
- 不在本规格中实现 #20、#12、#21。

## 3. 已选方案与未选方案

### 3.1 已选方案

**未知位置参数 fail-closed + 只杀 pidfile 中的 `-d` daemon。**

依据：

- 未知参数启动服务没有任何合法用途，应在副作用前拒绝。
- `mcpx -d` 的互相替换仍需要 pidfile 接管；这是本地后台模式的既有契约。
- 扫描 session leader 才会误杀 systemd / 手工前台实例。
- 仓库版本策略：不保留旧扫描接管行为。

### 3.2 未选方案及原因

| 方案 | 未选原因 |
| --- | --- |
| 继续扫描进程，另加关闭接管的开关 | 误杀路径仍在；多一个配置面；与「最小闭环、不加环境变量」冲突 |
| 先绑端口成功再杀旧进程 | 仍会打到 systemd 实例；同端口时序更绕，不能消除误杀 |
| 本轮只拒绝未知参数，接管逻辑不动 | `mcpx` 无参启动仍可能 SIGTERM 服务管理实例 |
| 最小闭环之外再写 README 部署专章 | 用户明确选择最小闭环，不扩大文档范围 |
| 再加 `MCPX_NO_TAKEOVER` | 用户明确拒绝新配置面；pidfile-only 已足够保护非 `-d` 实例 |

## 4. 用户流程与领域状态

### 4.1 角色

- **本机操作者**：在终端运行 `mcpx`。
- **服务管理器**（如 systemd）：以前台方式执行 `mcpx`（通常不带 `-d`，不写 pidfile）。
- **后台 daemon**：由 `mcpx -d` 拉起，状态写在 `~/.mcpx/mcpx-daemon.json`。

### 4.2 入口行为

| 入口 | 结果 |
| --- | --- |
| `mcpx skills`、`mcpx skills --help`、其它未知位置参数 | stderr 打印用法，退出码 2；不读配置、不写 pidfile、不发信号 |
| `mcpx`、`mcpx -d`、`mcpx -addr …` | 只停 pidfile 匹配 daemon，再启动 |
| `mcpx stop` | 只停 pidfile 匹配 daemon；没有存活实例时保持现有文案「没有正在运行的后台服务」，退出码 0 |
| `mcpx observe` / `workspace` / `oauth-register` / `update` / `desktop` / `help` | 现有子命令路径，不进入服务启动，也不因本规格改变其业务 |
| systemd 或手工前台、且未写 pidfile | 误敲未知子命令或另起 `mcpx` **不会**杀掉该实例 |

### 4.3 pidfile 状态

`mcpx-daemon.json` 含 `pid` 与 `executable`。处理顺序：

1. 读到有效记录：若进程存活且命令行匹配该 executable，则 `SIGTERM`（超时后按现有逻辑升级，Windows 保留进程树终止），然后删除 pidfile。
2. 进程不存在，或 PID 被复用导致命令行不匹配：不发信号，删除陈旧 pidfile（现有行为，保留）。
3. pidfile 不存在：不扫描其它进程，视为没有后台 daemon。
4. pidfile 存在但无法解码：返回错误，不回退到进程扫描。

`MCPX_HOME` 决定 pidfile 路径；本规格不改变该布局。

## 5. 架构 / 组件 / 接口 / 数据

```
os.Args[1]
  ├─ 已知子命令 → 现有 runXxx，进程退出
  ├─ 未知位置参数 → printUsage(); os.Exit(2)
  └─ 无位置参数 / 仅 flags
        → flag.Parse()
        → 若 -d：startBackground（先 stopPreviousBackground，再写新 pidfile）
        → 否则前台：stopPreviousBackground，再 server.New / Start
```

`stopPreviousBackground(statePath, executable)`：

- 输入：pidfile 路径、当前 `os.Executable()`。
- 输出：已停止的 PID 列表，或错误。
- 禁止再调用 `discoverBackgroundProcesses`。

数据面：不新增表、不改 SQLite、不改 YAML schema。唯一相关文件仍是 `~/.mcpx/mcpx-daemon.json`。

依赖方向：`main` → `background.go` 共用停止逻辑 → 平台文件 `background_unix.go` / `background_windows.go` 只负责「对给定 PID 发信号 / 核对命令行」。平台文件不再被要求「找出所有疑似 daemon」。

## 6. 错误、权限、安全、审计与恢复

### 6.1 错误

| 情况 | 行为 | 进程与 pidfile |
| --- | --- | --- |
| 未知位置参数 | 退出码 2，打印用法 | 无副作用 |
| pidfile 缺失 | `stop` 视为无后台服务，退出 0；启动则直接起新进程 | 不扫描 |
| pidfile 损坏 | 返回现有解码错误，非零退出 | 不扫描、不乱杀 |
| PID 活着但命令行不匹配 | 不发信号，删除陈旧 pidfile | 保护无关进程 |
| 匹配 daemon 停不掉 | 保持现有 `terminateBackgroundProcess` 错误 | 不回退扫描 |

面向用户的文案继续用现有中文提示（「没有正在运行的后台服务」「stopped previous background daemon」等），未知命令只复用 `printUsage()`，不暴露内部函数名或原始 `ps` 输出。

### 6.2 权限与安全

- 只对自己写下的 pidfile PID 发信号，且必须再次核对 executable。
- 不得根据「同路径 + session leader」扩大杀伤面。
- 不在规格中引入新的特权或网络接口。

### 6.3 审计与可观测性

- CLI 启动/停止不写 MCP 审计日志（与现状一致）。
- 成功停止后台时仍向 stdout 打印现有 `mcpx stopped previous background daemon (pid=…)`。

### 6.4 恢复

- 未知命令：无状态变化，改参数重试即可。
- 误杀服务管理实例：本设计下不再由本 CLI 路径触发。若运维把 `mcpx -d` 当作 `ExecStart`，CLI 仍可按 pidfile 停止它——这是 `-d` 契约，不是回归。
- 前台 `mcpx` 替换 `-d` daemon：仍通过 pidfile 完成，本地开发「先后台再前台」可用。

## 7. 测试与验收

### 7.1 测试策略

以 `cmd/mcpx-server` 包测试为主，不启动真实 MCP 服务：

1. 未知第一个位置参数（如 `skills`、`skills --help`）走拒绝路径：退出码 2，stderr 含 Usage，stdout 不含 `stopped previous background daemon`。
2. `stopPreviousBackground` 在无 pidfile、但存在「同 executable 的伪造 PID 列表」时，不得调用扫描/不得 terminate 扫描结果（通过去掉扫描调用后的单测或可注入假扫描断言）。
3. 有效 pidfile + 匹配进程：停止逻辑仍对该 PID 调用 terminate（可用 fake 进程状态，避免真杀）。
4. 陈旧 pidfile（PID 不匹配 executable）：不 terminate，pidfile 被移除。
5. 已知子命令 `help` / `-h` / `--help` 仍退出 0，不进入启动。

平台：Unix 与 Windows 共享 `stopPreviousBackground` 行为测试；进程树细节仍由现有 Windows 测试覆盖，本规格不改其断言意图。

### 7.2 验收标准

- `mcpx skills --help`：退出码 2；无 daemon 停止文案；已有前台/服务实例仍存活（手工或集成可观测；自动化至少保证未进入 stop 路径）。
- 有匹配 pidfile：`mcpx stop` 与随后的 `mcpx -d` 仍能停掉该 daemon。
- 无 pidfile、存在同路径前台进程：`mcpx stop` 与未知子命令都不杀它。
- `go test ./cmd/mcpx-server -count=1` 通过。
- `test -z "$(gofmt -l ./cmd ./internal)"`、`go vet ./...` 通过。

### 7.3 不可破坏的现有行为

- `mcpx -d` 仍写 pidfile，二次 `-d` 仍替换该 pidfile daemon。
- `mcpx stop` 在无 daemon 时退出 0。
- Windows 对**已确认 daemon PID** 的进程树终止保留。
- 子命令集合与 `printUsage` 所列入口一致，不借本规格新增子命令。

## 8. 兼容性、发布与回滚

- **无旧版本兼容：** 不再扫描 session leader。依赖「随便敲一个词也能把旧进程杀掉」的用法视为错误用法，予以删除。
- **契约变更：** 未知位置参数从「启动服务」改为「用法错误」。pidfile 缺失时不再杀同路径进程。
- **发布：** 随当前主线发布；不单独发兼容层。
- **回滚：** 还原 `main.go` 的 `default` 与 `stopPreviousBackground` 的扫描调用。回滚会恢复误杀风险，只应在 pidfile 方案被证明无法停止 `-d` daemon 时使用。
- **数据：** 无库迁移。残留的陈旧 pidfile 仍按「不匹配则删除」处理。

## 9. 外部依据

| 结论 | 来源 | 检索日期 | 冲突 | 采用理由 |
| --- | --- | --- | --- | --- |
| `Restart=on-failure` 不因干净退出（含 `SIGTERM`）而重启 | [systemd.service(5)](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html)（手册：干净退出含 exit 0 与 `SIGHUP`/`SIGINT`/`SIGTERM`/`SIGPIPE`；`on-failure` 排除这四个信号）。转述见 [SysTutorials 手册页](https://www.systutorials.com/linux-manual-page-5-systemd-service/)、[Super User 对手册的引用](https://superuser.com/questions/1530033/cannot-get-systemd-to-restart-a-service-if-it-crashes) | 2026-09-14 | 无实质冲突。部分博客把 `on-failure` 说成「被信号杀死就会重启」，与手册不符。 | 采用手册：Issue 复现与 `Restart=on-failure` + `SIGTERM` 一致。 |
| Issue 现象与建议 | [opentokenz/mcpx#19](https://github.com/opentokenz/mcpx/issues/19) | 2026-09-14 | 建议还包含「绑端口后再接管」和「服务部署 opt-out」。 | 用户选择 pidfile-only + 未知参数拒绝，不采用后两则。 |

Go `flag` 包对错误用法默认退出码 2。本规格未知命令使用退出码 2，与此对齐。该约定来自 Go 标准库行为，不另寻外部 RFC。

## 10. 非阻塞性假设

| 假设 | 影响 | 验证方式 | 验证时机 | 不成立时的调整 |
| --- | --- | --- | --- | --- |
| 现网 systemd 单元的 `ExecStart` 一般是前台 `mcpx`，不带 `-d`，因此没有 pidfile | 本方案即可避免误杀服务实例 | 实现前抽查仓库是否有示例 unit；实现后文档不强制改 | 实现前只读核对；若发现仓库内示例用了 `-d`，仅在实现授权后按最小闭环仍不改示例，除非用户另开文档任务 | 不自动加开关；若用户后续要求保护「systemd + `-d`」，再单独立项 |
| `discoverBackgroundProcesses` 删除后无其它调用方 | 可以删扫描代码 | `rg discoverBackgroundProcesses` | 实现时 | 若仍被测试或 Windows 专用路径使用，则只断开 `stopPreviousBackground` 的调用，保留函数供测试，或把测试改为 pidfile 夹具 |
| 退出码 2 对现有脚本可接受 | 未知命令从「启动成功/失败」变为用法错误 | 仓库内检索对 `mcpx` 未知参数的脚本假设 | 实现时 | 若必须保持退出码 1，只改退出码，不改 fail-closed 语义 |

以上假设不改变目标、范围、核心方案或验收标准。

## 11. 决策记录摘要

```text
事实：未知位置参数落入启动路径；stopPreviousBackground 在 pidfile 之外扫描 session leader。
决策：四份独立规格，顺序 #19 → #20 → #12 → #21；本文件只覆盖 #19。
决策：合法启动与 mcpx stop 只 terminate pidfile 且命令行仍匹配的 -d daemon。
决策：未知位置参数在任何副作用前打印用法并以退出码 2 结束。
决策：不改工具协议、不加配置/环境变量、不改 PR #15 进程树逻辑、不写部署专章。
非目标：MCPX_NO_TAKEOVER、systemd unit、扫进程杀残留 daemon、本文件内的 #20/#12/#21。
外部依据：systemd.service(5) Restart=on-failure 排除 SIGTERM；Issue #19。
验证：cmd/mcpx-server 单测 + gofmt + go vet；未知命令无 stop 文案；无 pidfile 时不杀同路径前台进程。
```
