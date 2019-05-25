// Copyright 2019 DxChain, All rights reserved.
// Use of this source code is governed by an Apache
// License 2.0 that can be found in the LICENSE file.

package storageclient

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"
	"math/big"
	"math/bits"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/DxChainNetwork/godx/storage/storageclient/filesystem/dxfile"

	"github.com/DxChainNetwork/godx/accounts"
	"github.com/DxChainNetwork/godx/common"
	"github.com/DxChainNetwork/godx/common/hexutil"
	"github.com/DxChainNetwork/godx/common/threadmanager"
	"github.com/DxChainNetwork/godx/core/types"
	"github.com/DxChainNetwork/godx/crypto"
	"github.com/DxChainNetwork/godx/log"
	"github.com/DxChainNetwork/godx/p2p"
	"github.com/DxChainNetwork/godx/p2p/enode"
	"github.com/DxChainNetwork/godx/params"
	"github.com/DxChainNetwork/godx/rlp"
	"github.com/DxChainNetwork/godx/rpc"
	"github.com/DxChainNetwork/godx/storage"
	"github.com/DxChainNetwork/godx/storage/storageclient/memorymanager"
	"github.com/DxChainNetwork/godx/storage/storageclient/storagehostmanager"
	"github.com/DxChainNetwork/godx/storage/storagehost"
)

