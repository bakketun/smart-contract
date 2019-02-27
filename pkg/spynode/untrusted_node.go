package spynode

import (
	"context"
	"io"
	"net"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/pkg/errors"
	"github.com/tokenized/smart-contract/pkg/spynode/handlers"
	"github.com/tokenized/smart-contract/pkg/spynode/handlers/data"
	handlerstorage "github.com/tokenized/smart-contract/pkg/spynode/handlers/storage"
	"github.com/tokenized/smart-contract/pkg/spynode/logger"
	"github.com/tokenized/smart-contract/pkg/storage"
	"github.com/tokenized/smart-contract/pkg/wire"
)

type UntrustedNode struct {
	address      string
	config       data.Config
	state        *data.UntrustedState
	peers        *handlerstorage.PeerRepository
	blocks       *handlerstorage.BlockRepository
	txs          *handlerstorage.TxRepository
	txTracker    *data.TxTracker
	memPool      *data.MemPool
	handlers     map[string]handlers.CommandHandler
	conn         net.Conn
	outgoing     chan wire.Message
	outgoingOpen bool
	listeners    []handlers.Listener
	txFilters    []handlers.TxFilter
	stopping     bool
	Active       bool // Set to false when connection is closed
	mutex        sync.Mutex
}

func NewUntrustedNode(address string, config data.Config, store storage.Storage, peers *handlerstorage.PeerRepository, blocks *handlerstorage.BlockRepository, txs *handlerstorage.TxRepository, memPool *data.MemPool, listeners []handlers.Listener, txFilters []handlers.TxFilter) *UntrustedNode {
	result := UntrustedNode{
		address:      address,
		config:       config,
		state:        data.NewUntrustedState(),
		peers:        peers,
		blocks:       blocks,
		txs:          txs,
		txTracker:    data.NewTxTracker(),
		memPool:      memPool,
		outgoing:     make(chan wire.Message, 100),
		outgoingOpen: true,
		listeners:    listeners,
		txFilters:    txFilters,
		stopping:     false,
		Active:       true,
	}
	return &result
}

// Runs the node.
// Doesn't stop until there is a failure or Stop() is called.
func (node *UntrustedNode) Run(ctx context.Context) error {
	node.mutex.Lock()
	node.handlers = handlers.NewUntrustedCommandHandlers(ctx, node.state, node.peers, node.blocks, node.txs, node.txTracker, node.memPool, node.listeners, node.txFilters)

	if err := node.connect(); err != nil {
		node.peers.UpdateScore(ctx, node.address, -1)
		node.Active = false
		logger.Log(ctx, logger.Debug, "Connection failed to %s : %s", node.address, err.Error())
		node.mutex.Unlock()
		return err
	}

	// Queue version message to start handshake
	version := buildVersionMsg(node.config.UserAgent, int32(node.blocks.LastHeight()))
	node.outgoing <- version
	node.mutex.Unlock()

	wg := sync.WaitGroup{}
	wg.Add(3)

	go func() {
		defer wg.Done()
		node.monitorIncoming(ctx)
	}()

	go func() {
		defer wg.Done()
		node.monitorRequestTimeouts(ctx)
	}()

	go func() {
		defer wg.Done()
		sendOutgoing(ctx, node.conn, node.outgoing)
	}()

	// Block until goroutines finish as a result of Stop()
	wg.Wait()
	node.Active = false
	return nil
}

func (node *UntrustedNode) Stop() error {
	node.mutex.Lock()
	defer node.mutex.Unlock()
	node.stopping = true

	if node.outgoingOpen {
		close(node.outgoing)
		node.outgoingOpen = false
	}

	return node.disconnect()
}

// Broadcast a tx to the peer
func (node *UntrustedNode) BroadcastTx(ctx context.Context, tx *wire.MsgTx) error {
	node.mutex.Lock()
	defer node.mutex.Unlock()

	if !node.outgoingOpen {
		return errors.New("Node inactive")
	}

	node.outgoing <- tx
	return nil
}

