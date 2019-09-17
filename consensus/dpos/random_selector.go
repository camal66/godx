// Copyright 2019 DxChain, All rights reserved.
// Use of this source code is governed by an Apache
// License 2.0 that can be found in the LICENSE file

package dpos

import (
	"math/big"
	"math/rand"
	"sync"

	"github.com/DxChainNetwork/godx/common"
	"github.com/pkg/errors"
)

// randomAddressSelector is the random selection algorithm for selecting multiple addresses
// The entries are passed in during initialization
type randomAddressSelector interface {
	RandomSelect() []common.Address
}

const (
	typeLuckyWheel = iota
)

type (
	// luckyWheel is the structure for lucky wheel random selection
	luckyWheel struct {
		// Initialized fields
		rand    *rand.Rand
		entries randomSelectorEntries
		target  int

		// results
		results []common.Address

		// In process
		sumVotes common.BigInt
		once     sync.Once
	}

	// randomSelectorEntries is the list of randomSelectorEntry
	randomSelectorEntries []*randomSelectorEntry

	// randomSelectorEntry is the entry in the lucky wheel
	randomSelectorEntry struct {
		addr common.Address
		vote common.BigInt
	}
)

// newRandomAddressSelector creates a randomAddressSelector with sepecified typeCode
func newRandomAddressSelector(typeCode int, entries randomSelectorEntries, seed int64, target int) (randomAddressSelector, error) {
	switch typeCode {
	case typeLuckyWheel:
		return newLuckyWheel(entries, seed, target)
	}
	return nil, errors.New("unknown randomAddressSelector type")
}

// newLuckyWheel create a lucky wheel for random selection. target is used for specifying
// the target number to be selected
func newLuckyWheel(entries randomSelectorEntries, seed int64, target int) (*luckyWheel, error) {
	if len(entries) < target {
		return nil, errRandomSelectNotEnoughEntries
	}
	sumVotes := common.BigInt0
	for _, entry := range entries {
		sumVotes = sumVotes.Add(entry.vote)
	}
	return &luckyWheel{
		rand:     rand.New(rand.NewSource(seed)),
		entries:  entries,
		target:   target,
		results:  make([]common.Address, target),
		sumVotes: sumVotes,
	}, nil
}

// RandomSelect return the result of the random selection of lucky wheel
func (lw *luckyWheel) RandomSelect() []common.Address {
	lw.once.Do(lw.randomSelect)
	return lw.results
}

// RandomSelect is a helper function that randomly select addresses from the lucky wheel.
// The execution result is added to lw.results field
func (lw *luckyWheel) randomSelect() {
	for i := 0; i < lw.target; i++ {
		// Execute the selection
		selectedIndex := lw.selectSingleEntry()
		selectedEntry := lw.entries[selectedIndex]
		// Add to result, and remove from entry
		lw.results = append(lw.results, selectedEntry.addr)
		if selectedIndex == len(lw.entries)-1 {
			lw.entries = lw.entries[:len(lw.entries)-1]
		} else {
			lw.entries = append(lw.entries[:selectedIndex], lw.entries[selectedIndex+1:]...)
		}
		// Subtract the vote weight from sumVotes
		lw.sumVotes.Sub(selectedEntry.vote)
	}
}

// selectSingleEntry select a single entry from the lucky Wheel. Return the selected index.
// No values updated in this function.
func (lw *luckyWheel) selectSingleEntry() int {
	selected := randomBigInt(lw.rand, lw.sumVotes)
	for i, entry := range lw.entries {
		vote := entry.vote
		// The entry is selected
		if vote.Cmp(selected) <= 0 {
			return i
		}
		selected = selected.Sub(vote)
	}
	// Sanity: This shall never reached if code is correct. If this happens, currently
	// return the last entry of the entries
	// TODO: Should we panic here?
	return len(lw.entries) - 1
}

// listAddresses return the list of addresses of the entries
func (entries randomSelectorEntries) listAddresses() []common.Address {
	var res []common.Address
	for _, entry := range entries {
		res = append(res, entry.addr)
	}
	return res
}

// randomBigInt return a random big integer between 0 and max using r as randomization
func randomBigInt(r *rand.Rand, max common.BigInt) common.BigInt {
	randNum := new(big.Int).Rand(r, max.BigIntPtr())
	return common.PtrBigInt(randNum)
}