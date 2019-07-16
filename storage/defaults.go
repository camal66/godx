// Copyright 2019 DxChain, All rights reserved.
// Use of this source code is governed by an Apache
// License 2.0 that can be found in the LICENSE file.

package storage

import (
	"github.com/DxChainNetwork/godx/common"

	"math/big"
	"time"
)

const (
	IDLE = 0
	BUSY = 1

	//HostConfigRespMsg         = 0x20
	//HostSettingResponseMsg = 0x21

	// Storage Contract Negotiate Protocol belonging to eth/64
	// Storage Contract Creation/Renew Code Msg
	StorageContractCreationMsg                   = 0x22
	StorageContractCreationHostSignMsg           = 0x23
	StorageContractCreationClientRevisionSignMsg = 0x24
	StorageContractCreationHostRevisionSignMsg   = 0x25

	// Upload Data Segment Code Msg
	StorageContractUploadRequestMsg         = 0x26
	StorageContractUploadMerkleRootProofMsg = 0x27
	StorageContractUploadClientRevisionMsg  = 0x28
	StorageContractUploadHostRevisionMsg    = 0x29

	// Download Data Segment Code Msg
	StorageContractDownloadRequestMsg      = 0x33
	StorageContractDownloadDataMsg         = 0x34
	StorageContractDownloadHostRevisionMsg = 0x35
	// error msg code
	NegotiationErrorMsg = 0x33
	// stop msg code
	NegotiationStopMsg = 0x34

	////////////////////////////////////

	// Client Handle Message Set
	HostConfigRespMsg    = 0x20
	UploadMerkleProofMsg = 0x21

	// Host Handle Message Set
	HostConfigReqMsg     = 0x30
	ContractCreateReqMsg = 0x31
	UploadReqMsg         = 0x32
)

// The block generation rate for Ethereum is 15s/block. Therefore, 240 blocks
// can be generated in an hour
var (
	BlockPerMin    = uint64(4)
	BlockPerHour   = uint64(240)
	BlocksPerDay   = 24 * BlockPerHour
	BlocksPerWeek  = 7 * BlocksPerDay
	BlocksPerMonth = 30 * BlocksPerDay
	BlocksPerYear  = 365 * BlocksPerDay

	ResponsibilityLockTimeout = 60 * time.Second
)

// Default rentPayment values
var (
	DefaultRentPayment = RentPayment{
		Fund:         common.PtrBigInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
		StorageHosts: 3,
		Period:       3 * BlocksPerDay,
		RenewWindow:  12 * BlockPerHour,

		ExpectedStorage:    1e12,                           // 1 TB
		ExpectedUpload:     uint64(200e9) / BlocksPerMonth, // 200 GB per month
		ExpectedDownload:   uint64(100e9) / BlocksPerMonth, // 100 GB per month
		ExpectedRedundancy: 2.0,
	}
)
