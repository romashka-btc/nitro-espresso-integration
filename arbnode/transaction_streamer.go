// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/nitro/blob/master/LICENSE

package arbnode

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	tagged_base64 "github.com/EspressoSystems/espresso-sequencer-go/tagged-base64"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"math/big"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	espressoClient "github.com/EspressoSystems/espresso-sequencer-go/client"
	espressoTypes "github.com/EspressoSystems/espresso-sequencer-go/types"

	"errors"

	"github.com/cockroachdb/pebble"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	flag "github.com/spf13/pflag"
	"github.com/syndtr/goleveldb/leveldb"

	"github.com/offchainlabs/nitro/arbos"
	"github.com/offchainlabs/nitro/arbos/arbostypes"
	"github.com/offchainlabs/nitro/arbutil"
	"github.com/offchainlabs/nitro/broadcaster"
	m "github.com/offchainlabs/nitro/broadcaster/message"
	"github.com/offchainlabs/nitro/execution"
	"github.com/offchainlabs/nitro/staker"
	"github.com/offchainlabs/nitro/util/arbmath"
	"github.com/offchainlabs/nitro/util/sharedmetrics"
	"github.com/offchainlabs/nitro/util/stopwaiter"
)

// TransactionStreamer produces blocks from a node's L1 messages, storing the results in the blockchain and recording their positions
// The streamer is notified when there's new batches to process
type TransactionStreamer struct {
	stopwaiter.StopWaiter

	chainConfig      *params.ChainConfig
	exec             execution.ExecutionSequencer
	execLastMsgCount arbutil.MessageIndex
	validator        *staker.BlockValidator

	db             ethdb.Database
	fatalErrChan   chan<- error
	config         TransactionStreamerConfigFetcher
	snapSyncConfig *SnapSyncConfig

	insertionMutex                  sync.Mutex // cannot be acquired while reorgMutex is held
	reorgMutex                      sync.RWMutex
	espressoTxnsStateInsertionMutex sync.Mutex

	newMessageNotifier     chan struct{}
	newSovereignTxNotifier chan struct{}

	nextAllowedFeedReorgLog time.Time

	broadcasterQueuedMessages            []arbostypes.MessageWithMetadataAndBlockHash
	broadcasterQueuedMessagesPos         uint64
	broadcasterQueuedMessagesActiveReorg bool

	coordinator     *SeqCoordinator
	broadcastServer *broadcaster.Broadcaster
	inboxReader     *InboxReader
	delayedBridge   *DelayedBridge
	espressoClient  *espressoClient.Client
}

type TransactionStreamerConfig struct {
	MaxBroadcasterQueueSize int           `koanf:"max-broadcaster-queue-size"`
	MaxReorgResequenceDepth int64         `koanf:"max-reorg-resequence-depth" reload:"hot"`
	ExecuteMessageLoopDelay time.Duration `koanf:"execute-message-loop-delay" reload:"hot"`

	// Espresso specific fields
	SovereignSequencerEnabled   bool          `koanf:"sovereign-sequencer-enabled"`
	HotShotUrl                  string        `koanf:"hotshot-url"`
	EspressoNamespace           uint64        `koanf:"espresso-namespace"`
	EspressoTxnsPollingInterval time.Duration `koanf:"espresso-txns-polling-interval"`
}

type TransactionStreamerConfigFetcher func() *TransactionStreamerConfig

var DefaultTransactionStreamerConfig = TransactionStreamerConfig{
	MaxBroadcasterQueueSize:     50_000,
	MaxReorgResequenceDepth:     1024,
	ExecuteMessageLoopDelay:     time.Millisecond * 100,
	SovereignSequencerEnabled:   false,
	HotShotUrl:                  "",
	EspressoTxnsPollingInterval: time.Millisecond * 100,
}

var TestTransactionStreamerConfig = TransactionStreamerConfig{
	MaxBroadcasterQueueSize:     10_000,
	MaxReorgResequenceDepth:     128 * 1024,
	ExecuteMessageLoopDelay:     time.Millisecond,
	SovereignSequencerEnabled:   false,
	HotShotUrl:                  "",
	EspressoTxnsPollingInterval: time.Millisecond * 100,
}

func TransactionStreamerConfigAddOptions(prefix string, f *flag.FlagSet) {
	f.Int(prefix+".max-broadcaster-queue-size", DefaultTransactionStreamerConfig.MaxBroadcasterQueueSize, "maximum cache of pending broadcaster messages")
	f.Int64(prefix+".max-reorg-resequence-depth", DefaultTransactionStreamerConfig.MaxReorgResequenceDepth, "maximum number of messages to attempt to resequence on reorg (0 = never resequence, -1 = always resequence)")
	f.Duration(prefix+".execute-message-loop-delay", DefaultTransactionStreamerConfig.ExecuteMessageLoopDelay, "delay when polling calls to execute messages")
	f.Bool(prefix+".sovereign-sequencer-enabled", DefaultTransactionStreamerConfig.SovereignSequencerEnabled, "if true, transactions will be sent to espresso's sovereign sequencer to be notarized by espresso network")
	f.String(prefix+".hotshot-url", DefaultTransactionStreamerConfig.HotShotUrl, "url of the hotshot sequencer")
	f.Uint64(prefix+".espresso-namespace", DefaultTransactionStreamerConfig.EspressoNamespace, "espresso namespace that corresponds the L2 chain")
	f.Duration(prefix+".espresso-txns-polling-interval", DefaultTransactionStreamerConfig.EspressoTxnsPollingInterval, "interval between polling for transactions to be included in the block")
}

func NewTransactionStreamer(
	db ethdb.Database,
	chainConfig *params.ChainConfig,
	exec execution.ExecutionSequencer,
	broadcastServer *broadcaster.Broadcaster,
	fatalErrChan chan<- error,
	config TransactionStreamerConfigFetcher,
	snapSyncConfig *SnapSyncConfig,
) (*TransactionStreamer, error) {
	streamer := &TransactionStreamer{
		exec:               exec,
		chainConfig:        chainConfig,
		db:                 db,
		newMessageNotifier: make(chan struct{}, 1),
		broadcastServer:    broadcastServer,
		fatalErrChan:       fatalErrChan,
		config:             config,
		snapSyncConfig:     snapSyncConfig,
	}

	if config().SovereignSequencerEnabled {
		espressoClient := espressoClient.NewClient(config().HotShotUrl)
		streamer.espressoClient = espressoClient

	}

	err := streamer.cleanupInconsistentState()
	if err != nil {
		return nil, err
	}
	return streamer, nil
}

// Represents a block's hash in the database.
// Necessary because RLP decoder doesn't produce nil values by default.
type blockHashDBValue struct {
	BlockHash *common.Hash `rlp:"nil"`
}

const (
	BlockHashMismatchLogMsg = "BlockHash from feed doesn't match locally computed hash. Check feed source."
)

// Encodes a uint64 as bytes in a lexically sortable manner for database iteration.
// Generally this is only used for database keys, which need sorted.
// A shorter RLP encoding is usually used for database values.
func uint64ToKey(x uint64) []byte {
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, x)
	return data
}

