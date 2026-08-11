package ingest

import "sync"

type durabilityRegistry struct {
	manager *Manager
	wg      sync.WaitGroup
}

func newDurabilityRegistry(m *Manager) *durabilityRegistry { return &durabilityRegistry{manager: m} }

func (r *durabilityRegistry) wait() { r.wg.Wait() }
