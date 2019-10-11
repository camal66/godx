// Copyright 2019 DxChain, All rights reserved.
// Use of this source code is governed by an Apache
// License 2.0 that can be found in the LICENSE file

package hostnegotiation

import (
	"fmt"
	"sort"

	"github.com/DxChainNetwork/godx/accounts"

	"github.com/DxChainNetwork/godx/core/types"

	"github.com/DxChainNetwork/godx/common"
	"github.com/DxChainNetwork/godx/crypto/merkle"
	"github.com/DxChainNetwork/godx/p2p"
	"github.com/DxChainNetwork/godx/storage"
	"github.com/DxChainNetwork/godx/storage/storagehost"
)

func ContractUploadHandler(np Protocol, sp storage.Peer, uploadReqMsg p2p.Msg) {
	var negotiateErr error
	var nd uploadNegotiationData
	defer handleNegotiationErr(&negotiateErr, sp, np)

	// decode the upload request and get the storage responsibility
	uploadReq, sr, err := decodeUploadReqAndGetSr(np, &nd, uploadReqMsg)
	if err != nil {
		negotiateErr = err
		return
	}

	// get the storage host configuration and start to parse and handle the upload actions
	hostConfig := np.GetHostConfig()
	if err := parseAndHandleUploadActions(uploadReq, &nd, sr, hostConfig.UploadBandwidthPrice); err != nil {
		negotiateErr = err
		return
	}

	// construct and verify new contract revision
	newRevision, err := constructAndVerifyNewRevision(np, &nd, sr, uploadReq, hostConfig)
	if err != nil {
		negotiateErr = err
		return
	}

	// merkleProof Negotiation
	clientRevisionSign, err := merkleProofNegotiation(sp, &nd, sr)
	if err != nil {
		negotiateErr = err
		return
	}

	// host sign and update revision
	if err := hostSignAndUpdateRevision(np, &newRevision, clientRevisionSign); err != nil {
		negotiateErr = err
		return
	}

	// host revision sign negotiation
	if err := hostRevisionSignNegotiation(sp, np, hostConfig, &nd, sr, newRevision); err != nil {
		negotiateErr = err
		return
	}
}

// decodeUploadReqAndGetSr will decode the upload request and get the storage responsibility
// based on the storage id. In the end, the storage responsibility will be snapshot and stored
// in the upload negotiation data
func decodeUploadReqAndGetSr(np Protocol, nd *uploadNegotiationData, uploadReqMsg p2p.Msg) (storage.UploadRequest, storagehost.StorageResponsibility, error) {
	var uploadReq storage.UploadRequest
	// decode upload request
	if err := uploadReqMsg.Decode(&uploadReq); err != nil {
		err = fmt.Errorf("failed to decode the upload request: %s", err.Error())
		return storage.UploadRequest{}, storagehost.StorageResponsibility{}, err
	}

	// get the storage responsibility
	sr, err := np.GetStorageResponsibility(uploadReq.StorageContractID)
	if err != nil {
		err = fmt.Errorf("failed to retrieve the storage responsibility: %s", err.Error())
		return storage.UploadRequest{}, storagehost.StorageResponsibility{}, err
	}

	// snapshot the storage responsibility, and return
	nd.srSnapshot = sr
	return uploadReq, sr, nil
}

// parseAndHandleUploadActions will parse the upload actions based on the type of the action
// currently, there is only one type which is append. During the action handling process, the
// following data will be calculated and recorded in the uploadNegotiationData
// 	1. newRoots
//  2. sectorGained
//  3. gainedSectorData
//  4. sectorsChanged
//  5. bandwidthRevenue
func parseAndHandleUploadActions(uploadReq storage.UploadRequest, nd *uploadNegotiationData, sr storagehost.StorageResponsibility, uploadBandwidthPrice common.BigInt) error {
	// data preparation
	nd.newRoots = append(nd.newRoots, sr.SectorRoots...)
	nd.sectorsChanged = make(map[uint64]struct{})

	// loop and handle each action
	for _, action := range uploadReq.Actions {
		switch action.Type {
		case storage.UploadActionAppend:
			handleUploadAppendType(action, nd, uploadBandwidthPrice)
		default:
			return fmt.Errorf("failed to parse the upload action, unknown upload action type: %s", action.Type)
		}
	}

	return nil
}