func (s *TransactionStreamer) SetBlockValidator(validator *staker.BlockValidator) {
	if s.Started() {
		panic("trying to set coordinator after start")
	}
	if s.validator != nil {
		panic("trying to set coordinator when already set")
	}
	s.validator = validator
}

func (s *TransactionStreamer) SetSeqCoordinator(coordinator *SeqCoordinator) {
	if s.Started() {
		panic("trying to set coordinator after start")
	}
	if s.coordinator != nil {
		panic("trying to set coordinator when already set")
	}
	s.coordinator = coordinator
}

func (s *TransactionStreamer) SetInboxReaders(inboxReader *InboxReader, delayedBridge *DelayedBridge) {
	if s.Started() {
		panic("trying to set inbox reader after start")
	}
	if s.inboxReader != nil || s.delayedBridge != nil {
		panic("trying to set inbox reader when already set")
	}
	s.inboxReader = inboxReader
	s.delayedBridge = delayedBridge
}

func (s *TransactionStreamer) ChainConfig() *params.ChainConfig {
	return s.chainConfig
}

func (s *TransactionStreamer) cleanupInconsistentState() error {
	// If it doesn't exist yet, set the message count to 0
	hasMessageCount, err := s.db.Has(messageCountKey)
	if err != nil {
		return err
	}
	if !hasMessageCount {
		err := setMessageCount(s.db, 0)
		if err != nil {
			return err
		}
	}
	// TODO remove trailing messageCountToMessage and messageCountToBlockPrefix entries
	return nil
}

func (s *TransactionStreamer) ReorgTo(count arbutil.MessageIndex) error {
	return s.ReorgToAndEndBatch(s.db.NewBatch(), count)
}

func (s *TransactionStreamer) ReorgToAndEndBatch(batch ethdb.Batch, count arbutil.MessageIndex) error {
	s.insertionMutex.Lock()
	defer s.insertionMutex.Unlock()
	err := s.reorg(batch, count, nil)
	if err != nil {
		return err
	}
	err = batch.Write()
	if err != nil {
		return err
	}
	return nil
}

func deleteStartingAt(db ethdb.Database, batch ethdb.Batch, prefix []byte, minKey []byte) error {
	iter := db.NewIterator(prefix, minKey)
	defer iter.Release()
	for iter.Next() {
		err := batch.Delete(iter.Key())
		if err != nil {
			return err
		}
	}
	return iter.Error()
}

// deleteFromRange deletes key ranging from startMinKey(inclusive) to endMinKey(exclusive)
// might have deleted some keys even if returning an error
func deleteFromRange(ctx context.Context, db ethdb.Database, prefix []byte, startMinKey uint64, endMinKey uint64) ([]uint64, error) {
	batch := db.NewBatch()
	startIter := db.NewIterator(prefix, uint64ToKey(startMinKey))
	defer startIter.Release()
	var prunedKeysRange []uint64
	for startIter.Next() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		currentKey := binary.BigEndian.Uint64(bytes.TrimPrefix(startIter.Key(), prefix))
		if currentKey >= endMinKey {
			break
		}
		if len(prunedKeysRange) == 0 || len(prunedKeysRange) == 1 {
			prunedKeysRange = append(prunedKeysRange, currentKey)
		} else {
			prunedKeysRange[1] = currentKey
		}
		err := batch.Delete(startIter.Key())
		if err != nil {
			return nil, err
		}
		if batch.ValueSize() >= ethdb.IdealBatchSize {
			if err := batch.Write(); err != nil {
				return nil, err
			}
			batch.Reset()
		}
	}
	if batch.ValueSize() > 0 {
		if err := batch.Write(); err != nil {
			return nil, err
		}
	}
	return prunedKeysRange, nil
}

// The insertion mutex must be held. This acquires the reorg mutex.
// Note: oldMessages will be empty if reorgHook is nil
func (s *TransactionStreamer) reorg(batch ethdb.Batch, count arbutil.MessageIndex, newMessages []arbostypes.MessageWithMetadataAndBlockHash) error {
	if count == 0 {
		return errors.New("cannot reorg out init message")
	}
	lastDelayedSeqNum, err := s.getPrevPrevDelayedRead(count)
	if err != nil {
		return err
	}
	var oldMessages []*arbostypes.MessageWithMetadata

	targetMsgCount, err := s.GetMessageCount()
	if err != nil {
		return err
	}
	config := s.config()
	maxResequenceMsgCount := count + arbutil.MessageIndex(config.MaxReorgResequenceDepth)
	if config.MaxReorgResequenceDepth >= 0 && maxResequenceMsgCount < targetMsgCount {
		log.Error(
			"unable to re-sequence all old messages because there are too many",
			"reorgingToCount", count,
			"removingMessages", targetMsgCount-count,
			"maxReorgResequenceDepth", config.MaxReorgResequenceDepth,
		)
		targetMsgCount = maxResequenceMsgCount
	}
	for i := count; i < targetMsgCount; i++ {
		oldMessage, err := s.GetMessage(i)
		if err != nil {
			log.Error("unable to lookup old message for re-sequencing", "position", i, "err", err)
			break
		}

		if oldMessage.Message == nil || oldMessage.Message.Header == nil {
			continue
		}

		header := oldMessage.Message.Header

		if header.RequestId != nil {
			// This is a delayed message
			delayedSeqNum := header.RequestId.Big().Uint64()
			if delayedSeqNum+1 != oldMessage.DelayedMessagesRead {
				log.Error("delayed message header RequestId doesn't match database DelayedMessagesRead", "header", oldMessage.Message.Header, "delayedMessagesRead", oldMessage.DelayedMessagesRead)
				continue
			}
			if delayedSeqNum != lastDelayedSeqNum {
				// This is the wrong position for the delayed message
				continue
			}
			if s.inboxReader != nil {
				// this is a delayed message. Should be resequenced if all 3 agree:
				// oldMessage, accumulator stored in tracker, and the message re-read from l1
				expectedAcc, err := s.inboxReader.tracker.GetDelayedAcc(delayedSeqNum)
				if err != nil {
					if !isErrNotFound(err) {
						log.Error("reorg-resequence: failed to read expected accumulator", "err", err)
					}
					continue
				}
				msgBlockNum := new(big.Int).SetUint64(oldMessage.Message.Header.BlockNumber)
				delayedInBlock, err := s.delayedBridge.LookupMessagesInRange(s.GetContext(), msgBlockNum, msgBlockNum, nil)
				if err != nil {
					log.Error("reorg-resequence: failed to serialize old delayed message from database", "err", err)
					continue
				}
				messageFound := false
			delayedInBlockLoop:
				for _, delayedFound := range delayedInBlock {
					if delayedFound.Message.Header.RequestId.Big().Uint64() != delayedSeqNum {
						continue delayedInBlockLoop
					}
					if expectedAcc == delayedFound.AfterInboxAcc() && delayedFound.Message.Equals(oldMessage.Message) {
						messageFound = true
					}
					break delayedInBlockLoop
				}
				if !messageFound {
					continue
				}
			}
			lastDelayedSeqNum++
		}

		oldMessages = append(oldMessages, oldMessage)
	}

	s.reorgMutex.Lock()
	defer s.reorgMutex.Unlock()

	messagesResults, err := s.exec.Reorg(count, newMessages, oldMessages)
	if err != nil {
		return err
	}

	messagesWithComputedBlockHash := make([]arbostypes.MessageWithMetadataAndBlockHash, 0, len(messagesResults))
	for i := 0; i < len(messagesResults); i++ {
		messagesWithComputedBlockHash = append(messagesWithComputedBlockHash, arbostypes.MessageWithMetadataAndBlockHash{
			MessageWithMeta: newMessages[i].MessageWithMeta,
			BlockHash:       &messagesResults[i].BlockHash,
		})
	}
	s.broadcastMessages(messagesWithComputedBlockHash, count)

	if s.validator != nil {
		err = s.validator.Reorg(s.GetContext(), count)
		if err != nil {
			return err
		}
	}

	err = deleteStartingAt(s.db, batch, blockHashInputFeedPrefix, uint64ToKey(uint64(count)))
	if err != nil {
		return err
	}
	err = deleteStartingAt(s.db, batch, messagePrefix, uint64ToKey(uint64(count)))
	if err != nil {
		return err
	}

	return setMessageCount(batch, count)
}

