package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qoryai/runner/internal/descriptor"
)

// hookTimeout is the seconds a runtime gives the forwarder before cancelling it.
const hookTimeout = 5

// installHooks adds the forwarder as a command hook for each of the descriptor's
// events, the way the descriptor's installer says, and returns the arguments that make
// the runtime read it. The one installer today, claude-settings, reads the JSON file
// the arguments name after --settings, or starts from an empty document when they name
// none, adds a hook group per event under "hooks", writes the result as settings.json
// in the run directory, and names that file instead. The file the launch passed is not
// modified.
func installHooks(hooks *descriptor.Hooks, args []string, dir string, forwarder []string) ([]string, error) {
	if hooks.Install != "claude-settings" {
		return nil, fmt.Errorf("hook installer %q is not one this runner implements", hooks.Install)
	}
	settings := map[string]any{}
	at := -1
	for i, a := range args {
		if a == "--settings" && i+1 < len(args) {
			at = i + 1
			break
		}
		if v, ok := strings.CutPrefix(a, "--settings="); ok {
			at = i
			args = append(append([]string(nil), args[:i]...), append([]string{"--settings", v}, args[i+1:]...)...)
			at = i + 1
			break
		}
	}
	if at >= 0 {
		var b []byte
		var err error
		if strings.HasPrefix(strings.TrimSpace(args[at]), "{") {
			b = []byte(args[at])
		} else if b, err = os.ReadFile(args[at]); err != nil {
			return nil, fmt.Errorf("settings %s: %w", args[at], err)
		}
		if err := json.Unmarshal(b, &settings); err != nil {
			return nil, fmt.Errorf("settings %s: %w", args[at], err)
		}
	}
	groups, _ := settings["hooks"].(map[string]any)
	if groups == nil {
		groups = map[string]any{}
	}
	entry := map[string]any{"type": "command", "command": shellLine(forwarder), "timeout": hookTimeout}
	for _, ev := range hooks.Events {
		existing, _ := groups[ev].([]any)
		groups[ev] = append(existing, map[string]any{"hooks": []any{entry}})
	}
	settings["hooks"] = groups
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		return nil, err
	}
	if at >= 0 {
		args = append([]string(nil), args...)
		args[at] = path
		return args, nil
	}
	return append(append([]string(nil), args...), "--settings", path), nil
}

// shellLine quotes a command for a shell, since a command hook runs under sh -c.
func shellLine(words []string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = shellQuote(w)
	}
	return strings.Join(quoted, " ")
}

func shellQuote(w string) string {
	if w != "" && strings.IndexFunc(w, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:@%+,", r))
	}) < 0 {
		return w
	}
	return "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
}
