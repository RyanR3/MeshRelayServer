package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// writeFileAtomic replaces path with data, readable by the server's own user
// only (0600). The data goes to a temp file in the same directory first and
// is renamed over the target, so a crash or full disk mid-write never leaves a
// truncated users.json / config behind, and readers never see half a file.
// Because the rename puts a NEW inode in place, a file that was created 0644
// by an older version becomes 0600 on its next save.
func writeFileAtomic(path string, data []byte) error {
	// A symlinked data file keeps its link: write through to the target.
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	// Not a regular file (e.g. /dev/null in tests): nothing to replace
	// atomically, write in place.
	if fi, err := os.Stat(path); err == nil && !fi.Mode().IsRegular() {
		return os.WriteFile(path, data, 0o600)
	}
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	// CreateTemp already uses 0600; Chmod makes it independent of that detail.
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// writeJSONFileAtomic writes v as indented JSON (the same layout the old
// json.Encoder-based savers produced, trailing newline included) with
// writeFileAtomic.
func writeJSONFileAtomic(path string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}