func setMessageCount(batch ethdb.KeyValueWriter, count arbutil.MessageIndex) error {
	countBytes, err := rlp.EncodeToBytes(count)
	if err != nil {
		return err
	}
	err = batch.Put(messageCountKey, countBytes)
	if err != nil {
		return err
	}
	sharedmetrics.UpdateSequenceNumberGauge(count)

	return nil
}

func dbKey(prefix []byte, pos uint64) []byte {
	var key []byte
	key = append(key, prefix...)
	key = append(key, uint64ToKey(pos)...)
	return key
}

func isErrNotFound(err error) bool {
	return errors.Is(err, leveldb.ErrNotFound) || errors.Is(err, pebble.ErrNotFound) || errors.Is(err, memorydb.ErrMemorydbNotFound)
}

// Note: if changed to acquire the mutex, some internal users may need to be updated to a non-locking version.
func (s *TransactionStreamer) GetMessage(seqNum arbutil.MessageIndex) (*arbostypes.MessageWithMetadata, error) {
	key := dbKey(messagePrefix, uint64(seqNum))
	data, err := s.db.Get(key)
	if err != nil {
		return nil, err
	}
	var message arbostypes.MessageWithMetadata
	err = rlp.DecodeBytes(data, &message)
	if err != nil {
		return nil, err
	}

	return &message, nil
}

func (s *TransactionStreamer) getMessageWithMetadataAndBlockHash(seqNum arbutil.MessageIndex) (*arbostypes.MessageWithMetadataAndBlockHash, error) {
	msg, err := s.GetMessage(seqNum)
	if err != nil {
		return nil, err
	}

	// Get block hash.
	// To keep it backwards compatible, since it is possible that a message related
	// to a sequence number exists in the database, but the block hash doesn't.
	key := dbKey(blockHashInputFeedPrefix, uint64(seqNum))
	var blockHash *common.Hash
	data, err := s.db.Get(key)
	if err == nil {
		var blockHashDBVal blockHashDBValue
		err = rlp.DecodeBytes(data, &blockHashDBVal)
		if err != nil {
			return nil, err
		}
		blockHash = blockHashDBVal.BlockHash
	} else if !isErrNotFound(err) {
		return nil, err
	}

	msgWithBlockHash := arbostypes.MessageWithMetadataAndBlockHash{
		MessageWithMeta: *msg,
		BlockHash:       blockHash,
	}
	return &msgWithBlockHash, nil
}

// Note: if changed to acquire the mutex, some internal users may need to be updated to a non-locking version.
func (s *TransactionStreamer) GetMessageCount() (arbutil.MessageIndex, error) {
	posBytes, err := s.db.Get(messageCountKey)
	if err != nil {
		return 0, err
	}
	var pos uint64
	err = rlp.DecodeBytes(posBytes, &pos)
	if err != nil {
		return 0, err
	}
	return arbutil.MessageIndex(pos), nil
}

func (s *TransactionStreamer) GetProcessedMessageCount() (arbutil.MessageIndex, error) {
	msgCount, err := s.GetMessageCount()
	if err != nil {
		return 0, err
	}
	digestedHead, err := s.exec.HeadMessageNumber()
	if err != nil {
		return 0, err
	}
	if msgCount > digestedHead+1 {
		return digestedHead + 1, nil
	}
	return msgCount, nil
}

func (s *TransactionStreamer) AddMessages(pos arbutil.MessageIndex, messagesAreConfirmed bool, messages []arbostypes.MessageWithMetadata) error {
	return s.AddMessagesAndEndBatch(pos, messagesAreConfirmed, messages, nil)
}

func (s *TransactionStreamer) FeedPendingMessageCount() arbutil.MessageIndex {
	pos := atomic.LoadUint64(&s.broadcasterQueuedMessagesPos)
	if pos == 0 {
		return 0
	}

	s.insertionMutex.Lock()
	defer s.insertionMutex.Unlock()
	pos = atomic.LoadUint64(&s.broadcasterQueuedMessagesPos)
	if pos == 0 {
		return 0
	}
	return arbutil.MessageIndex(pos + uint64(len(s.broadcasterQueuedMessages)))
}

