// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package vm

import (
	"encoding/binary"
	"errors"
	"math/big"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/DxChainNetwork/godx/common"
	"github.com/DxChainNetwork/godx/consensus/dpos"
	"github.com/DxChainNetwork/godx/core/types"
	"github.com/DxChainNetwork/godx/crypto"
	"github.com/DxChainNetwork/godx/log"
	"github.com/DxChainNetwork/godx/params"
	"github.com/DxChainNetwork/godx/rlp"
	"github.com/DxChainNetwork/godx/storage/coinchargemaintenance"
)

// emptyCodeHash is used by create to ensure deployment is disallowed to already
// deployed contract addresses (relevant after the account abstraction).
var (
	emptyCodeHash = crypto.Keccak256Hash(nil)

	errUnknownStorageContractTx = errors.New("unknown storage contract tx")
	errUnknownDposOperationTx   = errors.New("unknown dpos operation tx")
)

type (
	// CanTransferFunc is the signature of a transfer guard function
	CanTransferFunc func(StateDB, common.Address, *big.Int) bool
	// TransferFunc is the signature of a transfer function
	TransferFunc func(StateDB, common.Address, common.Address, *big.Int)
	// GetHashFunc returns the nth block hash in the blockchain
	// and is used by the BLOCKHASH EVM op code.
	GetHashFunc func(uint64) common.Hash
)

// run runs the given contract and takes care of running precompiles with a fallback to the byte code interpreter.
func run(evm *EVM, contract *Contract, input []byte, readOnly bool) ([]byte, error) {
	if contract.CodeAddr != nil {
		precompiles := PrecompiledContractsHomestead
		if evm.ChainConfig().IsByzantium(evm.BlockNumber) {
			precompiles = PrecompiledContractsByzantium
		}
		if p := precompiles[*contract.CodeAddr]; p != nil {
			return RunPrecompiledContract(p, input, contract)
		}
	}
	for _, interpreter := range evm.interpreters {
		if interpreter.CanRun(contract.Code) {
			if evm.interpreter != interpreter {
				// Ensure that the interpreter pointer is set back
				// to its current value upon return.
				defer func(i Interpreter) {
					evm.interpreter = i
				}(evm.interpreter)
				evm.interpreter = interpreter
			}
			return interpreter.Run(contract, input, readOnly)
		}
	}
	return nil, ErrNoCompatibleInterpreter
}

// Context provides the EVM with auxiliary information. Once provided
// it shouldn't be modified.
type Context struct {
	// CanTransfer returns whether the account contains
	// sufficient ether to transfer the value
	CanTransfer CanTransferFunc
	// Transfer transfers ether from one account to the other
	Transfer TransferFunc
	// GetHash returns the hash corresponding to n
	GetHash GetHashFunc

	// Message information
	Origin   common.Address // Provides information for ORIGIN
	GasPrice *big.Int       // Provides information for GASPRICE

	// Block information
	Coinbase    common.Address // Provides information for COINBASE
	GasLimit    uint64         // Provides information for GASLIMIT
	BlockNumber *big.Int       // Provides information for NUMBER
	Time        *big.Int       // Provides information for TIME
	Difficulty  *big.Int       // Provides information for DIFFICULTY
}

// EVM is the Ethereum Virtual Machine base object and provides
// the necessary tools to run a contract on the given state with
// the provided context. It should be noted that any error
// generated through any of the calls should be considered a
// revert-state-and-consume-all-gas operation, no checks on
// specific errors should ever be performed. The interpreter makes
// sure that any errors generated are to be considered faulty code.
//
// The EVM should never be reused and is not thread safe.
type EVM struct {
	// Context provides auxiliary blockchain related information
	Context
	// StateDB gives access to the underlying state
	StateDB StateDB
	// Depth is the current call stack
	depth int

	// chainConfig contains information about the current chain
	chainConfig *params.ChainConfig
	// chain rules contains the chain rules for the current epoch
	chainRules params.Rules
	// virtual machine configuration options used to initialise the
	// evm.
	vmConfig Config
	// global (to this context) ethereum virtual machine
	// used throughout the execution of the tx.
	interpreters []Interpreter
	interpreter  Interpreter
	// abort is used to abort the EVM calling operations
	// NOTE: must be set atomically
	abort int32
	// callGasTemp holds the gas available for the current call. This is needed because the
	// available gas is calculated in gasCall* according to the 63/64 rule and later
	// applied in opCall*.
	callGasTemp uint64
}

