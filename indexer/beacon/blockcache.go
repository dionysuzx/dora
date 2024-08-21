package beacon

import (
	"bytes"
	"sort"
	"sync"

	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/ethpandaops/dora/clients/consensus"
	"github.com/ethpandaops/dora/db"
)

// blockCache is a cache for storing blocks.
type blockCache struct {
	indexer     *Indexer
	cacheMutex  sync.RWMutex
	highestSlot int64
	lowestSlot  int64
	slotMap     map[phase0.Slot][]*Block
	rootMap     map[phase0.Root]*Block
	latestBlock *Block // latest added block (might not be the head block, just a marker for cache changes)
}

// newBlockCache creates a new instance of blockCache.
func newBlockCache(indexer *Indexer) *blockCache {
	return &blockCache{
		indexer: indexer,
		slotMap: map[phase0.Slot][]*Block{},
		rootMap: map[phase0.Root]*Block{},
	}
}

// createOrGetBlock creates a new block with the given root and slot, or returns an existing block if it already exists.
// It returns the created block and a boolean indicating whether the block was newly created or not.
func (cache *blockCache) createOrGetBlock(root phase0.Root, slot phase0.Slot) (*Block, bool) {
	cache.cacheMutex.Lock()
	defer cache.cacheMutex.Unlock()

	if cache.rootMap[root] != nil {
		return cache.rootMap[root], false
	}

	cacheBlock := newBlock(cache.indexer.dynSsz, root, slot)
	cache.rootMap[root] = cacheBlock

	if cache.slotMap[slot] == nil {
		cache.slotMap[slot] = []*Block{cacheBlock}
	} else {
		cache.slotMap[slot] = append(cache.slotMap[slot], cacheBlock)
	}

	if int64(slot) > cache.highestSlot {
		cache.highestSlot = int64(slot)
	}

	if cache.lowestSlot < 0 || int64(slot) < cache.lowestSlot {
		cache.lowestSlot = int64(slot)
	}

	return cacheBlock, true
}

// getBlockByRoot returns the cached block with the given root.
func (cache *blockCache) getBlockByRoot(root phase0.Root) *Block {
	cache.cacheMutex.RLock()
	defer cache.cacheMutex.RUnlock()

	return cache.rootMap[root]
}

// getBlockBySlot returns the cached blocks with the given slot.
func (cache *blockCache) getBlocksBySlot(slot phase0.Slot) []*Block {
	cache.cacheMutex.RLock()
	defer cache.cacheMutex.RUnlock()

	blocks := make([]*Block, len(cache.slotMap[slot]))
	if len(blocks) > 0 {
		copy(blocks, cache.slotMap[slot])
	}

	return blocks
}

// getBlocksByParentRoot returns a slice of blocks that have the given parent root.
func (cache *blockCache) getBlocksByParentRoot(parentRoot phase0.Root) []*Block {
	cache.cacheMutex.RLock()
	defer cache.cacheMutex.RUnlock()

	parentBlock := cache.rootMap[parentRoot]

	resBlocks := []*Block{}
	for slot, blocks := range cache.slotMap {
		if parentBlock != nil && slot <= parentBlock.Slot {
			continue
		}

		for _, block := range blocks {
			blockParentRoot := block.GetParentRoot()
			if blockParentRoot == nil {
				continue
			}

			if bytes.Equal((*blockParentRoot)[:], parentRoot[:]) {
				resBlocks = append(resBlocks, block)
			}
		}
	}

	return resBlocks
}

// getBlockByStateRoot returns the block with the given state root.
func (cache *blockCache) getBlockByStateRoot(stateRoot phase0.Root) *Block {
	cache.cacheMutex.RLock()
	defer cache.cacheMutex.RUnlock()

	for _, block := range cache.rootMap {
		blockHeader := block.GetHeader()
		if blockHeader == nil {
			continue
		}

		if bytes.Equal(blockHeader.Message.StateRoot[:], stateRoot[:]) {
			return block
		}
	}

	return nil
}

func (cache *blockCache) getBlocksByExecutionBlockHash(blockHash phase0.Hash32) []*Block {
	cache.cacheMutex.RLock()
	defer cache.cacheMutex.RUnlock()

	resBlocks := []*Block{}
	for _, block := range cache.rootMap {
		if block.blockIndex != nil {
			if bytes.Equal(block.blockIndex.ExecutionHash[:], blockHash[:]) {
				resBlocks = append(resBlocks, block)
			}
			continue
		}

		blockBody := block.GetBlock()
		if blockBody == nil {
			continue
		}

		executionHash, _ := blockBody.ExecutionBlockHash()
		if bytes.Equal(executionHash[:], blockHash[:]) {
			resBlocks = append(resBlocks, block)
		}
	}

	return resBlocks
}

