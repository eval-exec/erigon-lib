/*
   Copyright 2021 Erigon contributors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package txpool

import (
	"bytes"
	"container/heap"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/VictoriaMetrics/metrics"
	"github.com/go-stack/stack"
	"github.com/google/btree"
	"github.com/hashicorp/golang-lru/simplelru"
	"github.com/holiman/uint256"
	"github.com/ledgerwatch/erigon-lib/chain"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/fixedgas"
	"github.com/ledgerwatch/erigon-lib/gointerfaces"
	"github.com/ledgerwatch/erigon-lib/gointerfaces/grpcutil"
	"github.com/ledgerwatch/erigon-lib/gointerfaces/remote"
	proto_txpool "github.com/ledgerwatch/erigon-lib/gointerfaces/txpool"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/kvcache"
	"github.com/ledgerwatch/erigon-lib/kv/mdbx"
	"github.com/ledgerwatch/log/v3"
	"go.uber.org/atomic"
)

var (
	processBatchTxsTimer    = metrics.NewSummary(`pool_process_remote_txs`)
	addRemoteTxsTimer       = metrics.NewSummary(`pool_add_remote_txs`)
	newBlockTimer           = metrics.NewSummary(`pool_new_block`)
	writeToDbTimer          = metrics.NewSummary(`pool_write_to_db`)
	propagateToNewPeerTimer = metrics.NewSummary(`pool_propagate_to_new_peer`)
	propagateNewTxsTimer    = metrics.NewSummary(`pool_propagate_new_txs`)
	writeToDbBytesCounter   = metrics.GetOrCreateCounter(`pool_write_to_db_bytes`)
)

const ASSERT = false

type Config struct {
	DBDir                 string
	SyncToNewPeersEvery   time.Duration
	ProcessRemoteTxsEvery time.Duration
	CommitEvery           time.Duration
	LogEvery              time.Duration

	PendingSubPoolLimit int
	BaseFeeSubPoolLimit int
	QueuedSubPoolLimit  int

	MinFeeCap     uint64
	AccountSlots  uint64   // Number of executable transaction slots guaranteed per account
	PriceBump     uint64   // Price bump percentage to replace an already existing transaction
	TracedSenders []string // List of senders for which tx pool should print out debugging info
}

var DefaultConfig = Config{
	SyncToNewPeersEvery:   2 * time.Minute,
	ProcessRemoteTxsEvery: 100 * time.Millisecond,
	CommitEvery:           15 * time.Second,
	LogEvery:              30 * time.Second,

	PendingSubPoolLimit: 10_000,
	BaseFeeSubPoolLimit: 10_000,
	QueuedSubPoolLimit:  10_000,

	MinFeeCap:    1,
	AccountSlots: 16, //TODO: to choose right value (16 to be compat with Geth)
	PriceBump:    10, // Price bump percentage to replace an already existing transaction
}

// Pool is interface for the transaction pool
// This interface exists for the convinience of testing, and not yet because
// there are multiple implementations
type Pool interface {
	ValidateSerializedTxn(serializedTxn []byte) error

	// Handle 3 main events - new remote txs from p2p, new local txs from RPC, new blocks from execution layer
	AddRemoteTxs(ctx context.Context, newTxs TxSlots)
	AddLocalTxs(ctx context.Context, newTxs TxSlots) ([]DiscardReason, error)
	OnNewBlock(ctx context.Context, stateChanges *remote.StateChangeBatch, unwindTxs, minedTxs TxSlots, tx kv.Tx) error

	// IdHashKnown check whether transaction with given Id hash is known to the pool
	IdHashKnown(tx kv.Tx, hash []byte) (bool, error)
	Started() bool
	GetRlp(tx kv.Tx, hash []byte) ([]byte, error)

	AddNewGoodPeer(peerID PeerID)
}

var _ Pool = (*TxPool)(nil) // compile-time interface check

// SubPoolMarker ordered bitset responsible to sort transactions by sub-pools. Bits meaning:
// 1. Minimum fee requirement. Set to 1 if feeCap of the transaction is no less than in-protocol parameter of minimal base fee. Set to 0 if feeCap is less than minimum base fee, which means this transaction will never be included into this particular chain.
// 2. Absence of nonce gaps. Set to 1 for transactions whose nonce is N, state nonce for the sender is M, and there are transactions for all nonces between M and N from the same sender. Set to 0 is the transaction's nonce is divided from the state nonce by one or more nonce gaps.
// 3. Sufficient balance for gas. Set to 1 if the balance of sender's account in the state is B, nonce of the sender in the state is M, nonce of the transaction is N, and the sum of feeCap x gasLimit + transferred_value of all transactions from this sender with nonces N+1 ... M is no more than B. Set to 0 otherwise. In other words, this bit is set if there is currently a guarantee that the transaction and all its required prior transactions will be able to pay for gas.
// 4. Dynamic fee requirement. Set to 1 if feeCap of the transaction is no less than baseFee of the currently pending block. Set to 0 otherwise.
// 5. Local transaction. Set to 1 if transaction is local.
type SubPoolMarker uint8

const (
	EnoughFeeCapProtocol = 0b100000
	NoNonceGaps          = 0b010000
	EnoughBalance        = 0b001000
	NotTooMuchGas        = 0b000100
	EnoughFeeCapBlock    = 0b000010
	IsLocal              = 0b000001

	BaseFeePoolBits = EnoughFeeCapProtocol + NoNonceGaps + EnoughBalance + NotTooMuchGas
	QueuedPoolBits  = EnoughFeeCapProtocol
)

type DiscardReason uint8

const (
	NotSet              DiscardReason = 0 // analog of "nil-value", means it will be set in future
	Success             DiscardReason = 1
	AlreadyKnown        DiscardReason = 2
	Mined               DiscardReason = 3
	ReplacedByHigherTip DiscardReason = 4
	UnderPriced         DiscardReason = 5
	ReplaceUnderpriced  DiscardReason = 6 // if a transaction is attempted to be replaced with a different one without the required price bump.
	FeeTooLow           DiscardReason = 7
	OversizedData       DiscardReason = 8
	InvalidSender       DiscardReason = 9
	NegativeValue       DiscardReason = 10 // ensure no one is able to specify a transaction with a negative value.
	Spammer             DiscardReason = 11
	PendingPoolOverflow DiscardReason = 12
	BaseFeePoolOverflow DiscardReason = 13
	QueuedPoolOverflow  DiscardReason = 14
	GasUintOverflow     DiscardReason = 15
	IntrinsicGas        DiscardReason = 16
	RLPTooLong          DiscardReason = 17
	NonceTooLow         DiscardReason = 18
	InsufficientFunds   DiscardReason = 19
	NotReplaced         DiscardReason = 20 // There was an existing transaction with the same sender and nonce, not enough price bump to replace
	DuplicateHash       DiscardReason = 21 // There was an existing transaction with the same hash
)

func (r DiscardReason) String() string {
	switch r {
	case NotSet:
		return "not set"
	case Success:
		return "success"
	case AlreadyKnown:
		return "already known"
	case Mined:
		return "mined"
	case ReplacedByHigherTip:
		return "replaced by transaction with higher tip"
	case UnderPriced:
		return "underpriced"
	case ReplaceUnderpriced:
		return "replacement transaction underpriced"
	case FeeTooLow:
		return "fee too low"
	case OversizedData:
		return "oversized data"
	case InvalidSender:
		return "invalid sender"
	case NegativeValue:
		return "negative value"
	case PendingPoolOverflow:
		return "pending sub-pool is full"
	case BaseFeePoolOverflow:
		return "baseFee sub-pool is full"
	case QueuedPoolOverflow:
		return "queued sub-pool is full"
	case GasUintOverflow:
		return "GasUintOverflow"
	case IntrinsicGas:
		return "IntrinsicGas"
	case RLPTooLong:
		return "RLPTooLong"
	case NonceTooLow:
		return "nonce too low"
	case InsufficientFunds:
		return "insufficient funds"
	case NotReplaced:
		return "could not replace existing tx"
	case DuplicateHash:
		return "existing tx with same hash"
	default:
		panic(fmt.Sprintf("discard reason: %d", r))
	}
}

// metaTx holds transaction and some metadata
type metaTx struct {
	Tx                        *TxSlot
	subPool                   SubPoolMarker
	nonceDistance             uint64 // how far their nonces are from the state's nonce for the sender
	cumulativeBalanceDistance uint64 // how far their cumulativeRequiredBalance are from the state's balance for the sender
	minFeeCap                 uint64
	minTip                    uint64
	bestIndex                 int
	worstIndex                int
	currentSubPool            SubPoolType
	timestamp                 uint64 // when it was added to pool
}

func newMetaTx(slot *TxSlot, isLocal bool, timestmap uint64) *metaTx {
	mt := &metaTx{Tx: slot, worstIndex: -1, bestIndex: -1, timestamp: timestmap}
	if isLocal {
		mt.subPool = IsLocal
	}
	return mt
}

type SubPoolType uint8

const PendingSubPool SubPoolType = 1
const BaseFeeSubPool SubPoolType = 2
const QueuedSubPool SubPoolType = 3

func (sp SubPoolType) String() string {
	switch sp {
	case PendingSubPool:
		return "Pending"
	case BaseFeeSubPool:
		return "BaseFee"
	case QueuedSubPool:
		return "Queued"
	}
	return fmt.Sprintf("Unknown:%d", sp)
}

// sender - immutable structure which stores only nonce and balance of account
type sender struct {
	balance uint256.Int
	nonce   uint64
}

func newSender(nonce uint64, balance uint256.Int) *sender {
	return &sender{nonce: nonce, balance: balance}
}

var emptySender = newSender(0, *uint256.NewInt(0))

type sortByNonce struct{ *metaTx }

func (i sortByNonce) Less(than btree.Item) bool {
	if i.metaTx.Tx.senderID != than.(sortByNonce).metaTx.Tx.senderID {
		return i.metaTx.Tx.senderID < than.(sortByNonce).metaTx.Tx.senderID
	}
	return i.metaTx.Tx.nonce < than.(sortByNonce).metaTx.Tx.nonce
}

func calcProtocolBaseFee(baseFee uint64) uint64 {
	return 7
}

// TxPool - holds all pool-related data structures and lock-based tiny methods
// most of logic implemented by pure tests-friendly functions
//
// txpool doesn't start any goroutines - "leave concurrency to user" design
// txpool has no DB or TX fields - "leave db transactions management to user" design
// txpool has _chainDB field - but it must maximize local state cache hit-rate - and perform minimum _chainDB transactions
//
// It preserve TxSlot objects immutable
type TxPool struct {
	lock *sync.RWMutex

	started        atomic.Bool
	lastSeenBlock  atomic.Uint64
	pendingBaseFee atomic.Uint64
	blockGasLimit  atomic.Uint64

	// batch processing of remote transactions
	// handling works fast without batching, but batching allow:
	//   - reduce amount of _chainDB transactions
	//   - batch notifications about new txs (reduce P2P spam to other nodes about txs propagation)
	//   - and as a result reducing pool.RWLock contention
	unprocessedRemoteTxs    *TxSlots
	unprocessedRemoteByHash map[string]int // to reject duplicates

	byHash            map[string]*metaTx // tx_hash => tx : only not committed to db yet records
	discardReasonsLRU *simplelru.LRU     // tx_hash => discard_reason : non-persisted
	pending           *PendingPool
	baseFee, queued   *SubPool
	isLocalLRU        *simplelru.LRU    // tx_hash => is_local : to restore isLocal flag of unwinded transactions
	newPendingTxs     chan Hashes       // notifications about new txs in Pending sub-pool
	deletedTxs        []*metaTx         // list of discarded txs since last db commit
	all               *BySenderAndNonce // senderID => (sorted map of tx nonce => *metaTx)
	promoted          Hashes            // pre-allocated temporary buffer to write promoted to pending pool txn hashes
	_chainDB          kv.RoDB           // remote db - use it wisely
	_stateCache       kvcache.Cache
	cfg               Config

	recentlyConnectedPeers *recentlyConnectedPeers // all txs will be propagated to this peers eventually, and clear list
	senders                *sendersBatch

	chainID uint256.Int
}

func New(newTxs chan Hashes, coreDB kv.RoDB, cfg Config, cache kvcache.Cache, chainID uint256.Int) (*TxPool, error) {
	localsHistory, err := simplelru.NewLRU(10_000, nil)
	if err != nil {
		return nil, err
	}
	discardHistory, err := simplelru.NewLRU(10_000, nil)
	if err != nil {
		return nil, err
	}

	byNonce := &BySenderAndNonce{
		tree:             btree.New(32),
		search:           sortByNonce{&metaTx{Tx: &TxSlot{}}},
		senderIDTxnCount: map[uint64]int{},
	}
	tracedSenders := make(map[string]struct{})
	for _, sender := range cfg.TracedSenders {
		tracedSenders[sender] = struct{}{}
	}
	return &TxPool{
		lock:                    &sync.RWMutex{},
		byHash:                  map[string]*metaTx{},
		isLocalLRU:              localsHistory,
		discardReasonsLRU:       discardHistory,
		all:                     byNonce,
		recentlyConnectedPeers:  &recentlyConnectedPeers{},
		pending:                 NewPendingSubPool(PendingSubPool, cfg.PendingSubPoolLimit),
		baseFee:                 NewSubPool(BaseFeeSubPool, cfg.BaseFeeSubPoolLimit),
		queued:                  NewSubPool(QueuedSubPool, cfg.QueuedSubPoolLimit),
		newPendingTxs:           newTxs,
		_stateCache:             cache,
		senders:                 newSendersCache(tracedSenders),
		_chainDB:                coreDB,
		cfg:                     cfg,
		chainID:                 chainID,
		unprocessedRemoteTxs:    &TxSlots{},
		unprocessedRemoteByHash: map[string]int{},
		promoted:                make(Hashes, 0, 32*1024),
	}, nil
}

func (p *TxPool) OnNewBlock(ctx context.Context, stateChanges *remote.StateChangeBatch, unwindTxs, minedTxs TxSlots, tx kv.Tx) error {
	defer newBlockTimer.UpdateDuration(time.Now())
	//t := time.Now()

	cache := p.cache()
	cache.OnNewBlock(stateChanges)
	coreTx, err := p.coreDB().BeginRo(ctx)
	if err != nil {
		return err
	}
	defer coreTx.Rollback()

	p.lock.Lock()
	defer p.lock.Unlock()

	p.lastSeenBlock.Store(stateChanges.ChangeBatch[len(stateChanges.ChangeBatch)-1].BlockHeight)
	if !p.started.Load() {
		if err := p.fromDB(ctx, tx, coreTx); err != nil {
			return fmt.Errorf("loading txs from DB: %w", err)
		}
	}

	cacheView, err := cache.View(ctx, coreTx)
	if err != nil {
		return err
	}
	if ASSERT {
		if _, err := kvcache.AssertCheckValues(ctx, coreTx, cache); err != nil {
			log.Error("AssertCheckValues", "err", err, "stack", stack.Trace().String())
		}
	}

	if err := minedTxs.Valid(); err != nil {
		return err
	}
	baseFee := stateChanges.PendingBlockBaseFee

	pendingBaseFee, baseFeeChanged := p.setBaseFee(baseFee)
	// Update pendingBase for all pool queues and slices
	if baseFeeChanged {
		p.pending.best.pendingBaseFee = pendingBaseFee
		p.pending.worst.pendingBaseFee = pendingBaseFee
		p.baseFee.best.pendingBastFee = pendingBaseFee
		p.baseFee.worst.pendingBaseFee = pendingBaseFee
		p.queued.best.pendingBastFee = pendingBaseFee
		p.queued.worst.pendingBaseFee = pendingBaseFee
	}

	p.blockGasLimit.Store(stateChanges.BlockGasLimit)
	if err := p.senders.onNewBlock(stateChanges, unwindTxs, minedTxs); err != nil {
		return err
	}
	_, unwindTxs, err = p.validateTxs(&unwindTxs, cacheView)
	if err != nil {
		return err
	}

	if ASSERT {
		for _, txn := range unwindTxs.txs {
			if txn.senderID == 0 {
				panic(fmt.Errorf("onNewBlock.unwindTxs: senderID can't be zero"))
			}
		}
		for _, txn := range minedTxs.txs {
			if txn.senderID == 0 {
				panic(fmt.Errorf("onNewBlock.minedTxs: senderID can't be zero"))
			}
		}
	}

	if err := removeMined(p.all, minedTxs.txs, p.pending, p.baseFee, p.queued, p.discardLocked); err != nil {
		return err
	}

	//log.Debug("[txpool] new block", "unwinded", len(unwindTxs.txs), "mined", len(minedTxs.txs), "baseFee", baseFee, "blockHeight", blockHeight)

	p.pending.resetAddedHashes()
	p.baseFee.resetAddedHashes()
	if err := addTxsOnNewBlock(p.lastSeenBlock.Load(), cacheView, stateChanges, p.senders, unwindTxs,
		pendingBaseFee, stateChanges.BlockGasLimit,
		p.pending, p.baseFee, p.queued, p.all, p.byHash, p.addLocked, p.discardLocked); err != nil {
		return err
	}
	p.pending.EnforceWorstInvariants()
	p.baseFee.EnforceInvariants()
	p.queued.EnforceInvariants()
	promote(p.pending, p.baseFee, p.queued, pendingBaseFee, p.discardLocked)
	p.pending.EnforceBestInvariants()
	p.promoted = p.pending.appendAddedHashes(p.promoted[:0])
	p.promoted = p.baseFee.appendAddedHashes(p.promoted)

	if p.started.CAS(false, true) {
		log.Info("[txpool] Started")
	}

	if p.promoted.Len() > 0 {
		select {
		case p.newPendingTxs <- common.Copy(p.promoted):
		default:
		}
	}

	//log.Info("[txpool] new block", "number", p.lastSeenBlock.Load(), "pendngBaseFee", pendingBaseFee, "in", time.Since(t))
	return nil
}

func (p *TxPool) processRemoteTxs(ctx context.Context) error {
	if !p.started.Load() {
		return fmt.Errorf("txpool not started yet")
	}

	cache := p.cache()
	defer processBatchTxsTimer.UpdateDuration(time.Now())
	coreTx, err := p.coreDB().BeginRo(ctx)
	if err != nil {
		return err
	}
	defer coreTx.Rollback()
	cacheView, err := cache.View(ctx, coreTx)
	if err != nil {
		return err
	}

	//t := time.Now()
	p.lock.Lock()
	defer p.lock.Unlock()

	l := len(p.unprocessedRemoteTxs.txs)
	if l == 0 {
		return nil
	}

	err = p.senders.registerNewSenders(p.unprocessedRemoteTxs)
	if err != nil {
		return err
	}

	_, newTxs, err := p.validateTxs(p.unprocessedRemoteTxs, cacheView)
	if err != nil {
		return err
	}

	p.pending.resetAddedHashes()
	p.baseFee.resetAddedHashes()
	if _, err := addTxs(p.lastSeenBlock.Load(), cacheView, p.senders, newTxs,
		p.pendingBaseFee.Load(), p.blockGasLimit.Load(), p.pending, p.baseFee, p.queued, p.all, p.byHash, p.addLocked, p.discardLocked); err != nil {
		return err
	}
	p.promoted = p.pending.appendAddedHashes(p.promoted[:0])
	p.promoted = p.baseFee.appendAddedHashes(p.promoted)

	if p.promoted.Len() > 0 {
		select {
		case <-ctx.Done():
			return nil
		case p.newPendingTxs <- common.Copy(p.promoted):
		default:
		}
	}

	p.unprocessedRemoteTxs.Resize(0)
	p.unprocessedRemoteByHash = map[string]int{}

	//log.Info("[txpool] on new txs", "amount", len(newPendingTxs.txs), "in", time.Since(t))
	return nil
}
func (p *TxPool) getRlpLocked(tx kv.Tx, hash []byte) (rlpTxn []byte, sender []byte, isLocal bool, err error) {
	txn, ok := p.byHash[string(hash)]
	if ok && txn.Tx.rlp != nil {
		return txn.Tx.rlp, p.senders.senderID2Addr[txn.Tx.senderID], txn.subPool&IsLocal > 0, nil
	}
	v, err := tx.GetOne(kv.PoolTransaction, hash)
	if err != nil {
		return nil, nil, false, err
	}
	if v == nil {
		return nil, nil, false, nil
	}
	return v[20:], v[:20], txn != nil && txn.subPool&IsLocal > 0, nil
}
func (p *TxPool) GetRlp(tx kv.Tx, hash []byte) ([]byte, error) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	rlpTx, _, _, err := p.getRlpLocked(tx, hash)
	return common.Copy(rlpTx), err
}
func (p *TxPool) AppendLocalHashes(buf []byte) []byte {
	p.lock.RLock()
	defer p.lock.RUnlock()
	for hash, txn := range p.byHash {
		if txn.subPool&IsLocal == 0 {
			continue
		}
		buf = append(buf, hash...)
	}
	return buf
}
func (p *TxPool) AppendRemoteHashes(buf []byte) []byte {
	p.lock.RLock()
	defer p.lock.RUnlock()

	for hash, txn := range p.byHash {
		if txn.subPool&IsLocal != 0 {
			continue
		}
		buf = append(buf, hash...)
	}
	for hash := range p.unprocessedRemoteByHash {
		buf = append(buf, hash...)
	}
	return buf
}
func (p *TxPool) AppendAllHashes(buf []byte) []byte {
	buf = p.AppendLocalHashes(buf)
	buf = p.AppendRemoteHashes(buf)
	return buf
}
func (p *TxPool) IdHashKnown(tx kv.Tx, hash []byte) (bool, error) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	if _, ok := p.discardReasonsLRU.Get(string(hash)); ok {
		return true, nil
	}
	if _, ok := p.unprocessedRemoteByHash[string(hash)]; ok {
		return true, nil
	}
	if _, ok := p.byHash[string(hash)]; ok {
		return true, nil
	}
	return tx.Has(kv.PoolTransaction, hash)
}
func (p *TxPool) IsLocal(idHash []byte) bool {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.isLocalLRU.Contains(string(idHash))
}
func (p *TxPool) AddNewGoodPeer(peerID PeerID) { p.recentlyConnectedPeers.AddPeer(peerID) }
func (p *TxPool) Started() bool                { return p.started.Load() }

// Best - returns top `n` elements of pending queue
// id doesn't perform full copy of txs, hovewer underlying elements are immutable
func (p *TxPool) Best(n uint16, txs *TxsRlp, tx kv.Tx) error {
	p.lock.RLock()
	defer p.lock.RUnlock()

	txs.Resize(uint(min(uint64(n), uint64(len(p.pending.best.ms)))))

	best := p.pending.best
	for i, j := 0, 0; j < int(n) && i < len(best.ms); i++ {
		if best.ms[i].Tx.gas >= p.blockGasLimit.Load() {
			// Skip transactions with very large gas limit
			continue
		}
		rlpTx, sender, isLocal, err := p.getRlpLocked(tx, best.ms[i].Tx.IdHash[:])
		if err != nil {
			return err
		}
		if len(rlpTx) == 0 {
			continue
		}
		txs.Txs[j] = rlpTx
		copy(txs.Senders.At(j), sender)
		txs.IsLocal[j] = isLocal
		j++
	}
	return nil
}

func (p *TxPool) CountContent() (int, int, int) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.pending.Len(), p.baseFee.Len(), p.queued.Len()
}
func (p *TxPool) AddRemoteTxs(_ context.Context, newTxs TxSlots) {
	defer addRemoteTxsTimer.UpdateDuration(time.Now())
	p.lock.Lock()
	defer p.lock.Unlock()
	for i, txn := range newTxs.txs {
		_, ok := p.unprocessedRemoteByHash[string(txn.IdHash[:])]
		if ok {
			continue
		}
		p.unprocessedRemoteTxs.Append(txn, newTxs.senders.At(i), false)
	}
}

func (p *TxPool) validateTx(txn *TxSlot, isLocal bool, stateCache kvcache.CacheView) DiscardReason {
	// Drop non-local transactions under our own minimal accepted gas price or tip
	if !isLocal && txn.feeCap < p.cfg.MinFeeCap {
		if txn.traced {
			log.Info(fmt.Sprintf("TX TRACING: validateTx underpriced idHash=%x local=%t, feeCap=%d, cfg.MinFeeCap=%d", txn.IdHash, isLocal, txn.feeCap, p.cfg.MinFeeCap))
		}
		return UnderPriced
	}
	gas, reason := CalcIntrinsicGas(uint64(txn.dataLen), uint64(txn.dataNonZeroLen), nil, txn.creation, true, true)
	if txn.traced {
		log.Info(fmt.Sprintf("TX TRACING: validateTx intrinsic gas idHash=%x gas=%d", txn.IdHash, gas))
	}
	if reason != Success {
		if txn.traced {
			log.Info(fmt.Sprintf("TX TRACING: validateTx intrinsic gas calculated failed idHash=%x reason=%s", txn.IdHash, reason))
		}
		return reason
	}
	if gas > txn.gas {
		if txn.traced {
			log.Info(fmt.Sprintf("TX TRACING: validateTx intrinsic gas > txn.gas idHash=%x gas=%d, txn.gas=%d", txn.IdHash, gas, txn.gas))
		}
		return IntrinsicGas
	}
	if uint64(p.all.count(txn.senderID)) > p.cfg.AccountSlots {
		if txn.traced {
			log.Info(fmt.Sprintf("TX TRACING: validateTx marked as spamming idHash=%x slots=%d, limit=%d", txn.IdHash, p.all.count(txn.senderID), p.cfg.AccountSlots))
		}
		return Spammer
	}

	// check nonce and balance
	senderNonce, senderBalance, _ := p.senders.info(stateCache, txn.senderID)
	if senderNonce > txn.nonce {
		if txn.traced {
			log.Info(fmt.Sprintf("TX TRACING: validateTx nonce too low idHash=%x nonce in state=%d, txn.nonce=%d", txn.IdHash, senderNonce, txn.nonce))
		}
		return NonceTooLow
	}
	// Transactor should have enough funds to cover the costs
	total := uint256.NewInt(txn.gas)
	total.Mul(total, uint256.NewInt(txn.tip))
	total.Add(total, &txn.value)
	if senderBalance.Cmp(total) < 0 {
		if txn.traced {
			log.Info(fmt.Sprintf("TX TRACING: validateTx insufficient funds idHash=%x balance in state=%d, txn.gas*txn.tip=%d", txn.IdHash, senderBalance, total))
		}
		return InsufficientFunds
	}
	return Success
}

func (p *TxPool) ValidateSerializedTxn(serializedTxn []byte) error {
	const (
		// txSlotSize is used to calculate how many data slots a single transaction
		// takes up based on its size. The slots are used as DoS protection, ensuring
		// that validating a new transaction remains a constant operation (in reality
		// O(maxslots), where max slots are 4 currently).
		txSlotSize = 32 * 1024

		// txMaxSize is the maximum size a single transaction can have. This field has
		// non-trivial consequences: larger transactions are significantly harder and
		// more expensive to propagate; larger transactions also take more resources
		// to validate whether they fit into the pool or not.
		txMaxSize = 4 * txSlotSize // 128KB
	)
	if len(serializedTxn) > txMaxSize {
		return fmt.Errorf(RLPTooLong.String())
	}
	return nil
}
func (p *TxPool) validateTxs(txs *TxSlots, stateCache kvcache.CacheView) (reasons []DiscardReason, goodTxs TxSlots, err error) {
	// reasons is pre-sized for direct indexing, with the default zero
	// value DiscardReason of NotSet
	reasons = make([]DiscardReason, len(txs.txs))

	if err := txs.Valid(); err != nil {
		return reasons, goodTxs, err
	}

	goodCount := 0
	for i, txn := range txs.txs {
		reason := p.validateTx(txn, txs.isLocal[i], stateCache)
		if reason == Success {
			goodCount++
			// Success here means no DiscardReason yet, so leave it NotSet
			continue
		}
		if reason == Spammer {
			p.punishSpammer(txn.senderID)
		}
		reasons[i] = reason
	}

	goodTxs.Resize(uint(goodCount))

	j := 0
	for i, txn := range txs.txs {
		if reasons[i] == NotSet {
			goodTxs.txs[j] = txn
			goodTxs.isLocal[j] = txs.isLocal[i]
			copy(goodTxs.senders.At(j), txs.senders.At(i))
			j++
		}
	}
	return reasons, goodTxs, nil
}

// punishSpammer by drop half of it's transactions with high nonce
func (p *TxPool) punishSpammer(spammer uint64) {
	count := p.all.count(spammer) / 2
	if count > 0 {
		txsToDelete := make([]*metaTx, 0, count)
		p.all.descend(spammer, func(mt *metaTx) bool {
			txsToDelete = append(txsToDelete, mt)
			count--
			return count > 0
		})
		for _, mt := range txsToDelete {
			p.discardLocked(mt, Spammer) // can't call it while iterating by all
		}
	}
}

func fillDiscardReasons(reasons []DiscardReason, newTxs TxSlots, discardReasonsLRU *simplelru.LRU) []DiscardReason {
	for i := range reasons {
		if reasons[i] != NotSet {
			continue
		}
		reason, ok := discardReasonsLRU.Get(string(newTxs.txs[i].IdHash[:]))
		if ok {
			reasons[i] = reason.(DiscardReason)
		} else {
			reasons[i] = Success
		}
	}
	return reasons
}

func (p *TxPool) AddLocalTxs(ctx context.Context, newTransactions TxSlots) ([]DiscardReason, error) {
	coreTx, err := p.coreDB().BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer coreTx.Rollback()

	cacheView, err := p.cache().View(ctx, coreTx)
	if err != nil {
		return nil, err
	}

	if !p.Started() {
		return nil, fmt.Errorf("pool not started yet")
	}

	p.lock.Lock()
	defer p.lock.Unlock()

	if err = p.senders.registerNewSenders(&newTransactions); err != nil {
		return nil, err
	}

	reasons, newTxs, err := p.validateTxs(&newTransactions, cacheView)
	if err != nil {
		return nil, err
	}

	p.pending.resetAddedHashes()
	p.baseFee.resetAddedHashes()
	if addReasons, err := addTxs(p.lastSeenBlock.Load(), cacheView, p.senders, newTxs,
		p.pendingBaseFee.Load(), p.blockGasLimit.Load(), p.pending, p.baseFee, p.queued, p.all, p.byHash, p.addLocked, p.discardLocked); err == nil {
		for i, reason := range addReasons {
			if reason != NotSet {
				reasons[i] = reason
			}
		}
	} else {
		return nil, err
	}
	p.promoted = p.pending.appendAddedHashes(p.promoted[:0])
	p.promoted = p.baseFee.appendAddedHashes(p.promoted)

	reasons = fillDiscardReasons(reasons, newTxs, p.discardReasonsLRU)
	for i, reason := range reasons {
		if reason == Success {
			txn := newTxs.txs[i]
			if txn.traced {
				log.Info(fmt.Sprintf("TX TRACING: AddLocalTxs promotes idHash=%x, senderId=%d", txn.IdHash, txn.senderID))
			}
			p.promoted = append(p.promoted, txn.IdHash[:]...)
		}
	}
	if p.promoted.Len() > 0 {
		select {
		case p.newPendingTxs <- common.Copy(p.promoted):
		default:
		}
	}
	return reasons, nil
}

func (p *TxPool) coreDB() kv.RoDB {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p._chainDB
}

func (p *TxPool) cache() kvcache.Cache {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p._stateCache
}
func addTxs(blockNum uint64, cacheView kvcache.CacheView, senders *sendersBatch,
	newTxs TxSlots, pendingBaseFee, blockGasLimit uint64,
	pending *PendingPool, baseFee, queued *SubPool,
	byNonce *BySenderAndNonce, byHash map[string]*metaTx, add func(*metaTx) DiscardReason, discard func(*metaTx, DiscardReason)) ([]DiscardReason, error) {
	protocolBaseFee := calcProtocolBaseFee(pendingBaseFee)
	if ASSERT {
		for _, txn := range newTxs.txs {
			if txn.senderID == 0 {
				panic(fmt.Errorf("senderID can't be zero"))
			}
		}
	}
	// This can be thought of a reverse operation from the one described before.
	// When a block that was deemed "the best" of its height, is no longer deemed "the best", the
	// transactions contained in it, are now viable for inclusion in other blocks, and therefore should
	// be returned into the transaction pool.
	// An interesting note here is that if the block contained any transactions local to the node,
	// by being first removed from the pool (from the "local" part of it), and then re-injected,
	// they effective lose their priority over the "remote" transactions. In order to prevent that,
	// somehow the fact that certain transactions were local, needs to be remembered for some
	// time (up to some "immutability threshold").
	sendersWithChangedState := map[uint64]struct{}{}
	discardReasons := make([]DiscardReason, len(newTxs.txs))
	for i, txn := range newTxs.txs {
		if _, ok := byHash[string(txn.IdHash[:])]; ok {
			discardReasons[i] = DuplicateHash
			continue
		}
		mt := newMetaTx(txn, newTxs.isLocal[i], blockNum)
		if reason := add(mt); reason != NotSet {
			discardReasons[i] = reason
			continue
		}
		discardReasons[i] = NotSet
		if txn.traced {
			log.Info(fmt.Sprintf("TX TRACING: schedule sendersWithChangedState idHash=%x senderId=%d", txn.IdHash, mt.Tx.senderID))
		}
		sendersWithChangedState[mt.Tx.senderID] = struct{}{}
	}

	for senderID := range sendersWithChangedState {
		nonce, balance, err := senders.info(cacheView, senderID)
		if err != nil {
			return discardReasons, err
		}
		onSenderStateChange(senderID, nonce, balance, byNonce,
			protocolBaseFee, blockGasLimit, pending, baseFee, queued, false, discard)
	}

	promote(pending, baseFee, queued, pendingBaseFee, discard)
	pending.EnforceBestInvariants()

	return discardReasons, nil
}
func addTxsOnNewBlock(blockNum uint64, cacheView kvcache.CacheView, stateChanges *remote.StateChangeBatch,
	senders *sendersBatch, newTxs TxSlots, pendingBaseFee uint64, blockGasLimit uint64,
	pending *PendingPool, baseFee, queued *SubPool,
	byNonce *BySenderAndNonce, byHash map[string]*metaTx, add func(*metaTx) DiscardReason, discard func(*metaTx, DiscardReason)) error {
	protocolBaseFee := calcProtocolBaseFee(pendingBaseFee)
	if ASSERT {
		for _, txn := range newTxs.txs {
			if txn.senderID == 0 {
				panic(fmt.Errorf("senderID can't be zero"))
			}
		}
	}
	// This can be thought of a reverse operation from the one described before.
	// When a block that was deemed "the best" of its height, is no longer deemed "the best", the
	// transactions contained in it, are now viable for inclusion in other blocks, and therefore should
	// be returned into the transaction pool.
	// An interesting note here is that if the block contained any transactions local to the node,
	// by being first removed from the pool (from the "local" part of it), and then re-injected,
	// they effective lose their priority over the "remote" transactions. In order to prevent that,
	// somehow the fact that certain transactions were local, needs to be remembered for some
	// time (up to some "immutability threshold").
	sendersWithChangedState := map[uint64]struct{}{}
	for i, txn := range newTxs.txs {
		if _, ok := byHash[string(txn.IdHash[:])]; ok {
			continue
		}
		mt := newMetaTx(txn, newTxs.isLocal[i], blockNum)
		if reason := add(mt); reason != NotSet {
			discard(mt, reason)
			continue
		}
		sendersWithChangedState[mt.Tx.senderID] = struct{}{}
	}
	// add senders changed in state to `sendersWithChangedState` list
	for _, changesList := range stateChanges.ChangeBatch {
		for _, change := range changesList.Changes {
			switch change.Action {
			case remote.Action_UPSERT, remote.Action_UPSERT_CODE:
				if change.Incarnation > 0 {
					continue
				}
				addr := gointerfaces.ConvertH160toAddress(change.Address)
				id, ok := senders.getID(addr[:])
				if !ok {
					continue
				}
				sendersWithChangedState[id] = struct{}{}
			}
		}
	}

	for senderID := range sendersWithChangedState {
		nonce, balance, err := senders.info(cacheView, senderID)
		if err != nil {
			return err
		}
		onSenderStateChange(senderID, nonce, balance, byNonce,
			protocolBaseFee, blockGasLimit, pending, baseFee, queued, true, discard)
	}

	return nil
}

func (p *TxPool) setBaseFee(baseFee uint64) (uint64, bool) {
	changed := false
	if baseFee > 0 {
		changed = baseFee != p.pendingBaseFee.Load()
		p.pendingBaseFee.Store(baseFee)
	}
	return p.pendingBaseFee.Load(), changed
}

func (p *TxPool) addLocked(mt *metaTx) DiscardReason {
	// Insert to pending pool, if pool doesn't have txn with same Nonce and bigger Tip
	found := p.all.get(mt.Tx.senderID, mt.Tx.nonce)
	if found != nil {
		tipThreshold := found.Tx.tip * (100 + p.cfg.PriceBump) / 100
		feecapThreshold := found.Tx.feeCap * (100 + p.cfg.PriceBump) / 100
		if mt.Tx.tip < tipThreshold || mt.Tx.feeCap < feecapThreshold {
			// Both tip and feecap need to be larger than previously to replace the transaction
			return NotReplaced
		}

		switch found.currentSubPool {
		case PendingSubPool:
			p.pending.Remove(found)
		case BaseFeeSubPool:
			p.baseFee.Remove(found)
		case QueuedSubPool:
			p.queued.Remove(found)
		default:
			//already removed
		}

		p.discardLocked(found, ReplacedByHigherTip)
	}

	p.byHash[string(mt.Tx.IdHash[:])] = mt

	if replaced := p.all.replaceOrInsert(mt); replaced != nil {
		if ASSERT {
			panic("must neve happen")
		}
	}

	if mt.subPool&IsLocal != 0 {
		p.isLocalLRU.Add(string(mt.Tx.IdHash[:]), struct{}{})
	}
	// All transactions are first added to the queued pool and then immediately promoted from there if required
	p.queued.Add(mt)
	return NotSet
}

// dropping transaction from all sub-structures and from db
// Important: don't call it while iterating by all
func (p *TxPool) discardLocked(mt *metaTx, reason DiscardReason) {
	delete(p.byHash, string(mt.Tx.IdHash[:]))
	p.deletedTxs = append(p.deletedTxs, mt)
	p.all.delete(mt)
	p.discardReasonsLRU.Add(string(mt.Tx.IdHash[:]), reason)
}

func (p *TxPool) NonceFromAddress(addr [20]byte) (nonce uint64, inPool bool) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	senderId, found := p.senders.getID(addr[:])
	if !found {
		return 0, false
	}
	return p.all.nonce(senderId)
}

// removeMined - apply new highest block (or batch of blocks)
//
// 1. New best block arrives, which potentially changes the balance and the nonce of some senders.
// We use senderIds data structure to find relevant senderId values, and then use senders data structure to
// modify state_balance and state_nonce, potentially remove some elements (if transaction with some nonce is
// included into a block), and finally, walk over the transaction records and update SubPool fields depending on
// the actual presence of nonce gaps and what the balance is.
func removeMined(byNonce *BySenderAndNonce, minedTxs []*TxSlot, pending *PendingPool, baseFee, queued *SubPool, discard func(*metaTx, DiscardReason)) error {
	noncesToRemove := map[uint64]uint64{}
	for _, txn := range minedTxs {
		nonce, ok := noncesToRemove[txn.senderID]
		if !ok || txn.nonce > nonce {
			noncesToRemove[txn.senderID] = txn.nonce
		}
	}

	var toDel []*metaTx // can't delete items while iterate them
	for senderID, nonce := range noncesToRemove {
		//if sender.all.Len() > 0 {
		//log.Debug("[txpool] removing mined", "senderID", tx.senderID, "sender.all.len()", sender.all.Len())
		//}
		// delete mined transactions from everywhere
		byNonce.ascend(senderID, func(mt *metaTx) bool {
			//log.Debug("[txpool] removing mined, cmp nonces", "tx.nonce", it.metaTx.Tx.nonce, "sender.nonce", sender.nonce)
			if mt.Tx.nonce > nonce {
				return false
			}
			if mt.Tx.traced {
				log.Info(fmt.Sprintf("TX TRACING: removeMined idHash=%x senderId=%d, currentSubPool=%s", mt.Tx.IdHash, mt.Tx.senderID, mt.currentSubPool))
			}
			toDel = append(toDel, mt)
			// del from sub-pool
			switch mt.currentSubPool {
			case PendingSubPool:
				pending.Remove(mt)
			case BaseFeeSubPool:
				baseFee.Remove(mt)
			case QueuedSubPool:
				queued.Remove(mt)
			default:
				//already removed
			}
			return true
		})

		for _, mt := range toDel {
			discard(mt, Mined)
		}
		toDel = toDel[:0]
	}
	return nil
}

// onSenderStateChange is the function that recalculates ephemeral fields of transactions and determines
// which sub pool they will need to go to. Sice this depends on other transactions from the same sender by with lower
// nonces, and also affect other transactions from the same sender with higher nonce, it loops through all transactions
// for a given senderID
func onSenderStateChange(senderID uint64, senderNonce uint64, senderBalance uint256.Int, byNonce *BySenderAndNonce,
	protocolBaseFee, blockGasLimit uint64, pending *PendingPool, baseFee, queued *SubPool, unsafe bool, discard func(*metaTx, DiscardReason)) {
	noGapsNonce := senderNonce
	cumulativeRequiredBalance := uint256.NewInt(0)
	minFeeCap := uint64(math.MaxUint64)
	minTip := uint64(math.MaxUint64)
	var toDel []*metaTx // can't delete items while iterate them
	byNonce.ascend(senderID, func(mt *metaTx) bool {
		if mt.Tx.traced {
			log.Info(fmt.Sprintf("TX TRACING: onSenderStateChange loop iteration idHash=%x senderID=%d, senderNonce=%d, txn.nonce=%d, currentSubPool=%s", mt.Tx.IdHash, senderID, senderNonce, mt.Tx.nonce, mt.currentSubPool))
		}
		if senderNonce > mt.Tx.nonce {
			if mt.Tx.traced {
				log.Info(fmt.Sprintf("TX TRACING: removing due to low nonce for idHash=%x senderID=%d, senderNonce=%d, txn.nonce=%d, currentSubPool=%s", mt.Tx.IdHash, senderID, senderNonce, mt.Tx.nonce, mt.currentSubPool))
			}
			// del from sub-pool
			switch mt.currentSubPool {
			case PendingSubPool:
				pending.Remove(mt)
			case BaseFeeSubPool:
				baseFee.Remove(mt)
			case QueuedSubPool:
				queued.Remove(mt)
			default:
				//already removed
			}
			toDel = append(toDel, mt)
			return true
		}
		minFeeCap = min(minFeeCap, mt.Tx.feeCap)
		mt.minFeeCap = minFeeCap
		minTip = min(minTip, mt.Tx.tip)
		mt.minTip = minTip

		mt.nonceDistance = 0
		if mt.Tx.nonce > senderNonce { // no uint underflow
			mt.nonceDistance = mt.Tx.nonce - senderNonce
		}

		// Sender has enough balance for: gasLimit x feeCap + transferred_value
		needBalance := uint256.NewInt(mt.Tx.gas)
		needBalance.Mul(needBalance, uint256.NewInt(mt.Tx.feeCap))
		needBalance.Add(needBalance, &mt.Tx.value)
		// 1. Minimum fee requirement. Set to 1 if feeCap of the transaction is no less than in-protocol
		// parameter of minimal base fee. Set to 0 if feeCap is less than minimum base fee, which means
		// this transaction will never be included into this particular chain.
		mt.subPool &^= EnoughFeeCapProtocol
		if mt.minFeeCap >= protocolBaseFee {
			mt.subPool |= EnoughFeeCapProtocol
		} else {
			mt.subPool = 0 // TODO: we immediately drop all transactions if they have no first bit - then maybe we don't need this bit at all? And don't add such transactions to queue?
			return true
		}

		// 2. Absence of nonce gaps. Set to 1 for transactions whose nonce is N, state nonce for
		// the sender is M, and there are transactions for all nonces between M and N from the same
		// sender. Set to 0 is the transaction's nonce is divided from the state nonce by one or more nonce gaps.
		mt.subPool &^= NoNonceGaps
		if noGapsNonce == mt.Tx.nonce {
			mt.subPool |= NoNonceGaps
			noGapsNonce++
		}

		// 3. Sufficient balance for gas. Set to 1 if the balance of sender's account in the
		// state is B, nonce of the sender in the state is M, nonce of the transaction is N, and the
		// sum of feeCap x gasLimit + transferred_value of all transactions from this sender with
		// nonces N+1 ... M is no more than B. Set to 0 otherwise. In other words, this bit is
		// set if there is currently a guarantee that the transaction and all its required prior
		// transactions will be able to pay for gas.
		mt.subPool &^= EnoughBalance
		mt.cumulativeBalanceDistance = math.MaxUint64
		if mt.Tx.nonce >= senderNonce {
			cumulativeRequiredBalance = cumulativeRequiredBalance.Add(cumulativeRequiredBalance, needBalance) // already deleted all transactions with nonce <= sender.nonce
			if senderBalance.Gt(cumulativeRequiredBalance) || senderBalance.Eq(cumulativeRequiredBalance) {
				mt.subPool |= EnoughBalance
			} else {
				if cumulativeRequiredBalance.IsUint64() && senderBalance.IsUint64() {
					mt.cumulativeBalanceDistance = cumulativeRequiredBalance.Uint64() - senderBalance.Uint64()
				}
			}
		}

		mt.subPool &^= NotTooMuchGas
		if mt.Tx.gas < blockGasLimit {
			mt.subPool |= NotTooMuchGas
		}

		if mt.Tx.traced {
			log.Info(fmt.Sprintf("TX TRACING: onSenderStateChange loop iteration idHash=%x senderId=%d subPool=%b", mt.Tx.IdHash, mt.Tx.senderID, mt.subPool))
		}

		// 5. Local transaction. Set to 1 if transaction is local.
		// can't change

		if !unsafe {
			switch mt.currentSubPool {
			case PendingSubPool:
				pending.Updated(mt)
			case BaseFeeSubPool:
				baseFee.Updated(mt)
			case QueuedSubPool:
				queued.Updated(mt)
			}
		}
		return true
	})
	for _, mt := range toDel {
		discard(mt, NonceTooLow)
	}
}

// promote reasserts invariants of the subpool and returns the list of transactions that ended up
// being promoted to the pending or basefee pool, for re-broadcasting
func promote(pending *PendingPool, baseFee, queued *SubPool, pendingBaseFee uint64, discard func(*metaTx, DiscardReason)) {
	// Demote worst transactions that do not qualify for pending sub pool anymore, to other sub pools, or discard
	for worst := pending.Worst(); pending.Len() > 0 && (worst.subPool < BaseFeePoolBits || worst.minFeeCap < pendingBaseFee); worst = pending.Worst() {
		if worst.subPool >= BaseFeePoolBits {
			baseFee.Add(pending.PopWorst())
		} else if worst.subPool >= QueuedPoolBits {
			queued.Add(pending.PopWorst())
		} else {
			discard(pending.PopWorst(), FeeTooLow)
		}
	}

	// Promote best transactions from base fee pool to pending pool while they qualify
	for best := baseFee.Best(); baseFee.Len() > 0 && best.subPool >= BaseFeePoolBits && best.minFeeCap >= pendingBaseFee; best = baseFee.Best() {
		pending.Add(baseFee.PopBest())
	}

	// Demote worst transactions that do not qualify for base fee pool anymore, to queued sub pool, or discard
	for worst := baseFee.Worst(); baseFee.Len() > 0 && worst.subPool < BaseFeePoolBits; worst = baseFee.Worst() {
		if worst.subPool >= QueuedPoolBits {
			queued.Add(baseFee.PopWorst())
		} else {
			discard(baseFee.PopWorst(), FeeTooLow)
		}
	}

	// Promote best transactions from the queued pool to either pending or base fee pool, while they qualify
	for best := queued.Best(); queued.Len() > 0 && best.subPool >= BaseFeePoolBits; best = queued.Best() {
		if best.minFeeCap >= pendingBaseFee {
			pending.Add(queued.PopBest())
		} else {
			baseFee.Add(queued.PopBest())
		}
	}

	// Discard worst transactions from the queued sub pool if they do not qualify
	for worst := queued.Worst(); queued.Len() > 0 && worst.subPool < QueuedPoolBits; worst = queued.Worst() {
		discard(queued.PopWorst(), FeeTooLow)
	}

	// Discard worst transactions from pending pool until it is within capacity limit
	for pending.Len() > pending.limit {
		discard(pending.PopWorst(), PendingPoolOverflow)
	}

	// Discard worst transactions from pending sub pool until it is within capacity limits
	for baseFee.Len() > baseFee.limit {
		discard(baseFee.PopWorst(), BaseFeePoolOverflow)
	}

	// Discard worst transactions from the queued sub pool until it is within its capacity limits
	for _ = queued.Worst(); queued.Len() > queued.limit; _ = queued.Worst() {
		discard(queued.PopWorst(), QueuedPoolOverflow)
	}
}

// MainLoop - does:
// send pending byHash to p2p:
//      - new byHash
//      - all pooled byHash to recently connected peers
//      - all local pooled byHash to random peers periodically
// promote/demote transactions
// reorgs
func MainLoop(ctx context.Context, db kv.RwDB, coreDB kv.RoDB, p *TxPool, newTxs chan Hashes, send *Send, newSlotsStreams *NewSlotsStreams, notifyMiningAboutNewSlots func()) {
	syncToNewPeersEvery := time.NewTicker(p.cfg.SyncToNewPeersEvery)
	defer syncToNewPeersEvery.Stop()
	processRemoteTxsEvery := time.NewTicker(p.cfg.ProcessRemoteTxsEvery)
	defer processRemoteTxsEvery.Stop()
	commitEvery := time.NewTicker(p.cfg.CommitEvery)
	defer commitEvery.Stop()
	logEvery := time.NewTicker(p.cfg.LogEvery)
	defer logEvery.Stop()

	for {
		select {
		case <-ctx.Done():
			_, _ = p.flush(db)
			return
		case <-logEvery.C:
			p.logStats()
		case <-processRemoteTxsEvery.C:
			if !p.Started() {
				continue
			}

			if err := p.processRemoteTxs(ctx); err != nil {
				if grpcutil.IsRetryLater(err) || grpcutil.IsEndOfStream(err) {
					time.Sleep(3 * time.Second)
					continue
				}

				log.Error("[txpool] process batch remote txs", "err", err)
			}
		case <-commitEvery.C:
			if db != nil && p.Started() {
				t := time.Now()
				written, err := p.flush(db)
				if err != nil {
					log.Error("[txpool] flush is local history", "err", err)
					continue
				}
				writeToDbBytesCounter.Set(written)
				log.Debug("[txpool] Commit", "written_kb", written/1024, "in", time.Since(t))
			}
		case h := <-newTxs:
			go func() {
				for i := 0; i < 16; i++ { // drain more events from channel, then merge and dedup them
					select {
					case a := <-newTxs:
						h = append(h, a...)
						continue
					default:
					}
					break
				}
				if h.Len() == 0 {
					return
				}
				defer propagateNewTxsTimer.UpdateDuration(time.Now())

				h = h.DedupCopy()

				notifyMiningAboutNewSlots()

				var localTxHashes Hashes
				var localTxRlps [][]byte
				var remoteTxHashes Hashes
				var remoteTxRlps [][]byte
				slotsRlp := make([][]byte, 0, h.Len())

				if err := db.View(ctx, func(tx kv.Tx) error {
					for i := 0; i < h.Len(); i++ {
						hash := h.At(i)
						slotRlp, err := p.GetRlp(tx, hash)
						if err != nil {
							return err
						}
						if len(slotRlp) == 0 {
							continue
						}

						// Empty rlp can happen if a transaction we want to broadcase has just been mined, for example
						slotsRlp = append(slotsRlp, slotRlp)
						if p.IsLocal(hash) {
							localTxHashes = append(localTxHashes, hash...)
							localTxRlps = append(localTxRlps, slotRlp)
						} else {
							remoteTxHashes = append(localTxHashes, hash...)
							remoteTxRlps = append(remoteTxRlps, slotRlp)
						}
					}
					return nil
				}); err != nil {
					log.Error("[txpool] collect info to propagate", "err", err)
					return
				}
				if newSlotsStreams != nil {
					newSlotsStreams.Broadcast(&proto_txpool.OnAddReply{RplTxs: slotsRlp})
				}

				// first broadcast all local txs to all peers, then non-local to random sqrt(peersAmount) peers
				txSentTo := send.BroadcastPooledTxs(localTxRlps)
				hashSentTo := send.AnnouncePooledTxs(localTxHashes)
				for i := 0; i < localTxHashes.Len(); i++ {
					hash := localTxHashes.At(i)
					log.Info("local tx propagated", "tx_hash", fmt.Sprintf("%x", hash), "announced to peers", hashSentTo[i], "broadcast to peers", txSentTo[i], "baseFee", p.pendingBaseFee.Load())
				}
				send.BroadcastPooledTxs(remoteTxRlps)
				send.AnnouncePooledTxs(remoteTxHashes)
			}()
		case <-syncToNewPeersEvery.C: // new peer
			newPeers := p.recentlyConnectedPeers.GetAndClean()
			if len(newPeers) == 0 {
				continue
			}
			t := time.Now()
			var hashes Hashes
			hashes = p.AppendAllHashes(hashes[:0])
			go send.PropagatePooledTxsToPeersList(newPeers, hashes)
			propagateToNewPeerTimer.UpdateDuration(t)
		}
	}
}

func (p *TxPool) flush(db kv.RwDB) (written uint64, err error) {
	defer writeToDbTimer.UpdateDuration(time.Now())
	p.lock.Lock()
	defer p.lock.Unlock()
	//it's important that write db tx is done inside lock, to make last writes visible for all read operations
	if err := db.Update(context.Background(), func(tx kv.RwTx) error {
		err = p.flushLocked(tx)
		if err != nil {
			return err
		}
		written, _, err = tx.(*mdbx.MdbxTx).SpaceDirty()
		if err != nil {
			return err
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return written, nil
}
func (p *TxPool) flushLocked(tx kv.RwTx) (err error) {
	for i, mt := range p.deletedTxs {
		id := mt.Tx.senderID
		idHash := mt.Tx.IdHash[:]
		if !p.all.hasTxs(id) {
			addr, ok := p.senders.senderID2Addr[id]
			if ok {
				delete(p.senders.senderID2Addr, id)
				delete(p.senders.senderIDs, string(addr))
			}
		}
		//fmt.Printf("del:%d,%d,%d\n", mt.Tx.senderID, mt.Tx.nonce, mt.Tx.tip)
		has, err := tx.Has(kv.PoolTransaction, idHash)
		if err != nil {
			return err
		}
		if has {
			if err := tx.Delete(kv.PoolTransaction, idHash, nil); err != nil {
				return err
			}
		}
		p.deletedTxs[i] = nil // for gc
	}

	txHashes := p.isLocalLRU.Keys()
	encID := make([]byte, 8)
	if err := tx.ClearBucket(kv.RecentLocalTransaction); err != nil {
		return err
	}
	for i, txHash := range txHashes {
		binary.BigEndian.PutUint64(encID, uint64(i))
		if err := tx.Append(kv.RecentLocalTransaction, encID, []byte(txHash.(string))); err != nil {
			return err
		}
	}

	v := make([]byte, 0, 1024)
	for txHash, metaTx := range p.byHash {
		if metaTx.Tx.rlp == nil {
			continue
		}
		v = common.EnsureEnoughSize(v, 20+len(metaTx.Tx.rlp))
		for addr, id := range p.senders.senderIDs { // no inverted index - tradeoff flush speed for memory usage
			if id == metaTx.Tx.senderID {
				copy(v[:20], addr)
				break
			}
		}
		copy(v[20:], metaTx.Tx.rlp)

		has, err := tx.Has(kv.PoolTransaction, []byte(txHash))
		if err != nil {
			return err
		}
		if !has {
			if err := tx.Put(kv.PoolTransaction, []byte(txHash), v); err != nil {
				return err
			}
		}
		metaTx.Tx.rlp = nil
	}

	binary.BigEndian.PutUint64(encID, p.pendingBaseFee.Load())
	if err := tx.Put(kv.PoolInfo, PoolPendingBaseFeeKey, encID); err != nil {
		return err
	}
	if err := PutLastSeenBlock(tx, p.lastSeenBlock.Load(), encID); err != nil {
		return err
	}

	// clean - in-memory data structure as later as possible - because if during this Tx will happen error,
	// DB will stay consitant but some in-memory structures may be alread cleaned, and retry will not work
	// failed write transaction must not create side-effects
	p.deletedTxs = p.deletedTxs[:0]
	return nil
}

func (p *TxPool) fromDB(ctx context.Context, tx kv.Tx, coreTx kv.Tx) error {
	if p.lastSeenBlock.Load() == 0 {
		lastSeenBlock, err := LastSeenBlock(tx)
		if err != nil {
			return err
		}
		p.lastSeenBlock.Store(lastSeenBlock)
	}

	cacheView, err := p._stateCache.View(ctx, coreTx)
	if err != nil {
		return err
	}
	if err := tx.ForEach(kv.RecentLocalTransaction, nil, func(k, v []byte) error {
		//fmt.Printf("is local restored from db: %x\n", k)
		p.isLocalLRU.Add(string(v), struct{}{})
		return nil
	}); err != nil {
		return err
	}

	txs := TxSlots{}
	parseCtx := NewTxParseContext(p.chainID)
	parseCtx.WithSender(false)

	i := 0
	if err := tx.ForEach(kv.PoolTransaction, nil, func(k, v []byte) error {
		addr, txRlp := v[:20], v[20:]
		txn := &TxSlot{}

		_, err := parseCtx.ParseTransaction(txRlp, 0, txn, nil, false /* hasEnvelope */)
		if err != nil {
			return fmt.Errorf("err: %w, rlp: %x", err, txRlp)
		}
		txn.rlp = nil // means that we don't need store it in db anymore

		txn.senderID, txn.traced = p.senders.getOrCreateID(addr)
		binary.BigEndian.Uint64(v)

		isLocalTx := p.isLocalLRU.Contains(string(k))

		if reason := p.validateTx(txn, isLocalTx, cacheView); reason != NotSet && reason != Success {
			return nil
		}
		txs.Resize(uint(i + 1))
		txs.txs[i] = txn
		txs.isLocal[i] = isLocalTx
		copy(txs.senders.At(i), addr)
		i++
		return nil
	}); err != nil {
		return err
	}

	var pendingBaseFee uint64
	{
		v, err := tx.GetOne(kv.PoolInfo, PoolPendingBaseFeeKey)
		if err != nil {
			return err
		}
		if len(v) > 0 {
			pendingBaseFee = binary.BigEndian.Uint64(v)
		}
	}
	err = p.senders.registerNewSenders(&txs)
	if err != nil {
		return err
	}
	if _, err := addTxs(p.lastSeenBlock.Load(), cacheView, p.senders, txs,
		pendingBaseFee, math.MaxUint64 /* blockGasLimit */, p.pending, p.baseFee, p.queued, p.all, p.byHash, p.addLocked, p.discardLocked); err != nil {
		return err
	}
	p.pendingBaseFee.Store(pendingBaseFee)

	return nil
}
func LastSeenBlock(tx kv.Getter) (uint64, error) {
	v, err := tx.GetOne(kv.PoolInfo, PoolLastSeenBlockKey)
	if err != nil {
		return 0, err
	}
	if len(v) == 0 {
		return 0, nil
	}
	return binary.BigEndian.Uint64(v), nil
}
func PutLastSeenBlock(tx kv.Putter, n uint64, buf []byte) error {
	buf = common.EnsureEnoughSize(buf, 8)
	binary.BigEndian.PutUint64(buf, n)
	err := tx.Put(kv.PoolInfo, PoolLastSeenBlockKey, buf)
	if err != nil {
		return err
	}
	return nil
}
func ChainConfig(tx kv.Getter) (*chain.Config, error) {
	v, err := tx.GetOne(kv.PoolInfo, PoolChainConfigKey)
	if err != nil {
		return nil, err
	}
	if len(v) == 0 {
		return nil, nil
	}
	var config chain.Config
	if err := json.Unmarshal(v, &config); err != nil {
		return nil, fmt.Errorf("invalid chain config JSON in pool db: %w", err)
	}
	return &config, nil
}
func PutChainConfig(tx kv.Putter, cc *chain.Config, buf []byte) error {
	wr := bytes.NewBuffer(buf)
	if err := json.NewEncoder(wr).Encode(cc); err != nil {
		return fmt.Errorf("invalid chain config JSON in pool db: %w", err)
	}
	if err := tx.Put(kv.PoolInfo, PoolChainConfigKey, wr.Bytes()); err != nil {
		return err
	}
	return nil
}

