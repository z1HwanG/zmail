package config

import (
	"fmt"
	"os"
	"strings"
)

// EnvFileVar 指定 .env 文件路径的环境变量名。
//
// 取值说明：
//   - 未设置：按当前工作目录下的 .env 加载（不存在则忽略）；
//   - 设置为普通路径：加载该文件，文件不存在视为错误；
//   - 设置为 "-"/"none"/"off"：禁用 .env 加载。
const EnvFileVar = "ENV_FILE"

// LoadDotEnv 按上述规则加载 .env 文件到进程环境变量。
//
// 已存在且非空的环境变量优先，不会被 .env 覆盖（便于 systemd/CI 覆盖配置）。
// 返回实际加载的文件路径；未加载时返回空字符串。
func LoadDotEnv() (string, error) {
	path := strings.TrimSpace(os.Getenv(EnvFileVar))
	if path == "-" || strings.EqualFold(path, "none") || strings.EqualFold(path, "off") {
		return "", nil
	}
	explicit := path != ""
	if !explicit {
		path = ".env"
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return "", nil
		}
		return "", fmt.Errorf("config: 读取环境变量文件 %s 失败: %w", path, err)
	}

	entries, err := parseDotEnv(string(data))
	if err != nil {
		return "", fmt.Errorf("config: 解析 %s 失败: %w", path, err)
	}
	for _, e := range entries {
		if os.Getenv(e.Key) == "" {
			if err := os.Setenv(e.Key, e.Value); err != nil {
				return "", fmt.Errorf("config: 设置环境变量 %s 失败: %w", e.Key, err)
			}
		}
	}
	return path, nil
}

type dotEnvEntry struct {
	Key   string
	Value string
	Line  int
}

// parseDotEnv 解析 .env 内容，支持：
//   - 空行与 # 注释
//   - 可选的 export 前缀
//   - KEY=VALUE、KEY = VALUE
//   - 双引号（支持 \n \r \t \" \\ 转义）与单引号（原样保留）
//   - 未加引号时的行内注释（空白 + # 之后的内容）
func parseDotEnv(content string) ([]dotEnvEntry, error) {
	content = strings.TrimPrefix(content, "\ufeff")
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	out := make([]dotEnvEntry, 0, len(lines))

	for i, raw := range lines {
		lineNo := i + 1
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if rest, ok := trimExportPrefix(line); ok {
			line = rest
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("第 %d 行缺少合法的 KEY=VALUE 形式: %q", lineNo, line)
		}
		key := strings.TrimSpace(line[:eq])
		if !validEnvKey(key) {
			return nil, fmt.Errorf("第 %d 行变量名不合法: %q", lineNo, key)
		}
		value, err := parseDotEnvValue(strings.TrimSpace(line[eq+1:]))
		if err != nil {
			return nil, fmt.Errorf("第 %d 行 %s: %w", lineNo, key, err)
		}
		out = append(out, dotEnvEntry{Key: key, Value: value, Line: lineNo})
	}
	return out, nil
}

func trimExportPrefix(line string) (string, bool) {
	const prefix = "export"
	if len(line) <= len(prefix) || !strings.EqualFold(line[:len(prefix)], prefix) {
		return line, false
	}
	rest := line[len(prefix):]
	if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return line, false
	}
	return strings.TrimSpace(rest), true
}

func validEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func parseDotEnvValue(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	switch raw[0] {
	case '"':
		end := strings.LastIndexByte(raw, '"')
		if end == 0 || strings.TrimSpace(raw[end+1:]) != "" {
			return "", fmt.Errorf("双引号未闭合或引号后有多余内容")
		}
		return unescapeDoubleQuoted(raw[1:end]), nil
	case '\'':
		end := strings.LastIndexByte(raw, '\'')
		if end == 0 || strings.TrimSpace(raw[end+1:]) != "" {
			return "", fmt.Errorf("单引号未闭合或引号后有多余内容")
		}
		return raw[1:end], nil
	default:
		if idx := strings.Index(raw, " #"); idx >= 0 {
			raw = raw[:idx]
		} else if idx := strings.Index(raw, "\t#"); idx >= 0 {
			raw = raw[:idx]
		}
		return strings.TrimSpace(raw), nil
	}
}

func unescapeDoubleQuoted(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
