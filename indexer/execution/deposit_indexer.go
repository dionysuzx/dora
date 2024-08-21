package execution

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math/big"
	"strings"
	"time"

	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/jmoiron/sqlx"
	blsu "github.com/protolambda/bls12-381-util"
	zrnt_common "github.com/protolambda/zrnt/eth2/beacon/common"
	"github.com/protolambda/ztyp/tree"
	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/dora/clients/execution"
	"github.com/ethpandaops/dora/db"
	"github.com/ethpandaops/dora/dbtypes"
	"github.com/ethpandaops/dora/utils"
)

type DepositIndexer struct {
	indexer             *IndexerCtx
	logger              logrus.FieldLogger
	state               *dbtypes.DepositIndexerState
	batchSize           int
	depositContract     common.Address
	depositContractAbi  *abi.ABI
	depositEventTopic   []byte
	depositSigDomain    zrnt_common.BLSDomain
	unfinalizedDeposits map[uint64]map[common.Hash]bool
}

const depositContractAbi = `[{"inputs":[],"stateMutability":"nonpayable","type":"constructor"},{"anonymous":false,"inputs":[{"indexed":false,"internalType":"bytes","name":"pubkey","type":"bytes"},{"indexed":false,"internalType":"bytes","name":"withdrawal_credentials","type":"bytes"},{"indexed":false,"internalType":"bytes","name":"amount","type":"bytes"},{"indexed":false,"internalType":"bytes","name":"signature","type":"bytes"},{"indexed":false,"internalType":"bytes","name":"index","type":"bytes"}],"name":"DepositEvent","type":"event"},{"inputs":[{"internalType":"bytes","name":"pubkey","type":"bytes"},{"internalType":"bytes","name":"withdrawal_credentials","type":"bytes"},{"internalType":"bytes","name":"signature","type":"bytes"},{"internalType":"bytes32","name":"deposit_data_root","type":"bytes32"}],"name":"deposit","outputs":[],"stateMutability":"payable","type":"function"},{"inputs":[],"name":"get_deposit_count","outputs":[{"internalType":"bytes","name":"","type":"bytes"}],"stateMutability":"view","type":"function"},{"inputs":[],"name":"get_deposit_root","outputs":[{"internalType":"bytes32","name":"","type":"bytes32"}],"stateMutability":"view","type":"function"},{"inputs":[{"internalType":"bytes4","name":"interfaceId","type":"bytes4"}],"name":"supportsInterface","outputs":[{"internalType":"bool","name":"","type":"bool"}],"stateMutability":"pure","type":"function"}]`

func NewDepositIndexer(indexer *IndexerCtx) *DepositIndexer {
	batchSize := utils.Config.ExecutionApi.DepositLogBatchSize
	if batchSize == 0 {
		batchSize = 1000
	}

	contractAbi, err := abi.JSON(strings.NewReader(depositContractAbi))
	if err != nil {
		log.Fatal(err)
	}

	depositEventTopic := crypto.Keccak256Hash([]byte(contractAbi.Events["DepositEvent"].Sig))

	specs := indexer.chainState.GetSpecs()
	genesisForkVersion := specs.GenesisForkVersion
	depositSigDomain := zrnt_common.ComputeDomain(zrnt_common.DOMAIN_DEPOSIT, zrnt_common.Version(genesisForkVersion), zrnt_common.Root{})

	ds := &DepositIndexer{
		indexer:             indexer,
		logger:              indexer.logger.WithField("indexer", "deposit"),
		batchSize:           batchSize,
		depositContract:     common.Address(specs.DepositContractAddress),
		depositContractAbi:  &contractAbi,
		depositEventTopic:   depositEventTopic[:],
		depositSigDomain:    depositSigDomain,
		unfinalizedDeposits: map[uint64]map[common.Hash]bool{},
	}

	go ds.runDepositIndexerLoop()

	return ds
}

func (ds *DepositIndexer) runDepositIndexerLoop() {
	defer utils.HandleSubroutinePanic("runCacheLoop")

	for {
		time.Sleep(60 * time.Second)
		ds.logger.Debugf("run deposit indexer logic")

		err := ds.runDepositIndexer()
		if err != nil {
			ds.logger.Errorf("deposit indexer error: %v", err)
		}
	}
}