//nolint
func (p *TxPool) printDebug(prefix string) {
	fmt.Printf("%s.pool.byHash\n", prefix)
	for _, j := range p.byHash {
		fmt.Printf("\tsenderID=%d, nonce=%d, tip=%d\n", j.Tx.senderID, j.Tx.nonce, j.Tx.tip)
	}
	fmt.Printf("%s.pool.queues.len: %d,%d,%d\n", prefix, p.pending.Len(), p.baseFee.Len(), p.queued.Len())
	for _, mt := range p.pending.best.ms {
		mt.Tx.printDebug(fmt.Sprintf("%s.pending: %b,%d,%d,%d", prefix, mt.subPool, mt.Tx.senderID, mt.Tx.nonce, mt.Tx.tip))
	}
	for _, mt := range p.baseFee.best.ms {
		mt.Tx.printDebug(fmt.Sprintf("%s.baseFee : %b,%d,%d,%d", prefix, mt.subPool, mt.Tx.senderID, mt.Tx.nonce, mt.Tx.tip))
	}
	for _, mt := range p.queued.best.ms {
		mt.Tx.printDebug(fmt.Sprintf("%s.queued : %b,%d,%d,%d", prefix, mt.subPool, mt.Tx.senderID, mt.Tx.nonce, mt.Tx.tip))
	}
}
func (p *TxPool) logStats() {
	if !p.started.Load() {
		//log.Info("[txpool] Not started yet, waiting for new blocks...")
		return
	}
	//protocolBaseFee, pendingBaseFee := p.protocolBaseFee.Load(), p.pendingBaseFee.Load()

	p.lock.RLock()
	defer p.lock.RUnlock()

	//idsInMem := p.senders.idsCount()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	ctx := []interface{}{
		//"baseFee", fmt.Sprintf("%d, %dm", protocolBaseFee, pendingBaseFee/1_000_000),
		"block", p.lastSeenBlock.Load(),
		"pending", p.pending.Len(),
		"baseFee", p.baseFee.Len(),
		"queued", p.queued.Len(),
	}
	cacheKeys := p._stateCache.Len()
	if cacheKeys > 0 {
		ctx = append(ctx, "cache_keys", cacheKeys)
	}
	ctx = append(ctx, "alloc_mb", m.Alloc/1024/1024, "sys_mb", m.Sys/1024/1024)
	log.Info("[txpool] stat", ctx...)
	//if ASSERT {
	//stats := kvcache.DebugStats(p.senders.cache)
	//log.Info(fmt.Sprintf("[txpool] cache %T, roots amount %d", p.senders.cache, len(stats)))
	//for i := range stats {
	//	log.Info("[txpool] cache", "root", stats[i].BlockNum, "len", stats[i].Lenght)
	//}
	//stats := kvcache.DebugStats(p.senders.cache)
	//log.Info(fmt.Sprintf("[txpool] cache %T, roots amount %d", p.senders.cache, len(stats)))
	//for i := range stats {
	//	log.Info("[txpool] cache", "root", stats[i].BlockNum, "len", stats[i].Lenght)
	//}
	//ages := kvcache.DebugAges(p.senders.cache)
	//for i := range ages {
	//	log.Info("[txpool] age", "age", ages[i].BlockNum, "amount", ages[i].Lenght)
	//}
	//}
}

