package spectest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

type Vector struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type vectorFile struct {
	Schema  string   `json:"schema"`
	Vectors []Vector `json:"vectors"`
}

func LoadVectors(name string) ([]Vector, error) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return nil, fmt.Errorf("locate spectest vectors.go")
	}
	path := filepath.Join(filepath.Dir(self), "..", "..", "conformance", "vectors", name)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var file vectorFile
	if err := json.Unmarshal(b, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if file.Schema == "" || len(file.Vectors) == 0 {
		return nil, fmt.Errorf("%s: empty schema or vector set", path)
	}
	return file.Vectors, nil
}