// constructAndVerifyNewRevision will construct a new storage contract revision
// and verify the new revision
func constructAndVerifyNewRevision(np Protocol, nd *uploadNegotiationData, sr storagehost.StorageResponsibility, uploadReq storage.UploadRequest, hostConfig storage.HostIntConfig) (types.StorageContractRevision, error) {
	// get the latest revision and update the revision
	currentRev := sr.StorageContractRevisions[len(sr.StorageContractRevisions)-1]
	newRev := currentRev
	newRev.NewRevisionNumber = uploadReq.NewRevisionNumber

	// update contract revision
	updateRevisionFileSize(&newRev, uploadReq)
	calcAndUpdateRevisionMerkleRoot(nd, &newRev)
	updateRevisionMissedAndValidPayback(nd, &newRev, currentRev, uploadReq)

	// contract revision validation
	blockHeight := np.GetBlockHeight()
	hostRevenue := calcHostRevenue(nd, sr, blockHeight, hostConfig)
	sr.SectorRoots, nd.newRoots = nd.newRoots, sr.SectorRoots
	if err := uploadRevisionValidation(sr, newRev, blockHeight, hostRevenue); err != nil {
		return types.StorageContractRevision{}, err
	}
	sr.SectorRoots, nd.newRoots = nd.newRoots, sr.SectorRoots

	// after validation, return the new revision
	return newRev, nil
}

func constructUploadMerkleProof(nd *uploadNegotiationData, sr storagehost.StorageResponsibility) (storage.UploadMerkleProof, error) {
	proofRanges := calcAndSortProofRanges(sr, *nd)
	leafHashes := calcLeafHashes(proofRanges, sr)
	oldHashSet, err := calcOldHashSet(sr, proofRanges)
	if err != nil {
		err = fmt.Errorf("failed to construct upload merkle proof: %s", err.Error())
		return storage.UploadMerkleProof{}, err
	}

	// construct the merkle proof
	merkleProof := storage.UploadMerkleProof{
		OldSubtreeHashes: oldHashSet,
		OldLeafHashes:    leafHashes,
		NewMerkleRoot:    nd.newMerkleRoot,
	}

	// update uploadNegotiationData for merkle proof
	nd.merkleProof = merkleProof
	return merkleProof, nil
}

func merkleProofNegotiation(sp storage.Peer, nd *uploadNegotiationData, sr storagehost.StorageResponsibility) ([]byte, error) {
	// construct merkle proof
	merkleProof, err := constructUploadMerkleProof(nd, sr)
	if err != nil {
		return []byte{}, err
	}

	// send the merkleProof to storage host
	if err := sp.SendUploadMerkleProof(merkleProof); err != nil {
		err := fmt.Errorf("host failed to send upload merkleProof: %s", err.Error())
		return []byte{}, err
	}

	// wait for client revision sign
	return waitAndHandleClientRevSignResp(sp)
}

func hostSignAndUpdateRevision(np Protocol, newRev *types.StorageContractRevision, clientRevisionSign []byte) error {
	// get the wallet
	account := accounts.Account{Address: newRev.NewValidProofOutputs[validProofPaybackHostAddressIndex].Address}
	wallet, err := np.FindWallet(account)
	if err != nil {
		return fmt.Errorf("hostSignAndUpdateRevision failed, cannot find the wallet: %s", err.Error())
	}

	// sign the revision
	hostRevisionSign, err := wallet.SignHash(account, newRev.RLPHash().Bytes())
	if err != nil {
		return fmt.Errorf("hostSignAndUpdateRevision failed, failed to sign the contract reivision: %s", err.Error())
	}

	// update the revision
	newRev.Signatures = [][]byte{clientRevisionSign, hostRevisionSign}
	return nil
}