//Deprecated need switch to streaming-like
func (p *TxPool) deprecatedForEach(_ context.Context, f func(rlp, sender []byte, t SubPoolType), tx kv.Tx) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	p.all.ascendAll(func(mt *metaTx) bool {
		slot := mt.Tx
		slotRlp := slot.rlp
		if slot.rlp == nil {
			v, err := tx.GetOne(kv.PoolTransaction, slot.IdHash[:])
			if err != nil {
				log.Warn("[txpool] foreach: get tx from db", "err", err)
				return true
			}
			if v == nil {
				log.Warn("[txpool] foreach: tx not found in db")
				return true
			}
			slotRlp = v[20:]
		}
		if sender, found := p.senders.senderID2Addr[slot.senderID]; found {
			f(slotRlp, sender, mt.currentSubPool)
		}
		return true
	})
}

// CalcIntrinsicGas computes the 'intrinsic gas' for a message with the given data.
func CalcIntrinsicGas(dataLen, dataNonZeroLen uint64, accessList AccessList, isContractCreation bool, isHomestead, isEIP2028 bool) (uint64, DiscardReason) {
	// Set the starting gas for the raw transaction
	var gas uint64
	if isContractCreation && isHomestead {
		gas = fixedgas.TxGasContractCreation
	} else {
		gas = fixedgas.TxGas
	}
	// Bump the required gas by the amount of transactional data
	if dataLen > 0 {
		// Zero and non-zero bytes are priced differently
		nz := dataNonZeroLen
		// Make sure we don't exceed uint64 for all data combinations
		nonZeroGas := fixedgas.TxDataNonZeroGasFrontier
		if isEIP2028 {
			nonZeroGas = fixedgas.TxDataNonZeroGasEIP2028
		}
		if (math.MaxUint64-gas)/nonZeroGas < nz {
			return 0, GasUintOverflow
		}
		gas += nz * nonZeroGas

		z := dataLen - nz
		if (math.MaxUint64-gas)/fixedgas.TxDataZeroGas < z {
			return 0, GasUintOverflow
		}
		gas += z * fixedgas.TxDataZeroGas
	}
	if accessList != nil {
		gas += uint64(len(accessList)) * fixedgas.TxAccessListAddressGas
		gas += uint64(accessList.StorageKeys()) * fixedgas.TxAccessListStorageKeyGas
	}
	return gas, Success
}

