package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

type skill struct {
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

func loadSkills(workspaceRoot string) (map[string]skill, error) {
	dirs := []struct {
		path   string
		source string
	}{
		{path: filepath.Join(workspaceRoot, ".agents", "skills"), source: "project"},
	}

	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		dirs = append(dirs, struct {
			path   string
			source string
		}{path: filepath.Join(home, ".agents", "skills"), source: "user"})
	}
	return loadSkillsFromDirs(dirs)
}

func loadSkillsFromDirs(dirs []struct {
	path   string
	source string
}) (map[string]skill, error) {
	skills := map[string]skill{}

	// Load user first, then project to make project precedence deterministic.
	for _, pass := range []string{"user", "project"} {
		for _, dir := range dirs {
			if dir.source != pass {
				continue
			}
			entries, err := os.ReadDir(dir.path)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, fmt.Errorf("reading skills dir %s: %w", dir.path, err)
			}
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				skillPath := filepath.Join(dir.path, entry.Name(), "SKILL.md")
				sk, err := parseSkillFile(skillPath, dir.source)
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

func parseSkillFile(path, source string) (skill, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return skill{}, err
	}
	content := string(raw)

	header, err := extractFrontMatter(content)
	if err != nil {
		return skill{}, fmt.Errorf("parsing skill file %s: %w", path, err)
	}

	var meta skillFrontMatter
	if err := yaml.Unmarshal([]byte(header), &meta); err != nil {
		return skill{}, fmt.Errorf("parsing skill front matter %s: %w", path, err)
	}
	name := strings.TrimSpace(meta.Name)
	if name == "" {
		name = filepath.Base(filepath.Dir(path))
	}
	return skill{
		Name:        name,
		Description: strings.TrimSpace(meta.Description),
		Path:        path,
		Source:      source,
		Content:     content,
		Runs:        strings.TrimSpace(meta.Runs),
		Commands:    extractExecutableCommands(content, meta.Runs),
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

func extractExecutableCommands(content, runs string) []string {
	commands := map[string]struct{}{}

	addCommand := func(line string) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			return
		}
		token := strings.Fields(line)
		if len(token) == 0 {
			return
		}
		cmd := token[0]
		if isSimpleCommandToken(cmd) {
			commands[cmd] = struct{}{}
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
