package spvwallet

import (
	"errors"
	"fmt"
	"sync"

	"github.com/elastos/Elastos.ELA.SPV/bloom"
	. "github.com/elastos/Elastos.ELA.SPV/common"
	tx "github.com/elastos/Elastos.ELA.SPV/core/transaction"
	"github.com/elastos/Elastos.ELA.SPV/p2p"
	"github.com/elastos/Elastos.ELA.SPV/sdk"
)

type RequestQueueHandler interface {
	OnSendRequest(peer *p2p.Peer, reqType uint8, hash Uint256)
	OnRequestError(error)
	OnRequestFinished(*FinishedReqPool)
}

type RequestQueue struct {
	size             int
	peer             *p2p.Peer
	hashesQueue      chan Uint256
	blocksQueue      chan Uint256
	blockTxsQueue    chan Uint256
	blockReqsLock    *sync.Mutex
	blockRequests    map[Uint256]*Request
	blockTxsReqsLock *sync.Mutex
	blockTxsRequests map[Uint256]*BlockTxsRequest
	blockTxs         map[Uint256]Uint256
	finished         *FinishedReqPool
	handler          RequestQueueHandler
}

func NewRequestQueue(size int, handler RequestQueueHandler) *RequestQueue {
	queue := new(RequestQueue)
	queue.size = size
	queue.hashesQueue = make(chan Uint256, size*2)
	queue.blocksQueue = make(chan Uint256, size)
	queue.blockTxsQueue = make(chan Uint256, size)
	queue.blockReqsLock = new(sync.Mutex)
	queue.blockRequests = make(map[Uint256]*Request)
	queue.blockTxsReqsLock = new(sync.Mutex)
	queue.blockTxsRequests = make(map[Uint256]*BlockTxsRequest)
	queue.blockTxs = make(map[Uint256]Uint256)
	queue.finished = &FinishedReqPool{
		requests: make(map[Uint256]*BlockTxsRequest),
	}
	queue.handler = handler

	go queue.start()
	return queue
}

func (queue *RequestQueue) start() {
	for hash := range queue.hashesQueue {
		queue.StartBlockRequest(queue.peer, hash)
	}
}

// This method will block when request queue is filled
func (queue *RequestQueue) PushHashes(peer *p2p.Peer, hashes []Uint256) {
	queue.peer = peer
	for _, hash := range hashes {
		queue.hashesQueue <- hash
	}
}

func (queue *RequestQueue) StartBlockRequest(peer *p2p.Peer, hash Uint256) {
	queue.blockReqsLock.Lock()
	if _, ok := queue.blockRequests[hash]; ok {
		queue.blockReqsLock.Unlock()
		return
	}
	queue.blockReqsLock.Unlock()

	// Block the method when queue is filled
	queue.blocksQueue <- hash

	queue.blockReqsLock.Lock()
	// Create a new block request
	blockRequest := &Request{
		peer:    peer,
		hash:    hash,
		reqType: sdk.BLOCK,
		handler: queue,
	}

	// Add to request queue
	queue.blockRequests[hash] = blockRequest

	// Start block request
	blockRequest.Start()
	queue.blockReqsLock.Unlock()
}

func (queue *RequestQueue) StartBlockTxsRequest(
	peer *p2p.Peer, block *bloom.MerkleBlock, txIds []*Uint256) {

	queue.blockTxsReqsLock.Lock()
	blockHash := *block.BlockHeader.Hash()
	if _, ok := queue.blockTxsRequests[blockHash]; ok {
		return
	}
	queue.blockTxsReqsLock.Unlock()

	queue.blockTxsQueue <- blockHash

	queue.blockTxsReqsLock.Lock()
	txRequestQueue := make(map[Uint256]*Request)
	for _, txId := range txIds {
		// Mark txId related block
		queue.blockTxs[*txId] = blockHash
		// Start a tx request
		txRequest := &Request{
			peer:    peer,
			hash:    *txId,
			reqType: sdk.TRANSACTION,
			handler: queue,
		}
		txRequestQueue[*txId] = txRequest
		txRequest.Start()
	}

	blockTxsRequest := &BlockTxsRequest{
		block:          *block,
		txRequestQueue: txRequestQueue,
	}

	queue.blockTxsRequests[blockHash] = blockTxsRequest
	queue.blockTxsReqsLock.Unlock()
}