var PoolChainConfigKey = []byte("chain_config")
var PoolLastSeenBlockKey = []byte("last_seen_block")
var PoolPendingBaseFeeKey = []byte("pending_base_fee")

// recentlyConnectedPeers does buffer IDs of recently connected good peers
// then sync of pooled Transaction can happen to all of then at once
// DoS protection and performance saving
// it doesn't track if peer disconnected, it's fine
type recentlyConnectedPeers struct {
	lock  sync.RWMutex
	peers []PeerID
}

func (l *recentlyConnectedPeers) AddPeer(p PeerID) {
	l.lock.Lock()
	defer l.lock.Unlock()
	l.peers = append(l.peers, p)
}

func (l *recentlyConnectedPeers) GetAndClean() []PeerID {
	l.lock.Lock()
	defer l.lock.Unlock()
	peers := l.peers
	l.peers = nil
	return peers
}

//nolint
func (sc *sendersBatch) printDebug(prefix string) {
	fmt.Printf("%s.sendersBatch.sender\n", prefix)
	//for i, j := range sc.senderInfo {
	//	fmt.Printf("\tid=%d,nonce=%d,balance=%d\n", i, j.nonce, j.balance.Uint64())
	//}
}

// sendersBatch stores in-memory senders-related objects - which are different from DB (updated/dirty)
// flushing to db periodicaly. it doesn't play as read-cache (because db is small and memory-mapped - doesn't need cache)
// non thread-safe
type sendersBatch struct {
	senderID      uint64
	senderIDs     map[string]uint64
	senderID2Addr map[uint64][]byte
	tracedSenders map[string]struct{}
}