// NewEVM returns a new EVM. The returned EVM is not thread safe and should
// only ever be used *once*.
func NewEVM(ctx Context, statedb StateDB, chainConfig *params.ChainConfig, vmConfig Config) *EVM {
	evm := &EVM{
		Context:      ctx,
		StateDB:      statedb,
		vmConfig:     vmConfig,
		chainConfig:  chainConfig,
		chainRules:   chainConfig.Rules(ctx.BlockNumber),
		interpreters: make([]Interpreter, 0, 1),
	}

	if chainConfig.IsEWASM(ctx.BlockNumber) {
		// to be implemented by EVM-C and Wagon PRs.
		// if vmConfig.EWASMInterpreter != "" {
		//  extIntOpts := strings.Split(vmConfig.EWASMInterpreter, ":")
		//  path := extIntOpts[0]
		//  options := []string{}
		//  if len(extIntOpts) > 1 {
		//    options = extIntOpts[1..]
		//  }
		//  evm.interpreters = append(evm.interpreters, NewEVMVCInterpreter(evm, vmConfig, options))
		// } else {
		// 	evm.interpreters = append(evm.interpreters, NewEWASMInterpreter(evm, vmConfig))
		// }
		panic("No supported ewasm interpreter yet.")
	}

	// vmConfig.EVMInterpreter will be used by EVM-C, it won't be checked here
	// as we always want to have the built-in EVM as the failover option.
	evm.interpreters = append(evm.interpreters, NewEVMInterpreter(evm, vmConfig))
	evm.interpreter = evm.interpreters[0]

	return evm
}

// Cancel cancels any running EVM operation. This may be called concurrently and
// it's safe to be called multiple times.
func (evm *EVM) Cancel() {
	atomic.StoreInt32(&evm.abort, 1)
}

// Interpreter returns the current interpreter
func (evm *EVM) Interpreter() Interpreter {
	return evm.interpreter
}

// Call executes the contract associated with the addr with the given input as
// parameters. It also handles any necessary value transfer required and takes
// the necessary steps to create accounts and reverses the state in case of an
// execution error or failed value transfer.
func (evm *EVM) Call(caller ContractRef, addr common.Address, input []byte, gas uint64, value *big.Int) (ret []byte, leftOverGas uint64, err error) {
	if evm.vmConfig.NoRecursion && evm.depth > 0 {
		return nil, gas, nil
	}

	// Fail if we're trying to execute above the call depth limit
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, ErrDepth
	}
	// Fail if we're trying to transfer more than the available balance
	if !evm.Context.CanTransfer(evm.StateDB, caller.Address(), value) {
		return nil, gas, ErrInsufficientBalance
	}
	var (
		to       = AccountRef(addr)
		snapshot = evm.StateDB.Snapshot()
	)
	if !evm.StateDB.Exist(addr) {
		precompiles := PrecompiledContractsHomestead
		if evm.ChainConfig().IsByzantium(evm.BlockNumber) {
			precompiles = PrecompiledContractsByzantium
		}
		if precompiles[addr] == nil && evm.ChainConfig().IsEIP158(evm.BlockNumber) && value.Sign() == 0 {
			// Calling a non existing account, don't do anything, but ping the tracer
			if evm.vmConfig.Debug && evm.depth == 0 {
				evm.vmConfig.Tracer.CaptureStart(caller.Address(), addr, false, input, gas, value)
				evm.vmConfig.Tracer.CaptureEnd(ret, 0, 0, nil)
			}
			return nil, gas, nil
		}
		evm.StateDB.CreateAccount(addr)
	}
	evm.Transfer(evm.StateDB, caller.Address(), to.Address(), value)
	// Initialise a new contract and set the code that is to be used by the EVM.
	// The contract is a scoped environment for this execution context only.
	contract := NewContract(caller, to, value, gas)
	contract.SetCallCode(&addr, evm.StateDB.GetCodeHash(addr), evm.StateDB.GetCode(addr))

	// Even if the account has no code, we need to continue because it might be a precompile
	start := time.Now()

	// Capture the tracer start/end events in debug mode
	if evm.vmConfig.Debug && evm.depth == 0 {
		evm.vmConfig.Tracer.CaptureStart(caller.Address(), addr, false, input, gas, value)

		defer func() { // Lazy evaluation of the parameters
			evm.vmConfig.Tracer.CaptureEnd(ret, gas-contract.Gas, time.Since(start), err)
		}()
	}
	ret, err = run(evm, contract, input, false)

	// When an error was returned by the EVM or when setting the creation code
	// above we revert to the snapshot and consume any gas remaining. Additionally
	// when we're in homestead this also counts for code storage gas errors.
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != errExecutionReverted {
			contract.UseGas(contract.Gas)
		}
	}
	return ret, contract.Gas, err
}

