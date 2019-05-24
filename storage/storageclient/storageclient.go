// Copyright 2019 DxChain, All rights reserved.
// Use of this source code is governed by an Apache
// License 2.0 that can be found in the LICENSE file.

package storageclient

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"errors"
	"io"
	"math/big"
	"math/bits"
	"path/filepath"
	"sync"

	"github.com/DxChainNetwork/godx/accounts"
	"github.com/DxChainNetwork/godx/common"
	"github.com/DxChainNetwork/godx/common/hexutil"
	"github.com/DxChainNetwork/godx/common/threadmanager"
	"github.com/DxChainNetwork/godx/core/types"
	"github.com/DxChainNetwork/godx/crypto"
	"github.com/DxChainNetwork/godx/log"
	"github.com/DxChainNetwork/godx/p2p"
	"github.com/DxChainNetwork/godx/params"
	"github.com/DxChainNetwork/godx/rlp"
	"github.com/DxChainNetwork/godx/rpc"
	"github.com/DxChainNetwork/godx/storage"
	"github.com/DxChainNetwork/godx/storage/storagehost"

	"github.com/DxChainNetwork/godx/storage/storageclient/memorymanager"
	"github.com/DxChainNetwork/godx/storage/storageclient/storagehostmanager"
)

var (
	zeroValue = new(big.Int).SetInt64(0)
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
	b          Backend

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

	account := accounts.Account{Address: clientAddr}
	wallet, err := sc.ethBackend.AccountManager().Find(account)
	if err != nil {
		return storagehost.ExtendErr("find client account error", err)
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
	if err != nil {
		return storagehost.ExtendErr("setup connection with host failed", err)
	}
	defer sc.ethBackend.Disconnect(host.NetAddress)

	clientContractSign, err := wallet.SignHash(account, storageContract.RLPHash().Bytes())
	if err != nil {
		return storagehost.ExtendErr("contract sign by client failed", err)
	}

	// Send the ContractCreate request
	req := storage.ContractCreateRequest{
		StorageContract: storageContract,
		Sign:            clientContractSign,
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

	storageContract.Signatures[0] = clientContractSign

	clientRevisionSign, err := wallet.SignHash(account, storageContractRevision.RLPHash().Bytes())
	if err != nil {
		return storagehost.ExtendErr("client sign revision error", err)
	}
	storageContractRevision.Signatures = [][]byte{clientRevisionSign}

	if err := session.SendStorageContractCreationClientRevisionSign(clientRevisionSign); err != nil {
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

	// TODO: 获取最新的storage contractlast revision
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
	price = price.MultFloat64(1 + 0.02)

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

	// TODO: Increase Successful/Failed interactions accordingly
	//defer func() {
	//	if err != nil {
	//		s.hdb.IncrementFailedInteractions(contract.HostPublicKey())
	//	} else {
	//		s.hdb.IncrementSuccessfulInteractions(contract.HostPublicKey())
	//	}
	//}()

	// Disrupt before sending the signed revision to the host.
	//if s.deps.Disrupt("InterruptDownloadBeforeSendingRevision") {
	//	return errors.New("InterruptDownloadBeforeSendingRevision disrupt")
	//}

	// send download request
	//extendDeadline(s.conn, modules.NegotiateDownloadTime)
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

	// TODO: 逐个读取host发送过来的data数据
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

		// TODO: 如果下载数据都收到了，但是host那边的签名还没有收到，那么还需要等待读取以下host发送的签名
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

	// Disrupt before commiting.
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
func (client *StorageClient) Downlaod(s *storage.Session, root common.Hash, offset, length uint32) ([]byte, error) {
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