func (cache *blockCache) getBlocksByExecutionBlockNumber(blockNumber uint64) []*Block {
	cache.cacheMutex.RLock()
	defer cache.cacheMutex.RUnlock()

	resBlocks := []*Block{}
	for _, block := range cache.rootMap {
		if block.blockIndex != nil {
			if block.blockIndex.ExecutionNumber == blockNumber {
				resBlocks = append(resBlocks, block)
				continue
			}
		}

		if block.block == nil {
			continue
		}
		blockBody := block.block

		executionNumber, err := blockBody.ExecutionBlockNumber()
		if err == nil && executionNumber == blockNumber {
			resBlocks = append(resBlocks, block)
		}
	}

	return resBlocks
}

// getPruningBlocks returns the blocks that can be pruned based on the given finalized slot.
func (cache *blockCache) getPruningBlocks(minInMemorySlot phase0.Slot) []*Block {
	cache.cacheMutex.RLock()
	defer cache.cacheMutex.RUnlock()

	blocks := []*Block{}
	for slot, slotBlocks := range cache.slotMap {
		if slot >= minInMemorySlot {
			continue
		}

		for _, block := range slotBlocks {
			if block.block == nil {
				continue
			}

			blocks = append(blocks, block)
		}
	}

	return blocks
}

// getForkBlocks returns a slice of blocks that belong to the specified forkId.
func (cache *blockCache) getForkBlocks(forkId ForkKey) []*Block {
	cache.cacheMutex.RLock()
	defer cache.cacheMutex.RUnlock()

	blocks := []*Block{}

	for _, slotBlocks := range cache.slotMap {
		for _, block := range slotBlocks {
			if block.forkId != forkId {
				continue
			}

			blocks = append(blocks, block)
		}
	}

	return blocks
}

// getLatestBlocks returns the latest blocks from the block cache, up to the specified limit.
// If a forkId is provided, only blocks with matching forkId will be returned.
func (cache *blockCache) getLatestBlocks(limit uint64, forkId *ForkKey) []*Block {
	cache.cacheMutex.RLock()
	defer cache.cacheMutex.RUnlock()

	blocks := []*Block{}

	slots := make([]phase0.Slot, 0, len(cache.slotMap))
	for slot := range cache.slotMap {
		slots = append(slots, slot)
	}
	sort.Slice(slots, func(i, j int) bool {
		return slots[i] > slots[j]
	})

	for _, slot := range slots {
		for _, block := range cache.slotMap[slot] {
			if block.block == nil && !block.isInFinalizedDb && !block.isInUnfinalizedDb {
				continue
			}
			if forkId != nil && block.forkId != *forkId {
				continue
			}

			blocks = append(blocks, block)
			if limit > 0 && uint64(len(blocks)) >= limit {
				return blocks
			}
		}
	}

	return blocks
}

// removeBlock removes the given block from the block cache.
func (cache *blockCache) removeBlock(block *Block) {
	cache.cacheMutex.Lock()
	defer cache.cacheMutex.Unlock()

	delete(cache.rootMap, block.Root)

	slotBlocks := cache.slotMap[block.Slot]
	if len(slotBlocks) == 1 && slotBlocks[0] == block {
		delete(cache.slotMap, block.Slot)
	} else if len(slotBlocks) > 1 {
		for i, slotBlock := range slotBlocks {
			if slotBlock == block {
				cache.slotMap[block.Slot] = append(slotBlocks[:i], slotBlocks[i+1:]...)
				break
			}
		}
	}
}

// getEpochBlocks returns the blocks that belong to the specified epoch.
func (cache *blockCache) getEpochBlocks(epoch phase0.Epoch) []*Block {
	cache.cacheMutex.RLock()
	defer cache.cacheMutex.RUnlock()

	blocks := []*Block{}

	for slot, slotBlocks := range cache.slotMap {
		if cache.indexer.consensusPool.GetChainState().EpochOfSlot(slot) != epoch {
			continue
		}

		blocks = append(blocks, slotBlocks...)
	}

	return blocks
}

