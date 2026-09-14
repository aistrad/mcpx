package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	buildversion "mcpx/internal/version"
)

func TestResolveBuildProvenancePrefersLinkerValues(t *testing.T) {
	got := resolveBuildProvenance("1.2.3", "release-commit", "2026-08-09T00:00:00Z", []debug.BuildSetting{
		{Key: "vcs.revision", Value: "fallback-commit"},
		{Key: "vcs.modified", Value: "true"},
	})
	if got.Version != "1.2.3" || got.Commit != "release-commit" || got.Date != "2026-08-09T00:00:00Z" {
		t.Fatalf("linker provenance must win: %+v", got)
	}
}

func TestResolveBuildProvenanceFallsBackToVCSRevision(t *testing.T) {
	got := resolveBuildProvenance("", "none", "unknown", []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0123456789abcdef"},
		{Key: "vcs.modified", Value: "true"},
	})
	if got.Version != buildversion.Current || got.Commit != "0123456789abcdef-dirty" || got.Date != "unknown" {
		t.Fatalf("unexpected VCS fallback provenance: %+v", got)
	}
}

func TestIsUnknownPositionalArg(t *testing.T) {
	unknown := []string{"skills", "foo", "start", "daemon"}
	for _, arg := range unknown {
		if !isUnknownPositionalArg(arg) {
			t.Fatalf("%q must be rejected as an unknown positional argument", arg)
		}
	}
	known := []string{"", "-addr", "--help", "-h", "-d", "-version", "observe", "workspace", "oauth-register", "update", "stop", "desktop", "help"}
	for _, arg := range known {
		if isUnknownPositionalArg(arg) {
			t.Fatalf("%q must not be treated as an unknown positional argument", arg)
		}
	}
}

func TestUnknownCommandStatusPrintsUsageAndExitsTwo(t *testing.T) {
	orig := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = writer
	code := unknownCommandStatus()
	_ = writer.Close()
	os.Stderr = orig
	output, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if code != 2 {
		t.Fatalf("unknown command status=%d, want 2", code)
	}
	if !strings.Contains(string(output), "Usage:") {
		t.Fatalf("unknown command must print usage, got %q", output)
	}
	if strings.Contains(string(output), "stopped previous background daemon") {
		t.Fatalf("unknown command must not stop a daemon: %q", output)
	}
}

func TestStopPreviousBackgroundWithoutPidfileIsNoop(t *testing.T) {
	stopped, err := stopPreviousBackground(filepath.Join(t.TempDir(), daemonStateFilename))
	if err != nil {
		t.Fatal(err)
	}
	if len(stopped) != 0 {
		t.Fatalf("missing pidfile must not stop processes: %v", stopped)
	}
}

func TestStopPreviousBackgroundCorruptStateReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), daemonStateFilename)
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := stopPreviousBackground(path); err == nil {
		t.Fatal("corrupt pidfile must fail closed without scanning processes")
	}
}

func TestBackgroundChildSubcommandIsInternal(t *testing.T) {
	if backgroundChildSubcommand != "__background-child" {
		t.Fatalf("unexpected internal background child subcommand %q", backgroundChildSubcommand)
	}
}

func TestBackgroundChildArgsRemovesDaemonFlag(t *testing.T) {
	got := backgroundChildArgs([]string{"-addr", "127.0.0.1:9999", "-d", "-log-level", "debug"})
	want := []string{"-addr", "127.0.0.1:9999", "-log-level", "debug"}
	if len(got) != len(want) {
		t.Fatalf("unexpected args length: got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected arg at %d: got=%q want=%q", i, got[i], want[i])
		}
	}
}

func TestDaemonStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), daemonStateFilename)
	want := daemonState{PID: 4321, Executable: "/opt/mcpx/bin/mcpx"}
	if err := writeDaemonState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := readDaemonState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("daemon state mismatch: got=%+v want=%+v", got, want)
	}
}

