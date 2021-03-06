package sync

import (
	"context"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/kevinms/leakybucket-go"
	"github.com/pkg/errors"
	ethpb "github.com/prysmaticlabs/ethereumapis/eth/v1alpha1"
	"github.com/prysmaticlabs/prysm/beacon-chain/blockchain"
	"github.com/prysmaticlabs/prysm/beacon-chain/cache"
	blockfeed "github.com/prysmaticlabs/prysm/beacon-chain/core/feed/block"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/feed/operation"
	statefeed "github.com/prysmaticlabs/prysm/beacon-chain/core/feed/state"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/helpers"
	"github.com/prysmaticlabs/prysm/beacon-chain/db"
	"github.com/prysmaticlabs/prysm/beacon-chain/operations/attestations"
	"github.com/prysmaticlabs/prysm/beacon-chain/operations/slashings"
	"github.com/prysmaticlabs/prysm/beacon-chain/operations/voluntaryexits"
	"github.com/prysmaticlabs/prysm/beacon-chain/p2p"
	"github.com/prysmaticlabs/prysm/beacon-chain/state/stategen"
	"github.com/prysmaticlabs/prysm/shared"
	"github.com/prysmaticlabs/prysm/shared/runutil"
)

var _ = shared.Service(&Service{})

const allowedBlocksPerSecond = 32.0
const allowedBlocksBurst = 10 * allowedBlocksPerSecond
const seenBlockSize = 1000
const seenAttSize = 10000
const seenExitSize = 100
const seenAttesterSlashingSize = 100
const seenProposerSlashingSize = 100

// Config to set up the regular sync service.
type Config struct {
	P2P                 p2p.P2P
	DB                  db.NoHeadAccessDatabase
	AttPool             attestations.Pool
	ExitPool            *voluntaryexits.Pool
	SlashingPool        *slashings.Pool
	Chain               blockchainService
	InitialSync         Checker
	StateNotifier       statefeed.Notifier
	BlockNotifier       blockfeed.Notifier
	AttestationNotifier operation.Notifier
	StateSummaryCache   *cache.StateSummaryCache
	StateGen            *stategen.State
}

// This defines the interface for interacting with block chain service
type blockchainService interface {
	blockchain.BlockReceiver
	blockchain.HeadFetcher
	blockchain.FinalizationFetcher
	blockchain.ForkFetcher
	blockchain.AttestationReceiver
	blockchain.TimeFetcher
	blockchain.GenesisFetcher
}

// NewRegularSync service.
func NewRegularSync(cfg *Config) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Service{
		ctx:                  ctx,
		cancel:               cancel,
		db:                   cfg.DB,
		p2p:                  cfg.P2P,
		attPool:              cfg.AttPool,
		exitPool:             cfg.ExitPool,
		slashingPool:         cfg.SlashingPool,
		chain:                cfg.Chain,
		initialSync:          cfg.InitialSync,
		attestationNotifier:  cfg.AttestationNotifier,
		slotToPendingBlocks:  make(map[uint64]*ethpb.SignedBeaconBlock),
		seenPendingBlocks:    make(map[[32]byte]bool),
		blkRootToPendingAtts: make(map[[32]byte][]*ethpb.SignedAggregateAttestationAndProof),
		stateNotifier:        cfg.StateNotifier,
		blockNotifier:        cfg.BlockNotifier,
		stateSummaryCache:    cfg.StateSummaryCache,
		stateGen:             cfg.StateGen,
		blocksRateLimiter:    leakybucket.NewCollector(allowedBlocksPerSecond, allowedBlocksBurst, false /* deleteEmptyBuckets */),
	}

	r.registerRPCHandlers()
	go r.registerSubscribers()

	return r
}

