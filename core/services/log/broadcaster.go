package log

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"

	"github.com/smartcontractkit/chainlink/core/null"
	"gorm.io/gorm"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/tevino/abool"

	"github.com/smartcontractkit/chainlink/core/internal/gethwrappers/generated"
	"github.com/smartcontractkit/chainlink/core/logger"
	"github.com/smartcontractkit/chainlink/core/service"
	"github.com/smartcontractkit/chainlink/core/services/eth"
	httypes "github.com/smartcontractkit/chainlink/core/services/headtracker/types"
	"github.com/smartcontractkit/chainlink/core/store/models"
	"github.com/smartcontractkit/chainlink/core/utils"
)

//go:generate mockery --name Broadcaster --output ./mocks/ --case=underscore --structname Broadcaster --filename broadcaster.go
//go:generate mockery --name Listener --output ./mocks/ --case=underscore --structname Listener --filename listener.go

type (
	// The Broadcaster manages log subscription requests for the Chainlink node.  Instead
	// of creating a new subscription for each request, it multiplexes all subscriptions
	// to all of the relevant contracts over a single connection and forwards the logs to the
	// relevant subscribers.
	//
	// In case of node crash and/or restart, the logs will be backfilled from the latest head from DB,
	// for subscribers that are added before all dependents of LogBroadcaster are done.
	//
	// If a subscriber is added after the LogBroadcaster does the initial backfill,
	// then it's possible/likely that the backfill fill only have depth: 1 (from latest head)
	//
	// Of course, these backfilled logs + any new logs will only be sent after the NumConfirmations for given subscriber.
	Broadcaster interface {
		utils.DependentAwaiter
		service.Service
		httypes.HeadTrackable
		ReplayFromBlock(number int64)

		IsConnected() bool
		Register(listener Listener, opts ListenerOpts) (unsubscribe func())

		WasAlreadyConsumed(db *gorm.DB, lb Broadcast) (bool, error)
		MarkConsumed(db *gorm.DB, lb Broadcast) error
		// NOTE: WasAlreadyConsumed and MarkConsumed MUST be used within a single goroutine in order for WasAlreadyConsumed to be accurate
	}

	BroadcasterInTest interface {
		Broadcaster
		BackfillBlockNumber() null.Int64
		TrackedAddressesCount() uint32
	}

	broadcaster struct {
		orm       ORM
		config    Config
		connected *abool.AtomicBool

		// a block number to start backfill from
		backfillBlockNumber null.Int64

		ethSubscriber *ethSubscriber
		registrations *registrations
		logPool       *logPool

		addSubscriber *utils.Mailbox
		rmSubscriber  *utils.Mailbox
		newHeads      *utils.Mailbox

		utils.StartStopOnce
		utils.DependentAwaiter

		chStop                chan struct{}
		wgDone                sync.WaitGroup
		trackedAddressesCount uint32
		replayChannel         chan int64
		highestSavedHead      *models.Head
		lastSeenHeadNumber    int64
	}

	Config interface {
		BlockBackfillDepth() uint64
		BlockBackfillSkip() bool
		EthFinalityDepth() uint
		EthLogBackfillBatchSize() uint32
	}

	ListenerOpts struct {
		Contract common.Address

		// Event types to receive, with value filter for each field in the event
		// No filter or an empty filter for a given field position mean: all values allowed
		// the key should be a result of AbigenLog.Topic() call
		LogsWithTopics map[common.Hash][][]Topic

		ParseLog ParseLogFunc

		// Minimum number of block confirmations before the log is received
		NumConfirmations uint64
	}

	ParseLogFunc func(log types.Log) (generated.AbigenLog, error)

	registration struct {
		listener Listener
		opts     ListenerOpts
	}

	Topic common.Hash
)

var _ Broadcaster = (*broadcaster)(nil)