func (s *TransactionStreamer) AddBroadcastMessages(feedMessages []*m.BroadcastFeedMessage) error {
	if len(feedMessages) == 0 {
		return nil
	}
	broadcastStartPos := feedMessages[0].SequenceNumber
	var messages []arbostypes.MessageWithMetadataAndBlockHash
	broadcastAfterPos := broadcastStartPos
	for _, feedMessage := range feedMessages {
		if broadcastAfterPos != feedMessage.SequenceNumber {
			return fmt.Errorf("invalid sequence number %v, expected %v", feedMessage.SequenceNumber, broadcastAfterPos)
		}
		if feedMessage.Message.Message == nil || feedMessage.Message.Message.Header == nil {
			return fmt.Errorf("invalid feed message at sequence number %v", feedMessage.SequenceNumber)
		}
		msgWithBlockHash := arbostypes.MessageWithMetadataAndBlockHash{
			MessageWithMeta: feedMessage.Message,
			BlockHash:       feedMessage.BlockHash,
		}
		messages = append(messages, msgWithBlockHash)
		broadcastAfterPos++
	}

	s.insertionMutex.Lock()
	defer s.insertionMutex.Unlock()

	var feedReorg bool
	var err error
	// Skip any messages already in the database
	// prevDelayedRead set to 0 because it's only used to compute the output prevDelayedRead which is not used here
	// Messages from feed are not confirmed, so confirmedMessageCount is 0 and confirmedReorg can be ignored
	dups, feedReorg, oldMsg, err := s.countDuplicateMessages(broadcastStartPos, messages, nil)
	if err != nil {
		return err
	}
	messages = messages[dups:]
	broadcastStartPos += arbutil.MessageIndex(dups)
	if oldMsg != nil {
		s.logReorg(broadcastStartPos, oldMsg, &messages[0].MessageWithMeta, false)
	}
	if len(messages) == 0 {
		// No new messages received
		return nil
	}

	if len(s.broadcasterQueuedMessages) == 0 || (feedReorg && !s.broadcasterQueuedMessagesActiveReorg) {
		// Empty cache or feed different from database, save current feed messages until confirmed L1 messages catch up.
		s.broadcasterQueuedMessages = messages
		atomic.StoreUint64(&s.broadcasterQueuedMessagesPos, uint64(broadcastStartPos))
		s.broadcasterQueuedMessagesActiveReorg = feedReorg
	} else {
		broadcasterQueuedMessagesPos := arbutil.MessageIndex(atomic.LoadUint64(&s.broadcasterQueuedMessagesPos))
		if broadcasterQueuedMessagesPos >= broadcastStartPos {
			// Feed messages older than cache
			s.broadcasterQueuedMessages = messages
			atomic.StoreUint64(&s.broadcasterQueuedMessagesPos, uint64(broadcastStartPos))
			s.broadcasterQueuedMessagesActiveReorg = feedReorg
		} else if broadcasterQueuedMessagesPos+arbutil.MessageIndex(len(s.broadcasterQueuedMessages)) == broadcastStartPos {
			// Feed messages can be added directly to end of cache
			maxQueueSize := s.config().MaxBroadcasterQueueSize
			if maxQueueSize == 0 || len(s.broadcasterQueuedMessages) <= maxQueueSize {
				s.broadcasterQueuedMessages = append(s.broadcasterQueuedMessages, messages...)
			}
			broadcastStartPos = broadcasterQueuedMessagesPos
			// Do not change existing reorg state
		} else {
			if len(s.broadcasterQueuedMessages) > 0 {
				log.Warn(
					"broadcaster queue jumped positions",
					"queuedMessages", len(s.broadcasterQueuedMessages),
					"expectedNextPos", broadcasterQueuedMessagesPos+arbutil.MessageIndex(len(s.broadcasterQueuedMessages)),
					"gotPos", broadcastStartPos,
				)
			}
			s.broadcasterQueuedMessages = messages
			atomic.StoreUint64(&s.broadcasterQueuedMessagesPos, uint64(broadcastStartPos))
			s.broadcasterQueuedMessagesActiveReorg = feedReorg
		}
	}

	if s.broadcasterQueuedMessagesActiveReorg || len(s.broadcasterQueuedMessages) == 0 {
		// Broadcaster never triggered reorg or no messages to add
		return nil
	}

	if broadcastStartPos > 0 {
		_, err := s.GetMessage(broadcastStartPos - 1)
		if err != nil {
			if !isErrNotFound(err) {
				return err
			}
			// Message before current message doesn't exist in database, so don't add current messages yet
			return nil
		}
	}

	err = s.addMessagesAndEndBatchImpl(broadcastStartPos, false, nil, nil)
	if err != nil {
		return fmt.Errorf("error adding pending broadcaster messages: %w", err)
	}

	return nil
}

// AddFakeInitMessage should only be used for testing or running a local dev node
func (s *TransactionStreamer) AddFakeInitMessage() error {
	chainConfigJson, err := json.Marshal(s.chainConfig)
	if err != nil {
		return fmt.Errorf("failed to serialize chain config: %w", err)
	}
	chainIdBytes := arbmath.U256Bytes(s.chainConfig.ChainID)
	msg := append(append(chainIdBytes, 0), chainConfigJson...)
	return s.AddMessages(0, false, []arbostypes.MessageWithMetadata{{
		Message: &arbostypes.L1IncomingMessage{
			Header: &arbostypes.L1IncomingMessageHeader{
				Kind:      arbostypes.L1MessageType_Initialize,
				RequestId: &common.Hash{},
				L1BaseFee: common.Big0,
			},
			L2msg: msg,
		},
		DelayedMessagesRead: 1,
	}})
}

// Used in redis tests
func (s *TransactionStreamer) GetMessageCountSync(t *testing.T) (arbutil.MessageIndex, error) {
	s.insertionMutex.Lock()
	defer s.insertionMutex.Unlock()
	return s.GetMessageCount()
}

func endBatch(batch ethdb.Batch) error {
	if batch == nil {
		return nil
	}
	return batch.Write()
}

func (s *TransactionStreamer) AddMessagesAndEndBatch(pos arbutil.MessageIndex, messagesAreConfirmed bool, messages []arbostypes.MessageWithMetadata, batch ethdb.Batch) error {
	messagesWithBlockHash := make([]arbostypes.MessageWithMetadataAndBlockHash, 0, len(messages))
	for _, message := range messages {
		messagesWithBlockHash = append(messagesWithBlockHash, arbostypes.MessageWithMetadataAndBlockHash{
			MessageWithMeta: message,
		})
	}

	if messagesAreConfirmed {
		// Trim confirmed messages from l1pricedataCache
		s.exec.MarkFeedStart(pos + arbutil.MessageIndex(len(messages)))
		s.reorgMutex.RLock()
		dups, _, _, err := s.countDuplicateMessages(pos, messagesWithBlockHash, &batch)
		s.reorgMutex.RUnlock()
		if err != nil {
			return err
		}
		if dups == len(messages) {
			return endBatch(batch)
		}
		// cant keep reorg lock when catching insertionMutex.
		// we have to re-evaluate all messages
		// happy cases for confirmed messages:
		// 1: were previously in feed. We saved work
		// 2: are new (syncing). We wasted very little work.
	}
	s.insertionMutex.Lock()
	defer s.insertionMutex.Unlock()

	return s.addMessagesAndEndBatchImpl(pos, messagesAreConfirmed, messagesWithBlockHash, batch)
}

func (s *TransactionStreamer) getPrevPrevDelayedRead(pos arbutil.MessageIndex) (uint64, error) {
	if s.snapSyncConfig.Enabled && uint64(pos) == s.snapSyncConfig.PrevBatchMessageCount {
		return s.snapSyncConfig.PrevDelayedRead, nil
	}
	var prevDelayedRead uint64
	if pos > 0 {
		prevMsg, err := s.GetMessage(pos - 1)
		if err != nil {
			return 0, fmt.Errorf("failed to get previous message for pos %d: %w", pos, err)
		}
		prevDelayedRead = prevMsg.DelayedMessagesRead
	}

	return prevDelayedRead, nil
}

