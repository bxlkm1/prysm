package state

import "time"

const (
	// BlockProcessed is sent after a block has been processed and updated the state database.
	BlockProcessed = iota + 1
	// ChainStarted is sent when enough validators are active to start proposing blocks.
	ChainStarted
	// Initialized is sent when the internal beacon node's state is ready to be accessed.
	Initialized
	// Synced is sent when the beacon node has completed syncing and is ready to participate in the network.
	Synced
)

// BlockProcessedData is the data sent with BlockProcessed events.
type BlockProcessedData struct {
	// Slot is the slot of the processed block.
	Slot uint64
	// BlockRoot is the hash of the processed block.
	BlockRoot [32]byte
	// Verified is true if the block's BLS contents have been verified.
	Verified bool
}

// ChainStartedData is the data sent with ChainStarted events.
type ChainStartedData struct {
	// StartTime is the time at which the chain started.
	StartTime time.Time
}

// SyncedData is the data sent with Synced events.
type SyncedData struct {
	// StartTime is the time at which the chain started.
	StartTime time.Time
}

// InitializedData is the data sent with Initialized events.
type InitializedData struct {
	// StartTime is the time at which the chain started.
	StartTime time.Time
	// GenesisValidatorsRoot represents ssz.HashTreeRoot(state.validators).
	GenesisValidatorsRoot []byte
}
