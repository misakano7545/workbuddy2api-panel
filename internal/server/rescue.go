package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/prompt"
)

// rescueMemory retains only hashes and confirmed transformation sequences.
// Entries are isolated by realm, model, mode, system/developer and tool schema.
type rescueMemory struct {
	mu      sync.Mutex
	entries map[string]rescueEntry
}
type rescueEntry struct {
	steps         []int
	created, used time.Time
}

func rescueKey(body []byte, realm, model, mode, text string) string {
	if mode != "passthrough" && mode != "append" {
		return ""
	}
	var request struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Tools any `json:"tools"`
	}
	if json.Unmarshal(body, &request) != nil {
		return ""
	}
	var systems []any
	for _, m := range request.Messages {
		if m.Role != "system" && m.Role != "developer" {
			continue
		}
		content := m.Content
		if s, ok := content.(string); ok {
			var lines []string
			for _, line := range strings.Split(s, "\n") {
				if !strings.HasPrefix(strings.TrimSpace(line), "x-anthropic-billing-header:") {
					lines = append(lines, line)
				}
			}
			content = strings.Join(lines, "\n")
		}
		systems = append(systems, []any{m.Role, content})
	}
	if len(systems) == 0 {
		return ""
	}
	b, err := json.Marshal([]any{realm, model, mode, text, systems, request.Tools})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (m *rescueMemory) lookup(key string, now time.Time) []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		return nil
	}
	// Retry original input every 30 minutes to restore role semantics when the
	// upstream rule relaxes. Successful cache hits do not renew this deadline.
	if now.Sub(e.created) >= 30*time.Minute {
		delete(m.entries, key)
		return nil
	}
	e.used = now
	m.entries[key] = e
	return append([]int(nil), e.steps...)
}
func (m *rescueMemory) forget(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, key)
}
func (m *rescueMemory) remember(key string, steps []int, now time.Time) {
	if key == "" || len(steps) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = make(map[string]rescueEntry)
	}
	if e, ok := m.entries[key]; ok {
		e.used = now
		m.entries[key] = e
		return
	}
	if len(m.entries) >= 1024 {
		var oldest string
		var at time.Time
		for k, e := range m.entries {
			if oldest == "" || e.used.Before(at) {
				oldest, at = k, e.used
			}
		}
		delete(m.entries, oldest)
	}
	m.entries[key] = rescueEntry{append([]int(nil), steps...), now, now}
}

// Replay exactly in order: moving after splitting differs from moving the
// original complete system message.
func applyRescueStep(body []byte, step int) ([]byte, bool) {
	switch step {
	case 1:
		return prompt.SplitSystemMessages(body)
	case 2:
		return prompt.MoveSystemToUser(body, prompt.Degraded)
	case 3:
		return prompt.Rewrite(body, prompt.Degraded), true
	}
	return body, false
}