func hostRevisionSignNegotiation(sp storage.Peer, np Protocol, hostConfig storage.HostIntConfig, nd *uploadNegotiationData, sr storagehost.StorageResponsibility, newRev types.StorageContractRevision) error {
	// get the host revision sign from the new revision, and send the upload host revision sign
	hostRevSign := newRev.Signatures[hostSignIndex]
	if err := sp.SendUploadHostRevisionSign(hostRevSign); err != nil {
		return fmt.Errorf("hostRevisionSignNeogtiation failed, failed to send the host rev sign: %s", err.Error())
	}

	// storage host wait and handle the client's response
	return waitAndHandleClientCommitRespUpload(sp, np, nd, sr, hostConfig, newRev)
}

func waitAndHandleClientCommitRespUpload(sp storage.Peer, np Protocol, nd *uploadNegotiationData, sr storagehost.StorageResponsibility, hostConfig storage.HostIntConfig, newRev types.StorageContractRevision) error {
	// wait for storage host's response
	msg, err := sp.HostWaitContractResp()
	if err != nil {
		return fmt.Errorf("waitAndHandleClientCommitRespUpload failed, host falied to wait for the client's response: %s", err.Error())
	}

	// based on the message code, handle the client's upload commit response
	if err := handleClientUploadCommitResp(msg, sp, np, nd, sr, hostConfig, newRev); err != nil {
		return err
	}

	return nil
}

// handleClientUploadCommitResp will handle client's response based on the message code
func handleClientUploadCommitResp(msg p2p.Msg, sp storage.Peer, np Protocol, nd *uploadNegotiationData, sr storagehost.StorageResponsibility, hostConfig storage.HostIntConfig, newRev types.StorageContractRevision) error {
	switch msg.Code {
	case storage.ClientCommitSuccessMsg:
		return handleClientUploadSuccessCommit(sp, np, nd, sr, hostConfig, newRev)
	case storage.ClientCommitFailedMsg:
		return storage.ErrClientCommit
	case storage.ClientNegotiateErrorMsg:
		return storage.ErrClientNegotiate
	default:
		return fmt.Errorf("failed to reconize the message code")
	}
}

func handleClientUploadSuccessCommit(sp storage.Peer, np Protocol, nd *uploadNegotiationData, sr storagehost.StorageResponsibility, hostConfig storage.HostIntConfig, newRev types.StorageContractRevision) error {
	// update and modify the storage responsibility
	sr = updateStorageResponsibilityUpload(nd, sr, hostConfig, newRev)
	if err := np.ModifyStorageResponsibility(sr, nil, nd.sectorGained, nd.gainedSectorData); err != nil {
		return storage.ErrHostCommit
	}

	// if the storage host successfully commit the storage responsibility, set the connection to be static
	np.CheckAndSetStaticConnection(sp)

	// at the end, send the storage host ack message
	if err := sp.SendHostAckMsg(); err != nil {
		_ = np.RollbackUploadStorageResponsibility(nd.srSnapshot, nd.sectorGained, nil, nil)
		return fmt.Errorf("failed to send the host ack message at the end during the upload, negotiation failed: %s", err.Error())
	}

	return nil
}

func updateStorageResponsibilityUpload(nd *uploadNegotiationData, sr storagehost.StorageResponsibility, hostConfig storage.HostIntConfig, newRev types.StorageContractRevision) storagehost.StorageResponsibility {
	// calculate the bandwidthRevenue after added merkle proof
	bandwidthRevenue := calcBandwidthRevenueForProof(nd, len(nd.merkleProof.OldSubtreeHashes), len(nd.merkleProof.OldLeafHashes), hostConfig.DownloadBandwidthPrice)

	// update the storage responsibility
	sr.SectorRoots = nd.newRoots
	sr.PotentialStorageRevenue = sr.PotentialStorageRevenue.Add(nd.storageRevenue)
	sr.RiskedStorageDeposit = sr.RiskedStorageDeposit.Add(nd.newDeposit)
	sr.PotentialUploadRevenue = sr.PotentialUploadRevenue.Add(bandwidthRevenue)
	sr.StorageContractRevisions = append(sr.StorageContractRevisions, newRev)

	// return the updated storage responsibility
	return sr
}

