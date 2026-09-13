package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"mcpx/internal/terminal"
)

// argv 仅用于直接进程启动，display 仅用于现有策略、审计和确认，不再执行。
func executeArgv(payload map[string]any) (spec *terminal.ProcessSpec, display, payloadDigest string, err error) {
	raw, present := payload["argv"]
	if !present {
		if _, present := payload["shell"]; present {
			return nil, "", "", fmt.Errorf("shell requires argv")
		}
		return nil, "", "", nil
	}
	if shell, ok := payload["shell"].(bool); !ok || shell {
		return nil, "", "", fmt.Errorf("argv requires shell=false")
	}
	for _, key := range []string{"command", "task", "runtime", "script", "database"} {
		if _, present := payload[key]; present {
			return nil, "", "", fmt.Errorf("argv and %s are mutually exclusive", key)
		}
	}
	values, ok := raw.([]any)
	if !ok || len(values) == 0 {
		return nil, "", "", fmt.Errorf("argv must be a non-empty string array")
	}
	argv := make([]string, len(values))
	quoted := make([]string, len(values))
	for i, value := range values {
		arg, ok := value.(string)
		if !ok || !utf8.ValidString(arg) || strings.ContainsRune(arg, 0) {
			return nil, "", "", fmt.Errorf("argv[%d] must be a UTF-8 string without NUL", i)
		}
		argv[i] = arg
		quoted[i] = quoteArgvForPolicy(arg)
	}
	if strings.TrimSpace(argv[0]) == "" {
		return nil, "", "", fmt.Errorf("argv[0] must name an executable")
	}
	encoded, _ := json.Marshal(argv)
	identity, _ := json.Marshal(payload["expected_workspace"])
	transition, _ := json.Marshal(payload["workspace_transition"])
	return &terminal.ProcessSpec{Executable: argv[0], Args: argv[1:]}, strings.Join(quoted, " "), "argv:" + string(encoded) + ":workspace:" + string(identity) + ":transition:" + string(transition), nil
}

func quoteArgvForPolicy(arg string) string {
	if arg != "" && strings.IndexFunc(arg, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("_./:=+-", r))
	}) < 0 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
}