// Service is responsible for handling all run time p2p related operations as the
// main entry point for network messages.
type Service struct {
	ctx                       context.Context
	cancel                    context.CancelFunc
	p2p                       p2p.P2P
	db                        db.NoHeadAccessDatabase
	attPool                   attestations.Pool
	exitPool                  *voluntaryexits.Pool
	slashingPool              *slashings.Pool
	chain                     blockchainService
	slotToPendingBlocks       map[uint64]*ethpb.SignedBeaconBlock
	seenPendingBlocks         map[[32]byte]bool
	blkRootToPendingAtts      map[[32]byte][]*ethpb.SignedAggregateAttestationAndProof
	pendingAttsLock           sync.RWMutex
	pendingQueueLock          sync.RWMutex
	chainStarted              bool
	initialSync               Checker
	validateBlockLock         sync.RWMutex
	stateNotifier             statefeed.Notifier
	blockNotifier             blockfeed.Notifier
	blocksRateLimiter         *leakybucket.Collector
	attestationNotifier       operation.Notifier
	seenBlockLock             sync.RWMutex
	seenBlockCache            *lru.Cache
	seenAttestationLock       sync.RWMutex
	seenAttestationCache      *lru.Cache
	seenExitLock              sync.RWMutex
	seenExitCache             *lru.Cache
	seenProposerSlashingLock  sync.RWMutex
	seenProposerSlashingCache *lru.Cache
	seenAttesterSlashingLock  sync.RWMutex
	seenAttesterSlashingCache *lru.Cache
	stateSummaryCache         *cache.StateSummaryCache
	stateGen                  *stategen.State
}

// Start the regular sync service.
func (r *Service) Start() {
	if err := r.initCaches(); err != nil {
		panic(err)
	}

	r.p2p.AddConnectionHandler(r.reValidatePeer)
	r.p2p.AddDisconnectionHandler(r.removeDisconnectedPeerStatus)
	r.p2p.AddPingMethod(r.sendPingRequest)
	r.processPendingBlocksQueue()
	r.processPendingAttsQueue()
	r.maintainPeerStatuses()
	r.resyncIfBehind()

	// Update sync metrics.
	runutil.RunEvery(r.ctx, time.Second*10, r.updateMetrics)
}

// Stop the regular sync service.
func (r *Service) Stop() error {
	defer r.cancel()
	return nil
}

// Status of the currently running regular sync service.
func (r *Service) Status() error {
	if r.chainStarted {
		if r.initialSync.Syncing() {
			return errors.New("waiting for initial sync")
		}
		// If our head slot is on a previous epoch and our peers are reporting their head block are
		// in the most recent epoch, then we might be out of sync.
		if headEpoch := helpers.SlotToEpoch(r.chain.HeadSlot()); headEpoch+1 < helpers.SlotToEpoch(r.chain.CurrentSlot()) &&
			headEpoch+1 < r.p2p.Peers().CurrentEpoch() {
			return errors.New("out of sync")
		}
	}
	return nil
}

// This initializes the caches to update seen beacon objects coming in from the wire
// and prevent DoS.
func (r *Service) initCaches() error {
	blkCache, err := lru.New(seenBlockSize)
	if err != nil {
		return err
	}
	attCache, err := lru.New(seenAttSize)
	if err != nil {
		return err
	}
	exitCache, err := lru.New(seenExitSize)
	if err != nil {
		return err
	}
	attesterSlashingCache, err := lru.New(seenAttesterSlashingSize)
	if err != nil {
		return err
	}
	proposerSlashingCache, err := lru.New(seenProposerSlashingSize)
	if err != nil {
		return err
	}
	r.seenBlockCache = blkCache
	r.seenAttestationCache = attCache
	r.seenExitCache = exitCache
	r.seenAttesterSlashingCache = attesterSlashingCache
	r.seenProposerSlashingCache = proposerSlashingCache

	return nil
}

// Checker defines a struct which can verify whether a node is currently
// synchronizing a chain with the rest of peers in the network.
type Checker interface {
	Syncing() bool
	Status() error
	Resync() error
}