// CallCode executes the contract associated with the addr with the given input
// as parameters. It also handles any necessary value transfer required and takes
// the necessary steps to create accounts and reverses the state in case of an
// execution error or failed value transfer.
//
// CallCode differs from Call in the sense that it executes the given address'
// code with the caller as context.
func (evm *EVM) CallCode(caller ContractRef, addr common.Address, input []byte, gas uint64, value *big.Int) (ret []byte, leftOverGas uint64, err error) {
	if evm.vmConfig.NoRecursion && evm.depth > 0 {
		return nil, gas, nil
	}

	// Fail if we're trying to execute above the call depth limit
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, ErrDepth
	}
	// Fail if we're trying to transfer more than the available balance
	if !evm.CanTransfer(evm.StateDB, caller.Address(), value) {
		return nil, gas, ErrInsufficientBalance
	}

	var (
		snapshot = evm.StateDB.Snapshot()
		to       = AccountRef(caller.Address())
	)
	// initialise a new contract and set the code that is to be used by the
	// EVM. The contract is a scoped environment for this execution context
	// only.
	contract := NewContract(caller, to, value, gas)
	contract.SetCallCode(&addr, evm.StateDB.GetCodeHash(addr), evm.StateDB.GetCode(addr))

	ret, err = run(evm, contract, input, false)
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != errExecutionReverted {
			contract.UseGas(contract.Gas)
		}
	}
	return ret, contract.Gas, err
}

// DelegateCall executes the contract associated with the addr with the given input
// as parameters. It reverses the state in case of an execution error.
//
// DelegateCall differs from CallCode in the sense that it executes the given address'
// code with the caller as context and the caller is set to the caller of the caller.
func (evm *EVM) DelegateCall(caller ContractRef, addr common.Address, input []byte, gas uint64) (ret []byte, leftOverGas uint64, err error) {
	if evm.vmConfig.NoRecursion && evm.depth > 0 {
		return nil, gas, nil
	}
	// Fail if we're trying to execute above the call depth limit
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, ErrDepth
	}

	var (
		snapshot = evm.StateDB.Snapshot()
		to       = AccountRef(caller.Address())
	)

	// Initialise a new contract and make initialise the delegate values
	contract := NewContract(caller, to, nil, gas).AsDelegate()
	contract.SetCallCode(&addr, evm.StateDB.GetCodeHash(addr), evm.StateDB.GetCode(addr))

	ret, err = run(evm, contract, input, false)
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != errExecutionReverted {
			contract.UseGas(contract.Gas)
		}
	}
	return ret, contract.Gas, err
}

// StaticCall executes the contract associated with the addr with the given input
// as parameters while disallowing any modifications to the state during the call.
// Opcodes that attempt to perform such modifications will result in exceptions
// instead of performing the modifications.
func (evm *EVM) StaticCall(caller ContractRef, addr common.Address, input []byte, gas uint64) (ret []byte, leftOverGas uint64, err error) {
	if evm.vmConfig.NoRecursion && evm.depth > 0 {
		return nil, gas, nil
	}
	// Fail if we're trying to execute above the call depth limit
	if evm.depth > int(params.CallCreateDepth) {
		return nil, gas, ErrDepth
	}

	var (
		to       = AccountRef(addr)
		snapshot = evm.StateDB.Snapshot()
	)
	// Initialise a new contract and set the code that is to be used by the
	// EVM. The contract is a scoped environment for this execution context
	// only.
	contract := NewContract(caller, to, new(big.Int), gas)
	contract.SetCallCode(&addr, evm.StateDB.GetCodeHash(addr), evm.StateDB.GetCode(addr))

	// We do an AddBalance of zero here, just in order to trigger a touch.
	// This doesn't matter on Mainnet, where all empties are gone at the time of Byzantium,
	// but is the correct thing to do and matters on other networks, in tests, and potential
	// future scenarios
	evm.StateDB.AddBalance(addr, bigZero)

	// When an error was returned by the EVM or when setting the creation code
	// above we revert to the snapshot and consume any gas remaining. Additionally
	// when we're in Homestead this also counts for code storage gas errors.
	ret, err = run(evm, contract, input, true)
	if err != nil {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != errExecutionReverted {
			contract.UseGas(contract.Gas)
		}
	}
	return ret, contract.Gas, err
}

type codeAndHash struct {
	code []byte
	hash common.Hash
}

func (c *codeAndHash) Hash() common.Hash {
	if c.hash == (common.Hash{}) {
		c.hash = crypto.Keccak256Hash(c.code)
	}
	return c.hash
}

