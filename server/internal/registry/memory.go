package registry

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// MemoryStore 是 Store 的内存实现：单测与本地联调用，也可在 PG 不可用时兜底。
// 生产路径由 backend-core 的 store 适配（同签名）。
type MemoryStore struct {
	mu      sync.RWMutex
	records map[string]Record
}

// NewMemoryStore 构造空的内存 store。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: make(map[string]Record)}
}

var _ Store = (*MemoryStore)(nil)

// UpsertTester 写入或覆盖一行；content_sha 不同不在这里拦（由 Registry 判定）。
func (m *MemoryStore) UpsertTester(ctx context.Context, rec Record) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := Key(rec.Spec.Name, rec.Spec.Version)
	_, existed := m.records[key]
	m.records[key] = rec
	return !existed, nil
}

// GetTester 按 name+version 读一行；version 为空取最新。
func (m *MemoryStore) GetTester(ctx context.Context, name, version string) (Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if version == "" {
		for _, rec := range m.records {
			if rec.Spec.Name != name {
				continue
			}
			if version == "" || isLater(rec.Spec.Version, version) {
				version = rec.Spec.Version
			}
		}
	}
	rec, ok := m.records[Key(name, version)]
	if !ok {
		return Record{}, fmt.Errorf("registry: tester %s@%s: %w", name, version, domain.ErrNotFound)
	}
	return rec, nil
}

// ListTesters 列出全部记录，按 name/version 排序。
func (m *MemoryStore) ListTesters(ctx context.Context) ([]Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Record, 0, len(m.records))
	for _, rec := range m.records {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Spec.Name != out[j].Spec.Name {
			return out[i].Spec.Name < out[j].Spec.Name
		}
		return out[i].Spec.Version < out[j].Spec.Version
	})
	return out, nil
}

// MarkTesterOrphaned 把一行标记为 orphaned（只标记，不删）。
func (m *MemoryStore) MarkTesterOrphaned(ctx context.Context, name, version string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := Key(name, version)
	rec, ok := m.records[key]
	if !ok {
		return fmt.Errorf("registry: mark orphaned %s: %w", key, domain.ErrNotFound)
	}
	rec.Orphaned = true
	m.records[key] = rec
	return nil
}

func isLater(candidate, current string) bool {
	return versionLess(current, candidate)
}

// normalizeRole 让 List 的 role 过滤容忍大小写与空白。
func normalizeRole(role string) string { return strings.ToLower(strings.TrimSpace(role)) }
