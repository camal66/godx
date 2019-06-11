// Copyright 2019 DxChain, All rights reserved.
// Use of this source code is governed by an Apache
// License 2.0 that can be found in the LICENSE file.

package storagehost

import (
	"errors"
	"time"

	"github.com/DxChainNetwork/godx/common"
)

var (
	errObligationLocked = errors.New("already locked")
)

//If not locked, create a new one
func (h *StorageHost) checkAndLockStorageObligation(soid common.Hash) {
	h.lock.Lock()
	defer h.lock.Unlock()

	tl, exists := h.lockedStorageObligations[soid]
	if !exists {
		tl = new(TryMutex)
		h.lockedStorageObligations[soid] = tl
	}
	tl.Lock()
}

//Try to lock this storage obligation
func (h *StorageHost) checkAndTryLockStorageObligation(soid common.Hash, timeout time.Duration) error {
	h.lock.Lock()
	defer h.lock.Unlock()
	tl, exists := h.lockedStorageObligations[soid]
	if !exists {
		tl = new(TryMutex)
		h.lockedStorageObligations[soid] = tl
	}

	if tl.TryLockTimed(timeout) {
		return nil
	}
	return errObligationLocked
}

//If it exists, unlock it
func (h *StorageHost) checkAndUnlockStorageObligation(soid common.Hash) {
	h.lock.Lock()
	defer h.lock.Unlock()

	tl, exists := h.lockedStorageObligations[soid]
	if !exists {
		return
	}
	tl.Unlock()

}
