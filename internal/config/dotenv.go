package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// LoadDotEnv sets variables from path when they are not already in the environment.
// A missing file is ignored.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	vals, err := parseDotEnv(f)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	for key, value := range vals {
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return nil
}

func parseDotEnv(r io.Reader) (map[string]string, error) {
	vals := make(map[string]string)
	scanner := bufio.NewScanner(r)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		line = strings.TrimPrefix(line, "\uFEFF")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE", lineNo)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("line %d: empty key", lineNo)
		}
		vals[key] = unquote(strings.TrimSpace(value))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return vals, nil
}

func unquote(value string) string {
	if len(value) < 2 {
		return value
	}
	if value[0] == '"' && value[len(value)-1] == '"' {
		return strings.Trim(value, `"`)
	}
	if value[0] == '\'' && value[len(value)-1] == '\'' {
		return strings.Trim(value, "'")
	}
	return value
}
