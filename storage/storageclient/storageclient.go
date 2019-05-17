// Copyright 2019 DxChain, All rights reserved.
// Use of this source code is governed by an Apache
// License 2.0 that can be found in the LICENSE file.

package storageclient

import (
	"errors"
	"github.com/DxChainNetwork/godx/common"
	"github.com/DxChainNetwork/godx/p2p"
	"github.com/DxChainNetwork/godx/storage"
	"path/filepath"
	"sync"

	"github.com/DxChainNetwork/godx/common/threadmanager"
	"github.com/DxChainNetwork/godx/log"
	"github.com/DxChainNetwork/godx/storage/storageclient/memorymanager"
	"github.com/DxChainNetwork/godx/storage/storageclient/storagehostmanager"
)

// ************** MOCKING DATA *****************
// *********************************************
type (
	contractManager   struct{}
	StorageContractID struct{}
	StorageHostEntry  struct{}
	streamCache       struct{}
	Wal               struct{}
)

// *********************************************
// *********************************************

// StorageClient contains fileds that are used to perform StorageHost
// selection operation, file uploading, downloading operations, and etc.
type StorageClient struct {
	// TODO (jacky): File Management Related

	// TODO (jacky): File Download Related

	// TODO (jacky): File Upload Related

	// Todo (jacky): File Recovery Related

	// Memory Management
	memoryManager *memorymanager.MemoryManager

	// contract manager and storage host manager
	contractManager    *contractManager
	storageHostManager *storagehostmanager.StorageHostManager

	// TODO (jacky): workerpool

	// Cache the hosts from the last price estimation result
	lastEstimationStorageHost []StorageHostEntry

	// Directories and File related
	persist        persistence
	persistDir     string
	staticFilesDir string

	// Utilities
	streamCache *streamCache
	log         log.Logger
	lock        sync.Mutex
	tm          threadmanager.ThreadManager
	wal         Wal

	// information on network, block chain, and etc.
	info       ParsedAPI
	ethBackend storage.EthBackend

	// get the P2P server for adding peer
	p2pServer *p2p.Server
}

// New initializes StorageClient object
func New(persistDir string) (*StorageClient, error) {

	// TODO (Jacky): data initialization
	sc := &StorageClient{
		persistDir:     persistDir,
		staticFilesDir: filepath.Join(persistDir, DxPathRoot),
	}

	sc.memoryManager = memorymanager.New(DefaultMaxMemory, sc.tm.StopChan())
	sc.storageHostManager = storagehostmanager.New(sc.persistDir)

	return sc, nil
}

// Start controls go routine checking and updating process
func (sc *StorageClient) Start(b storage.EthBackend, server *p2p.Server) error {
	// get the eth backend
	sc.ethBackend = b

	// validation
	if server == nil {
		return errors.New("failed to get the P2P server")
	}

	// get the p2p server for the adding peers
	sc.p2pServer = server

	// getting all needed API functions
	err := sc.filterAPIs(b.APIs())
	if err != nil {
		return err
	}

	// TODO: (mzhang) Initialize ContractManager & HostManager -> assign to StorageClient
	err = sc.storageHostManager.Start(sc.p2pServer, sc)
	if err != nil {
		return err
	}

	// Load settings from persist file
	if err := sc.loadPersist(); err != nil {
		return err
	}

	// TODO (mzhang): Subscribe consensus change

	// TODO (Jacky): DxFile / DxDirectory Update & Initialize Stream Cache

	// TODO (Jacky): Starting Worker, Checking file healthy, etc.

	// TODO (mzhang): Register On Stop Thread Control Function, waiting for WAL

	return nil
}

func (sc *StorageClient) Close() error {
	err := sc.storageHostManager.Close()
	errSC := sc.tm.Stop()
	return common.ErrCompose(err, errSC)
}

func (sc *StorageClient) setBandwidthLimits(uploadSpeedLimit int64, downloadSpeedLimit int64) error {
	// validation
	if uploadSpeedLimit < 0 || downloadSpeedLimit < 0 {
		return errors.New("upload/download speed limit cannot be negative")
	}

	// Update the contract settings accordingly
	if uploadSpeedLimit == 0 && downloadSpeedLimit == 0 {
		// TODO (mzhang): update contract settings using contract manager
	} else {
		// TODO (mzhang): update contract settings to the loaded data
	}

	return nil
}