// isCanonicalBlock checks if the block with the given blockRoot is a canonical block with respect to the block with the given head.
func (cache *blockCache) isCanonicalBlock(blockRoot phase0.Root, head phase0.Root) bool {
	res, _ := cache.getCanonicalDistance(blockRoot, head, 0)
	return res
}

// getCanonicalDistance returns the canonical distance between the block with the given blockRoot and the block with the given head.
// It returns a boolean indicating whether the block with blockRoot is a canonical block, and the distance between the two blocks.
func (cache *blockCache) getCanonicalDistance(blockRoot phase0.Root, head phase0.Root, maxDistance uint64) (bool, uint64) {
	block := cache.getBlockByRoot(blockRoot)
	if block == nil {
		return false, 0
	}

	canonicalBlock := cache.getBlockByRoot(head)
	if canonicalBlock == nil {
		return false, 0
	}

	var distance uint64 = 0
	if bytes.Equal(canonicalBlock.Root[:], blockRoot[:]) {
		return true, distance
	}

	for canonicalBlock != nil {
		if canonicalBlock.Slot < block.Slot {
			return false, 0
		}

		parentRoot := canonicalBlock.GetParentRoot()
		if parentRoot == nil {
			return false, 0
		}

		distance++
		if maxDistance > 0 && distance > maxDistance {
			return false, 0
		}

		if bytes.Equal(parentRoot[:], blockRoot[:]) {
			return true, distance
		}

		canonicalBlock = cache.getBlockByRoot(*parentRoot)
		if canonicalBlock == nil {
			return false, 0
		}
	}

	return false, 0
}

// getDependentBlock returns the dependent block of the given block based on the chain state.
func (cache *blockCache) getDependentBlock(chainState *consensus.ChainState, block *Block, client *Client) *Block {
	if block.dependentRoot != nil {
		dependentBlock := cache.getBlockByRoot(*block.dependentRoot)
		if dependentBlock == nil {
			blockHead := db.GetBlockHeadByRoot((*block.dependentRoot)[:])
			if blockHead != nil {
				dependentBlock = newBlock(cache.indexer.dynSsz, phase0.Root(blockHead.Root), phase0.Slot(blockHead.Slot))
				dependentBlock.isInFinalizedDb = true
				parentRootVal := phase0.Root(blockHead.ParentRoot)
				dependentBlock.parentRoot = &parentRootVal
			}
		}

		if dependentBlock == nil && client != nil {
			blockHead, _ := LoadBeaconHeader(client.getContext(), client, *block.dependentRoot)
			if blockHead != nil {
				dependentBlock = newBlock(cache.indexer.dynSsz, *block.dependentRoot, phase0.Slot(blockHead.Message.Slot))
				parentRootVal := phase0.Root(blockHead.Message.ParentRoot)
				dependentBlock.parentRoot = &parentRootVal
			}
		}

		return dependentBlock
	}

	if block.Slot == 0 {
		return block
	}

	parentRoot := block.GetParentRoot()
	blockEpoch := chainState.EpochOfSlot(block.Slot)

	for {
		if parentRoot == nil {
			break
		}

		parentBlock := cache.getBlockByRoot(*parentRoot)
		if parentBlock == nil {
			blockHead := db.GetBlockHeadByRoot((*parentRoot)[:])
			if blockHead != nil {
				parentBlock = newBlock(cache.indexer.dynSsz, phase0.Root(blockHead.Root), phase0.Slot(blockHead.Slot))
				parentBlock.isInFinalizedDb = true
				parentRootVal := phase0.Root(blockHead.ParentRoot)
				parentBlock.parentRoot = &parentRootVal
			}
		}

		if parentBlock == nil && client != nil {
			blockHead, _ := LoadBeaconHeader(client.getContext(), client, *parentRoot)
			client = nil // only load one header, that's probably the dependent root block (last block of previous epoch)
			if blockHead != nil {
				parentBlock = newBlock(cache.indexer.dynSsz, *parentRoot, phase0.Slot(blockHead.Message.Slot))
				parentRootVal := phase0.Root(blockHead.Message.ParentRoot)
				parentBlock.parentRoot = &parentRootVal
			}
		}

		if parentBlock == nil {
			break
		}

		if chainState.EpochOfSlot(parentBlock.Slot) < blockEpoch || parentBlock.Slot == 0 {
			block.dependentRoot = &parentBlock.Root
			return parentBlock
		}

		parentRoot = parentBlock.GetParentRoot()
	}

	return nil
}
