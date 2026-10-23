package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

type settings struct {
	Name   string `json:"name"`
	Hidden bool   `json:"hidden"`
}
type preferences struct {
	mu   sync.RWMutex
	file string
	data map[int]settings
}

func defaultDataDir() string {
	if dir := os.Getenv("BNDR_DATA_DIR"); dir != "" {
		return dir
	}
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "bndr")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "bndr")
}
func loadPreferences(dir string) (*preferences, error) {
	p := &preferences{file: filepath.Join(dir, "apps.json"), data: make(map[int]settings)}
	raw, err := os.ReadFile(p.file)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	var records map[string]json.RawMessage
	if err = json.Unmarshal(raw, &records); err != nil || records == nil {
		return nil, fmt.Errorf("invalid preferences at %s", p.file)
	}
	for key, record := range records {
		port, err := strconv.Atoi(key)
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid saved port %q", key)
		}
		var value struct {
			Name   *string `json:"name"`
			Hidden *bool   `json:"hidden"`
		}
		if err = json.Unmarshal(record, &value); err != nil || value.Name == nil || value.Hidden == nil {
			return nil, fmt.Errorf("invalid settings for port %s", key)
		}
		p.data[port] = settings{*value.Name, *value.Hidden}
	}
	return p, nil
}
func (p *preferences) get(port int) settings { p.mu.RLock(); defer p.mu.RUnlock(); return p.data[port] }
func (p *preferences) snapshot() map[int]settings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	copy := make(map[int]settings, len(p.data))
	for k, v := range p.data {
		copy[k] = v
	}
	return copy
}
func (p *preferences) update(port int, name *string, hidden *bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	next := make(map[int]settings, len(p.data)+1)
	for k, v := range p.data {
		next[k] = v
	}
	value := next[port]
	if name != nil {
		value.Name = *name
	}
	if hidden != nil {
		value.Hidden = *hidden
	}
	next[port] = value
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(p.file), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(p.file), ".apps-*.json")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if _, err = file.Write(append(data, '\n')); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(temp, p.file); err != nil {
		return err
	}
	p.data = next
	return nil
}