func newSendersCache(tracedSenders map[string]struct{}) *sendersBatch {
	return &sendersBatch{senderIDs: map[string]uint64{}, senderID2Addr: map[uint64][]byte{}, tracedSenders: tracedSenders}
}

func (sc *sendersBatch) getID(addr []byte) (uint64, bool) {
	id, ok := sc.senderIDs[string(addr)]
	return id, ok
}
func (sc *sendersBatch) getOrCreateID(addr []byte) (uint64, bool) {
	_, traced := sc.tracedSenders[string(addr)]
	id, ok := sc.senderIDs[string(addr)]
	if !ok {
		copyAddr := common.Copy(addr)
		sc.senderID++
		id = sc.senderID
		sc.senderIDs[string(copyAddr)] = id
		sc.senderID2Addr[id] = copyAddr
		if traced {
			log.Info(fmt.Sprintf("TX TRACING: allocated senderID %d to sender %x", id, addr))
		}
	}
	return id, traced
}
func (sc *sendersBatch) info(cacheView kvcache.CacheView, id uint64) (nonce uint64, balance uint256.Int, err error) {
	addr, ok := sc.senderID2Addr[id]
	if !ok {
		panic("must not happen")
	}
	encoded, err := cacheView.Get(addr)
	if err != nil {
		return 0, emptySender.balance, err
	}
	if len(encoded) == 0 {
		return emptySender.nonce, emptySender.balance, nil
	}
	nonce, balance, err = DecodeSender(encoded)
	if err != nil {
		return 0, emptySender.balance, err
	}
	return nonce, balance, nil
}

