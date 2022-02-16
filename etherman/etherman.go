package etherman

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/hermeznetwork/hermez-core/etherman/smartcontracts/bridge"
	"github.com/hermeznetwork/hermez-core/log"
)

var (
	ownershipTransferredSignatureHash      = crypto.Keccak256Hash([]byte("OwnershipTransferred(address,address)"))
	depositEventSignatureHash              = crypto.Keccak256Hash([]byte("DepositEvent(address,uint256,uint32,address,uint32)"))
	updateGlobalExitRootEventSignatureHash = crypto.Keccak256Hash([]byte("UpdateGlobalExitRoot(bytes32,bytes32)"))
	claimEventSignatureHash                = crypto.Keccak256Hash([]byte("WithdrawEvent(uint64,uint32,address,uint256,address)"))
)

type ethClienter interface {
	ethereum.ChainReader
	ethereum.LogFilterer
	ethereum.TransactionReader
}

// ClientEtherMan struct
type ClientEtherMan struct {
	EtherClient ethClienter
	Bridge      *bridge.Bridge
	SCAddresses []common.Address
}

// EtherMan interface
type EtherMan interface {
	GetBridgeInfoByBlockRange(ctx context.Context, fromBlock uint64, toBlock *uint64) ([]Block, map[common.Hash][]Order, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
}

// NewEtherman creates a new etherman
func NewEtherman(cfg Config, bridgeAddr common.Address) (*ClientEtherMan, error) {
	// TODO: PoEAddr can be got from bridge smc. Son only bridge smc is required
	// Connect to ethereum node
	ethClient, err := ethclient.Dial(cfg.URL)
	if err != nil {
		log.Errorf("error connecting to %s: %+v", cfg.URL, err)
		return nil, err
	}
	// Create smc clients
	bridge, err := bridge.NewBridge(bridgeAddr, ethClient)
	if err != nil {
		return nil, err
	}
	var scAddresses []common.Address
	scAddresses = append(scAddresses, bridgeAddr)

	return &ClientEtherMan{EtherClient: ethClient, Bridge: bridge, SCAddresses: scAddresses}, nil
}

// GetBridgeInfoByBlockRange function retrieves the Bridge information that are included in all this ethereum blocks
// from block x to block y
func (etherMan *ClientEtherMan) GetBridgeInfoByBlockRange(ctx context.Context, fromBlock uint64, toBlock *uint64) ([]Block, map[common.Hash][]Order, error) {
	// First filter query
	query := ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(fromBlock),
		Addresses: etherMan.SCAddresses,
	}
	if toBlock != nil {
		query.ToBlock = new(big.Int).SetUint64(*toBlock)
	}
	blocks, order, err := etherMan.readEvents(ctx, query)
	if err != nil {
		return nil, nil, err
	}
	return blocks, order, nil
}

func (etherMan *ClientEtherMan) readEvents(ctx context.Context, query ethereum.FilterQuery) ([]Block, map[common.Hash][]Order, error) {
	logs, err := etherMan.EtherClient.FilterLogs(ctx, query)
	if err != nil {
		return []Block{}, nil, err
	}

	blockOrder := make(map[common.Hash][]Order)
	blocks := make(map[common.Hash]Block)
	var blockKeys []common.Hash

	for _, vLog := range logs {
		block, err := etherMan.processEvent(ctx, vLog)
		if err != nil {
			log.Warnf("error processing event. Retrying... Error: %w. vLog: %+v", err, vLog)
			break
		}
		if block == nil {
			continue
		}
		if b, exists := blocks[block.BlockHash]; exists {
			if len(block.Deposits) != 0 {
				b.Deposits = append(blocks[block.BlockHash].Deposits, block.Deposits...)
				or := Order{
					Name: DepositsOrder,
					Pos:  len(b.Deposits) - 1,
				}
				blockOrder[b.BlockHash] = append(blockOrder[b.BlockHash], or)
			}
			if len(block.GlobalExitRoots) != 0 {
				b.GlobalExitRoots = append(blocks[block.BlockHash].GlobalExitRoots, block.GlobalExitRoots...)
				or := Order{
					Name: GlobalExitRootsOrder,
					Pos:  len(b.GlobalExitRoots) - 1,
				}
				blockOrder[b.BlockHash] = append(blockOrder[b.BlockHash], or)
			}
			if len(block.Claims) != 0 {
				b.Claims = append(blocks[block.BlockHash].Claims, block.Claims...)
				or := Order{
					Name: ClaimsOrder,
					Pos:  len(b.Claims) - 1,
				}
				blockOrder[b.BlockHash] = append(blockOrder[b.BlockHash], or)
			}
			blocks[block.BlockHash] = b
		} else {
			if len(block.Deposits) != 0 {
				or := Order{
					Name: DepositsOrder,
					Pos:  len(block.Deposits) - 1,
				}
				blockOrder[block.BlockHash] = append(blockOrder[block.BlockHash], or)
			}
			if len(block.GlobalExitRoots) != 0 {
				or := Order{
					Name: GlobalExitRootsOrder,
					Pos:  len(block.GlobalExitRoots) - 1,
				}
				blockOrder[block.BlockHash] = append(blockOrder[block.BlockHash], or)
			}
			if len(block.Claims) != 0 {
				or := Order{
					Name: ClaimsOrder,
					Pos:  len(block.Claims) - 1,
				}
				blockOrder[block.BlockHash] = append(blockOrder[block.BlockHash], or)
			}
			blocks[block.BlockHash] = *block
			blockKeys = append(blockKeys, block.BlockHash)
		}
	}
	var blockArr []Block
	for _, hash := range blockKeys {
		blockArr = append(blockArr, blocks[hash])
	}
	return blockArr, blockOrder, nil
}