var (
	zeroValue = new(big.Int).SetInt64(0)

	extraRatio = 0.02
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

// Backend allows Ethereum object to be passed in as interface
type Backend interface {
	APIs() []rpc.API
	AccountManager() *accounts.Manager
	SuggestPrice(ctx context.Context) (*big.Int, error)
	GetPoolNonce(ctx context.Context, addr common.Address) (uint64, error)
	ChainConfig() *params.ChainConfig
	CurrentBlock() *types.Block
	SendTx(ctx context.Context, signedTx *types.Transaction) error
}

// StorageClient contains fileds that are used to perform StorageHost
// selection operation, file uploading, downloading operations, and etc.
type StorageClient struct {

	// TODO (jacky): File Upload Related

	// Todo (jacky): File Recovery Related

	// Memory Management
	memoryManager *memorymanager.MemoryManager

	// contract manager and storage host manager
	contractManager    *contractManager
	storageHostManager *storagehostmanager.StorageHostManager

	// Download management. The heap has a separate mutex because it is always
	// accessed in isolation.
	downloadHeapMu sync.Mutex           // Used to protect the downloadHeap.
	downloadHeap   *downloadSegmentHeap // A heap of priority-sorted segments to download.
	newDownloads   chan struct{}        // Used to notify download loop that new downloads are available.

	// List of workers that can be used for uploading and/or downloading.
	workerPool map[common.Hash]*worker

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
	b          Backend

	// get the P2P server for adding peer
	p2pServer *p2p.Server

	// file management.
	staticFileSet *dxfile.FileSet
}

// New initializes StorageClient object
func New(persistDir string) (*StorageClient, error) {

	// TODO (Jacky): data initialization
	sc := &StorageClient{
		persistDir:     persistDir,
		staticFilesDir: filepath.Join(persistDir, DxPathRoot),
		newDownloads:   make(chan struct{}, 1),
		downloadHeap:   new(downloadSegmentHeap),
		workerPool:     make(map[common.Hash]*worker),
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

	// active the work pool to get a worker for a upload/download task.
	sc.activateWorkerPool()

	// loop to download
	go sc.downloadLoop()

	// kill workers on shutdown.
	sc.tm.OnStop(func() error {
		sc.lock.Lock()
		for _, worker := range sc.workerPool {
			close(worker.killChan)
		}
		sc.lock.Unlock()
		return nil
	})

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

func (sc *StorageClient) ContractCreate(params ContractParams) error {
	// Extract vars from params, for convenience
	allowance, funding, clientPublicKey, startHeight, endHeight, host := params.Allowance, params.Funding, params.ClientPublicKey, params.StartHeight, params.EndHeight, params.Host

	// Calculate the payouts for the renter, host, and whole contract
	period := endHeight - startHeight
	expectedStorage := allowance.ExpectedStorage / allowance.Hosts
	renterPayout, hostPayout, _, err := RenterPayoutsPreTax(host, funding, zeroValue, zeroValue, period, expectedStorage)
	if err != nil {
		return err
	}

	uc := types.UnlockConditions{
		PublicKeys: []ecdsa.PublicKey{
			clientPublicKey,
			host.PublicKey,
		},
		SignaturesRequired: 2,
	}

	clientAddr := crypto.PubkeyToAddress(clientPublicKey)
	hostAddr := crypto.PubkeyToAddress(host.PublicKey)

	// Create storage contract
	storageContract := types.StorageContract{
		FileSize:         0,
		FileMerkleRoot:   common.Hash{}, // no proof possible without data
		WindowStart:      endHeight,
		WindowEnd:        endHeight + host.WindowSize,
		RenterCollateral: types.DxcoinCollateral{types.DxcoinCharge{Value: renterPayout}},
		HostCollateral:   types.DxcoinCollateral{types.DxcoinCharge{Value: hostPayout}},
		UnlockHash:       uc.UnlockHash(),
		RevisionNumber:   0,
		ValidProofOutputs: []types.DxcoinCharge{
			// Deposit is returned to client
			{Value: renterPayout, Address: clientAddr},
			// Deposit is returned to host
			{Value: hostPayout, Address: hostAddr},
		},
		MissedProofOutputs: []types.DxcoinCharge{
			{Value: renterPayout, Address: clientAddr},
			{Value: hostPayout, Address: hostAddr},
		},
	}

	// TODO: 记录与当前host协商交互的结果，用于后续健康度检查
	//defer func() {
	//	if err != nil {
	//		hdb.IncrementFailedInteractions(host.PublicKey)
	//		err = errors.Extend(err, modules.ErrHostFault)
	//	} else {
	//		hdb.IncrementSuccessfulInteractions(host.PublicKey)
	//	}
	//}()

	// Setup connection with storage host
	session, err := sc.ethBackend.SetupConnection(host.NetAddress)
	defer sc.ethBackend.Disconnect(host.NetAddress)

	// Send the ContractCreate request
	req := storage.ContractCreateRequest{
		StorageContract: storageContract,
		ClientPK:        uc.PublicKeys[0],
	}

	if err := session.SendStorageContractCreation(req); err != nil {
		return err
	}

	var hostSign []byte
	if msg, err := session.ReadMsg(); err != nil {
		if err := msg.Decode(&hostSign); err != nil {
			return err
		}
	} else {
		return err
	}
	storageContract.Signatures[1] = hostSign

	// Assemble init revision and sign it
	storageContractRevision := types.StorageContractRevision{
		ParentID:          storageContract.RLPHash(),
		UnlockConditions:  uc,
		NewRevisionNumber: 1,

		NewFileSize:           storageContract.FileSize,
		NewFileMerkleRoot:     storageContract.FileMerkleRoot,
		NewWindowStart:        storageContract.WindowStart,
		NewWindowEnd:          storageContract.WindowEnd,
		NewValidProofOutputs:  storageContract.ValidProofOutputs,
		NewMissedProofOutputs: storageContract.MissedProofOutputs,
		NewUnlockHash:         storageContract.UnlockHash,
	}

	account := accounts.Account{Address: storageContract.ValidProofOutputs[0].Address}
	wallet, err := sc.ethBackend.AccountManager().Find(account)
	if err != nil {
		return storagehost.ExtendErr("find client account error", err)
	}

	clientContractSign, err := wallet.SignHash(account, storageContract.RLPHash().Bytes())
	if err != nil {
		return storagehost.ExtendErr("client sign contract error", err)
	}
	storageContract.Signatures[0] = clientContractSign

	clientRevisionSign, err := wallet.SignHash(account, storageContractRevision.RLPHash().Bytes())
	if err != nil {
		return storagehost.ExtendErr("client sign revision error", err)
	}
	storageContractRevision.Signatures = [][]byte{clientRevisionSign}

	clientSigns := storage.ContractCreateSignature{ContractSign: clientContractSign, RevisionSign: clientRevisionSign}
	if err := session.SendStorageContractCreationClientRevisionSign(clientSigns); err != nil {
		return storagehost.ExtendErr("send revision sign by client error", err)
	}

	var hostRevisionSign []byte
	if msg, err := session.ReadMsg(); err != nil {
		if err := msg.Decode(&hostRevisionSign); err != nil {
			return err
		}
	} else {
		return err
	}

	scBytes, err := rlp.EncodeToBytes(storageContract)
	if err != nil {
		return err
	}

	sendAPI := NewStorageContractTxAPI(sc.b)
	args := SendStorageContractTxArgs{
		From: clientAddr,
	}
	addr := common.Address{}
	addr.SetBytes([]byte{10})
	args.To = &addr
	args.Input = (*hexutil.Bytes)(&scBytes)
	ctx := context.Background()
	if _, err := sendAPI.SendFormContractTX(ctx, args); err != nil {
		return storagehost.ExtendErr("Send storage contract transaction error", err)
	}

	// TODO: 构造这个合约信息.
	//header := contractHeader{
	//	Transaction: revisionTxn,
	//	SecretKey:   ourSK,
	//	StartHeight: startHeight,
	//	TotalCost:   funding,
	//	ContractFee: host.ContractPrice,
	//	TxnFee:      txnFee,
	//	SiafundFee:  types.Tax(startHeight, fc.Payout),
	//	Utility: modules.ContractUtility{
	//		GoodForUpload: true,
	//		GoodForRenew:  true,
	//	},
	//}

	// TODO: 保存这个合约信息到本地
	//meta, err := cs.managedInsertContract(header, nil) // no Merkle roots yet
	//if err != nil {
	//	return RenterContract{}, err
	//}
	return nil
}

func (sc *StorageClient) Append(session *storage.Session, data []byte) error {
	return sc.Write(session, []storage.UploadAction{{Type: storage.UploadActionAppend, Data: data}})
}

func (sc *StorageClient) Write(session *storage.Session, actions []storage.UploadAction) error {

	// TODO: 获取最新的storage contract revision
	// Retrieve the contract
	// TODO client.contractManager.GetContract()
	contractRevision := &types.StorageContractRevision{}

	// Calculate price per sector
	hostInfo := session.HostInfo()
	blockBytes := storage.SectorSize * uint64(contractRevision.NewWindowEnd-sc.ethBackend.GetCurrentBlockHeight())
	sectorBandwidthPrice := hostInfo.UploadBandwidthPrice.MultUint64(storage.SectorSize)
	sectorStoragePrice := hostInfo.StoragePrice.MultUint64(blockBytes)
	sectorDeposit := hostInfo.Deposit.MultUint64(blockBytes)

	// Calculate the new Merkle root set and total cost/collateral
	var bandwidthPrice, storagePrice, deposit *big.Int
	newFileSize := contractRevision.NewFileSize
	for _, action := range actions {
		switch action.Type {
		case storage.UploadActionAppend:
			bandwidthPrice = bandwidthPrice.Add(bandwidthPrice, sectorBandwidthPrice.BigIntPtr())
			newFileSize += storage.SectorSize
		}
	}

	if newFileSize > contractRevision.NewFileSize {
		addedSectors := (newFileSize - contractRevision.NewFileSize) / storage.SectorSize
		storagePrice = sectorStoragePrice.MultUint64(addedSectors).BigIntPtr()
		deposit = sectorDeposit.MultUint64(addedSectors).BigIntPtr()
	}

	// Estimate cost of Merkle proof
	proofSize := storage.HashSize * (128 + len(actions))
	bandwidthPrice = bandwidthPrice.Add(bandwidthPrice, hostInfo.DownloadBandwidthPrice.MultUint64(uint64(proofSize)).BigIntPtr())

	cost := new(big.Int).Add(bandwidthPrice.Add(bandwidthPrice, storagePrice), hostInfo.BaseRPCPrice.BigIntPtr())

	// Check that enough funds are available
	if contractRevision.NewValidProofOutputs[0].Value.Cmp(cost) < 0 {
		return errors.New("contract has insufficient funds to support upload")
	}
	if contractRevision.NewMissedProofOutputs[1].Value.Cmp(deposit) < 0 {
		return errors.New("contract has insufficient collateral to support upload")
	}

	// Create the revision; we will update the Merkle root later
	rev := NewRevision(*contractRevision, cost)
	rev.NewMissedProofOutputs[1].Value = rev.NewMissedProofOutputs[1].Value.Sub(rev.NewMissedProofOutputs[1].Value, deposit)
	rev.NewFileSize = newFileSize

	// Create the request
	req := storage.UploadRequest{
		StorageContractID: contractRevision.ParentID,
		Actions:           actions,
		NewRevisionNumber: rev.NewRevisionNumber,
	}
	req.NewValidProofValues = make([]*big.Int, len(rev.NewValidProofOutputs))
	for i, o := range rev.NewValidProofOutputs {
		req.NewValidProofValues[i] = o.Value
	}
	req.NewMissedProofValues = make([]*big.Int, len(rev.NewMissedProofOutputs))
	for i, o := range rev.NewMissedProofOutputs {
		req.NewMissedProofValues[i] = o.Value
	}

	// 1. Send storage upload request
	if err := session.SendStorageContractUploadRequest(req); err != nil {
		return err
	}

	// 2. Read merkle proof response from host
	var merkleResp storage.UploadMerkleProof
	if msg, err := session.ReadMsg(); err != nil {
		return err
	} else {
		if err := msg.Decode(&merkleResp); err != nil {
			return err
		}
	}

	// Verify merkle proof
	numSectors := contractRevision.NewFileSize / storage.SectorSize
	proofRanges := storage.CalculateProofRanges(actions, numSectors)
	proofHashes := merkleResp.OldSubtreeHashes
	leafHashes := merkleResp.OldLeafHashes
	oldRoot, newRoot := contractRevision.NewFileMerkleRoot, merkleResp.NewMerkleRoot
	if !storage.VerifyDiffProof(proofRanges, numSectors, proofHashes, leafHashes, oldRoot) {
		return errors.New("invalid Merkle proof for old root")
	}
	// ...then by modifying the leaves and verifying the new Merkle root
	leafHashes = storage.ModifyLeaves(leafHashes, actions, numSectors)
	proofRanges = storage.ModifyProofRanges(proofRanges, actions, numSectors)
	if !storage.VerifyDiffProof(proofRanges, numSectors, proofHashes, leafHashes, newRoot) {
		return errors.New("invalid Merkle proof for new root")
	}

	// Update the revision, sign it, and send it
	rev.NewFileMerkleRoot = newRoot

	var clientRevisionSign []byte
	// TODO get account and sign revision
	if err := session.SendStorageContractUploadClientRevisionSign(clientRevisionSign); err != nil {
		return err
	}
	rev.Signatures[0] = clientRevisionSign

	// Read the host's signature
	var hostRevisionSig []byte
	if msg, err := session.ReadMsg(); err != nil {
		return err
	} else {
		if err := msg.Decode(&hostRevisionSig); err != nil {
			return err
		}
	}
	rev.Signatures[1] = clientRevisionSign

	// TODO update contract
	//err = sc.commitUpload(walTxn, txn, crypto.Hash{}, storagePrice, bandwidthPrice)
	//if err != nil {
	//	return modules.RenterContract{}, err
	//}

	return nil
}

// download

// Read calls the Read RPC, writing the requested data to w. The RPC can be
// cancelled (with a granularity of one section) via the cancel channel.
func (client *StorageClient) Read(s *storage.Session, w io.Writer, req storage.DownloadRequest, cancel <-chan struct{}) (err error) {

	// TODO: Reset deadline when finished.
	//defer extendDeadline(s.conn, time.Hour)

	// Sanity-check the request.
	for _, sec := range req.Sections {
		if uint64(sec.Offset)+uint64(sec.Length) > storage.SectorSize {
			return errors.New("illegal offset and/or length")
		}
		if req.MerkleProof {
			if sec.Offset%storage.SegmentSize != 0 || sec.Length%storage.SegmentSize != 0 {
				return errors.New("offset and length must be multiples of SegmentSize when requesting a Merkle proof")
			}
		}
	}

	// calculate estimated bandwidth
	var totalLength uint64
	for _, sec := range req.Sections {
		totalLength += uint64(sec.Length)
	}
	var estProofHashes uint64
	if req.MerkleProof {

		// use the worst-case proof size of 2*tree depth (this occurs when
		// proving across the two leaves in the center of the tree)
		estHashesPerProof := 2 * bits.Len64(storage.SectorSize/storage.SegmentSize)
		estProofHashes = uint64(len(req.Sections) * estHashesPerProof)
	}
	estBandwidth := totalLength + estProofHashes*uint64(storage.HashSize)
	if estBandwidth < storage.RPCMinLen {
		estBandwidth = storage.RPCMinLen
	}

	// calculate sector accesses
	sectorAccesses := make(map[common.Hash]struct{})
	for _, sec := range req.Sections {
		sectorAccesses[sec.MerkleRoot] = struct{}{}
	}

	// TODO: 获取最新的storage contract last revision
	lastRevision := types.StorageContractRevision{}

	// calculate price
	hostInfo := s.HostInfo()
	bandwidthPrice := hostInfo.DownloadBandwidthPrice.MultUint64(estBandwidth)
	sectorAccessPrice := hostInfo.SectorAccessPrice.MultUint64(uint64(len(sectorAccesses)))

	price := hostInfo.BaseRPCPrice.Add(bandwidthPrice).Add(sectorAccessPrice)
	if lastRevision.NewValidProofOutputs[0].Value.Cmp(price.BigIntPtr()) < 0 {
		return errors.New("contract has insufficient funds to support download")
	}

	// To mitigate small errors (e.g. differing block heights), fudge the
	// price and collateral by 0.2%.
	price = price.MultFloat64(1 + extraRatio)

	// create the download revision and sign it
	newRevision := storage.NewDownloadRevision(lastRevision, price.BigIntPtr())

	// client sign the revision
	am := client.ethBackend.AccountManager()
	account := accounts.Account{Address: newRevision.NewValidProofOutputs[0].Address}
	wallet, err := am.Find(account)
	if err != nil {
		return err
	}

	sig, err := wallet.SignHash(account, newRevision.RLPHash().Bytes())
	if err != nil {
		return err
	}

	req.NewRevisionNumber = newRevision.NewRevisionNumber
	req.NewValidProofValues = make([]*big.Int, len(newRevision.NewValidProofOutputs))
	for i, nvpo := range newRevision.NewValidProofOutputs {
		req.NewValidProofValues[i] = nvpo.Value
	}
	req.NewMissedProofValues = make([]*big.Int, len(newRevision.NewMissedProofOutputs))
	for i, nmpo := range newRevision.NewMissedProofOutputs {
		req.NewMissedProofValues[i] = nmpo.Value
	}
	req.Signature = sig[:]

	// TODO: 记录renter对合约的修改，以防节点断电或其他异常导致的revision被中断，下次节点启动就可以继续完成这个revision，相当于断点续传
	//walTxn, err := sc.recordDownloadIntent(newRevision, price)
	//if err != nil {
	//	return err
	//}

	// Increase Successful/Failed interactions accordingly
	defer func() {
		hostPubkey := lastRevision.UnlockConditions.PublicKeys[1]
		pubkeyHex := PubkeyToHex(&hostPubkey)
		hostID := enode.HexID(pubkeyHex)
		if err != nil {
			client.storageHostManager.IncrementFailedInteractions(hostID)
		} else {
			client.storageHostManager.IncrementSuccessfulInteractions(hostID)
		}
	}()

	// TODO: Disrupt before sending the signed revision to the host.
	//if s.deps.Disrupt("InterruptDownloadBeforeSendingRevision") {
	//	return errors.New("InterruptDownloadBeforeSendingRevision disrupt")
	//}

	// send download request
	//TODO: extendDeadline(s.conn, modules.NegotiateDownloadTime)
	err = s.SendStorageContractDownloadRequest(req)
	if err != nil {
		return err
	}

	// spawn a goroutine to handle cancellation
	doneChan := make(chan struct{})
	go func() {
		select {
		case <-cancel:
		case <-doneChan:
		}

		// TODO: 是否需要发送stop消息通知host
		//s.writeResponse(modules.RPCLoopReadStop, nil)
	}()

	// ensure we send DownloadStop before returning
	defer close(doneChan)

	// read responses
	var hostSig []byte
	for _, sec := range req.Sections {
		var resp storage.DownloadResponse
		msg, err := s.ReadMsg()
		if err != nil {
			return err
		}

		err = msg.Decode(&resp)
		if err != nil {
			return err
		}

		// The host may have sent data, a signature, or both. If they sent data, validate it.
		if len(resp.Data) > 0 {
			if len(resp.Data) != int(sec.Length) {
				return errors.New("host did not send enough sector data")
			}
			if req.MerkleProof {
				proofStart := int(sec.Offset) / storage.SegmentSize
				proofEnd := int(sec.Offset+sec.Length) / storage.SegmentSize
				if !storage.VerifyRangeProof(resp.Data, resp.MerkleProof, proofStart, proofEnd, sec.MerkleRoot) {
					return errors.New("host provided incorrect sector data or Merkle proof")
				}
			}

			// write sector data
			if _, err := w.Write(resp.Data); err != nil {
				return err
			}
		}

		// If the host sent a signature, exit the loop; they won't be sending any more data
		if len(resp.Signature) > 0 {
			hostSig = resp.Signature
			break
		}
	}
	if hostSig == nil {

		// the host is required to send a signature; if they haven't sent one
		// yet, they should send an empty response containing just the signature.
		var resp storage.DownloadResponse
		msg, err := s.ReadMsg()
		if err != nil {
			return err
		}

		err = msg.Decode(&resp)
		if err != nil {
			return err
		}

		hostSig = resp.Signature
	}
	newRevision.Signatures[1] = hostSig

	// TODO: Disrupt before commiting.
	//if s.deps.Disrupt("InterruptDownloadAfterSendingRevision") {
	//	return errors.New("InterruptDownloadAfterSendingRevision disrupt")
	//}

	// TODO: update contract and metrics
	//if err := sc.commitDownload(walTxn, txn, price); err != nil {
	//	return err
	//}

	return nil
}

// Download calls the Read RPC with a single section and returns the
// requested data. A Merkle proof is always requested.
func (client *StorageClient) Download(s *storage.Session, root common.Hash, offset, length uint32) ([]byte, error) {
	client.lock.Lock()
	defer client.lock.Unlock()

	req := storage.DownloadRequest{
		Sections: []storage.DownloadRequestSection{{
			MerkleRoot: root,
			Offset:     offset,
			Length:     length,
		}},
		MerkleProof: true,
	}
	var buf bytes.Buffer
	err := client.Read(s, &buf, req, nil)
	return buf.Bytes(), err
}

// newDownload creates and initializes a download task based on the provided parameters from outer request
func (client *StorageClient) newDownload(params downloadParams) (*download, error) {

	// params validation.
	if params.file == nil {
		return nil, errors.New("no file provided when requesting download")
	}
	if params.length < 0 {
		return nil, errors.New("download length must be zero or a positive whole number")
	}
	if params.offset < 0 {
		return nil, errors.New("download offset cannot be a negative number")
	}
	if params.offset+params.length > params.file.FileSize() {
		return nil, errors.New("download is requesting data past the boundary of the file")
	}

	// instantiate the download object.
	d := &download{
		completeChan:          make(chan struct{}),
		staticStartTime:       time.Now(),
		destination:           params.destination,
		destinationString:     params.destinationString,
		staticDestinationType: params.destinationType,
		staticLatencyTarget:   params.latencyTarget,
		staticLength:          params.length,
		staticOffset:          params.offset,
		staticOverdrive:       params.overdrive,
		staticDxFilePath:      params.file.DxPath(),
		staticPriority:        params.priority,
		log:                   client.log,
		memoryManager:         client.memoryManager,
	}

	// set the end time of the download when it's done.
	d.onComplete(func(_ error) error {
		d.endTime = time.Now()
		return nil
	})

	// nothing more to do for 0-byte files or 0-length downloads.
	if d.staticLength == 0 {
		d.markComplete()
		return d, nil
	}

	// determine which segments to download.
	minSegment, minSegmentOffset := params.file.SegmentIndexByOffset(params.offset)
	maxSegment, maxSegmentOffset := params.file.SegmentIndexByOffset(params.offset + params.length)

	// if the maxSegmentOffset is exactly 0 we need to subtract 1 segment. e.g. if
	// the segmentSize is 100 bytes and we want to download 100 bytes from offset
	// 0, maxSegment would be 1 and maxSegmentOffset would be 0. We want maxSegment
	// to be 0 though since we don't actually need any data from segment 1.
	if maxSegment > 0 && maxSegmentOffset == 0 {
		maxSegment--
	}

	// make sure the requested segments are within the boundaries.
	if minSegment == params.file.NumSegments() || maxSegment == params.file.NumSegments() {
		return nil, errors.New("download is requesting a segment that is past the boundary of the file")
	}

	// for each segment, assemble a mapping from the contract id to the index of
	// the sector within the segment that the contract is responsible for.
	segmentMaps := make([]map[string]downloadSectorInfo, maxSegment-minSegment+1)
	for segmentIndex := minSegment; segmentIndex <= maxSegment; segmentIndex++ {
		segmentMaps[segmentIndex-minSegment] = make(map[string]downloadSectorInfo)
		sectors, err := params.file.Sectors(uint64(segmentIndex))
		if err != nil {
			return nil, err
		}
		for sectorIndex, sectorSet := range sectors {
			for _, sector := range sectorSet {

				// sanity check - a worker should not have two sectors for the same segment.
				_, exists := segmentMaps[segmentIndex-minSegment][sector.HostID.String()]
				if exists {
					client.log.Error("ERROR: Worker has multiple sectors uploaded for the same segment.")
				}
				segmentMaps[segmentIndex-minSegment][sector.HostID.String()] = downloadSectorInfo{
					index: uint64(sectorIndex),
					root:  sector.MerkleRoot,
				}
			}
		}
	}

	// where to write a segment within the download destination
	writeOffset := int64(0)
	d.segmentsRemaining += maxSegment - minSegment + 1

	// queue the downloads for each segment.
	for i := minSegment; i <= maxSegment; i++ {
		uds := &unfinishedDownloadSegment{
			destination:        params.destination,
			erasureCode:        params.file.ErasureCode(),
			masterKey:          params.file.CipherKey(),
			staticSegmentIndex: i,
			staticCacheID:      fmt.Sprintf("%v:%v", d.staticDxFilePath, i),
			staticSegmentMap:   segmentMaps[i-minSegment],
			staticSegmentSize:  params.file.SegmentSize(),
			staticSectorSize:   params.file.SectorSize(),

			// increase target by 25ms per segment
			staticLatencyTarget: params.latencyTarget + (25 * time.Duration(i-minSegment)),
			staticNeedsMemory:   params.needsMemory,
			staticPriority:      params.priority,
			completedSectors:    make([]bool, params.file.ErasureCode().NumSectors()),
			physicalSegmentData: make([][]byte, params.file.ErasureCode().NumSectors()),
			sectorUsage:         make([]bool, params.file.ErasureCode().NumSectors()),
			download:            d,
			renterFile:          params.file,
			staticStreamCache:   client.streamCache,
		}

		// set the offset within the segment that we start downloading from
		if i == minSegment {
			uds.staticFetchOffset = minSegmentOffset
		} else {
			uds.staticFetchOffset = 0
		}

		// set the number of bytes to fetch within the segment that we start downloading from
		if i == maxSegment && maxSegmentOffset != 0 {
			uds.staticFetchLength = maxSegmentOffset - uds.staticFetchOffset
		} else {
			uds.staticFetchLength = params.file.SegmentSize() - uds.staticFetchOffset
		}

		// set the writeOffset within the destination for where the data be written.
		uds.staticWriteOffset = writeOffset
		writeOffset += int64(uds.staticFetchLength)

		uds.staticOverdrive = uint32(params.overdrive)

		// add this segment to the segment heap, and notify the download loop a new task
		client.addSegmentToDownloadHeap(uds)
		select {
		case client.newDownloads <- struct{}{}:
		default:
		}
	}
	return d, nil
}

// managedDownload performs a file download and returns the download object
func (client *StorageClient) managedDownload(p storage.ClientDownloadParameters) (*download, error) {
	entry, err := client.staticFileSet.Open(p.DxFilePath)
	if err != nil {
		return nil, err
	}

	defer entry.Close()
	defer entry.SetTimeAccess(time.Now())

	// validate download parameters.
	isHTTPResp := p.Httpwriter != nil
	if p.Async && isHTTPResp {
		return nil, errors.New("cannot async download to http response")
	}
	if isHTTPResp && p.Destination != "" {
		return nil, errors.New("destination cannot be specified when downloading to http response")
	}
	if !isHTTPResp && p.Destination == "" {
		return nil, errors.New("destination not supplied")
	}
	if p.Destination != "" && !filepath.IsAbs(p.Destination) {
		return nil, errors.New("destination must be an absolute path")
	}

	if p.Offset == entry.FileSize() && entry.FileSize() != 0 {
		return nil, errors.New("offset equals filesize")
	}

	// if length == 0, download the rest file.
	if p.Length == 0 {
		if p.Offset > entry.FileSize() {
			return nil, errors.New("offset cannot be greater than file size")
		}
		p.Length = entry.FileSize() - p.Offset
	}

	// check whether offset and length is valid
	if p.Offset < 0 || p.Offset+p.Length > entry.FileSize() {
		return nil, fmt.Errorf("offset and length combination invalid, max byte is at index %d", entry.FileSize()-1)
	}

	// instantiate the correct downloadWriter implementation
	var dw downloadDestination
	var destinationType string
	if isHTTPResp {
		dw = newDownloadDestinationWriter(p.Httpwriter)
		destinationType = "http stream"
	} else {
		osFile, err := os.OpenFile(p.Destination, os.O_CREATE|os.O_WRONLY, entry.FileMode())
		if err != nil {
			return nil, err
		}
		dw = osFile
		destinationType = "file"
	}

	if isHTTPResp {
		w, ok := p.Httpwriter.(http.ResponseWriter)
		if ok {
			w.Header().Set("Content-Length", fmt.Sprint(p.Length))
		}
	}

	// create the download object.
	d, err := client.newDownload(downloadParams{
		destination:       dw,
		destinationType:   destinationType,
		destinationString: p.Destination,
		file:              entry.DxFile.Snapshot(),
		latencyTarget:     25e3 * time.Millisecond,
		length:            p.Length,
		needsMemory:       true,
		offset:            p.Offset,
		overdrive:         3,
		priority:          5,
	})
	if closer, ok := dw.(io.Closer); err != nil && ok {
		closeErr := closer.Close()
		if closeErr != nil {
			return nil, errors.New(fmt.Sprintf("get something wrong when create download object: %v, destination close error: %v", err, closeErr))
		}
		return nil, errors.New(fmt.Sprintf("get something wrong when create download object: %v, destination close successfully", err))
	} else if err != nil {
		return nil, err
	}

	// register some cleanup for when the download is done.
	d.OnComplete(func(_ error) error {
		if closer, ok := dw.(io.Closer); ok {
			return closer.Close()
		}
		return nil
	})

	// TODO: 是否需要保存下载历史
	// Add the download object to the download history if it's not a stream.
	//if destinationType != destinationTypeSeekStream {
	//	r.downloadHistoryMu.Lock()
	//	r.downloadHistory = append(r.downloadHistory, d)
	//	r.downloadHistoryMu.Unlock()
	//}

	return d, nil
}

// NOTE: DownloadSync and DownloadAsync can directly be accessed to outer request via RPC or IPC ...

// performs a file download and blocks until the download is finished.
func (client *StorageClient) DownloadSync(p storage.ClientDownloadParameters) error {
	if err := client.tm.Add(); err != nil {
		return err
	}
	defer client.tm.Done()

	d, err := client.managedDownload(p)
	if err != nil {
		return err
	}

	// block until the download has completed
	select {
	case <-d.completeChan:
		return d.Err()
	case <-client.tm.StopChan():
		return errors.New("download interrupted by shutdown")
	}
}

// performs a file download without blocking until the download is finished
func (client *StorageClient) DownloadAsync(p storage.ClientDownloadParameters) error {
	if err := client.tm.Add(); err != nil {
		return err
	}
	defer client.tm.Done()

	_, err := client.managedDownload(p)
	return err
}