func (s *TransactionStreamer) countDuplicateMessages(
	pos arbutil.MessageIndex,
	messages []arbostypes.MessageWithMetadataAndBlockHash,
	batch *ethdb.Batch,
) (int, bool, *arbostypes.MessageWithMetadata, error) {
	curMsg := 0
	for {
		if len(messages) == curMsg {
			break
		}
		key := dbKey(messagePrefix, uint64(pos))
		hasMessage, err := s.db.Has(key)
		if err != nil {
			return 0, false, nil, err
		}
		if !hasMessage {
			break
		}
		haveMessage, err := s.db.Get(key)
		if err != nil {
			return 0, false, nil, err
		}
		nextMessage := messages[curMsg]
		wantMessage, err := rlp.EncodeToBytes(nextMessage.MessageWithMeta)
		if err != nil {
			return 0, false, nil, err
		}
		if !bytes.Equal(haveMessage, wantMessage) {
			// Current message does not exactly match message in database
			var dbMessageParsed arbostypes.MessageWithMetadata

			if err := rlp.DecodeBytes(haveMessage, &dbMessageParsed); err != nil {
				log.Warn("TransactionStreamer: Reorg detected! (failed parsing db message)",
					"pos", pos,
					"err", err,
				)
				return curMsg, true, nil, nil
			}
			var duplicateMessage bool
			if nextMessage.MessageWithMeta.Message != nil {
				if dbMessageParsed.Message.BatchGasCost == nil || nextMessage.MessageWithMeta.Message.BatchGasCost == nil {
					// Remove both of the batch gas costs and see if the messages still differ
					nextMessageCopy := nextMessage.MessageWithMeta
					nextMessageCopy.Message = new(arbostypes.L1IncomingMessage)
					*nextMessageCopy.Message = *nextMessage.MessageWithMeta.Message
					batchGasCostBkup := dbMessageParsed.Message.BatchGasCost
					dbMessageParsed.Message.BatchGasCost = nil
					nextMessageCopy.Message.BatchGasCost = nil
					if reflect.DeepEqual(dbMessageParsed, nextMessageCopy) {
						// Actually this isn't a reorg; only the batch gas costs differed
						duplicateMessage = true
						// If possible - update the message in the database to add the gas cost cache.
						if batch != nil && nextMessage.MessageWithMeta.Message.BatchGasCost != nil {
							if *batch == nil {
								*batch = s.db.NewBatch()
							}
							if err := s.writeMessage(pos, nextMessage, *batch); err != nil {
								return 0, false, nil, err
							}
						}
					}
					dbMessageParsed.Message.BatchGasCost = batchGasCostBkup
				}
				msg := nextMessage.MessageWithMeta.Message
				if arbos.IsEspressoMsg(dbMessageParsed.Message) &&
					arbos.IsEspressoMsg(msg) &&
					!bytes.Equal(msg.L2msg, dbMessageParsed.Message.L2msg) {
					// Check to see if the difference is the existence of block merkle proof in the new message,
					// The batcher can append this on the fly (see AddEspressoBlockMerkleProof).
					// If this is the case, update the database with the block merkle justification.
					_, newJst, err := arbos.ParseEspressoMsg(msg)
					if err != nil {
						return 0, false, nil, err

					}
					_, oldJst, err := arbos.ParseEspressoMsg(dbMessageParsed.Message)
					if err != nil {
						return 0, false, nil, err
					}
					if oldJst.BlockMerkleJustification == nil && newJst.BlockMerkleJustification != nil {
						duplicateMessage = true
						if batch != nil {
							log.Info("Writing new block justification to DB")
							if *batch == nil {
								*batch = s.db.NewBatch()
							}
							if err := s.writeMessage(pos, nextMessage, *batch); err != nil {
								return 0, false, nil, err
							}
						}
					}

				}
			}

			if !duplicateMessage {
				return curMsg, true, &dbMessageParsed, nil
			}
		}

		curMsg++
		pos++
	}

	return curMsg, false, nil, nil
}

func (s *TransactionStreamer) logReorg(pos arbutil.MessageIndex, dbMsg *arbostypes.MessageWithMetadata, newMsg *arbostypes.MessageWithMetadata, confirmed bool) {
	sendLog := confirmed
	if time.Now().After(s.nextAllowedFeedReorgLog) {
		sendLog = true
	}
	if sendLog {
		s.nextAllowedFeedReorgLog = time.Now().Add(time.Minute)
		log.Warn("TransactionStreamer: Reorg detected!",
			"confirmed", confirmed,
			"pos", pos,
			"got-delayed", newMsg.DelayedMessagesRead,
			"got-header", newMsg.Message.Header,
			"db-delayed", dbMsg.DelayedMessagesRead,
			"db-header", dbMsg.Message.Header,
		)
	}

}

func (s *TransactionStreamer) addMessagesAndEndBatchImpl(messageStartPos arbutil.MessageIndex, messagesAreConfirmed bool, messages []arbostypes.MessageWithMetadataAndBlockHash, batch ethdb.Batch) error {
	var confirmedReorg bool
	var oldMsg *arbostypes.MessageWithMetadata
	var lastDelayedRead uint64
	var hasNewConfirmedMessages bool
	var cacheClearLen int

	messagesAfterPos := messageStartPos + arbutil.MessageIndex(len(messages))
	broadcastStartPos := arbutil.MessageIndex(atomic.LoadUint64(&s.broadcasterQueuedMessagesPos))

	if messagesAreConfirmed {
		var duplicates int
		var err error
		duplicates, confirmedReorg, oldMsg, err = s.countDuplicateMessages(messageStartPos, messages, &batch)
		if err != nil {
			return err
		}
		if duplicates > 0 {
			lastDelayedRead = messages[duplicates-1].MessageWithMeta.DelayedMessagesRead
			messages = messages[duplicates:]
			messageStartPos += arbutil.MessageIndex(duplicates)
		}
		if len(messages) > 0 {
			hasNewConfirmedMessages = true
		}
	}

	clearQueueOnSuccess := false
	if (s.broadcasterQueuedMessagesActiveReorg && messageStartPos <= broadcastStartPos) ||
		(!s.broadcasterQueuedMessagesActiveReorg && broadcastStartPos <= messagesAfterPos) {
		// Active broadcast reorg and L1 messages at or before start of broadcast messages
		// Or no active broadcast reorg and broadcast messages start before or immediately after last L1 message
		if messagesAfterPos >= broadcastStartPos {
			broadcastSliceIndex := int(messagesAfterPos - broadcastStartPos)
			messagesOldLen := len(messages)
			if broadcastSliceIndex < len(s.broadcasterQueuedMessages) {
				// Some cached feed messages can be used
				messages = append(messages, s.broadcasterQueuedMessages[broadcastSliceIndex:]...)
			}
			// This calculation gives the exact length of cache which was appended to messages
			cacheClearLen = broadcastSliceIndex + len(messages) - messagesOldLen
		}

		// L1 used or replaced broadcast cache items
		clearQueueOnSuccess = true
	}

	var feedReorg bool
	if !hasNewConfirmedMessages {
		var duplicates int
		var err error
		duplicates, feedReorg, oldMsg, err = s.countDuplicateMessages(messageStartPos, messages, nil)
		if err != nil {
			return err
		}
		if duplicates > 0 {
			lastDelayedRead = messages[duplicates-1].MessageWithMeta.DelayedMessagesRead
			messages = messages[duplicates:]
			messageStartPos += arbutil.MessageIndex(duplicates)
		}
	}
	if oldMsg != nil {
		s.logReorg(messageStartPos, oldMsg, &messages[0].MessageWithMeta, confirmedReorg)
	}

	if feedReorg {
		// Never allow feed to reorg confirmed messages
		// Note that any remaining messages must be feed messages, so we're done here
		return endBatch(batch)
	}

	if lastDelayedRead == 0 {
		var err error
		lastDelayedRead, err = s.getPrevPrevDelayedRead(messageStartPos)
		if err != nil {
			return err
		}
	}

	// Validate delayed message counts of remaining messages
	for i, msg := range messages {
		msgPos := messageStartPos + arbutil.MessageIndex(i)
		diff := msg.MessageWithMeta.DelayedMessagesRead - lastDelayedRead
		if diff != 0 && diff != 1 {
			return fmt.Errorf("attempted to insert jump from %v delayed messages read to %v delayed messages read at message index %v", lastDelayedRead, msg.MessageWithMeta.DelayedMessagesRead, msgPos)
		}
		lastDelayedRead = msg.MessageWithMeta.DelayedMessagesRead
		if msg.MessageWithMeta.Message == nil {
			return fmt.Errorf("attempted to insert nil message at position %v", msgPos)
		}
	}

	if confirmedReorg {
		reorgBatch := s.db.NewBatch()
		err := s.reorg(reorgBatch, messageStartPos, messages)
		if err != nil {
			return err
		}
		err = reorgBatch.Write()
		if err != nil {
			return err
		}
	}
	if len(messages) == 0 {
		return endBatch(batch)
	}

	err := s.writeMessages(messageStartPos, messages, batch)
	if err != nil {
		return err
	}

	if clearQueueOnSuccess {
		// Check if new messages were added at the end of cache, if they were, then dont remove those particular messages
		if len(s.broadcasterQueuedMessages) > cacheClearLen {
			s.broadcasterQueuedMessages = s.broadcasterQueuedMessages[cacheClearLen:]
			atomic.StoreUint64(&s.broadcasterQueuedMessagesPos, uint64(broadcastStartPos)+uint64(cacheClearLen))
		} else {
			s.broadcasterQueuedMessages = s.broadcasterQueuedMessages[:0]
			atomic.StoreUint64(&s.broadcasterQueuedMessagesPos, 0)
		}
		s.broadcasterQueuedMessagesActiveReorg = false
	}

	return nil
}