func calcAndSortProofRanges(sr storagehost.StorageResponsibility, nd uploadNegotiationData) []merkle.SubTreeLimit {
	// calculate proof ranges
	oldNumSectors := uint64(len(sr.SectorRoots))
	var proofRanges []merkle.SubTreeLimit
	for i := range nd.sectorsChanged {
		if i < oldNumSectors {
			proofRange := merkle.SubTreeLimit{
				Left:  i,
				Right: i + 1,
			}

			proofRanges = append(proofRanges, proofRange)
		}
	}

	// sort proof ranges
	sort.Slice(proofRanges, func(i, j int) bool {
		return proofRanges[i].Left < proofRanges[j].Left
	})

	return proofRanges
}

func calcLeafHashes(proofRanges []merkle.SubTreeLimit, sr storagehost.StorageResponsibility) []common.Hash {
	var leafHashes []common.Hash
	for _, proofRange := range proofRanges {
		leafHashes = append(leafHashes, sr.SectorRoots[proofRange.Left])
	}

	return leafHashes
}

func calcOldHashSet(sr storagehost.StorageResponsibility, proofRanges []merkle.SubTreeLimit) ([]common.Hash, error) {
	return merkle.Sha256DiffProof(sr.SectorRoots, proofRanges, uint64(len(sr.SectorRoots)))
}

// updateRevisionFileSize will update the new contract revision's file size based on the
// type of the upload action
// 	 1. UploadActionAppend -> based on the number of append action, increase the file size by sectorSize
func updateRevisionFileSize(newRev *types.StorageContractRevision, uploadReq storage.UploadRequest) {
	for _, action := range uploadReq.Actions {
		switch action.Type {
		case storage.UploadActionAppend:
			newRev.NewFileSize += storage.SectorSize
		}
	}
}

// calcAndUpdateRevisionMerkleRoot will calculate the new file merkle root for storage contract revision
// and update both new revision and uploadNegotiationData
func calcAndUpdateRevisionMerkleRoot(nd *uploadNegotiationData, newRev *types.StorageContractRevision) {
	// calculate the new merkle roots and update the new revision
	nd.newMerkleRoot = merkle.Sha256CachedTreeRoot2(nd.newRoots)
	newRev.NewFileMerkleRoot = nd.newMerkleRoot
}

// updateRevisionMissedAndValidPayback will update the new contract revision missed and valid
// proof payback
func updateRevisionMissedAndValidPayback(nd *uploadNegotiationData, newRev *types.StorageContractRevision, currentRev types.StorageContractRevision, uploadReq storage.UploadRequest) {
	// update the revision valid proof outputs
	for i := range currentRev.NewValidProofOutputs {
		validProofOutput := types.DxcoinCharge{
			Value:   uploadReq.NewValidProofValues[i],
			Address: currentRev.NewValidProofOutputs[i].Address,
		}
		newRev.NewValidProofOutputs = append(newRev.NewValidProofOutputs, validProofOutput)
	}

	// update the revision missed proof outputs
	for i := range currentRev.NewValidProofOutputs {
		missedProofOutput := types.DxcoinCharge{
			Value:   uploadReq.NewMissedProofValues[i],
			Address: currentRev.NewMissedProofOutputs[i].Address,
		}
		newRev.NewMissedProofOutputs = append(newRev.NewMissedProofOutputs, missedProofOutput)
	}
}

// handleUploadAppendType will handle the upload action with the type UploadAppendAction
// by handing it, a bunch of data will be calculated and recorded in the uploadNegotiationData
func handleUploadAppendType(action storage.UploadAction, nd *uploadNegotiationData, uploadBandwidthPrice common.BigInt) {
	// update upload negotiation data
	newRoot := merkle.Sha256MerkleTreeRoot(action.Data)
	nd.newRoots = append(nd.newRoots, newRoot)
	nd.sectorGained = append(nd.sectorGained, newRoot)
	nd.gainedSectorData = append(nd.gainedSectorData, action.Data)
	nd.sectorsChanged[uint64(len(nd.newRoots)-1)] = struct{}{}
	nd.bandwidthRevenue = nd.bandwidthRevenue.Add(uploadBandwidthPrice.MultUint64(storage.SectorSize))
}