func (queue *RequestQueue) IsRunning() bool {
	return len(queue.hashesQueue) > 0 || len(queue.blocksQueue) > 0 || len(queue.blockTxsQueue) > 0
}

func (queue *RequestQueue) OnSendRequest(peer *p2p.Peer, reqType uint8, hash Uint256) {
	queue.handler.OnSendRequest(peer, reqType, hash)
}

func (queue *RequestQueue) OnRequestTimeout(hash Uint256) {
	queue.handler.OnRequestError(errors.New("Request timeout with hash: " + hash.String()))
}

func (queue *RequestQueue) OnBlockReceived(block *bloom.MerkleBlock, txIds []*Uint256) error {
	queue.blockReqsLock.Lock()
	defer queue.blockReqsLock.Unlock()

	blockHash := *block.BlockHeader.Hash()
	// Check if received block is in the request queue
	var ok bool
	var request *Request
	if request, ok = queue.blockRequests[blockHash]; !ok {
		fmt.Println("Unknown block received: ", blockHash.String())
		return nil
	}

	// Remove from block request list
	request.Finish()
	delete(queue.blockRequests, blockHash)
	<-queue.blocksQueue

	// No block transactions to request, notify request finished.
	if len(txIds) == 0 {
		// Notify request finished
		queue.OnRequestFinished(&BlockTxsRequest{block: *block})
		return nil
	}

	// Request block transactions
	queue.StartBlockTxsRequest(request.peer, block, txIds)

	return nil
}

func (queue *RequestQueue) OnTxReceived(tx *tx.Transaction) error {
	queue.blockTxsReqsLock.Lock()
	defer queue.blockTxsReqsLock.Unlock()

	txId := *tx.Hash()
	var ok bool
	var blockHash Uint256
	if blockHash, ok = queue.blockTxs[txId]; !ok {
		fmt.Println("Unknown transaction received: ", txId.String())
		return nil
	}

	// Remove from map
	delete(queue.blockTxs, txId)

	var blockTxsRequest *BlockTxsRequest
	if blockTxsRequest, ok = queue.blockTxsRequests[blockHash]; !ok {
		return errors.New("Request not exist with id: " + blockHash.String())
	}

	finished, err := blockTxsRequest.OnTxReceived(tx)
	if err != nil {
		return err
	}

	if finished {
		delete(queue.blockTxsRequests, blockHash)
		<-queue.blockTxsQueue
		queue.OnRequestFinished(blockTxsRequest)
	}
	return nil
}

func (queue *RequestQueue) OnRequestFinished(request *BlockTxsRequest) {
	// Add to finished pool
	queue.finished.Add(request)

	// Callback finish event and pass the finished requests pool
	queue.handler.OnRequestFinished(queue.finished)
}

func (queue *RequestQueue) Clear() {
	// Clear hashes chan
	for len(queue.hashesQueue) > 0 {
		<-queue.hashesQueue
	}
	// Clear block requests chan
	for len(queue.blocksQueue) > 0 {
		<-queue.blocksQueue
	}
	// Clear block txs requests chan
	for len(queue.blockTxsQueue) > 0 {
		<-queue.blockTxsQueue
	}

	// Clear block requests
	queue.blockReqsLock.Lock()
	for hash, request := range queue.blockRequests {
		request.Finish()
		delete(queue.blockRequests, hash)
	}
	queue.blockReqsLock.Unlock()

	// Clear block txs requests
	queue.blockTxsReqsLock.Lock()
	for hash, request := range queue.blockTxsRequests {
		request.Finish()
		delete(queue.blockTxsRequests, hash)
	}
	queue.blockTxsReqsLock.Unlock()
}