// create creates a new contract using code as deployment code.
func (evm *EVM) create(caller ContractRef, codeAndHash *codeAndHash, gas uint64, value *big.Int, address common.Address) ([]byte, common.Address, uint64, error) {
	// Depth check execution. Fail if we're trying to execute above the
	// limit.
	if evm.depth > int(params.CallCreateDepth) {
		return nil, common.Address{}, gas, ErrDepth
	}
	if !evm.CanTransfer(evm.StateDB, caller.Address(), value) {
		return nil, common.Address{}, gas, ErrInsufficientBalance
	}
	nonce := evm.StateDB.GetNonce(caller.Address())
	evm.StateDB.SetNonce(caller.Address(), nonce+1)

	// Ensure there's no existing contract already at the designated address
	contractHash := evm.StateDB.GetCodeHash(address)
	if evm.StateDB.GetNonce(address) != 0 || (contractHash != (common.Hash{}) && contractHash != emptyCodeHash) {
		return nil, common.Address{}, 0, ErrContractAddressCollision
	}
	// Create a new account on the state
	snapshot := evm.StateDB.Snapshot()
	evm.StateDB.CreateAccount(address)
	if evm.ChainConfig().IsEIP158(evm.BlockNumber) {
		evm.StateDB.SetNonce(address, 1)
	}
	evm.Transfer(evm.StateDB, caller.Address(), address, value)

	// initialise a new contract and set the code that is to be used by the
	// EVM. The contract is a scoped environment for this execution context
	// only.
	contract := NewContract(caller, AccountRef(address), value, gas)
	contract.SetCodeOptionalHash(&address, codeAndHash)

	if evm.vmConfig.NoRecursion && evm.depth > 0 {
		return nil, address, gas, nil
	}

	if evm.vmConfig.Debug && evm.depth == 0 {
		evm.vmConfig.Tracer.CaptureStart(caller.Address(), address, true, codeAndHash.code, gas, value)
	}
	start := time.Now()

	ret, err := run(evm, contract, nil, false)

	// check whether the max code size has been exceeded
	maxCodeSizeExceeded := evm.ChainConfig().IsEIP158(evm.BlockNumber) && len(ret) > params.MaxCodeSize
	// if the contract creation ran successfully and no errors were returned
	// calculate the gas required to store the code. If the code could not
	// be stored due to not enough gas set an error and let it be handled
	// by the error checking condition below.
	if err == nil && !maxCodeSizeExceeded {
		createDataGas := uint64(len(ret)) * params.CreateDataGas
		if contract.UseGas(createDataGas) {
			evm.StateDB.SetCode(address, ret)
		} else {
			err = ErrCodeStoreOutOfGas
		}
	}

	// When an error was returned by the EVM or when setting the creation code
	// above we revert to the snapshot and consume any gas remaining. Additionally
	// when we're in homestead this also counts for code storage gas errors.
	if maxCodeSizeExceeded || (err != nil && (evm.ChainConfig().IsHomestead(evm.BlockNumber) || err != ErrCodeStoreOutOfGas)) {
		evm.StateDB.RevertToSnapshot(snapshot)
		if err != errExecutionReverted {
			contract.UseGas(contract.Gas)
		}
	}
	// Assign err if contract code size exceeds the max while the err is still empty.
	if maxCodeSizeExceeded && err == nil {
		err = errMaxCodeSizeExceeded
	}
	if evm.vmConfig.Debug && evm.depth == 0 {
		evm.vmConfig.Tracer.CaptureEnd(ret, gas-contract.Gas, time.Since(start), err)
	}
	return ret, address, contract.Gas, err

}

// Create creates a new contract using code as deployment code.
func (evm *EVM) Create(caller ContractRef, code []byte, gas uint64, value *big.Int) (ret []byte, contractAddr common.Address, leftOverGas uint64, err error) {
	contractAddr = crypto.CreateAddress(caller.Address(), evm.StateDB.GetNonce(caller.Address()))
	return evm.create(caller, &codeAndHash{code: code}, gas, value, contractAddr)
}

// Create2 creates a new contract using code as deployment code.
//
// The different between Create2 with Create is Create2 uses sha3(0xff ++ msg.sender ++ salt ++ sha3(init_code))[12:]
// instead of the usual sender-and-nonce-hash as the address where the contract is initialized at.
func (evm *EVM) Create2(caller ContractRef, code []byte, gas uint64, endowment *big.Int, salt *big.Int) (ret []byte, contractAddr common.Address, leftOverGas uint64, err error) {
	codeAndHash := &codeAndHash{code: code}
	contractAddr = crypto.CreateAddress2(caller.Address(), common.BigToHash(salt), codeAndHash.Hash().Bytes())
	return evm.create(caller, codeAndHash, gas, endowment, contractAddr)
}

// ChainConfig returns the environment's chain configuration
func (evm *EVM) ChainConfig() *params.ChainConfig { return evm.chainConfig }

