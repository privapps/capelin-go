package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

type Skill struct {
	Name        string
	Description string
	Path        string
	Source      string
	Content     string
	Runs        string
	Commands    []string
}

type skillFrontMatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Runs        string `yaml:"runs"`
}

func Load(workspaceRoot string) (map[string]Skill, error) {
	dirs := []struct {
		Path   string
		Source string
	}{
		{Path: filepath.Join(workspaceRoot, ".agents", "skills"), Source: "project"},
	}

	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		dirs = append(dirs, struct {
			Path   string
			Source string
		}{Path: filepath.Join(home, ".agents", "skills"), Source: "user"})
	}
	return LoadFromDirs(dirs)
}

func LoadFromDirs(dirs []struct {
	Path   string
	Source string
}) (map[string]Skill, error) {
	skills := map[string]Skill{}

	// Load user first, then project to make project precedence deterministic.
	for _, pass := range []string{"user", "project"} {
		for _, dir := range dirs {
			if dir.Source != pass {
				continue
			}
			entries, err := os.ReadDir(dir.Path)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, fmt.Errorf("reading skills dir %s: %w", dir.Path, err)
			}
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				skillPath := filepath.Join(dir.Path, entry.Name(), "SKILL.md")
				sk, err := ParseFile(skillPath, dir.Source)
				if err != nil {
					if os.IsNotExist(err) {
						continue
					}
					return nil, err
				}
				skills[sk.Name] = sk
			}
		}
	}
	return skills, nil
}

func ParseFile(path, source string) (Skill, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, err
	}
	content := string(raw)

	header, err := extractFrontMatter(content)
	if err != nil {
		return Skill{}, fmt.Errorf("parsing skill file %s: %w", path, err)
	}

	var meta skillFrontMatter
	if err := yaml.Unmarshal([]byte(header), &meta); err != nil {
		return Skill{}, fmt.Errorf("parsing skill front matter %s: %w", path, err)
	}
	name := strings.TrimSpace(meta.Name)
	if name == "" {
		name = filepath.Base(filepath.Dir(path))
	}
	return Skill{
		Name:        name,
		Description: strings.TrimSpace(meta.Description),
		Path:        path,
		Source:      source,
		Content:     content,
		Runs:        strings.TrimSpace(meta.Runs),
		Commands:    ExtractExecutableCommands(content, meta.Runs),
	}, nil
}

func extractFrontMatter(content string) (string, error) {
	if !strings.HasPrefix(content, "---\n") {
		return "", fmt.Errorf("missing YAML front matter")
	}
	rest := strings.TrimPrefix(content, "---\n")
	idx := strings.Index(rest, "\n---\n")
	if idx < 0 {
		return "", fmt.Errorf("unterminated YAML front matter")
	}
	return rest[:idx], nil
}

func ExtractExecutableCommands(content, runs string) []string {
	commands := map[string]struct{}{}

	addCommand := func(line string) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			return
		}
		// Expand environment variables ($HOME, $USER, etc.) so that paths like
		// "$HOME/bin/exec-cli" become "/Users/jing/.../exec-cli" and are accepted.
		expanded := os.ExpandEnv(line)
		tokens := strings.Fields(expanded)
		if len(tokens) == 0 {
			return
		}
		cmd := tokens[0]
		// Skip lines that start with a flag (e.g. "--api-path") — those are CLI
		// options, not executable names.
		if strings.HasPrefix(cmd, "-") {
			return
		}
		if isSimpleCommandToken(cmd) {
			commands[cmd] = struct{}{}
			// Also register the basename so the LLM can use the short form.
			if base := filepath.Base(cmd); base != cmd && isSimpleCommandToken(base) {
				commands[base] = struct{}{}
			}
		}
	}

	// Prefer the explicit runs block when present.
	for _, line := range strings.Split(runs, "\n") {
		addCommand(line)
	}

	// Also parse shell/fenced command blocks in the markdown body.
	inCode := false
	isShellCode := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if !inCode {
				lang := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
				isShellCode = lang == "" || strings.EqualFold(lang, "bash") || strings.EqualFold(lang, "sh") || strings.EqualFold(lang, "shell")
				inCode = true
			} else {
				inCode = false
				isShellCode = false
			}
			continue
		}
		if inCode && isShellCode {
			addCommand(line)
		}
	}

	out := make([]string, 0, len(commands))
	for cmd := range commands {
		out = append(out, cmd)
	}
	slices.Sort(out)
	return out
}

var simpleCommandTokenPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

func isSimpleCommandToken(token string) bool {
	return simpleCommandTokenPattern.MatchString(token)
}
