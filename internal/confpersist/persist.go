// Package confpersist writes and reads the runtime JSON configuration.
package confpersist

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/bluenviron/mediamtx/internal/conf/jsonwrapper"
)

// JSONPath returns the runtime JSON path next to a YAML seed (or the path itself if it is already JSON).
func JSONPath(confPath string) string {
	if confPath == "" {
		return ""
	}

	ext := filepath.Ext(confPath)
	if strings.EqualFold(ext, ".json") {
		return confPath
	}
	if ext == "" {
		return confPath + ".json"
	}

	return strings.TrimSuffix(confPath, ext) + ".json"
}

// Exists reports whether jsonPath is a regular file.
func Exists(jsonPath string) bool {
	if jsonPath == "" {
		return false
	}

	st, err := os.Stat(jsonPath)
	return err == nil && !st.IsDir()
}

// Load unmarshals JSON from jsonPath into dest.
func Load(jsonPath string, dest any) error {
	byts, err := os.ReadFile(jsonPath)
	if err != nil {
		return err
	}

	return jsonwrapper.Unmarshal(byts, dest)
}

// Save marshals v as indented JSON and atomically replaces jsonPath.
// An empty jsonPath is a no-op.
func Save(jsonPath string, v any) error {
	if jsonPath == "" {
		return nil
	}

	byts, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	byts = append(byts, '\n')

	dir := filepath.Dir(jsonPath)
	if dir == "" {
		dir = "."
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(jsonPath)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	success := false
	defer func() {
		if !success {
			os.Remove(tmpName)
		}
	}()

	_, err = tmp.Write(byts)
	if err != nil {
		tmp.Close()
		return err
	}

	err = tmp.Sync()
	if err != nil {
		tmp.Close()
		return err
	}

	err = tmp.Close()
	if err != nil {
		return err
	}

	err = os.Chmod(tmpName, 0o600)
	if err != nil {
		return err
	}

	err = os.Rename(tmpName, jsonPath)
	if err != nil {
		removeErr := os.Remove(jsonPath)
		if removeErr == nil || os.IsNotExist(removeErr) {
			err = os.Rename(tmpName, jsonPath)
		}
		if err != nil {
			return err
		}
	}

	success = true
	return nil
}