// ApplyStorageContractTransaction distinguish and execute transactions
func (evm *EVM) ApplyStorageContractTransaction(caller ContractRef, txType string, data []byte, gas uint64) (ret []byte, leftOverGas uint64, err error) {
	stateSnap := evm.StateDB.Snapshot()
	defer func() {
		if err != nil {
			evm.StateDB.RevertToSnapshot(stateSnap)
		}
	}()

	switch txType {
	case HostAnnounceTransaction:
		return evm.HostAnnounceTx(caller, data, gas)
	case ContractCreateTransaction:
		return evm.CreateContractTx(caller, data, gas)
	case CommitRevisionTransaction:
		return evm.CommitRevisionTx(caller, data, gas)
	case StorageProofTransaction:
		return evm.StorageProofTx(caller, data, gas)
	default:
		return nil, gas, errUnknownStorageContractTx
	}
}

// ApplyDposTransaction handlers all dpos consensus txs
func (evm *EVM) ApplyDposTransaction(txType string, dposContext *types.DposContext, from common.Address, data []byte, gas uint64, value *big.Int) (ret []byte, leftOverGas uint64, err error) {
	dposSnap := dposContext.Snapshot()
	stateSnap := evm.StateDB.Snapshot()
	defer func() {
		if err != nil {
			dposContext.RevertToSnapShot(dposSnap)
			evm.StateDB.RevertToSnapshot(stateSnap)
		}
	}()

	switch txType {
	case ApplyCandidate:
		return evm.CandidateTx(from, data, gas, dposContext)
	case CancelCandidate:
		return evm.CandidateCancelTx(from, gas, dposContext)
	case Vote:
		return evm.VoteTx(from, dposContext, data, gas)
	case CancelVote:
		return evm.CancelVoteTx(from, dposContext, gas)
	default:
		return nil, gas, errUnknownDposOperationTx
	}
}

// HostAnnounceTx host declares its own information on the chain
func (evm *EVM) HostAnnounceTx(caller ContractRef, data []byte, gas uint64) ([]byte, uint64, error) {
	log.Trace("Enter host announce tx executing ... ")

	ha := types.HostAnnouncement{}
	gasDecode, resultDecode := RemainGas(gas, rlp.DecodeBytes, data, &ha)
	errDec, _ := resultDecode[0].(error)
	if errDec != nil {
		return nil, gasDecode, errDec
	}

	gasCheck, resultCheck := RemainGas(gasDecode, CheckMultiSignatures, ha, [][]byte{ha.Signature})
	errCheck, _ := resultCheck[0].(error)
	if errCheck != nil {
		log.Error("Failed to check signature for host announce", "err", errCheck)
		return nil, gasCheck, errCheck
	}

	log.Trace("Host announce tx execution done", "remain_gas", gasCheck, "host_address", ha.NetAddress)

	// return remain gas if everything is ok
	return nil, gasCheck, nil
}