// The caller must hold the insertionMutex
func (s *TransactionStreamer) ExpectChosenSequencer() error {
	if s.coordinator != nil {
		if !s.coordinator.CurrentlyChosen() {
			return fmt.Errorf("%w: not main sequencer", execution.ErrRetrySequencer)
		}
	}
	return nil
}

func (s *TransactionStreamer) WriteMessageFromSequencer(
	pos arbutil.MessageIndex,
	msgWithMeta arbostypes.MessageWithMetadata,
	msgResult execution.MessageResult,
) error {

	if err := s.ExpectChosenSequencer(); err != nil {
		return err
	}
	if !s.insertionMutex.TryLock() {
		return execution.ErrSequencerInsertLockTaken
	}
	defer s.insertionMutex.Unlock()

	msgCount, err := s.GetMessageCount()
	if err != nil {
		return err
	}

	if msgCount != pos {
		return fmt.Errorf("wrong pos got %d expected %d", pos, msgCount)
	}

	if s.coordinator != nil {
		if err := s.coordinator.SequencingMessage(pos, &msgWithMeta); err != nil {
			return err
		}
	}

	msgWithBlockHash := arbostypes.MessageWithMetadataAndBlockHash{
		MessageWithMeta: msgWithMeta,
		BlockHash:       &msgResult.BlockHash,
	}
	if err := s.writeMessages(pos, []arbostypes.MessageWithMetadataAndBlockHash{msgWithBlockHash}, s.db.NewBatch()); err != nil {
		return err
	}

	s.broadcastMessages([]arbostypes.MessageWithMetadataAndBlockHash{msgWithBlockHash}, pos)
	s.espressoTxnsStateInsertionMutex.Lock()
	defer s.espressoTxnsStateInsertionMutex.Unlock()

	if arbos.IsEspressoMsg(msgWithMeta.Message) {
		err = s.SubmitEspressoTransactionPos(pos, s.db.NewBatch())
		if err != nil {
			return err
		}
	}

	return nil
}

// PauseReorgs until a matching call to ResumeReorgs (may be called concurrently)
func (s *TransactionStreamer) PauseReorgs() {
	s.reorgMutex.RLock()
}

func (s *TransactionStreamer) ResumeReorgs() {
	s.reorgMutex.RUnlock()
}

func (s *TransactionStreamer) PopulateFeedBacklog() error {
	if s.broadcastServer == nil {
		return nil
	}
	return s.inboxReader.tracker.PopulateFeedBacklog(s.broadcastServer)
}

func (s *TransactionStreamer) writeMessage(pos arbutil.MessageIndex, msg arbostypes.MessageWithMetadataAndBlockHash, batch ethdb.Batch) error {
	// write message with metadata
	key := dbKey(messagePrefix, uint64(pos))
	msgBytes, err := rlp.EncodeToBytes(msg.MessageWithMeta)
	if err != nil {
		return err
	}
	if err := batch.Put(key, msgBytes); err != nil {
		return err
	}

	// write block hash
	blockHashDBVal := blockHashDBValue{
		BlockHash: msg.BlockHash,
	}
	key = dbKey(blockHashInputFeedPrefix, uint64(pos))
	msgBytes, err = rlp.EncodeToBytes(blockHashDBVal)
	if err != nil {
		return err
	}
	return batch.Put(key, msgBytes)
}

func (s *TransactionStreamer) broadcastMessages(
	msgs []arbostypes.MessageWithMetadataAndBlockHash,
	pos arbutil.MessageIndex,
) {
	if s.broadcastServer == nil {
		return
	}
	if err := s.broadcastServer.BroadcastMessages(msgs, pos); err != nil {
		log.Error("failed broadcasting messages", "pos", pos, "err", err)
	}
}

// The mutex must be held, and pos must be the latest message count.
// `batch` may be nil, which initializes a new batch. The batch is closed out in this function.
func (s *TransactionStreamer) writeMessages(pos arbutil.MessageIndex, messages []arbostypes.MessageWithMetadataAndBlockHash, batch ethdb.Batch) error {
	if batch == nil {
		batch = s.db.NewBatch()
	}
	for i, msg := range messages {
		err := s.writeMessage(pos+arbutil.MessageIndex(i), msg, batch)
		if err != nil {
			return err
		}
	}

	err := setMessageCount(batch, pos+arbutil.MessageIndex(len(messages)))
	if err != nil {
		return err
	}
	err = batch.Write()
	if err != nil {
		return err
	}

	select {
	case s.newMessageNotifier <- struct{}{}:
	default:
	}

	return nil
}

