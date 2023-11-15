package managedplugin

import "sync/atomic"

type Metrics struct {
	Errors   uint64
	Warnings uint64
}

func (m *Metrics) incrementErrors() {
	atomic.AddUint64(&m.Errors, 1)
}

func (m *Metrics) incrementWarnings() {
	atomic.AddUint64(&m.Warnings, 1)
}