func (etherMan *ClientEtherMan) processEvent(ctx context.Context, vLog types.Log) (*Block, error) {
	switch vLog.Topics[0] {
	case ownershipTransferredSignatureHash:
		ownership, err := etherMan.Bridge.ParseOwnershipTransferred(vLog)
		if err != nil {
			return nil, err
		}
		emptyAddr := common.Address{}
		if ownership.PreviousOwner == emptyAddr {
			log.Debug("New bridge smc deployment detected. Deployment account: ", ownership.NewOwner)
		} else {
			log.Debug("Bridge smc OwnershipTransferred from account ", ownership.PreviousOwner, " to ", ownership.NewOwner)
		}
		return nil, nil
	case depositEventSignatureHash:
		log.Debug("Deposit event detected")
		deposit, err := etherMan.Bridge.ParseDepositEvent(vLog)
		if err != nil {
			return nil, err
		}
		var (
			block      Block
			depositAux Deposit
		)
		depositAux.Amount = deposit.Amount
		depositAux.BlockNumber = vLog.BlockNumber
		depositAux.DestinationAddress = deposit.DestinationAddress
		depositAux.DestinationNetwork = uint(deposit.DestinationNetwork)
		depositAux.TokenAddres = deposit.TokenAddres
		block.BlockHash = vLog.BlockHash
		block.BlockNumber = vLog.BlockNumber
		fullBlock, err := etherMan.EtherClient.BlockByHash(ctx, vLog.BlockHash)
		if err != nil {
			return nil, fmt.Errorf("error getting hashParent. BlockNumber: %d. Error: %w", block.BlockNumber, err)
		}
		block.ParentHash = fullBlock.ParentHash()
		block.Deposits = append(block.Deposits, depositAux)
		return &block, nil
	case updateGlobalExitRootEventSignatureHash:
		log.Debug("UpdateGlobalExitRoot event detected")
		globalExitRoot, err := etherMan.Bridge.ParseUpdateGlobalExitRoot(vLog)
		if err != nil {
			return nil, err
		}
		var (
			block     Block
			gExitRoot GlobalExitRoot
		)
		gExitRoot.MainnetExitRoot = globalExitRoot.MainnetExitRoot
		gExitRoot.RollupExitRoot = globalExitRoot.RollupExitRoot
		block.BlockHash = vLog.BlockHash
		block.BlockNumber = vLog.BlockNumber
		fullBlock, err := etherMan.EtherClient.BlockByHash(ctx, vLog.BlockHash)
		if err != nil {
			return nil, fmt.Errorf("error getting hashParent. BlockNumber: %d. Error: %w", block.BlockNumber, err)
		}
		block.ParentHash = fullBlock.ParentHash()
		block.GlobalExitRoots = append(block.GlobalExitRoots, gExitRoot)
		return &block, nil
	case claimEventSignatureHash:
		log.Debug("Claim event detected")
		claim, err := etherMan.Bridge.ParseWithdrawEvent(vLog)
		if err != nil {
			return nil, err
		}
		var (
			block    Block
			claimAux Claim
		)
		claimAux.Amount = claim.Amount
		claimAux.DestinationAddress = claim.DestinationAddress
		claimAux.Index = claim.Index
		claimAux.OriginalNetwork = uint(claim.OriginalNetwork)
		claimAux.Token = claim.Token
		claimAux.BlockNumber = vLog.BlockNumber
		block.BlockHash = vLog.BlockHash
		block.BlockNumber = vLog.BlockNumber
		fullBlock, err := etherMan.EtherClient.BlockByHash(ctx, vLog.BlockHash)
		if err != nil {
			return nil, fmt.Errorf("error getting hashParent. BlockNumber: %d. Error: %w", block.BlockNumber, err)
		}
		block.ParentHash = fullBlock.ParentHash()
		block.Claims = append(block.Claims, claimAux)
		return &block, nil
	}
	log.Debug("Event not registered: ", vLog)
	return nil, nil
}

// HeaderByNumber returns a block header from the current canonical chain. If number is
// nil, the latest known header is returned.
func (etherMan *ClientEtherMan) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	return etherMan.EtherClient.HeaderByNumber(ctx, number)
}