package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// WriteConfigEnv merges updates into the config.env at path. Only the
// SettingKeys may be written and every value must pass
// ValidSettingValue; both are checked before the file is touched. Other
// lines are kept verbatim. The write is atomic (temp file in the same
// directory, then rename); a new directory is 0700 and the file is 0600.
func WriteConfigEnv(path string, updates map[string]string) error {
	if len(updates) == 0 {
		return errors.New("config.env: nothing to write")
	}
	for k, v := range updates {
		if !slices.Contains(SettingKeys, k) {
			return fmt.Errorf("config.env: key %q may not be written (allowed: %s)", k, strings.Join(SettingKeys, ", "))
		}
		if !ValidSettingValue(v) {
			return fmt.Errorf("config.env: invalid value for %s (want 1-64 of [A-Za-z0-9._-])", k)
		}
	}
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("config.env: %w", err)
	}
	if _, err := ParseEnv(bytes.NewReader(old), path); err != nil {
		return fmt.Errorf("config.env: refusing to merge into a malformed file: %w", err)
	}
	return writeAtomic(path, mergeEnv(old, updates))
}

// mergeEnv replaces the first line of each updated key, drops its later
// duplicates, and appends keys that were absent. old must already parse.
func mergeEnv(old []byte, updates map[string]string) []byte {
	var out bytes.Buffer
	written := map[string]bool{}
	text := strings.TrimSuffix(string(old), "\n")
	if text != "" {
		for line := range strings.SplitSeq(text, "\n") {
			key, _, ours, _ := parseLine(line)
			if v, upd := updates[key]; ours && upd {
				if !written[key] {
					fmt.Fprintf(&out, "%s=%s\n", key, v)
					written[key] = true
				}
				continue
			}
			out.WriteString(line + "\n")
		}
	}
	for _, k := range SettingKeys {
		if v, upd := updates[k]; upd && !written[k] {
			fmt.Fprintf(&out, "%s=%s\n", k, v)
		}
	}
	return out.Bytes()
}

// writeAtomic writes data to path via a 0600 temp file and rename, creating
// the parent directory 0700 when missing.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