// NewBroadcaster creates a new instance of the broadcaster
func NewBroadcaster(orm ORM, ethClient eth.Client, config Config, highestSavedHead *models.Head) *broadcaster {
	chStop := make(chan struct{})

	return &broadcaster{
		orm:              orm,
		config:           config,
		connected:        abool.New(),
		ethSubscriber:    newEthSubscriber(ethClient, config, chStop),
		registrations:    newRegistrations(),
		logPool:          newLogPool(),
		addSubscriber:    utils.NewMailbox(0),
		rmSubscriber:     utils.NewMailbox(0),
		newHeads:         utils.NewMailbox(1),
		DependentAwaiter: utils.NewDependentAwaiter(),
		chStop:           chStop,
		highestSavedHead: highestSavedHead,
		replayChannel:    make(chan int64, 1),
	}
}

func (b *broadcaster) Start() error {
	return b.StartOnce("LogBroadcaster", func() error {
		b.wgDone.Add(2)
		go b.awaitInitialSubscribers()
		return nil
	})
}

func (b *broadcaster) BackfillBlockNumber() null.Int64 {
	return b.backfillBlockNumber
}

func (b *broadcaster) TrackedAddressesCount() uint32 {
	return atomic.LoadUint32(&b.trackedAddressesCount)
}

func (b *broadcaster) ReplayFromBlock(number int64) {
	logger.Infof("LogBroadcaster: Replay requested from block number: %v", number)
	select {
	case b.replayChannel <- number:
	default:
	}
}

func (b *broadcaster) Close() error {
	return b.StopOnce("LogBroadcaster", func() error {
		close(b.chStop)
		b.wgDone.Wait()
		return nil
	})
}

func (b *broadcaster) awaitInitialSubscribers() {
	defer b.wgDone.Done()
	for {
		select {
		case <-b.addSubscriber.Notify():
			b.onAddSubscribers()

		case <-b.rmSubscriber.Notify():
			b.onRmSubscribers()

		case <-b.DependentAwaiter.AwaitDependents():
			go b.startResubscribeLoop()
			return

		case <-b.chStop:
			return
		}
	}
}

func (b *broadcaster) Register(listener Listener, opts ListenerOpts) (unsubscribe func()) {
	if len(opts.LogsWithTopics) == 0 {
		logger.Fatal("LogBroadcaster: Must supply at least 1 LogsWithTopics element to Register")
	}

	reg := registration{listener, opts}
	wasOverCapacity := b.addSubscriber.Deliver(reg)
	if wasOverCapacity {
		logger.Error("LogBroadcaster: Subscription mailbox is over capacity - dropped the oldest unprocessed subscription")
	}
	return func() {
		wasOverCapacity := b.rmSubscriber.Deliver(reg)
		if wasOverCapacity {
			logger.Error("LogBroadcaster: Subscription removal mailbox is over capacity - dropped the oldest unprocessed removal")
		}
	}
}

func (b *broadcaster) Connect(head *models.Head) error { return nil }

func (b *broadcaster) OnNewLongestChain(ctx context.Context, head models.Head) {
	wasOverCapacity := b.newHeads.Deliver(head)
	if wasOverCapacity {
		logger.Tracew("LogBroadcaster: Dropped the older head in the mailbox, while inserting latest (which is fine)", "latestBlockNumber", head.Number)
	}
}

func (b *broadcaster) IsConnected() bool {
	return b.connected.IsSet()
}