func (ds *DepositIndexer) runDepositIndexer() error {
	// get indexer state
	if ds.state == nil {
		ds.loadState()
	}

	justifiedEpoch, justifiedRoot := ds.indexer.chainState.GetJustifiedCheckpoint()
	if justifiedEpoch > 0 {

		finalizedBlock := ds.indexer.beaconIndexer.GetBlockByRoot(justifiedRoot)
		if finalizedBlock == nil {
			return fmt.Errorf("could not get finalized block from cache (0x%x)", justifiedRoot)
		}

		indexVals := finalizedBlock.GetBlockIndex()
		if indexVals == nil {
			return fmt.Errorf("could not get finalized block index values (0x%x)", justifiedRoot)
		}

		finalizedBlockNumber := indexVals.ExecutionNumber

		if finalizedBlockNumber < ds.state.FinalBlock {
			return fmt.Errorf("finalized block number (%v) smaller than index state (%v)", finalizedBlockNumber, ds.state.FinalBlock)
		}

		if finalizedBlockNumber > ds.state.FinalBlock {
			err := ds.processFinalizedBlocks(finalizedBlockNumber)
			if err != nil {
				return err
			}
		}
	}

	ds.processRecentBlocks()

	return nil
}

func (ds *DepositIndexer) loadState() {
	syncState := dbtypes.DepositIndexerState{}
	db.GetExplorerState("indexer.depositstate", &syncState)
	ds.state = &syncState
}

func (ds *DepositIndexer) loadFilteredLogs(ctx context.Context, client *execution.Client, query ethereum.FilterQuery) ([]types.Log, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	return client.GetRPCClient().GetEthClient().FilterLogs(ctx, query)
}

func (ds *DepositIndexer) loadTransactionByHash(ctx context.Context, client *execution.Client, hash common.Hash) (*types.Transaction, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tx, _, err := client.GetRPCClient().GetEthClient().TransactionByHash(ctx, hash)
	return tx, err
}

func (ds *DepositIndexer) loadHeaderByNumber(ctx context.Context, client *execution.Client, number uint64) (*types.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	return client.GetRPCClient().GetHeaderByNumber(ctx, number)
}

func (ds *DepositIndexer) processFinalizedBlocks(finalizedBlockNumber uint64) error {
	clients := ds.indexer.getFinalizedClients(execution.AnyClient)
	if len(clients) == 0 {
		return fmt.Errorf("no ready execution client found")
	}
	client := clients[0]

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for ds.state.FinalBlock < finalizedBlockNumber {
		toBlock := ds.state.FinalBlock + uint64(ds.batchSize)
		if toBlock > finalizedBlockNumber {
			toBlock = finalizedBlockNumber
		}

		query := ethereum.FilterQuery{
			FromBlock: big.NewInt(0).SetUint64(ds.state.FinalBlock + 1),
			ToBlock:   big.NewInt(0).SetUint64(toBlock),
			Addresses: []common.Address{
				ds.depositContract,
			},
		}

		logs, err := ds.loadFilteredLogs(ctx, client, query)
		if err != nil {
			return fmt.Errorf("error fetching deposit contract logs: %v", err)
		}

		var txHash []byte
		var txDetails *types.Transaction
		var txBlockHeader *types.Header

		depositTxs := []*dbtypes.DepositTx{}

		ds.logger.Infof("received deposit log for block %v - %v: %v events", ds.state.FinalBlock, toBlock, len(logs))

		for idx := range logs {
			log := &logs[idx]
			if !bytes.Equal(log.Topics[0][:], ds.depositEventTopic) {
				continue
			}

			event, err := ds.depositContractAbi.Unpack("DepositEvent", log.Data)
			if err != nil {
				return fmt.Errorf("error decoding deposit event (%v): %v", log.TxHash, err)

			}

			if txHash == nil || !bytes.Equal(txHash, log.TxHash[:]) {
				txDetails, err = ds.loadTransactionByHash(ctx, client, log.TxHash)
				if err != nil {
					return fmt.Errorf("could not load tx details (%v): %v", log.TxHash, err)
				}

				txBlockHeader, err = ds.loadHeaderByNumber(ctx, client, log.BlockNumber)
				if err != nil {
					return fmt.Errorf("could not load block details (%v): %v", log.TxHash, err)
				}

				txHash = log.TxHash[:]
			}

			txFrom, err := types.Sender(types.LatestSignerForChainID(txDetails.ChainId()), txDetails)
			if err != nil {
				return fmt.Errorf("could not decode tx sender (%v): %v", log.TxHash, err)
			}
			txTo := *txDetails.To()

			depositTx := &dbtypes.DepositTx{
				Index:                 binary.LittleEndian.Uint64(event[4].([]byte)),
				BlockNumber:           log.BlockNumber,
				BlockTime:             txBlockHeader.Time,
				BlockRoot:             log.BlockHash[:],
				PublicKey:             event[0].([]byte),
				WithdrawalCredentials: event[1].([]byte),
				Amount:                binary.LittleEndian.Uint64(event[2].([]byte)),
				Signature:             event[3].([]byte),
				TxHash:                log.TxHash[:],
				TxSender:              txFrom[:],
				TxTarget:              txTo[:],
			}
			ds.checkDepositValidity(depositTx)
			depositTxs = append(depositTxs, depositTx)
		}

		if len(depositTxs) > 0 {
			ds.logger.Infof("crawled deposits for block %v - %v: %v deposits", ds.state.FinalBlock, toBlock, len(depositTxs))

			depositCount := len(depositTxs)
			for depositIdx := 0; depositIdx < depositCount; depositIdx += 500 {
				endIdx := depositIdx + 500
				if endIdx > depositCount {
					endIdx = depositCount
				}

				err = ds.persistFinalizedDepositTxs(toBlock, depositTxs[depositIdx:endIdx])
				if err != nil {
					return fmt.Errorf("could not persist deposit txs: %v", err)
				}
			}

			for _, depositTx := range depositTxs {
				if ds.unfinalizedDeposits[depositTx.Index] != nil {
					delete(ds.unfinalizedDeposits, depositTx.Index)
				}
			}

			time.Sleep(1 * time.Second)
		} else {
			err = ds.persistFinalizedDepositTxs(toBlock, nil)
			if err != nil {
				return fmt.Errorf("could not persist deposit state: %v", err)
			}
		}
	}
	return nil
}

