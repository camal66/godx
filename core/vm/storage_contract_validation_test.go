package vm

import (
	"math/big"
	"net"
	"testing"

	"github.com/DxChainNetwork/godx/common"
	"github.com/DxChainNetwork/godx/core/types"
	"github.com/DxChainNetwork/godx/crypto"
	"github.com/DxChainNetwork/godx/p2p/enode"
	"github.com/magiconair/properties/assert"
	"golang.org/x/crypto/sha3"
)

func TestCheckMultiSignatures(t *testing.T) {
	prvKeyHost, err := crypto.GenerateKey()
	if err != nil {
		t.Errorf("failed to generate public/private key pairs for storage host: %v", err)
	}
	hostNode := enode.NewV4(&prvKeyHost.PublicKey, net.IP{127, 0, 0, 1}, int(8888), int(8888))

	prvKeyClient, err := crypto.GenerateKey()
	if err != nil {
		t.Errorf("failed to generate public/private key pairs for storage client: %v", err)
	}

	// test host announce signature(only one signature)
	ha := types.HostAnnouncement{
		NetAddress: hostNode.String(),
	}

	sigHa, err := crypto.Sign(ha.RLPHash().Bytes(), prvKeyHost)
	if err != nil {
		t.Errorf("failed to sign host announce: %v", err)
	}

	err = CheckMultiSignatures(ha, [][]byte{sigHa})
	if err != nil {
		t.Errorf("failed to check host announce signature: %v", err)
	}

	uc := types.UnlockConditions{
		PaymentAddresses:   []common.Address{crypto.PubkeyToAddress(prvKeyClient.PublicKey), crypto.PubkeyToAddress(prvKeyHost.PublicKey)},
		SignaturesRequired: 2,
	}

	// test storage contract signature(two signatures)
	sc := types.StorageContract{
		FileSize:       2048,
		FileMerkleRoot: common.HexToHash("0x51da85b8a745b0e2cf3bcd4cae108ad42f0dac49124419736e1bac49c2d44cd7"),
		WindowStart:    uint64(234),
		WindowEnd:      uint64(345),
		ClientCollateral: types.DxcoinCollateral{
			DxcoinCharge: types.DxcoinCharge{
				Address: common.HexToAddress("0xcf1FA0d741F155Bd2cF69A5a791C81BB8222118D"),
				Value:   new(big.Int).SetInt64(10000),
			},
		},
		HostCollateral: types.DxcoinCollateral{
			DxcoinCharge: types.DxcoinCharge{
				Address: common.HexToAddress("0xcf1FA0d741F155Bd2cF69A5a791C81BB8222118D"),
				Value:   new(big.Int).SetInt64(10000),
			},
		},
		ValidProofOutputs: []types.DxcoinCharge{
			{Address: common.HexToAddress("0xcf1FA0d741F155Bd2cF69A5a791C81BB8222118D"),
				Value: new(big.Int).SetInt64(10000)},
		},
		MissedProofOutputs: []types.DxcoinCharge{
			{Address: common.HexToAddress("0xcf1FA0d741F155Bd2cF69A5a791C81BB8222118D"),
				Value: new(big.Int).SetInt64(10000)},
		},
		UnlockHash:     uc.UnlockHash(),
		RevisionNumber: 111,
	}

	sigsScByHost, err := crypto.Sign(sc.RLPHash().Bytes(), prvKeyHost)
	if err != nil {
		t.Errorf("host failed to sign host announce: %v", err)
	}

	sigsScByClient, err := crypto.Sign(sc.RLPHash().Bytes(), prvKeyClient)
	if err != nil {
		t.Errorf("client failed to sign host announce: %v", err)
	}

	sc.Signatures = make([][]byte, 2)
	sc.Signatures[0] = sigsScByClient
	sc.Signatures[1] = sigsScByHost

	err = CheckMultiSignatures(sc, sc.Signatures)
	if err != nil {
		t.Errorf("failed to check storage contract signature: %v", err)
	}
}

var (
	leaveContent = []string{"jack", "lucy", "green", "apple"}
)

func mockMerkleProof(leaveContent []string) (common.Hash, []common.Hash) {
	hasher := sha3.NewLegacyKeccak256()

	// hash of leaves
	leaveHashes := make([][]byte, len(leaveContent))
	for i, lcontent := range leaveContent {
		leaveHashes[i] = HashSum(hasher, []byte(lcontent))
	}

	// caculate merkle root
	p1 := HashSum(hasher, leaveHashes[0], leaveHashes[1])
	p2 := HashSum(hasher, leaveHashes[2], leaveHashes[3])
	p0 := HashSum(hasher, p1, p2)
	hashSet := []common.Hash{common.BytesToHash(leaveHashes[1]), common.BytesToHash(p2)}
	return common.BytesToHash(p0), hashSet
}

func TestVerifySegment(t *testing.T) {
	root, hashSet := mockMerkleProof(leaveContent)

	// check "jack" merkle proof
	//assert.Equal(t, VerifySegment([]byte("jack"), hashSet, 4, 0, root), true, "incorrect verification merkle proof")
	assert.Equal(t, VerifySegment([]byte("lucy"), hashSet, 4, 0, root), false, "incorrect verification merkle proof")
}