// The subscription is closed in two cases:
//   - intentionally, when the set of contracts we're listening to changes
//   - on a connection error
//
// This method recreates the subscription in both cases.  In the event of a connection
// error, it attempts to reconnect.  Any time there'b a change in connection state, it
// notifies its subscribers.
func (b *broadcaster) startResubscribeLoop() {
	defer b.wgDone.Done()

	var subscription managedSubscription = newNoopSubscription()
	defer func() { subscription.Unsubscribe() }()

	var chRawLogs chan types.Log
	for {
		logger.Debug("LogBroadcaster: Resubscribing and backfilling logs...")
		addresses, topics := b.registrations.addressesAndTopics()

		newSubscription, abort := b.ethSubscriber.createSubscription(addresses, topics)
		if abort {
			return
		}

		if b.config.BlockBackfillSkip() && b.highestSavedHead != nil {
			logger.Warn("LogBroadcaster: BlockBackfillSkip is set to true, preventing a deep backfill - some earlier chain events might be missed.")
			b.highestSavedHead = nil
		}

		if b.highestSavedHead != nil {
			// The backfill needs to start at an earlier block than the one last saved in DB, to account for:
			// - keeping logs in the in-memory buffers in registration.go
			//   (which will be lost on node restart) for MAX(NumConfirmations of subscribers)
			// - HeadTracker saving the heads to DB asynchronously versus LogBroadcaster, where a head
			//   (or more heads on fast chains) may be saved but not yet processed by LB
			//   using BlockBackfillDepth makes sure the backfill will be dependent on the per-chain configuration
			from := b.highestSavedHead.Number -
				int64(b.registrations.highestNumConfirmations) -
				int64(b.config.BlockBackfillDepth())
			if from < 0 {
				from = 0
			}
			b.backfillBlockNumber = null.NewInt64(from, true)
			b.highestSavedHead = nil
		}

		if b.backfillBlockNumber.Valid {
			logger.Debugw("LogBroadcaster: Using an override as a start of the backfill",
				"blockNumber", b.backfillBlockNumber.Int64,
				"highestNumConfirmations", b.registrations.highestNumConfirmations,
				"blockBackfillDepth", b.config.BlockBackfillDepth(),
			)
		}

		chBackfilledLogs, abort := b.ethSubscriber.backfillLogs(b.backfillBlockNumber, addresses, topics)
		if abort {
			return
		}

		b.backfillBlockNumber.Valid = false

		// Each time this loop runs, chRawLogs is reconstituted as:
		// "remaining logs from last subscription <- backfilled logs <- logs from new subscription"
		// There will be duplicated logs in this channel.  It is the responsibility of subscribers
		// to account for this using the helpers on the Broadcast type.
		chRawLogs = b.appendLogChannel(chRawLogs, chBackfilledLogs)
		chRawLogs = b.appendLogChannel(chRawLogs, newSubscription.Logs())
		subscription.Unsubscribe()
		subscription = newSubscription

		b.connected.Set()

		atomic.StoreUint32(&b.trackedAddressesCount, uint32(len(addresses)))

		shouldResubscribe, err := b.eventLoop(chRawLogs, subscription.Err())
		if err != nil {
			logger.Warnw("LogBroadcaster: Error in the event loop - will reconnect", "err", err)
			b.connected.UnSet()
			continue
		} else if !shouldResubscribe {
			b.connected.UnSet()
			return
		}
	}
}

func (b *broadcaster) eventLoop(chRawLogs <-chan types.Log, chErr <-chan error) (shouldResubscribe bool, _ error) {
	// We debounce requests to subscribe and unsubscribe to avoid making too many
	// RPC calls to the Ethereum node, particularly on startup.
	var needsResubscribe bool
	debounceResubscribe := time.NewTicker(1 * time.Second)
	defer debounceResubscribe.Stop()

	logger.Debug("LogBroadcaster: Starting the event loop")
	for {
		select {
		case rawLog := <-chRawLogs:

			logger.Debugw("LogBroadcaster: Received a log",
				"blockNumber", rawLog.BlockNumber, "blockHash", rawLog.BlockHash, "address", rawLog.Address)

			b.onNewLog(rawLog)

		case <-b.newHeads.Notify():
			b.onNewHeads()

		case err := <-chErr:
			// Note we'll get a message on this channel
			// if the eth node terminates the connection.
			return true, err

		case <-b.addSubscriber.Notify():
			needsResubscribe = b.onAddSubscribers() || needsResubscribe

		case <-b.rmSubscriber.Notify():
			needsResubscribe = b.onRmSubscribers() || needsResubscribe

		case blockNumber := <-b.replayChannel:
			b.backfillBlockNumber.SetValid(blockNumber)
			logger.Debugw("LogBroadcaster: Returning from the event loop to replay logs from specific block number", "blockNumber", blockNumber)
			return true, nil

		case <-debounceResubscribe.C:
			if needsResubscribe {
				logger.Debug("LogBroadcaster: Returning from the event loop to resubscribe")
				return true, nil
			}

		case <-b.chStop:
			return false, nil
		}
	}
}