func (ds *DepositIndexer) processRecentBlocks() error {
	headForks := ds.indexer.getForksWithClients(execution.AnyClient)
	for _, headFork := range headForks {
		err := ds.processRecentBlocksForFork(headFork)
		if err != nil {
			if headFork.canonical {
				ds.logger.Errorf("could not process recent events from canonical fork %v: %v", headFork.forkId, err)
			} else {
				ds.logger.Warnf("could not process recent events from fork %v: %v", headFork.forkId, err)
			}
		}
	}
	return nil
}

func (ds *DepositIndexer) processRecentBlocksForFork(headFork *forkWithClients) error {
	elHeadBlock := ds.indexer.beaconIndexer.GetCanonicalHead(&headFork.forkId)
	if elHeadBlock == nil {
		return fmt.Errorf("head block not found")
	}

	elHeadBlockIndex := elHeadBlock.GetBlockIndex()
	if elHeadBlockIndex == nil {
		return fmt.Errorf("head block index not found")
	}

	elHeadBlockNumber := elHeadBlockIndex.ExecutionNumber

	var resError error
	var ctxCancel context.CancelFunc
	defer func() {
		if ctxCancel != nil {
			ctxCancel()
		}
	}()

	for retryCount := 0; retryCount < 3; retryCount++ {
		client := headFork.clients[retryCount%len(headFork.clients)]

		if ctxCancel != nil {
			ctxCancel()
		}
		ctx, cancel := context.WithCancel(context.Background())
		ctxCancel = cancel

		query := ethereum.FilterQuery{
			FromBlock: big.NewInt(0).SetUint64(ds.state.FinalBlock + 1),
			ToBlock:   big.NewInt(0).SetUint64(elHeadBlockNumber - 1),
			Addresses: []common.Address{
				ds.depositContract,
			},
		}

		logs, err := ds.loadFilteredLogs(ctx, client, query)
		if err != nil {
			return fmt.Errorf("error fetching deposit contract logs: %v", err)
		}

		var txHash []byte
		var txDetails *types.Transaction
		var txBlockHeader *types.Header

		depositTxs := []*dbtypes.DepositTx{}

		for idx := range logs {
			log := &logs[idx]
			if !bytes.Equal(log.Topics[0][:], ds.depositEventTopic) {
				continue
			}

			event, err := ds.depositContractAbi.Unpack("DepositEvent", log.Data)
			if err != nil {
				return fmt.Errorf("error decoding deposit event (%v): %v", log.TxHash, err)

			}

			depositIndex := binary.LittleEndian.Uint64(event[4].([]byte))
			if ds.unfinalizedDeposits[depositIndex] != nil && ds.unfinalizedDeposits[depositIndex][log.BlockHash] {
				continue
			}

			if txHash == nil || !bytes.Equal(txHash, log.TxHash[:]) {
				txHash = log.TxHash[:]

				txDetails, err = ds.loadTransactionByHash(ctx, client, log.TxHash)
				if err != nil {
					return fmt.Errorf("could not load tx details (%v): %v", log.TxHash, err)
				}

				txBlockHeader, err = ds.loadHeaderByNumber(ctx, client, log.BlockNumber)
				if err != nil {
					return fmt.Errorf("could not load block details (%v): %v", log.TxHash, err)
				}
			}

			depositForkId := headFork.forkId
			beaconBlock := ds.indexer.beaconIndexer.GetBlocksByExecutionBlockHash(phase0.Hash32(log.BlockHash))
			if len(beaconBlock) == 1 {
				depositForkId = beaconBlock[0].GetForkId()
			} else if len(beaconBlock) > 1 {
				depositForkId = beaconBlock[0].GetForkId()
				ds.logger.Warnf("found multiple beacon blocks for deposit block hash %v", log.BlockHash)
			}

			txFrom, err := types.Sender(types.LatestSignerForChainID(txDetails.ChainId()), txDetails)
			if err != nil {
				return fmt.Errorf("could not decode tx sender (%v): %v", log.TxHash, err)
			}
			txTo := *txDetails.To()

			depositTx := &dbtypes.DepositTx{
				Index:                 depositIndex,
				BlockNumber:           log.BlockNumber,
				BlockTime:             txBlockHeader.Time,
				BlockRoot:             log.BlockHash[:],
				PublicKey:             event[0].([]byte),
				WithdrawalCredentials: event[1].([]byte),
				Amount:                binary.LittleEndian.Uint64(event[2].([]byte)),
				Signature:             event[3].([]byte),
				Orphaned:              true,
				ForkId:                uint64(depositForkId),
				TxHash:                log.TxHash[:],
				TxSender:              txFrom[:],
				TxTarget:              txTo[:],
			}

			ds.checkDepositValidity(depositTx)
			depositTxs = append(depositTxs, depositTx)
		}

		if len(depositTxs) > 0 {
			ds.logger.Infof("crawled recent deposits for fork %v since block %v: %v deposits", headFork.forkId, ds.state.FinalBlock, len(depositTxs))

			depositCount := len(depositTxs)
			for depositIdx := 0; depositIdx < depositCount; depositIdx += 500 {
				endIdx := depositIdx + 500
				if endIdx > depositCount {
					endIdx = depositCount
				}

				err = ds.persistRecentDepositTxs(depositTxs[depositIdx:endIdx])
				if err != nil {
					return fmt.Errorf("could not persist deposit txs: %v", err)
				}
			}

			for _, depositTx := range depositTxs {
				if ds.unfinalizedDeposits[depositTx.Index] == nil {
					ds.unfinalizedDeposits[depositTx.Index] = map[common.Hash]bool{}
				}
				ds.unfinalizedDeposits[depositTx.Index][common.Hash(depositTx.BlockRoot)] = true
			}

			time.Sleep(1 * time.Second)
		}
	}

	return resError
}