// TODO: eventually there will be a table maintained by txStreamer itself
func (s *TransactionStreamer) ResultAtCount(count arbutil.MessageIndex) (*execution.MessageResult, error) {
	if count == 0 {
		return &execution.MessageResult{}, nil
	}
	return s.exec.ResultAtPos(count - 1)
}

func (s *TransactionStreamer) checkResult(msgResult *execution.MessageResult, expectedBlockHash *common.Hash) {
	if expectedBlockHash == nil {
		return
	}
	if msgResult.BlockHash != *expectedBlockHash {
		log.Error(
			BlockHashMismatchLogMsg,
			"expected", expectedBlockHash,
			"actual", msgResult.BlockHash,
		)
		return
	}
}

// exposed for testing
// return value: true if should be called again immediately
func (s *TransactionStreamer) ExecuteNextMsg(ctx context.Context, exec execution.ExecutionSequencer) bool {
	if ctx.Err() != nil {
		return false
	}
	if !s.reorgMutex.TryRLock() {
		return false
	}
	defer s.reorgMutex.RUnlock()
	prevMessageCount := s.execLastMsgCount
	msgCount, err := s.GetMessageCount()
	if err != nil {
		log.Error("feedOneMsg failed to get message count", "err", err)
		return false
	}
	s.execLastMsgCount = msgCount
	pos, err := s.exec.HeadMessageNumber()
	if err != nil {
		log.Error("feedOneMsg failed to get exec engine message count", "err", err)
		return false
	}
	pos++
	if pos >= msgCount {
		return false
	}
	msgAndBlockHash, err := s.getMessageWithMetadataAndBlockHash(pos)
	if err != nil {
		log.Error("feedOneMsg failed to readMessage", "err", err, "pos", pos)
		return false
	}
	var msgForPrefetch *arbostypes.MessageWithMetadata
	if pos+1 < msgCount {
		msg, err := s.GetMessage(pos + 1)
		if err != nil {
			log.Error("feedOneMsg failed to readMessage", "err", err, "pos", pos+1)
			return false
		}
		msgForPrefetch = msg
	}
	msgResult, err := s.exec.DigestMessage(pos, &msgAndBlockHash.MessageWithMeta, msgForPrefetch)
	if err != nil {
		logger := log.Warn
		if prevMessageCount < msgCount {
			logger = log.Debug
		}
		logger("feedOneMsg failed to send message to execEngine", "err", err, "pos", pos)
		return false
	}

	s.checkResult(msgResult, msgAndBlockHash.BlockHash)

	msgWithBlockHash := arbostypes.MessageWithMetadataAndBlockHash{
		MessageWithMeta: msgAndBlockHash.MessageWithMeta,
		BlockHash:       &msgResult.BlockHash,
	}
	s.broadcastMessages([]arbostypes.MessageWithMetadataAndBlockHash{msgWithBlockHash}, pos)
	return pos+1 < msgCount
}

func (s *TransactionStreamer) executeMessages(ctx context.Context, ignored struct{}) time.Duration {
	if s.ExecuteNextMsg(ctx, s.exec) {
		return 0
	}
	return s.config().ExecuteMessageLoopDelay
}

func (s *TransactionStreamer) PollSubmittedTransactionForFinality(ctx context.Context) time.Duration {
	submittedTxnPos, err := s.getEspressoSubmittedPos()
	if err != nil {
		log.Warn("submitted pos not found", "err", err)
		return s.config().EspressoTxnsPollingInterval
	}
	submittedTxHash, err := s.getEspressoSubmittedHash()
	if err != nil {
		log.Warn("submitted hash not found", "err", err)
		return s.config().EspressoTxnsPollingInterval
	}
	data, err := s.espressoClient.FetchTransactionByHash(ctx, &submittedTxHash)
	if err != nil {
		log.Error("failed to fetch the submitted transaction hash", "err", err, "hash", submittedTxHash.String())
		return s.config().EspressoTxnsPollingInterval
	}
	// get the message at the submitted txn position
	msg, err := s.getMessageWithMetadataAndBlockHash(submittedTxnPos)
	if err != nil {
		log.Error("failed to get espresso message at submitted txn pos", "err", err)
		return s.config().EspressoTxnsPollingInterval
	}
	// parse the message to get the transaction bytes and the justification
	txns, jst, err := arbos.ParseEspressoMsg(msg.MessageWithMeta.Message)
	if err != nil {
		log.Error("failed to parse espresso message", "err", err)
		return s.config().EspressoTxnsPollingInterval
	}

	espressoHeader, err := s.espressoClient.FetchHeaderByHeight(ctx, data.BlockHeight)
	if err != nil {
		log.Error("espresso: failed to fetch header by height ", "err", err)
		return s.config().EspressoTxnsPollingInterval
	}

	// fetch the namespace proof and vid common. Should use a more efficient way
	resp, err := s.espressoClient.FetchTransactionsInBlock(ctx, data.BlockHeight, s.config().EspressoNamespace)
	if err != nil {
		log.Warn("failed to fetch the transactions in block, will retry", "err", err)
		return s.config().EspressoTxnsPollingInterval
	}

	// Filling in the block justification with the header
	jst.Header = &espressoHeader
	jst.Proof = &resp.Proof
	jst.VidCommon = &resp.VidCommon

	// create a new message with the header and the txn and the updated block justification
	newMsg, err := arbos.MessageFromEspressoSovereignTx(txns[0], jst, msg.MessageWithMeta.Message.Header)
	if err != nil {
		log.Error("failed to parse espresso message", "err", err)
		return s.config().EspressoTxnsPollingInterval
	}
	msg.MessageWithMeta.Message = &newMsg

	s.espressoTxnsStateInsertionMutex.Lock()
	defer s.espressoTxnsStateInsertionMutex.Unlock()

	batch := s.db.NewBatch()
	err = s.writeMessage(submittedTxnPos, *msg, batch)
	if err != nil {
		log.Warn("failed to write the submitted txn pos to db ", "err", err)
		return s.config().EspressoTxnsPollingInterval
	}
	err = s.setEspressoSubmittedPos(batch, nil)
	if err != nil {
		log.Warn("failed to set the submitted pos to nil", "err", err)
		return s.config().EspressoTxnsPollingInterval
	}
	err = s.setEspressoSubmittedHash(batch, tagged_base64.TaggedBase64{})
	if err != nil {
		log.Warn("failed to set the submitted hash to nil", "err", err)
		return s.config().EspressoTxnsPollingInterval
	}

	err = batch.Write()
	if err != nil {
		log.Error("failed to write to db", "err", err)
		return s.config().EspressoTxnsPollingInterval
	}
	return time.Duration(0)
}

func (s *TransactionStreamer) getEspressoSubmittedPos() (arbutil.MessageIndex, error) {

	posBytes, err := s.db.Get(espressoSubmittedPos)
	if err != nil {
		return 0, err
	}

	var pos arbutil.MessageIndex
	err = rlp.DecodeBytes(posBytes, &pos)

	if err != nil {
		return 0, err
	}

	return arbutil.MessageIndex(pos), nil
}