func (sc *sendersBatch) registerNewSenders(newTxs *TxSlots) (err error) {
	for i, txn := range newTxs.txs {
		txn.senderID, txn.traced = sc.getOrCreateID(newTxs.senders.At(i))
	}
	return nil
}
func (sc *sendersBatch) onNewBlock(stateChanges *remote.StateChangeBatch, unwindTxs, minedTxs TxSlots) error {
	for _, diff := range stateChanges.ChangeBatch {
		for _, change := range diff.Changes { // merge state changes
			addrB := gointerfaces.ConvertH160toAddress(change.Address)
			sc.getOrCreateID(addrB[:])
		}

		for i, txn := range unwindTxs.txs {
			txn.senderID, txn.traced = sc.getOrCreateID(unwindTxs.senders.At(i))
		}

		for i, txn := range minedTxs.txs {
			txn.senderID, txn.traced = sc.getOrCreateID(minedTxs.senders.At(i))
		}
	}
	return nil
}

// BySenderAndNonce - designed to perform most expensive operation in TxPool:
// "recalculate all ephemeral fields of all transactions" by algo
//      - for all senders - iterate over all transactions in nonce growing order
//
// Performane decisions:
//  - All senders stored inside 1 large BTree - because iterate over 1 BTree is faster than over map[senderId]BTree
//  - sortByNonce used as non-pointer wrapper - because iterate over BTree of pointers is 2x slower
type BySenderAndNonce struct {
	tree             *btree.BTree
	search           sortByNonce
	senderIDTxnCount map[uint64]int // count of sender's txns in the pool - may differ from nonce
}