// CreateContractTx executes contract creation tx
func (evm *EVM) CreateContractTx(caller ContractRef, data []byte, gas uint64) ([]byte, uint64, error) {
	log.Trace("Enter create contract tx executing ... ")
	var (
		stateDB  = evm.StateDB
		snapshot = stateDB.Snapshot()
	)

	// rlp decode and calculate gas used
	sc := types.StorageContract{}
	gasRemainDecode, resultDecode := RemainGas(gas, rlp.DecodeBytes, data, &sc)
	errDecode, _ := resultDecode[0].(error)
	if errDecode != nil {
		return nil, gasRemainDecode, errDecode
	}

	// create the expired storage contract status address (e.g. "expired_storage_contract_1500")
	windowEndStr := strconv.FormatUint(sc.WindowEnd, 10)
	statusAddr := common.BytesToAddress([]byte(coinchargemaintenance.StrPrefixExpSC + windowEndStr))

	// create storage contract address, directly use the contract ID
	scID := sc.ID()
	contractAddr := common.BytesToAddress(scID[12:])

	// if the account not exist, create it
	if !stateDB.Exist(statusAddr) {
		stateDB.CreateAccount(statusAddr)

		// before reaching the height windowEnd, mark statusAddr as not empty account to avoid being deleted by stateDB
		stateDB.SetNonce(statusAddr, 1)
	}

	// check if this storage contract exist
	if stateDB.Exist(contractAddr) {
		return nil, gasRemainDecode, errors.New("this storage contract already exist")
	}
	stateDB.CreateAccount(contractAddr)

	// before this contract finished, mark contractAddr as not empty account to avoid being deleted by stateDB
	stateDB.SetNonce(contractAddr, 1)

	// check form contract and calculate gas used
	currentHeight := evm.BlockNumber.Uint64()
	gasRemainCheck, resultCheck := RemainGas(gasRemainDecode, CheckCreateContract, stateDB, sc, uint64(currentHeight))
	errCheck, _ := resultCheck[0].(error)
	if errCheck != nil {
		stateDB.RevertToSnapshot(snapshot)
		log.Error("Failed to check create contract", "err", errCheck)
		return nil, gasRemainCheck, errCheck
	}

	// set balances
	clientAddr := sc.ClientCollateral.Address
	hostAddr := sc.HostCollateral.Address
	clientCollateralAmount := sc.ClientCollateral.Value
	hostCollateralAmount := sc.HostCollateral.Value
	stateDB.SubBalance(clientAddr, clientCollateralAmount)
	stateDB.SubBalance(hostAddr, hostCollateralAmount)

	totalCollateral := new(big.Int).Add(clientCollateralAmount, hostCollateralAmount)
	stateDB.AddBalance(contractAddr, totalCollateral)

	// mark this new storage contract as not proofed
	notProofedStatus := append(coinchargemaintenance.NotProofedStatus, contractAddr[:]...)
	stateDB.SetState(statusAddr, scID, common.BytesToHash(notProofedStatus))

	// store storage contract in this contractAddr's stateDB
	stateDB.SetState(contractAddr, coinchargemaintenance.KeyClientAddress, common.BytesToHash(sc.ClientCollateral.Address.Bytes()))
	stateDB.SetState(contractAddr, coinchargemaintenance.KeyHostAddress, common.BytesToHash(sc.HostCollateral.Address.Bytes()))

	stateDB.SetState(contractAddr, coinchargemaintenance.KeyClientCollateral, common.BytesToHash(sc.ClientCollateral.Value.Bytes()))
	stateDB.SetState(contractAddr, coinchargemaintenance.KeyHostCollateral, common.BytesToHash(sc.HostCollateral.Value.Bytes()))

	uintBytes := Uint64ToBytes(sc.FileSize)
	stateDB.SetState(contractAddr, coinchargemaintenance.KeyFileSize, common.BytesToHash(uintBytes))

	stateDB.SetState(contractAddr, coinchargemaintenance.KeyUnlockHash, sc.UnlockHash)
	stateDB.SetState(contractAddr, coinchargemaintenance.KeyFileMerkleRoot, sc.FileMerkleRoot)

	uintBytes = Uint64ToBytes(sc.RevisionNumber)
	stateDB.SetState(contractAddr, coinchargemaintenance.KeyRevisionNumber, common.BytesToHash(uintBytes))

	uintBytes = Uint64ToBytes(sc.WindowStart)
	stateDB.SetState(contractAddr, coinchargemaintenance.KeyWindowStart, common.BytesToHash(uintBytes))

	uintBytes = Uint64ToBytes(sc.WindowEnd)
	stateDB.SetState(contractAddr, coinchargemaintenance.KeyWindowEnd, common.BytesToHash(uintBytes))

	stateDB.SetState(contractAddr, coinchargemaintenance.KeyClientValidProofOutput, common.BytesToHash(sc.ValidProofOutputs[0].Value.Bytes()))
	stateDB.SetState(contractAddr, coinchargemaintenance.KeyHostValidProofOutput, common.BytesToHash(sc.ValidProofOutputs[1].Value.Bytes()))

	stateDB.SetState(contractAddr, coinchargemaintenance.KeyClientMissedProofOutput, common.BytesToHash(sc.MissedProofOutputs[0].Value.Bytes()))
	stateDB.SetState(contractAddr, coinchargemaintenance.KeyHostMissedProofOutput, common.BytesToHash(sc.MissedProofOutputs[1].Value.Bytes()))

	// return remain gas if everything is ok
	log.Trace("Create contract tx execution done", "remain_gas", gasRemainCheck, "storage_contract_id", scID.Hex())
	return nil, gasRemainCheck, nil
}