func (ds *DepositIndexer) checkDepositValidity(depositTx *dbtypes.DepositTx) {
	depositMsg := &zrnt_common.DepositMessage{
		Pubkey:                zrnt_common.BLSPubkey(depositTx.PublicKey),
		WithdrawalCredentials: tree.Root(depositTx.WithdrawalCredentials),
		Amount:                zrnt_common.Gwei(depositTx.Amount),
	}
	depositRoot := depositMsg.HashTreeRoot(tree.GetHashFn())
	signingRoot := zrnt_common.ComputeSigningRoot(
		depositRoot,
		ds.depositSigDomain,
	)

	pubkey, err := depositMsg.Pubkey.Pubkey()
	sigData := zrnt_common.BLSSignature(depositTx.Signature)
	sig, err2 := sigData.Signature()
	if err == nil && err2 == nil && blsu.Verify(pubkey, signingRoot[:], sig) {
		depositTx.ValidSignature = true
	}
}

func (ds *DepositIndexer) persistFinalizedDepositTxs(toBlockNumber uint64, deposits []*dbtypes.DepositTx) error {
	return db.RunDBTransaction(func(tx *sqlx.Tx) error {
		if len(deposits) > 0 {
			err := db.InsertDepositTxs(deposits, tx)
			if err != nil {
				return fmt.Errorf("error while inserting deposit txs: %v", err)
			}
		}

		ds.state.FinalBlock = toBlockNumber
		if toBlockNumber > ds.state.HeadBlock {
			ds.state.HeadBlock = toBlockNumber
		}

		err := db.SetExplorerState("indexer.depositstate", ds.state, tx)
		if err != nil {
			return fmt.Errorf("error while updating deposit state: %v", err)
		}

		return nil
	})
}

func (ds *DepositIndexer) persistRecentDepositTxs(deposits []*dbtypes.DepositTx) error {
	return db.RunDBTransaction(func(tx *sqlx.Tx) error {
		err := db.InsertDepositTxs(deposits, tx)
		if err != nil {
			return fmt.Errorf("error while inserting deposit txs: %v", err)
		}

		return nil
	})
}