func (b *broadcaster) onNewLog(log types.Log) {
	b.maybeWarnOnLargeBlockNumberDifference(int64(log.BlockNumber))

	if log.Removed {
		b.logPool.removeLog(log)
		return
	} else if !b.registrations.isAddressRegistered(log.Address) {
		return
	}
	b.logPool.addLog(log)
}

func (b *broadcaster) onNewHeads() {
	var latestHead *models.Head
	for {
		// We only care about the most recent head
		item := b.newHeads.RetrieveLatestAndClear()
		if item == nil {
			break
		}
		head, ok := item.(models.Head)
		if !ok {
			logger.Errorf("expected `models.Head`, got %T", item)
			continue
		}
		latestHead = &head
	}

	// latestHead may sometimes be nil on high rate of heads,
	// when 'b.newHeads.Notify()' receives more times that the number of items in the mailbox
	// Some heads may be missed (which is fine for LogBroadcaster logic) but the latest one in a burst will be received
	if latestHead != nil {
		logger.Debugw("LogBroadcaster: Received head", "blockNumber", latestHead.Number,
			"blockHash", latestHead.Hash, "parentHash", latestHead.ParentHash, "chainLen", latestHead.ChainLength())

		atomic.StoreInt64(&b.lastSeenHeadNumber, latestHead.Number)

		keptLogsDepth := uint64(b.config.EthFinalityDepth())
		if b.registrations.highestNumConfirmations > keptLogsDepth {
			keptLogsDepth = b.registrations.highestNumConfirmations
		}

		latestBlockNum := latestHead.Number
		keptDepth := latestBlockNum - int64(keptLogsDepth)
		if keptDepth < 0 {
			keptDepth = 0
		}

		// if all subscribers requested 0 confirmations, we always get and delete all logs from the pool,
		// without comparing their block numbers to the current head's block number.
		if b.registrations.highestNumConfirmations == 0 {
			logs, lowest, highest := b.logPool.getAndDeleteAll()
			if len(logs) > 0 {
				broadcasts, err := b.orm.FindConsumedLogs(lowest, highest)
				if err != nil {
					logger.Errorf("Failed to query for log broadcasts, %v", err)
					return
				}
				b.registrations.sendLogs(logs, *latestHead, broadcasts)
			}
		} else {
			logs, minBlockNum := b.logPool.getLogsToSend(latestBlockNum)

			if len(logs) > 0 {
				broadcasts, err := b.orm.FindConsumedLogs(minBlockNum, latestBlockNum)
				if err != nil {
					logger.Errorf("LogBroadcaster: Failed to query for log broadcasts, %v", err)
					return
				}

				b.registrations.sendLogs(logs, *latestHead, broadcasts)
			}
			b.logPool.deleteOlderLogs(uint64(keptDepth))
		}
	}
}

func (b *broadcaster) onAddSubscribers() (needsResubscribe bool) {
	for {
		x, exists := b.addSubscriber.Retrieve()
		if !exists {
			break
		}
		reg, ok := x.(registration)
		if !ok {
			logger.Errorf("expected `registration`, got %T", x)
			continue
		}
		logger.Debugw("LogBroadcaster: Subscribing listener", "requiredBlockConfirmations", reg.opts.NumConfirmations, "address", reg.opts.Contract)
		needsResub := b.registrations.addSubscriber(reg)
		if needsResub {
			needsResubscribe = true
		}
	}
	return
}

