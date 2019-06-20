package storagehost

import (
	"github.com/davecgh/go-spew/spew"
)

// print the persist directory of the host
func (h *StorageHost) getPersistDir() string {
	h.lock.RLock()
	defer h.lock.RUnlock()

	return h.persistDir
}

// print the structure persistence of storage host
func (h *StorageHost) getHostPersist() {
	h.lock.RLock()
	defer h.lock.RUnlock()

	persist := h.extractPersistence()
	spew.Dump(persist)
}

// print the internal config of the host
func (h *StorageHost) getIntConfig() {
	spew.Dump(h.internalConfig())
}

// print the host financial metrics
func (h *StorageHost) getFinancialMetrics() {
	spew.Dump(h.financialStats())
}

// load the internal config back to default
// Warning: make sure you understand this step to continue do the operation
// It will rewrite the config file
func (h *StorageHost) setDefault() {
	h.config = defaultConfig()
	// synchronize to file
	if err := h.syncConfig(); err != nil {
		h.log.Warn(err.Error())
	}
}

// Set the broadcast to a boolean value and save into the config file
// Warning: make sure you understand this step to continue do the operation
// It will rewrite the config file
func (h *StorageHost) setBroadCast(b bool) {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.broadcast = b
	if err := h.syncConfig(); err != nil {
		h.log.Warn(err.Error())
	}
}

// Set the revision number and save into the config file
// Warning: make sure you understand this step to continue do the operation
// It will rewrite the config file
func (h *StorageHost) setRevisionNumber(num int) {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.revisionNumber = uint64(num)
	if err := h.syncConfig(); err != nil {
		h.log.Warn(err.Error())
	}
}

// load the financial metrics to the host
// Warning: make sure you understand this step to continue do the operation
// It will rewrite the config file
func (h *StorageHost) setFinancialMetrics(metric HostFinancialMetrics) {
	h.lock.Lock()
	defer h.lock.Unlock()

	// directly load the financial metrics to the host
	h.financialMetrics = metric

	// synchronize to file
	if err := h.syncConfig(); err != nil {
		h.log.Warn(err.Error())
	}
}
