package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Logger struct {
	path string
	mu   sync.Mutex
}
type Entry struct {
	Time    string `json:"time"`
	Action  string `json:"action"`
	Mode    string `json:"mode,omitempty"`
	Command string `json:"command,omitempty"`
	Success bool   `json:"success"`
	Detail  string `json:"detail,omitempty"`
}

func New(path string) *Logger { return &Logger{path: path} }
func (l *Logger) Write(e Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.Time = time.Now().UTC().Format(time.RFC3339Nano)
	if err := os.MkdirAll(filepath.Dir(l.path), 0750); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(e)
	_, err = f.Write(append(b, '\n'))
	return err
}