func (b *broadcaster) onRmSubscribers() (needsResubscribe bool) {
	for {
		x, exists := b.rmSubscriber.Retrieve()
		if !exists {
			break
		}
		reg, ok := x.(registration)
		if !ok {
			logger.Errorf("expected `registration`, got %T", x)
			continue
		}
		logger.Debugw("LogBroadcaster: Unsubscribing listener", "requiredBlockConfirmations", reg.opts.NumConfirmations, "address", reg.opts.Contract)
		needsResub := b.registrations.removeSubscriber(reg)
		if needsResub {
			needsResubscribe = true
		}
	}
	return
}

func (b *broadcaster) appendLogChannel(ch1, ch2 <-chan types.Log) chan types.Log {
	if ch1 == nil && ch2 == nil {
		return nil
	}

	chCombined := make(chan types.Log)

	go func() {
		defer close(chCombined)
		if ch1 != nil {
			for rawLog := range ch1 {
				select {
				case chCombined <- rawLog:
				case <-b.chStop:
					return
				}
			}
		}
		if ch2 != nil {
			for rawLog := range ch2 {
				select {
				case chCombined <- rawLog:
				case <-b.chStop:
					return
				}
			}
		}
	}()

	return chCombined
}

func (b *broadcaster) maybeWarnOnLargeBlockNumberDifference(logBlockNumber int64) {
	lastSeenHeadNumber := atomic.LoadInt64(&b.lastSeenHeadNumber)
	diff := logBlockNumber - lastSeenHeadNumber
	if diff < 0 {
		diff = -diff
	}

	if lastSeenHeadNumber > 0 && diff > 1000 {
		logger.Warnw("LogBroadcaster: Detected a large block number difference between a log and recently seen head. "+
			"This may indicate a problem with data received from the chain or major network delays.",
			"lastSeenHeadNumber", lastSeenHeadNumber, "logBlockNumber", logBlockNumber, "diff", diff)
	}
}

// WasAlreadyConsumed reports whether the given consumer had already consumed the given log
func (b *broadcaster) WasAlreadyConsumed(db *gorm.DB, lb Broadcast) (bool, error) {
	return b.orm.WasBroadcastConsumed(db, lb.RawLog().BlockHash, lb.RawLog().Index, lb.JobID())
}

// MarkConsumed marks the log as having been successfully consumed by the subscriber
func (b *broadcaster) MarkConsumed(db *gorm.DB, lb Broadcast) error {
	return b.orm.MarkBroadcastConsumed(db, lb.RawLog().BlockHash, lb.RawLog().BlockNumber, lb.RawLog().Index, lb.JobID())
}

type NullBroadcaster struct{ ErrMsg string }

func (n *NullBroadcaster) IsConnected() bool { return false }
func (n *NullBroadcaster) Register(listener Listener, opts ListenerOpts) (unsubscribe func()) {
	return func() {}
}

func (n *NullBroadcaster) ReplayFromBlock(number int64) {
}

func (n *NullBroadcaster) BackfillBlockNumber() null.Int64 {
	return null.NewInt64(0, false)
}
func (n *NullBroadcaster) TrackedAddressesCount() uint32 {
	return 0
}
func (n *NullBroadcaster) WasAlreadyConsumed(db *gorm.DB, lb Broadcast) (bool, error) {
	return false, errors.New(n.ErrMsg)
}
func (n *NullBroadcaster) MarkConsumed(db *gorm.DB, lb Broadcast) error {
	return errors.New(n.ErrMsg)
}

func (n *NullBroadcaster) AddDependents(int) {}
func (n *NullBroadcaster) AwaitDependents() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
func (n *NullBroadcaster) DependentReady()                                {}
func (n *NullBroadcaster) Start() error                                   { return nil }
func (n *NullBroadcaster) Close() error                                   { return nil }
func (n *NullBroadcaster) Healthy() error                                 { return nil }
func (n *NullBroadcaster) Ready() error                                   { return nil }
func (n *NullBroadcaster) Connect(*models.Head) error                     { return nil }
func (n *NullBroadcaster) OnNewLongestChain(context.Context, models.Head) {}