func TestStopPreviousBackgroundRemovesInvalidState(t *testing.T) {
	path := filepath.Join(t.TempDir(), daemonStateFilename)
	if err := writeDaemonState(path, daemonState{}); err != nil {
		t.Fatal(err)
	}
	stoppedPIDs, err := stopPreviousBackground(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(stoppedPIDs) != 0 {
		t.Fatalf("invalid state must not report stopped daemon pids: %v", stoppedPIDs)
	}
	if _, err := readDaemonState(path); err == nil {
		t.Fatal("daemon state should be removed")
	}
}

// runStop 对"本来就没有后台服务"必须是幂等的：托盘的停止按钮会被重复点，
// 每次都报错会让用户以为出了问题。
func TestRunStopIsIdempotentWithoutDaemon(t *testing.T) {
	t.Setenv("MCPX_HOME", t.TempDir())
	if code := runStop(); code != 0 {
		t.Fatalf("runStop without daemon = %d, want 0", code)
	}
	if code := runStop(); code != 0 {
		t.Fatalf("repeated runStop = %d, want 0", code)
	}
}

// backgroundSleepHelperEnv 让测试二进制以"长活子进程"的身份重新执行自己。
// 它提供了一个真实存活、但命令行与 daemon 可执行文件不匹配的 PID——正是
// PID 被系统复用后的状态。
const backgroundSleepHelperEnv = "MCPX_TEST_BACKGROUND_SLEEP_HELPER"

func TestBackgroundSleepHelper(t *testing.T) {
	if os.Getenv(backgroundSleepHelperEnv) != "1" {
		t.Skip("helper process: 仅在被其他测试重新执行时运行")
	}
	time.Sleep(30 * time.Second)
}

// daemon 异常退出后状态文件会残留；等系统把那个 PID 复用给别的程序，
// 旧实现会认定"pid 与 daemon 可执行文件不匹配"并直接报错返回，导致状态文件
// 永远删不掉，之后每一次 `mcpx -d` 都失败。
//
// 正确行为：不匹配的 PID 按定义就不是我们的 daemon，应当丢弃这条陈旧记录
// 并让启动继续，同时绝不对别人的进程发信号。
func TestStopPreviousBackgroundDiscardsReusedPID(t *testing.T) {
	helper := exec.Command(os.Args[0], "-test.run=TestBackgroundSleepHelper")
	helper.Env = append(os.Environ(), backgroundSleepHelperEnv+"=1")
	if err := helper.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	defer func() {
		_ = helper.Process.Kill()
		_, _ = helper.Process.Wait()
	}()

	const unrelated = "/definitely/not/a/real/mcpx"
	path := filepath.Join(t.TempDir(), daemonStateFilename)
	if err := writeDaemonState(path, daemonState{PID: helper.Process.Pid, Executable: unrelated}); err != nil {
		t.Fatal(err)
	}

	stoppedPIDs, err := stopPreviousBackground(path)
	if err != nil {
		t.Fatalf("被复用的 pid 不能让启动失败: %v", err)
	}
	if len(stoppedPIDs) != 0 {
		t.Fatalf("无关进程不应被报告为已停止: %v", stoppedPIDs)
	}
	if _, err := readDaemonState(path); err == nil {
		t.Fatal("陈旧的 daemon 状态必须被删除，否则下次启动仍会失败")
	}
}

func TestBackgroundStopMessageShowsStoppedDaemons(t *testing.T) {
	got := backgroundStopMessage([]int{35421, 35422})
	want := "mcpx stopped previous background daemon (pid=35421)\n" +
		"mcpx stopped previous background daemon (pid=35422)\n"
	if got != want {
		t.Fatalf("background stop message=%q, want %q", got, want)
	}
	if got := backgroundStopMessage(nil); got != "" {
		t.Fatalf("empty background stop message=%q", got)
	}
}

func TestBackgroundStartMessageShowsStoppedDaemonBeforeNewDaemon(t *testing.T) {
	got := backgroundStartMessage(35600, "/tmp/mcpx-daemon.log", []int{35421, 35422})
	want := "mcpx stopped previous background daemon (pid=35421)\n" +
		"mcpx stopped previous background daemon (pid=35422)\n" +
		"mcpx started in background (pid=35600, log=/tmp/mcpx-daemon.log)\n"
	if got != want {
		t.Fatalf("background start message=%q, want %q", got, want)
	}
}

func TestBackgroundStartMessageWithoutPreviousDaemonOnlyShowsStart(t *testing.T) {
	got := backgroundStartMessage(35600, "/tmp/mcpx-daemon.log", nil)
	want := "mcpx started in background (pid=35600, log=/tmp/mcpx-daemon.log)\n"
	if got != want {
		t.Fatalf("background start message=%q, want %q", got, want)
	}
}