// CommitRevisionTx host sends a revision transaction
func (evm *EVM) CommitRevisionTx(caller ContractRef, data []byte, gas uint64) ([]byte, uint64, error) {
	log.Trace("Enter storage contract revision tx executing ... ")
	var (
		stateDB = evm.StateDB
	)

	scr := types.StorageContractRevision{}
	gasRemainDecode, resultDecode := RemainGas(gas, rlp.DecodeBytes, data, &scr)
	errDec, _ := resultDecode[0].(error)
	if errDec != nil {
		return nil, gasRemainDecode, errDec
	}

	// check if the account exist
	contractAddr := common.BytesToAddress(scr.ParentID.Bytes()[12:])
	if !stateDB.Exist(contractAddr) {
		return nil, gasRemainDecode, errors.New("no this storage contract account")
	}

	// check storage contract reversion and calculate gas used
	currentHeight := evm.BlockNumber.Uint64()
	gasRemainCheck, resultCheck := RemainGas(gasRemainDecode, CheckRevisionContract, stateDB, scr, uint64(currentHeight), contractAddr)
	errCheck, _ := resultCheck[0].(error)
	if errCheck != nil {
		log.Error("Failed to check storage contract revision", "err", errCheck)
		return nil, gasRemainCheck, errCheck
	}

	// update revision info
	uintBytes := Uint64ToBytes(scr.NewFileSize)
	stateDB.SetState(contractAddr, coinchargemaintenance.KeyFileSize, common.BytesToHash(uintBytes))

	stateDB.SetState(contractAddr, coinchargemaintenance.KeyFileMerkleRoot, scr.NewFileMerkleRoot)

	uintBytes = Uint64ToBytes(scr.NewRevisionNumber)
	stateDB.SetState(contractAddr, coinchargemaintenance.KeyRevisionNumber, common.BytesToHash(uintBytes))

	stateDB.SetState(contractAddr, coinchargemaintenance.KeyClientValidProofOutput, common.BytesToHash(scr.NewValidProofOutputs[0].Value.Bytes()))
	stateDB.SetState(contractAddr, coinchargemaintenance.KeyHostValidProofOutput, common.BytesToHash(scr.NewValidProofOutputs[1].Value.Bytes()))

	stateDB.SetState(contractAddr, coinchargemaintenance.KeyClientMissedProofOutput, common.BytesToHash(scr.NewMissedProofOutputs[0].Value.Bytes()))
	stateDB.SetState(contractAddr, coinchargemaintenance.KeyHostMissedProofOutput, common.BytesToHash(scr.NewMissedProofOutputs[1].Value.Bytes()))

	log.Trace("Storage contract reversion tx execution done", "remain_gas", gasRemainCheck, "storage_contract_id", scr.ParentID.Hex())
	return nil, gasRemainCheck, nil
}

// StorageProofTx host send storage certificate transaction
func (evm *EVM) StorageProofTx(caller ContractRef, data []byte, gas uint64) ([]byte, uint64, error) {
	log.Trace("Enter storage proof tx executing ... ")
	var (
		stateDB = evm.StateDB
	)

	sp := types.StorageProof{}
	gasRemainDec, resultDec := RemainGas(gas, rlp.DecodeBytes, data, &sp)
	errDec, _ := resultDec[0].(error)
	if errDec != nil {
		return nil, gasRemainDec, errDec
	}

	currentHeight := evm.BlockNumber.Uint64()

	contractAddr := common.BytesToAddress(sp.ParentID[12:])
	if !stateDB.Exist(contractAddr) {
		return nil, gasRemainDec, errors.New("no this storage contract account")
	}

	// retrieve origin data in storage contract
	windowEndHash := stateDB.GetState(contractAddr, coinchargemaintenance.KeyWindowEnd)
	clientValidOutputHash := stateDB.GetState(contractAddr, coinchargemaintenance.KeyClientValidProofOutput)
	hostValidOutputHash := stateDB.GetState(contractAddr, coinchargemaintenance.KeyHostValidProofOutput)
	clientAddressHash := stateDB.GetState(contractAddr, coinchargemaintenance.KeyClientAddress)
	hostAddressHash := stateDB.GetState(contractAddr, coinchargemaintenance.KeyHostAddress)

	// get status account address
	windowEnd := new(big.Int).SetBytes(windowEndHash.Bytes()).Uint64()
	windowEndStr := strconv.FormatUint(windowEnd, 10)
	statusAddr := common.BytesToAddress([]byte(coinchargemaintenance.StrPrefixExpSC + windowEndStr))

	gasRemainCheck, resultCheck := RemainGas(gasRemainDec, CheckStorageProof, stateDB, sp, uint64(currentHeight), statusAddr, contractAddr)
	errCheck, _ := resultCheck[0].(error)
	if errCheck != nil {
		return nil, gasRemainCheck, errCheck
	}

	// effect valid proof outputs, first for client, second for host
	clientValidOutput := new(big.Int).SetBytes(clientValidOutputHash.Bytes())
	clientAddress := common.BytesToAddress(clientAddressHash.Bytes())
	stateDB.AddBalance(clientAddress, clientValidOutput)

	hostValidOutput := new(big.Int).SetBytes(hostValidOutputHash.Bytes())
	hostAddress := common.BytesToAddress(hostAddressHash.Bytes())
	stateDB.AddBalance(hostAddress, hostValidOutput)

	totalValue := new(big.Int).SetInt64(0)
	totalValue.Add(clientValidOutput, hostValidOutput)
	stateDB.SubBalance(contractAddr, totalValue)

	// set completed for this storage contract
	proofedStatus := append(coinchargemaintenance.ProofedStatus, contractAddr[:]...)
	stateDB.SetState(statusAddr, sp.ParentID, common.BytesToHash(proofedStatus))

	// this contract is finished, so mark it empty account that will be deleted by stateDB
	stateDB.SetNonce(contractAddr, 0)

	log.Trace("Storage proof tx execution done", "storage_contract_id", sp.ParentID.Hex())
	return nil, gasRemainCheck, nil
}