func (s *TransactionStreamer) getEspressoSubmittedHash() (espressoTypes.TaggedBase64, error) {
	posBytes, err := s.db.Get(espressoSubmittedHash)
	if err != nil {
		return espressoTypes.TaggedBase64{}, err
	}
	var hash string
	err = rlp.DecodeBytes(posBytes, &hash)
	if err != nil {
		return espressoTypes.TaggedBase64{}, err
	}
	hashParsed, err := tagged_base64.Parse(hash)
	if hashParsed == nil {
		return espressoTypes.TaggedBase64{}, err
	}
	return espressoTypes.TaggedBase64(*hashParsed), nil
}

func (s *TransactionStreamer) getEspressoPendingTxnsPos() ([]*arbutil.MessageIndex, error) {

	pendingTxnsBytes, err := s.db.Get(espressoPendingTxnsPositions)
	if err != nil {
		return nil, err
	}
	var pendingTxnsPos []*arbutil.MessageIndex
	err = rlp.DecodeBytes(pendingTxnsBytes, &pendingTxnsPos)
	if err != nil {
		return nil, err
	}
	return pendingTxnsPos, nil
}

func (s *TransactionStreamer) setEspressoSubmittedPos(batch ethdb.KeyValueWriter, pos *arbutil.MessageIndex) error {
	// if pos is nil, delete the key
	if pos == nil {
		err := batch.Delete(espressoSubmittedPos)
		return err
	}

	posBytes, err := rlp.EncodeToBytes(pos)
	if err != nil {
		return err
	}
	err = batch.Put(espressoSubmittedPos, posBytes)
	if err != nil {
		return err
	}

	return nil
}

func (s *TransactionStreamer) setEspressoSubmittedHash(batch ethdb.KeyValueWriter, hash espressoTypes.TaggedBase64) error {
	// if hash is nil, delete the key
	if hash.Value() == nil {
		err := batch.Delete(espressoSubmittedHash)
		return err
	}

	hashBytes, err := rlp.EncodeToBytes(hash.String())
	if err != nil {
		return err
	}
	err = batch.Put(espressoSubmittedHash, hashBytes)
	if err != nil {
		return err
	}

	return nil
}

func (s *TransactionStreamer) setEspressoPendingTxnsPos(batch ethdb.KeyValueWriter, pos []*arbutil.MessageIndex) error {
	if pos == nil {
		err := batch.Delete(espressoPendingTxnsPositions)
		return err
	}

	posBytes, err := rlp.EncodeToBytes(pos)
	if err != nil {
		return err
	}
	err = batch.Put(espressoPendingTxnsPositions, posBytes)
	if err != nil {
		return err

	}
	return nil
}

func (s *TransactionStreamer) SubmitEspressoTransactionPos(pos arbutil.MessageIndex, batch ethdb.Batch) error {
	pendingTxnsPos, err := s.getEspressoPendingTxnsPos()
	if err != nil && !isErrNotFound(err) {
		log.Error("failed to get the pending txns position", "err", err)
		return err
	}

	if err != nil && isErrNotFound(err) {
		// if the key doesn't exist, create a new array with the pos
		pendingTxnsPos = []*arbutil.MessageIndex{&pos}
	} else {
		pendingTxnsPos = append(pendingTxnsPos, &pos)
	}
	err = s.setEspressoPendingTxnsPos(batch, pendingTxnsPos)
	if err != nil {
		log.Error("failed to set the pending txns", "err", err)
		return err
	}

	err = batch.Write()
	if err != nil {
		return err
	}

	return nil
}

func (s *TransactionStreamer) submitEspressoTransactions(ctx context.Context, ignored struct{}) time.Duration {

	_, err := s.getEspressoSubmittedPos()

	if err != nil && !isErrNotFound(err) {
		log.Warn("error getting submitted pos", "err", err)
		return s.config().EspressoTxnsPollingInterval
	}

	if err == nil {
		if s.PollSubmittedTransactionForFinality(ctx) != time.Duration(0) {
			return s.config().EspressoTxnsPollingInterval
		}
	}

	pendingTxnsPos, err := s.getEspressoPendingTxnsPos()
	if err != nil {
		log.Warn("failed to get pending txns position", "err", err)
		return s.config().EspressoTxnsPollingInterval
	}

	if len(pendingTxnsPos) > 0 {
		// get the message at the pending txn position
		msg, err := s.GetMessage(*pendingTxnsPos[0])
		if err != nil {
			log.Error("failed to get espresso submitted pos", "err", err)
			return s.config().EspressoTxnsPollingInterval
		}
		bytes, _, err := arbos.ParseEspressoMsg(msg.Message)
		if err != nil {
			log.Error("failed to parse espresso message before submitting", "err", err)
			return s.config().EspressoTxnsPollingInterval
		}

		espressoTx := espressoTypes.Transaction{
			Payload:   bytes[0],
			Namespace: s.config().EspressoNamespace,
		}

		log.Info("submitting transaction to espresso using sovereign sequencer", "tx", espressoTx)

		hash, err := s.espressoClient.SubmitTransaction(ctx, espressoTypes.Transaction{
			Payload:   bytes[0],
			Namespace: s.config().EspressoNamespace,
		})

		if err != nil {
			log.Error("failed to submit transaction to espresso", "err", err)
			return s.config().EspressoTxnsPollingInterval
		}

		s.espressoTxnsStateInsertionMutex.Lock()
		defer s.espressoTxnsStateInsertionMutex.Unlock()

		batch := s.db.NewBatch()
		err = s.setEspressoSubmittedPos(batch, pendingTxnsPos[0])
		if err != nil {
			log.Error("failed to set the submitted txn pos", "err", err)
			return s.config().EspressoTxnsPollingInterval
		}
		pendingTxnsPos = pendingTxnsPos[1:]
		err = s.setEspressoPendingTxnsPos(batch, pendingTxnsPos)
		if err != nil {
			log.Error("failed to set the pending txns", "err", err)
			return s.config().EspressoTxnsPollingInterval
		}
		err = s.setEspressoSubmittedHash(batch, *hash)
		if err != nil {
			log.Error("failed to set the submitted hash", "err", err)
			return s.config().EspressoTxnsPollingInterval
		}

		err = batch.Write()
		if err != nil {
			log.Error("failed to write to db", "err", err)
			return s.config().EspressoTxnsPollingInterval
		}
	}

	return s.config().EspressoTxnsPollingInterval
}

func (s *TransactionStreamer) Start(ctxIn context.Context) error {
	s.StopWaiter.Start(ctxIn, s)

	if s.config().SovereignSequencerEnabled {
		err := stopwaiter.CallIterativelyWith[struct{}](&s.StopWaiterSafe, s.submitEspressoTransactions, s.newSovereignTxNotifier)
		if err != nil {
			return err
		}
	}

	return stopwaiter.CallIterativelyWith[struct{}](&s.StopWaiterSafe, s.executeMessages, s.newMessageNotifier)
}
