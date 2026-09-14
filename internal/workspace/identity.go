package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"mcpx/internal/winproc"
)

// GitIdentity 的状态只取指定 Git root，不扫描或继承外层仓库。
// RemoteSHA256 冻结远端配置字节，不把潜在凭据放入公开结果。
type GitIdentity struct {
	CanonicalPath string `json:"canonical_path"`
	RemoteName    string `json:"remote_name"`
	RemoteSHA256  string `json:"remote_sha256"`
	Ref           string `json:"ref"`
	HEAD          string `json:"head"`
	Tree          string `json:"tree"`
	RunID         string `json:"run_id"`
}

func CaptureGitIdentity(parent context.Context, path, remoteName, runID string) (GitIdentity, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(remoteName) == "" || strings.HasPrefix(remoteName, "-") {
		return GitIdentity{}, fmt.Errorf("run_id and remote_name are required")
	}
	physical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return GitIdentity{}, fmt.Errorf("cannot resolve target workspace")
	}
	physical, err = filepath.Abs(physical)
	if err != nil {
		return GitIdentity{}, fmt.Errorf("cannot resolve target workspace")
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", physical}, args...)...)
		// 继承的 Git 定位变量不能把只读身份查询重定向到另一个仓库。
		for _, entry := range os.Environ() {
			name, _, _ := strings.Cut(entry, "=")
			switch strings.ToUpper(name) {
			case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS", "GIT_NAMESPACE":
				return "", fmt.Errorf("Git environment overrides are not supported for workspace identity")
			}
		}
		winproc.ConfigureNoWindow(cmd)
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("cannot read target Git identity")
		}
		return strings.TrimSpace(string(out)), nil
	}
	top, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return GitIdentity{}, err
	}
	top, err = filepath.EvalSymlinks(top)
	if err != nil {
		return GitIdentity{}, fmt.Errorf("cannot resolve Git root")
	}
	top, err = filepath.Abs(top)
	if err != nil || filepath.Clean(top) != filepath.Clean(physical) {
		return GitIdentity{}, fmt.Errorf("target must be its own Git root; inherited outer repository is not accepted")
	}
	ref, err := git("symbolic-ref", "-q", "HEAD")
	if err != nil {
		// detached HEAD 是明确状态，仍必须有可回读 HEAD。
		ref = "DETACHED"
	}
	head, err := git("rev-parse", "--verify", "HEAD")
	if err != nil {
		return GitIdentity{}, err
	}
	tree, err := git("rev-parse", "--verify", head+"^{tree}")
	if err != nil {
		return GitIdentity{}, err
	}
	remote, err := git("remote", "get-url", "--all", remoteName)
	if err != nil {
		return GitIdentity{}, fmt.Errorf("cannot read requested remote")
	}
	push, err := git("remote", "get-url", "--push", "--all", remoteName)
	if err != nil {
		return GitIdentity{}, fmt.Errorf("cannot read requested push remote")
	}
	sum := sha256.Sum256([]byte(remote + "\x00" + push))
	finalHead, err := git("rev-parse", "--verify", "HEAD")
	if err != nil || finalHead != head {
		return GitIdentity{}, fmt.Errorf("target HEAD changed while reading identity")
	}
	finalRef, err := git("symbolic-ref", "-q", "HEAD")
	if err != nil {
		finalRef = "DETACHED"
	}
	if finalRef != ref {
		return GitIdentity{}, fmt.Errorf("target ref changed while reading identity")
	}
	return GitIdentity{CanonicalPath: filepath.Clean(physical), RemoteName: remoteName, RemoteSHA256: "sha256:" + hex.EncodeToString(sum[:]), Ref: ref, HEAD: head, Tree: tree, RunID: runID}, nil
}

// VerifyGitIdentity 不接受新值重冻结。正常 commit 后，调用方须先证明后置归因，
// 再将该后置身份明确用于下一次操作。
func VerifyGitIdentity(ctx context.Context, expected GitIdentity) error {
	if expected.CanonicalPath == "" || expected.RemoteSHA256 == "" || expected.Ref == "" || expected.HEAD == "" || expected.Tree == "" {
		return fmt.Errorf("complete expected Git identity is required")
	}
	actual, err := CaptureGitIdentity(ctx, expected.CanonicalPath, expected.RemoteName, expected.RunID)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("target Git identity drifted before operation")
	}
	return nil
}