// This is called when a block is being processed.
// It is responsible for any cleanup as a result of a block.
func (node *UntrustedNode) ProcessBlock(ctx context.Context, txids []chainhash.Hash) error {
	node.txTracker.Remove(ctx, txids)
	return nil
}

func (node *UntrustedNode) connect() error {
	node.disconnect()

	conn, err := net.DialTimeout("tcp", node.address, 10*time.Second)
	if err != nil {
		return err
	}

	node.conn = conn
	now := time.Now()
	node.state.ConnectedTime = &now
	return nil
}

func (node *UntrustedNode) disconnect() error {
	if node.conn == nil {
		return nil
	}

	// close the connection, ignoring any errors
	_ = node.conn.Close()

	node.conn = nil
	return nil
}

// Processes incoming messages.
//
// This is a blocking function that will run forever, so it should be run
// in a goroutine.
func (node *UntrustedNode) monitorIncoming(ctx context.Context) {
	for {
		if err := node.check(ctx); err != nil {
			logger.Log(ctx, logger.Warn, "Check failed : %s", err.Error())
			node.Stop()
			break
		}

		// read new messages, blocking
		msg, _, err := wire.ReadMessage(node.conn, wire.ProtocolVersion, MainNetBch)
		if err == io.EOF {
			// Happens when the connection is closed
			logger.Log(ctx, logger.Verbose, "Connection closed")
			node.Stop()
			break
		}
		if err != nil {
			// Happens when the connection is closed
			logger.Log(ctx, logger.Debug, "Failed to read message : %s", err.Error())
			node.Stop()
			break
		}

		if err := handleMessage(ctx, node.handlers, msg, node.outgoing); err != nil {
			node.peers.UpdateScore(ctx, node.address, -1)
			logger.Log(ctx, logger.Warn, "Failed to handle (%s) message : %s", msg.Command(), err.Error())
			node.Stop()
			break
		}
	}
}

// Check state
func (node *UntrustedNode) check(ctx context.Context) error {
	if !node.state.VersionReceived {
		return nil // Still performing handshake
	}

	if !node.state.HandshakeComplete {
		// Send header request to verify chain
		msg, err := buildHeaderRequest(ctx, node.state.ProtocolVersion, node.blocks, handlers.UntrustedHeaderDelta, 10)
		if err != nil {
			return err
		}
		node.outgoing <- msg
		now := time.Now()
		node.state.HeadersRequested = &now
		node.state.HandshakeComplete = true
	}

	// Check sync
	if !node.state.Verified {
		return nil
	}

	if !node.state.ScoreUpdated {
		node.peers.UpdateScore(ctx, node.address, 5)
		node.state.ScoreUpdated = true
	}

	if !node.state.AddressesRequested {
		addresses := wire.NewMsgGetAddr()
		node.outgoing <- addresses
		node.state.AddressesRequested = true
	}

	if !node.state.MemPoolRequested {
		// Send mempool request
		// This tells the peer to send inventory of all tx in their mempool.
		mempool := wire.NewMsgMemPool()
		node.outgoing <- mempool
		node.state.MemPoolRequested = true
	}

	responses, err := node.txTracker.Check(ctx, node.memPool)
	if err != nil {
		return err
	}
	// Queue messages to be sent in response
	for _, response := range responses {
		node.outgoing <- response
	}

	return nil
}

// Monitor for request timeouts
func (node *UntrustedNode) monitorRequestTimeouts(ctx context.Context) {
	for {
		sleepUntilStop(10, &node.stopping) // Only check every 10 seconds

		if err := node.state.CheckTimeouts(); err != nil {
			logger.Log(ctx, logger.Warn, "Timed out : %s", err.Error())
			node.peers.UpdateScore(ctx, node.address, -1)
			node.Stop()
			break
		}

		if node.stopping {
			break
		}
	}
}