func (b *BySenderAndNonce) nonce(senderID uint64) (nonce uint64, ok bool) {
	s := b.search
	s.metaTx.Tx.senderID = senderID
	s.metaTx.Tx.nonce = math.MaxUint64

	b.tree.DescendLessOrEqual(s, func(i btree.Item) bool {
		mt := i.(sortByNonce).metaTx
		if mt.Tx.senderID == senderID {
			nonce = mt.Tx.nonce
			ok = true
		}
		return false
	})
	return nonce, ok
}
func (b *BySenderAndNonce) ascendAll(f func(*metaTx) bool) {
	b.tree.Ascend(func(i btree.Item) bool {
		mt := i.(sortByNonce).metaTx
		return f(mt)
	})
}
func (b *BySenderAndNonce) ascend(senderID uint64, f func(*metaTx) bool) {
	s := b.search
	s.metaTx.Tx.senderID = senderID
	s.metaTx.Tx.nonce = 0
	b.tree.AscendGreaterOrEqual(s, func(i btree.Item) bool {
		mt := i.(sortByNonce).metaTx
		if mt.Tx.senderID != senderID {
			return false
		}
		return f(mt)
	})
}
func (b *BySenderAndNonce) descend(senderID uint64, f func(*metaTx) bool) {
	s := b.search
	s.metaTx.Tx.senderID = senderID
	s.metaTx.Tx.nonce = math.MaxUint64
	b.tree.DescendLessOrEqual(s, func(i btree.Item) bool {
		mt := i.(sortByNonce).metaTx
		if mt.Tx.senderID != senderID {
			return false
		}
		return f(mt)
	})
}
func (b *BySenderAndNonce) count(senderID uint64) int {
	return b.senderIDTxnCount[senderID]
}
func (b *BySenderAndNonce) hasTxs(senderID uint64) bool {
	has := false
	b.ascend(senderID, func(*metaTx) bool {
		has = true
		return false
	})
	return has
}
func (b *BySenderAndNonce) get(senderID, txNonce uint64) *metaTx {
	s := b.search
	s.metaTx.Tx.senderID = senderID
	s.metaTx.Tx.nonce = txNonce
	if found := b.tree.Get(s); found != nil {
		return found.(sortByNonce).metaTx
	}
	return nil
}

//nolint
func (b *BySenderAndNonce) has(mt *metaTx) bool {
	found := b.tree.Get(sortByNonce{mt})
	return found != nil
}
func (b *BySenderAndNonce) delete(mt *metaTx) {
	if b.tree.Delete(sortByNonce{mt}) != nil {
		senderID := mt.Tx.senderID
		count := b.senderIDTxnCount[senderID]
		if count > 1 {
			b.senderIDTxnCount[senderID] = count - 1
		} else {
			delete(b.senderIDTxnCount, senderID)
		}
	}
}
func (b *BySenderAndNonce) replaceOrInsert(mt *metaTx) *metaTx {
	it := b.tree.ReplaceOrInsert(sortByNonce{mt})
	if it != nil {
		return it.(sortByNonce).metaTx
	}
	b.senderIDTxnCount[mt.Tx.senderID]++
	return nil
}

// PendingPool - is different from other pools - it's best is Slice instead of Heap
// It's more expensive to maintain "slice sort" invariant, but it allow do cheap copy of
// pending.best slice for mining (because we consider txs and metaTx are immutable)
type PendingPool struct {
	limit  int
	t      SubPoolType
	best   *bestSlice
	worst  *WorstQueue
	adding bool
	added  Hashes
}