// Uint64ToBytes convert uint64 to bytes
func Uint64ToBytes(i uint64) []byte {
	var buf = make([]byte, 8)
	binary.BigEndian.PutUint64(buf, i)
	return buf
}

// CandidateTx campaign becomes a candidate and pledges part of the assets.
func (evm *EVM) CandidateTx(caller common.Address, data []byte, gas uint64, dposContext *types.DposContext) ([]byte, uint64, error) {
	log.Trace("Enter candidate tx executing ... ")
	var voteData *types.AddCandidateTxData
	gasRemainDec, resultDec := RemainGas(gas, rlp.DecodeBytes, data, &voteData)
	errDec, _ := resultDec[0].(error)
	if errDec != nil {
		return nil, gasRemainDec, errDec
	}
	// Add candidate in dpos
	if err := dpos.ProcessAddCandidate(evm.StateDB, dposContext, caller, voteData.Deposit, voteData.RewardRatio); err != nil {
		return nil, gasRemainDec, err
	}
	// defines that dposCtx.BecomeCandidate and SetState all cost params.SstoreSetGas
	ok, gasRemain := DeductGas(gasRemainDec, params.SstoreSetGas*3)
	if !ok {
		return nil, gasRemainDec, ErrOutOfGas
	}

	log.Trace("Candidate tx execution done")
	return nil, gasRemain, nil
}

// CandidateCancelTx cancellation of candidate thawing assets requires a defrosting period.
func (evm *EVM) CandidateCancelTx(caller common.Address, gas uint64, dposContext *types.DposContext) ([]byte, uint64, error) {
	log.Trace("Enter cancel candidate tx executing ... ")
	if err := dpos.ProcessCancelCandidate(evm.StateDB, dposContext, caller, evm.Time.Int64()); err != nil {
		return nil, gas, err
	}
	// defines that dposCtx.KickoutCandidate and markThawingAddress all cost params.SstoreSetGas
	ok, gasRemain := DeductGas(gas, params.SstoreSetGas*2)
	if !ok {
		return nil, gas, ErrOutOfGas
	}
	log.Trace("Cancel candidate tx execution done")
	return nil, gasRemain, nil
}

// VoteTx handles a new vote to some candidates that will remove last vote records
func (evm *EVM) VoteTx(caller common.Address, dposCtx *types.DposContext, data []byte, gas uint64) ([]byte, uint64, error) {
	log.Trace("Enter vote tx executing ... ")
	var voteData *types.VoteTxData
	gasRemainDec, resultDec := RemainGas(gas, rlp.DecodeBytes, data, &voteData)
	errDec, _ := resultDec[0].(error)
	if errDec != nil {
		return nil, gasRemainDec, errDec
	}
	successVote, err := dpos.ProcessVote(evm.StateDB, dposCtx, caller, voteData.Deposit, voteData.Candidates, evm.Time.Int64())
	if err != nil {
		return nil, gasRemainDec, err
	}
	// defines that dposCtx.Vote and SetState all cost params.SstoreSetGas
	ok, gasRemain := DeductGas(gasRemainDec, params.SstoreSetGas*4)
	if !ok {
		return nil, gasRemainDec, ErrOutOfGas
	}
	log.Trace("Vote tx execution done", "vote_count", successVote)
	return nil, gasRemain, nil
}

// CancelVoteTx handles a cancel vote tx that will remove all vote records
func (evm *EVM) CancelVoteTx(caller common.Address, dposCtx *types.DposContext, gas uint64) ([]byte, uint64, error) {
	log.Trace("Enter cancel vote tx executing ... ")
	// remove all vote record from dpos context

	if err := dpos.ProcessCancelVote(evm.StateDB, dposCtx, caller, evm.Time.Int64()); err != nil {
		return nil, gas, err
	}
	ok, gasRemain := DeductGas(gas, params.SstoreSetGas*2)
	if !ok {
		return nil, gas, ErrOutOfGas
	}

	log.Trace("Cancel vote tx execution done")
	return nil, gasRemain, nil
}
