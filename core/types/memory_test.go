// Copyright 2019 DxChain, All rights reserved.
// Use of this source code is governed by an Apache
// License 2.0 that can be found in the LICENSE file.

package types

import (
	"bytes"
	"fmt"
	"hash"
	"sync"
	"testing"

	"golang.org/x/crypto/sha3"
)

var testString = "ahsdhfkasjkdfkasdkfjkasdkfhakshfkasdlfjasdfhalksdjhfkljashdfjkashdfkaskdflasdkfhkasssdfkjashjdkfadkfkashdfkajskdhfkasdhjaslkhdflka"
var shaRes = []byte{239, 243, 164, 0, 131, 103, 23, 137, 161, 71, 38, 30, 110, 160, 132, 208, 112, 170, 112, 222, 60, 144, 218, 120, 67, 232, 75, 102, 8, 139, 12, 140}

func BenchmarkNewHasher_singleThread(b *testing.B)  { benchmarkHash(b, 1, newHashGetter{}) }
func BenchmarkNewHasher_multiThread(b *testing.B)   { benchmarkHash(b, 3, newHashGetter{}) }
func BenchmarkSyncHasher_singleThread(b *testing.B) { benchmarkHash(b, 1, newSyncHashGetter()) }
func BenchmarkSyncHasher_multiThread(b *testing.B)  { benchmarkHash(b, 3, newSyncHashGetter()) }

func benchmarkHash(b *testing.B, threads int, hg hashGetter) {
	b.SetParallelism(threads)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			testHash(hg)
		}
	})
}

func testHash(hg hashGetter) {
	h := hg.getHasher()
	res := make([]byte, h.Size())
	h.Write([]byte(testString))
	h.Read(res)
	hg.returnHasher(h)
	if !bytes.Equal(res, shaRes) {
		panic(fmt.Sprint("expect", shaRes, "got", res))
	}
	return
}

type hashGetter interface {
	getHasher() sha
	returnHasher(sha)
}

type sha interface {
	hash.Hash
	Read([]byte) (int, error)
}

type newHashGetter struct{}

func (hg newHashGetter) getHasher() sha {
	return sha3.NewLegacyKeccak256().(sha)
}

func (hg newHashGetter) returnHasher(sha) {
	return
}

type syncHashGetter struct {
	pool sync.Pool
}

func newSyncHashGetter() *syncHashGetter {
	return &syncHashGetter{
		pool: sync.Pool{
			New: func() interface{} {
				return sha3.NewLegacyKeccak256().(sha)
			},
		},
	}
}

func (hg *syncHashGetter) getHasher() sha {
	h := hg.pool.Get().(sha)
	return h
}

func (hg *syncHashGetter) returnHasher(s sha) {
	s.Reset()
	hg.pool.Put(s)
}