func NewPendingSubPool(t SubPoolType, limit int) *PendingPool {
	return &PendingPool{limit: limit, t: t, best: &bestSlice{ms: []*metaTx{}}, worst: &WorstQueue{ms: []*metaTx{}}}
}

func (p *PendingPool) resetAddedHashes() {
	p.added = p.added[:0]
	p.adding = true
}
func (p *PendingPool) appendAddedHashes(h Hashes) Hashes {
	h = append(h, p.added...)
	p.adding = false
	return h
}

// bestSlice - is similar to best queue, but with O(n log n) complexity and
// it maintains element.bestIndex field
type bestSlice struct {
	ms             []*metaTx
	pendingBaseFee uint64
}

func (s *bestSlice) Len() int { return len(s.ms) }
func (s *bestSlice) Swap(i, j int) {
	s.ms[i], s.ms[j] = s.ms[j], s.ms[i]
	s.ms[i].bestIndex, s.ms[j].bestIndex = i, j
}
func (s *bestSlice) Less(i, j int) bool { return s.ms[i].better(s.ms[j], s.pendingBaseFee) }
func (s *bestSlice) UnsafeRemove(i *metaTx) {
	s.Swap(i.bestIndex, len(s.ms)-1)
	s.ms[len(s.ms)-1].bestIndex = -1
	s.ms[len(s.ms)-1] = nil
	s.ms = s.ms[:len(s.ms)-1]
}
func (s *bestSlice) UnsafeAdd(i *metaTx) {
	i.bestIndex = len(s.ms)
	s.ms = append(s.ms, i)
}

func (p *PendingPool) EnforceWorstInvariants() {
	heap.Init(p.worst)
}
func (p *PendingPool) EnforceBestInvariants() {
	sort.Sort(p.best)
}

func (p *PendingPool) Best() *metaTx {
	if len(p.best.ms) == 0 {
		return nil
	}
	return p.best.ms[0]
}
func (p *PendingPool) Worst() *metaTx {
	if len(p.worst.ms) == 0 {
		return nil
	}
	return (p.worst.ms)[0]
}
func (p *PendingPool) PopWorst() *metaTx {
	i := heap.Pop(p.worst).(*metaTx)
	p.best.UnsafeRemove(i)
	return i
}
func (p *PendingPool) Updated(mt *metaTx) {
	heap.Fix(p.worst, mt.worstIndex)
}
func (p *PendingPool) Len() int { return len(p.best.ms) }

func (p *PendingPool) Remove(i *metaTx) {
	heap.Remove(p.worst, i.worstIndex)
	p.best.UnsafeRemove(i)
}

func (p *PendingPool) Add(i *metaTx) {
	if p.adding {
		p.added = append(p.added, i.Tx.IdHash[:]...)
	}
	if i.Tx.traced {
		log.Info(fmt.Sprintf("TX TRACING: moved to subpool %s, IdHash=%x, sender=%d", p.t, i.Tx.IdHash, i.Tx.senderID))
	}
	i.currentSubPool = p.t
	heap.Push(p.worst, i)
	p.best.UnsafeAdd(i)
}
func (p *PendingPool) DebugPrint(prefix string) {
	for i, it := range p.best.ms {
		fmt.Printf("%s.best: %d, %d, %d,%d\n", prefix, i, it.subPool, it.bestIndex, it.Tx.nonce)
	}
	for i, it := range p.worst.ms {
		fmt.Printf("%s.worst: %d, %d, %d,%d\n", prefix, i, it.subPool, it.worstIndex, it.Tx.nonce)
	}
}

type SubPool struct {
	limit  int
	t      SubPoolType
	best   *BestQueue
	worst  *WorstQueue
	adding bool
	added  Hashes
}

func NewSubPool(t SubPoolType, limit int) *SubPool {
	return &SubPool{limit: limit, t: t, best: &BestQueue{}, worst: &WorstQueue{}}
}

func (p *SubPool) resetAddedHashes() {
	p.added = p.added[:0]
	p.adding = true
}
func (p *SubPool) appendAddedHashes(h Hashes) Hashes {
	h = append(h, p.added...)
	p.adding = false
	return h
}

func (p *SubPool) EnforceInvariants() {
	heap.Init(p.worst)
	heap.Init(p.best)
}
func (p *SubPool) Best() *metaTx {
	if len(p.best.ms) == 0 {
		return nil
	}
	return p.best.ms[0]
}
func (p *SubPool) Worst() *metaTx {
	if len(p.worst.ms) == 0 {
		return nil
	}
	return p.worst.ms[0]
}
func (p *SubPool) PopBest() *metaTx {
	i := heap.Pop(p.best).(*metaTx)
	heap.Remove(p.worst, i.worstIndex)
	return i
}
func (p *SubPool) PopWorst() *metaTx {
	i := heap.Pop(p.worst).(*metaTx)
	heap.Remove(p.best, i.bestIndex)
	return i
}
func (p *SubPool) Len() int { return p.best.Len() }
func (p *SubPool) Add(i *metaTx) {
	if p.adding {
		p.added = append(p.added, i.Tx.IdHash[:]...)
	}
	if i.Tx.traced {
		log.Info(fmt.Sprintf("TX TRACING: moved to subpool %s, IdHash=%x, sender=%d", p.t, i.Tx.IdHash, i.Tx.senderID))
	}
	i.currentSubPool = p.t
	heap.Push(p.best, i)
	heap.Push(p.worst, i)
}

func (p *SubPool) Remove(i *metaTx) {
	heap.Remove(p.best, i.bestIndex)
	heap.Remove(p.worst, i.worstIndex)
	i.currentSubPool = 0
}

func (p *SubPool) Updated(i *metaTx) {
	heap.Fix(p.best, i.bestIndex)
	heap.Fix(p.worst, i.worstIndex)
}

func (p *SubPool) DebugPrint(prefix string) {
	for i, it := range p.best.ms {
		fmt.Printf("%s.best: %d, %d, %d\n", prefix, i, it.subPool, it.bestIndex)
	}
	for i, it := range p.worst.ms {
		fmt.Printf("%s.worst: %d, %d, %d\n", prefix, i, it.subPool, it.worstIndex)
	}
}

type BestQueue struct {
	ms             []*metaTx
	pendingBastFee uint64
}

func (mt *metaTx) better(than *metaTx, pendingBaseFee uint64) bool {
	subPool := mt.subPool
	thanSubPool := than.subPool
	if mt.minFeeCap >= pendingBaseFee {
		subPool |= EnoughFeeCapBlock
	}
	if than.minFeeCap >= pendingBaseFee {
		thanSubPool |= EnoughFeeCapBlock
	}
	if subPool != thanSubPool {
		return subPool > thanSubPool
	}

	switch mt.currentSubPool {
	case PendingSubPool:
		var effectiveTip, thanEffectiveTip uint64
		if pendingBaseFee <= mt.minFeeCap {
			effectiveTip = min(mt.minFeeCap-pendingBaseFee, mt.minTip)
		}
		if pendingBaseFee <= than.minFeeCap {
			thanEffectiveTip = min(than.minFeeCap-pendingBaseFee, than.minTip)
		}
		if effectiveTip != thanEffectiveTip {
			return effectiveTip > thanEffectiveTip
		}
	case BaseFeeSubPool:
		if mt.minFeeCap != than.minFeeCap {
			return mt.minFeeCap > than.minFeeCap
		}
	case QueuedSubPool:
		if mt.nonceDistance != than.nonceDistance {
			return mt.nonceDistance < than.nonceDistance
		}
		if mt.cumulativeBalanceDistance != than.cumulativeBalanceDistance {
			return mt.cumulativeBalanceDistance < than.cumulativeBalanceDistance
		}
	}
	return mt.timestamp < than.timestamp
}

func (mt *metaTx) worse(than *metaTx, pendingBaseFee uint64) bool {
	subPool := mt.subPool
	thanSubPool := than.subPool
	if mt.minFeeCap >= pendingBaseFee {
		subPool |= EnoughFeeCapBlock
	}
	if than.minFeeCap >= pendingBaseFee {
		thanSubPool |= EnoughFeeCapBlock
	}
	if subPool != thanSubPool {
		return subPool < thanSubPool
	}

	switch mt.currentSubPool {
	case PendingSubPool:
		if mt.minFeeCap != than.minFeeCap {
			return mt.minFeeCap < than.minFeeCap
		}
		if mt.nonceDistance != than.nonceDistance {
			return mt.nonceDistance > than.nonceDistance
		}
		if mt.cumulativeBalanceDistance != than.cumulativeBalanceDistance {
			return mt.cumulativeBalanceDistance > than.cumulativeBalanceDistance
		}
	case BaseFeeSubPool, QueuedSubPool:
		if mt.nonceDistance != than.nonceDistance {
			return mt.nonceDistance > than.nonceDistance
		}
		if mt.cumulativeBalanceDistance != than.cumulativeBalanceDistance {
			return mt.cumulativeBalanceDistance > than.cumulativeBalanceDistance
		}
	}
	return mt.timestamp > than.timestamp
}

func (p BestQueue) Len() int           { return len(p.ms) }
func (p BestQueue) Less(i, j int) bool { return p.ms[i].better(p.ms[j], p.pendingBastFee) }
func (p BestQueue) Swap(i, j int) {
	p.ms[i], p.ms[j] = p.ms[j], p.ms[i]
	p.ms[i].bestIndex = i
	p.ms[j].bestIndex = j
}
func (p *BestQueue) Push(x interface{}) {
	n := len(p.ms)
	item := x.(*metaTx)
	item.bestIndex = n
	p.ms = append(p.ms, item)
}

func (p *BestQueue) Pop() interface{} {
	old := p.ms
	n := len(old)
	item := old[n-1]
	old[n-1] = nil          // avoid memory leak
	item.bestIndex = -1     // for safety
	item.currentSubPool = 0 // for safety
	p.ms = old[0 : n-1]
	return item
}

type WorstQueue struct {
	ms             []*metaTx
	pendingBaseFee uint64
}

func (p WorstQueue) Len() int           { return len(p.ms) }
func (p WorstQueue) Less(i, j int) bool { return p.ms[i].worse(p.ms[j], p.pendingBaseFee) }
func (p WorstQueue) Swap(i, j int) {
	p.ms[i], p.ms[j] = p.ms[j], p.ms[i]
	p.ms[i].worstIndex = i
	p.ms[j].worstIndex = j
}
func (p *WorstQueue) Push(x interface{}) {
	n := len(p.ms)
	item := x.(*metaTx)
	item.worstIndex = n
	p.ms = append(p.ms, x.(*metaTx))
}
func (p *WorstQueue) Pop() interface{} {
	old := p.ms
	n := len(old)
	item := old[n-1]
	old[n-1] = nil          // avoid memory leak
	item.worstIndex = -1    // for safety
	item.currentSubPool = 0 // for safety
	p.ms = old[0 : n-1]
	return item
}

func min(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
