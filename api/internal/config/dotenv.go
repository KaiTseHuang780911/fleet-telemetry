// Package config handles process configuration.
package config

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"strings"
)

// LoadDotEnv reads KEY=VALUE pairs from path into the process environment.
//
// Real environment variables always win: in production the platform sets them,
// and a stale .env on a deployed box must never override that. Missing file is
// not an error — .env is a local-development convenience, not a requirement.
//
// This is ~30 lines rather than a dependency. It handles what a .env file
// actually contains: comments, blank lines, optional quotes, and `export`
// prefixes. It does not handle variable interpolation or multi-line values; if
// this project ever needs those, that is the moment to reach for a library.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		// Strip a single matching pair of surrounding quotes.
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}

		if key == "" {
			continue
		}
		// LookupEnv rather than Getenv: an explicitly empty environment
		// variable is still a deliberate setting and should not be replaced.
		if _, alreadySet := os.LookupEnv(key); alreadySet {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return scanner.Err()
}
