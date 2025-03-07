package routing

import (
	"bytes"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/coreos/bbolt"
	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"

	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/multimutex"
	"github.com/lightningnetwork/lnd/routing/chainview"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
)

const (
	// DefaultPayAttemptTimeout is the default payment attempt timeout. The
	// payment attempt timeout defines the duration after which we stop
	// trying more routes for a payment.
	DefaultPayAttemptTimeout = time.Duration(time.Second * 60)

	// DefaultChannelPruneExpiry is the default duration used to determine
	// if a channel should be pruned or not.
	DefaultChannelPruneExpiry = time.Duration(time.Hour * 24 * 14)
)

var (
	// ErrRouterShuttingDown is returned if the router is in the process of
	// shutting down.
	ErrRouterShuttingDown = fmt.Errorf("router shutting down")
)

// ChannelGraphSource represents the source of information about the topology
// of the lightning network. It's responsible for the addition of nodes, edges,
// applying edge updates, and returning the current block height with which the
// topology is synchronized.
type ChannelGraphSource interface {
	// AddNode is used to add information about a node to the router
	// database. If the node with this pubkey is not present in an existing
	// channel, it will be ignored.
	AddNode(node *channeldb.LightningNode) error

	// AddEdge is used to add edge/channel to the topology of the router,
	// after all information about channel will be gathered this
	// edge/channel might be used in construction of payment path.
	AddEdge(edge *channeldb.ChannelEdgeInfo) error

	// AddProof updates the channel edge info with proof which is needed to
	// properly announce the edge to the rest of the network.
	AddProof(chanID lnwire.ShortChannelID, proof *channeldb.ChannelAuthProof) error

	// UpdateEdge is used to update edge information, without this message
	// edge considered as not fully constructed.
	UpdateEdge(policy *channeldb.ChannelEdgePolicy) error

	// IsStaleNode returns true if the graph source has a node announcement
	// for the target node with a more recent timestamp. This method will
	// also return true if we don't have an active channel announcement for
	// the target node.
	IsStaleNode(node route.Vertex, timestamp time.Time) bool

	// IsPublicNode determines whether the given vertex is seen as a public
	// node in the graph from the graph's source node's point of view.
	IsPublicNode(node route.Vertex) (bool, error)

	// IsKnownEdge returns true if the graph source already knows of the
	// passed channel ID either as a live or zombie edge.
	IsKnownEdge(chanID lnwire.ShortChannelID) bool

	// IsStaleEdgePolicy returns true if the graph source has a channel
	// edge for the passed channel ID (and flags) that have a more recent
	// timestamp.
	IsStaleEdgePolicy(chanID lnwire.ShortChannelID, timestamp time.Time,
		flags lnwire.ChanUpdateChanFlags) bool

	// MarkEdgeLive clears an edge from our zombie index, deeming it as
	// live.
	MarkEdgeLive(chanID lnwire.ShortChannelID) error

	// ForAllOutgoingChannels is used to iterate over all channels
	// emanating from the "source" node which is the center of the
	// star-graph.
	ForAllOutgoingChannels(cb func(c *channeldb.ChannelEdgeInfo,
		e *channeldb.ChannelEdgePolicy) error) error

	// CurrentBlockHeight returns the block height from POV of the router
	// subsystem.
	CurrentBlockHeight() (uint32, error)

	// GetChannelByID return the channel by the channel id.
	GetChannelByID(chanID lnwire.ShortChannelID) (*channeldb.ChannelEdgeInfo,
		*channeldb.ChannelEdgePolicy, *channeldb.ChannelEdgePolicy, error)

	// FetchLightningNode attempts to look up a target node by its identity
	// public key. channeldb.ErrGraphNodeNotFound is returned if the node
	// doesn't exist within the graph.
	FetchLightningNode(route.Vertex) (*channeldb.LightningNode, error)

	// ForEachNode is used to iterate over every node in the known graph.
	ForEachNode(func(node *channeldb.LightningNode) error) error

	// ForEachChannel is used to iterate over every channel in the known
	// graph.
	ForEachChannel(func(chanInfo *channeldb.ChannelEdgeInfo,
		e1, e2 *channeldb.ChannelEdgePolicy) error) error
}

// PaymentAttemptDispatcher is used by the router to send payment attempts onto
// the network, and receive their results.
type PaymentAttemptDispatcher interface {
	// SendHTLC is a function that directs a link-layer switch to
	// forward a fully encoded payment to the first hop in the route
	// denoted by its public key. A non-nil error is to be returned if the
	// payment was unsuccessful.
	SendHTLC(firstHop lnwire.ShortChannelID,
		paymentID uint64,
		htlcAdd *lnwire.UpdateAddHTLC) error

	// GetPaymentResult returns the the result of the payment attempt with
	// the given paymentID. The method returns a channel where the payment
	// result will be sent when available, or an error is encountered
	// during forwarding. When a result is received on the channel, the
	// HTLC is guaranteed to no longer be in flight. The switch shutting
	// down is signaled by closing the channel. If the paymentID is
	// unknown, ErrPaymentIDNotFound will be returned.
	GetPaymentResult(paymentID uint64, paymentHash lntypes.Hash,
		deobfuscator htlcswitch.ErrorDecrypter) (
		<-chan *htlcswitch.PaymentResult, error)
}

// PaymentSessionSource is an interface that defines a source for the router to
// retrive new payment sessions.
type PaymentSessionSource interface {
	// NewPaymentSession creates a new payment session that will produce
	// routes to the given target. An optional set of routing hints can be
	// provided in order to populate additional edges to explore when
	// finding a path to the payment's destination.
	NewPaymentSession(routeHints [][]zpay32.HopHint,
		target route.Vertex) (PaymentSession, error)

	// NewPaymentSessionForRoute creates a new paymentSession instance that
	// is just used for failure reporting to missioncontrol, and will only
	// attempt the given route.
	NewPaymentSessionForRoute(preBuiltRoute *route.Route) PaymentSession

	// NewPaymentSessionEmpty creates a new paymentSession instance that is
	// empty, and will be exhausted immediately. Used for failure reporting
	// to missioncontrol for resumed payment we don't want to make more
	// attempts for.
	NewPaymentSessionEmpty() PaymentSession
}

// FeeSchema is the set fee configuration for a Lightning Node on the network.
// Using the coefficients described within the schema, the required fee to
// forward outgoing payments can be derived.
type FeeSchema struct {
	// BaseFee is the base amount of milli-satoshis that will be chained
	// for ANY payment forwarded.
	BaseFee lnwire.MilliSatoshi

	// FeeRate is the rate that will be charged for forwarding payments.
	// This value should be interpreted as the numerator for a fraction
	// (fixed point arithmetic) whose denominator is 1 million. As a result
	// the effective fee rate charged per mSAT will be: (amount *
	// FeeRate/1,000,000).
	FeeRate uint32
}

// ChannelPolicy holds the parameters that determine the policy we enforce
// when forwarding payments on a channel. These parameters are communicated
// to the rest of the network in ChannelUpdate messages.
type ChannelPolicy struct {
	// FeeSchema holds the fee configuration for a channel.
	FeeSchema

	// TimeLockDelta is the required HTLC timelock delta to be used
	// when forwarding payments.
	TimeLockDelta uint32
}

// Config defines the configuration for the ChannelRouter. ALL elements within
// the configuration MUST be non-nil for the ChannelRouter to carry out its
// duties.
type Config struct {
	// Graph is the channel graph that the ChannelRouter will use to gather
	// metrics from and also to carry out path finding queries.
	// TODO(roasbeef): make into an interface
	Graph *channeldb.ChannelGraph

	// Chain is the router's source to the most up-to-date blockchain data.
	// All incoming advertised channels will be checked against the chain
	// to ensure that the channels advertised are still open.
	Chain lnwallet.BlockChainIO

	// ChainView is an instance of a FilteredChainView which is used to
	// watch the sub-set of the UTXO set (the set of active channels) that
	// we need in order to properly maintain the channel graph.
	ChainView chainview.FilteredChainView

	// Payer is an instance of a PaymentAttemptDispatcher and is used by
	// the router to send payment attempts onto the network, and receive
	// their results.
	Payer PaymentAttemptDispatcher

	// Control keeps track of the status of ongoing payments, ensuring we
	// can properly resume them across restarts.
	Control ControlTower

	// MissionControl is a shared memory of sorts that executions of
	// payment path finding use in order to remember which vertexes/edges
	// were pruned from prior attempts. During SendPayment execution,
	// errors sent by nodes are mapped into a vertex or edge to be pruned.
	// Each run will then take into account this set of pruned
	// vertexes/edges to reduce route failure and pass on graph information
	// gained to the next execution.
	MissionControl PaymentSessionSource

	// ChannelPruneExpiry is the duration used to determine if a channel
	// should be pruned or not. If the delta between now and when the
	// channel was last updated is greater than ChannelPruneExpiry, then
	// the channel is marked as a zombie channel eligible for pruning.
	ChannelPruneExpiry time.Duration

	// GraphPruneInterval is used as an interval to determine how often we
	// should examine the channel graph to garbage collect zombie channels.
	GraphPruneInterval time.Duration

	// QueryBandwidth is a method that allows the router to query the lower
	// link layer to determine the up to date available bandwidth at a
	// prospective link to be traversed. If the  link isn't available, then
	// a value of zero should be returned. Otherwise, the current up to
	// date knowledge of the available bandwidth of the link should be
	// returned.
	QueryBandwidth func(edge *channeldb.ChannelEdgeInfo) lnwire.MilliSatoshi

	// NextPaymentID is a method that guarantees to return a new, unique ID
	// each time it is called. This is used by the router to generate a
	// unique payment ID for each payment it attempts to send, such that
	// the switch can properly handle the HTLC.
	NextPaymentID func() (uint64, error)

	// AssumeChannelValid toggles whether or not the router will check for
	// spentness of channel outpoints. For neutrino, this saves long rescans
	// from blocking initial usage of the daemon.
	AssumeChannelValid bool
}

// routeTuple is an entry within the ChannelRouter's route cache. We cache
// prospective routes based on first the destination, and then the target
// amount. We required the target amount as that will influence the available
// set of paths for a payment.
type routeTuple struct {
	amt  lnwire.MilliSatoshi
	dest [33]byte
}

// newRouteTuple creates a new route tuple from the target and amount.
func newRouteTuple(amt lnwire.MilliSatoshi, dest []byte) routeTuple {
	r := routeTuple{
		amt: amt,
	}
	copy(r.dest[:], dest)

	return r
}

// EdgeLocator is a struct used to identify a specific edge.
type EdgeLocator struct {
	// ChannelID is the channel of this edge.
	ChannelID uint64

	// Direction takes the value of 0 or 1 and is identical in definition to
	// the channel direction flag. A value of 0 means the direction from the
	// lower node pubkey to the higher.
	Direction uint8
}

// newEdgeLocatorByPubkeys returns an edgeLocator based on its end point
// pubkeys.
func newEdgeLocatorByPubkeys(channelID uint64, fromNode, toNode *route.Vertex) *EdgeLocator {
	// Determine direction based on lexicographical ordering of both
	// pubkeys.
	var direction uint8
	if bytes.Compare(fromNode[:], toNode[:]) == 1 {
		direction = 1
	}

	return &EdgeLocator{
		ChannelID: channelID,
		Direction: direction,
	}
}

// newEdgeLocator extracts an edgeLocator based for a full edge policy
// structure.
func newEdgeLocator(edge *channeldb.ChannelEdgePolicy) *EdgeLocator {
	return &EdgeLocator{
		ChannelID: edge.ChannelID,
		Direction: uint8(edge.ChannelFlags & lnwire.ChanUpdateDirection),
	}
}

// String returns a human readable version of the edgeLocator values.
func (e *EdgeLocator) String() string {
	return fmt.Sprintf("%v:%v", e.ChannelID, e.Direction)
}

// edge is a combination of a channel and the node pubkeys of both of its
// endpoints.
type edge struct {
	from, to route.Vertex
	channel  uint64
}

// ChannelRouter is the layer 3 router within the Lightning stack. Below the
// ChannelRouter is the HtlcSwitch, and below that is the Bitcoin blockchain
// itself. The primary role of the ChannelRouter is to respond to queries for
// potential routes that can support a payment amount, and also general graph
// reachability questions. The router will prune the channel graph
// automatically as new blocks are discovered which spend certain known funding
// outpoints, thereby closing their respective channels.
type ChannelRouter struct {
	ntfnClientCounter uint64 // To be used atomically.

	started uint32 // To be used atomically.
	stopped uint32 // To be used atomically.

	bestHeight uint32 // To be used atomically.

	// cfg is a copy of the configuration struct that the ChannelRouter was
	// initialized with.
	cfg *Config

	// selfNode is the center of the star-graph centered around the
	// ChannelRouter. The ChannelRouter uses this node as a starting point
	// when doing any path finding.
	selfNode *channeldb.LightningNode

	// newBlocks is a channel in which new blocks connected to the end of
	// the main chain are sent over, and blocks updated after a call to
	// UpdateFilter.
	newBlocks <-chan *chainview.FilteredBlock

	// staleBlocks is a channel in which blocks disconnected fromt the end
	// of our currently known best chain are sent over.
	staleBlocks <-chan *chainview.FilteredBlock

	// networkUpdates is a channel that carries new topology updates
	// messages from outside the ChannelRouter to be processed by the
	// networkHandler.
	networkUpdates chan *routingMsg

	// topologyClients maps a client's unique notification ID to a
	// topologyClient client that contains its notification dispatch
	// channel.
	topologyClients map[uint64]*topologyClient

	// ntfnClientUpdates is a channel that's used to send new updates to
	// topology notification clients to the ChannelRouter. Updates either
	// add a new notification client, or cancel notifications for an
	// existing client.
	ntfnClientUpdates chan *topologyClientUpdate

	// channelEdgeMtx is a mutex we use to make sure we process only one
	// ChannelEdgePolicy at a time for a given channelID, to ensure
	// consistency between the various database accesses.
	channelEdgeMtx *multimutex.Mutex

	sync.RWMutex

	quit chan struct{}
	wg   sync.WaitGroup
}

// A compile time check to ensure ChannelRouter implements the
// ChannelGraphSource interface.
var _ ChannelGraphSource = (*ChannelRouter)(nil)

// New creates a new instance of the ChannelRouter with the specified
// configuration parameters. As part of initialization, if the router detects
// that the channel graph isn't fully in sync with the latest UTXO (since the
// channel graph is a subset of the UTXO set) set, then the router will proceed
// to fully sync to the latest state of the UTXO set.
func New(cfg Config) (*ChannelRouter, error) {

	selfNode, err := cfg.Graph.SourceNode()
	if err != nil {
		return nil, err
	}

	r := &ChannelRouter{
		cfg:               &cfg,
		networkUpdates:    make(chan *routingMsg),
		topologyClients:   make(map[uint64]*topologyClient),
		ntfnClientUpdates: make(chan *topologyClientUpdate),
		channelEdgeMtx:    multimutex.NewMutex(),
		selfNode:          selfNode,
		quit:              make(chan struct{}),
	}

	return r, nil
}

// Start launches all the goroutines the ChannelRouter requires to carry out
// its duties. If the router has already been started, then this method is a
// noop.
func (r *ChannelRouter) Start() error {
	if !atomic.CompareAndSwapUint32(&r.started, 0, 1) {
		return nil
	}

	log.Tracef("Channel Router starting")

	bestHash, bestHeight, err := r.cfg.Chain.GetBestBlock()
	if err != nil {
		return err
	}

	// If the graph has never been pruned, or hasn't fully been created yet,
	// then we don't treat this as an explicit error.
	if _, _, err := r.cfg.Graph.PruneTip(); err != nil {
		switch {
		case err == channeldb.ErrGraphNeverPruned:
			fallthrough
		case err == channeldb.ErrGraphNotFound:
			// If the graph has never been pruned, then we'll set
			// the prune height to the current best height of the
			// chain backend.
			_, err = r.cfg.Graph.PruneGraph(
				nil, bestHash, uint32(bestHeight),
			)
			if err != nil {
				return err
			}
		default:
			return err
		}
	}

	// If AssumeChannelValid is present, then we won't rely on pruning
	// channels from the graph based on their spentness, but whether they
	// are considered zombies or not.
	if r.cfg.AssumeChannelValid {
		if err := r.pruneZombieChans(); err != nil {
			return err
		}
	} else {
		// Otherwise, we'll use our filtered chain view to prune
		// channels as soon as they are detected as spent on-chain.
		if err := r.cfg.ChainView.Start(); err != nil {
			return err
		}

		// Once the instance is active, we'll fetch the channel we'll
		// receive notifications over.
		r.newBlocks = r.cfg.ChainView.FilteredBlocks()
		r.staleBlocks = r.cfg.ChainView.DisconnectedBlocks()

		// Before we perform our manual block pruning, we'll construct
		// and apply a fresh chain filter to the active
		// FilteredChainView instance.  We do this before, as otherwise
		// we may miss on-chain events as the filter hasn't properly
		// been applied.
		channelView, err := r.cfg.Graph.ChannelView()
		if err != nil && err != channeldb.ErrGraphNoEdgesFound {
			return err
		}

		log.Infof("Filtering chain using %v channels active",
			len(channelView))

		if len(channelView) != 0 {
			err = r.cfg.ChainView.UpdateFilter(
				channelView, uint32(bestHeight),
			)
			if err != nil {
				return err
			}
		}

		// Before we begin normal operation of the router, we first need
		// to synchronize the channel graph to the latest state of the
		// UTXO set.
		if err := r.syncGraphWithChain(); err != nil {
			return err
		}

		// Finally, before we proceed, we'll prune any unconnected nodes
		// from the graph in order to ensure we maintain a tight graph
		// of "useful" nodes.
		err = r.cfg.Graph.PruneGraphNodes()
		if err != nil && err != channeldb.ErrGraphNodesNotFound {
			return err
		}
	}

	// If any payments are still in flight, we resume, to make sure their
	// results are properly handled.
	payments, err := r.cfg.Control.FetchInFlightPayments()
	if err != nil {
		return err
	}

	for _, payment := range payments {
		log.Infof("Resuming payment with hash %v", payment.Info.PaymentHash)
		r.wg.Add(1)
		go func(payment *channeldb.InFlightPayment) {
			defer r.wg.Done()

			// We create a dummy, empty payment session such that
			// we won't make another payment attempt when the
			// result for the in-flight attempt is received.
			//
			// PayAttemptTime doesn't need to be set, as there is
			// only a single attempt.
			paySession := r.cfg.MissionControl.NewPaymentSessionEmpty()

			lPayment := &LightningPayment{
				PaymentHash: payment.Info.PaymentHash,
			}

			_, _, err = r.sendPayment(payment.Attempt, lPayment, paySession)
			if err != nil {
				log.Errorf("Resuming payment with hash %v "+
					"failed: %v.", payment.Info.PaymentHash, err)
				return
			}

			log.Infof("Resumed payment with hash %v completed.",
				payment.Info.PaymentHash)
		}(payment)
	}

	r.wg.Add(1)
	go r.networkHandler()

	return nil
}

// Stop signals the ChannelRouter to gracefully halt all routines. This method
// will *block* until all goroutines have excited. If the channel router has
// already stopped then this method will return immediately.
func (r *ChannelRouter) Stop() error {
	if !atomic.CompareAndSwapUint32(&r.stopped, 0, 1) {
		return nil
	}

	log.Tracef("Channel Router shutting down")

	// Our filtered chain view could've only been started if
	// AssumeChannelValid isn't present.
	if !r.cfg.AssumeChannelValid {
		if err := r.cfg.ChainView.Stop(); err != nil {
			return err
		}
	}

	close(r.quit)
	r.wg.Wait()

	return nil
}

// syncGraphWithChain attempts to synchronize the current channel graph with
// the latest UTXO set state. This process involves pruning from the channel
// graph any channels which have been closed by spending their funding output
// since we've been down.
func (r *ChannelRouter) syncGraphWithChain() error {
	// First, we'll need to check to see if we're already in sync with the
	// latest state of the UTXO set.
	bestHash, bestHeight, err := r.cfg.Chain.GetBestBlock()
	if err != nil {
		return err
	}
	r.bestHeight = uint32(bestHeight)

	pruneHash, pruneHeight, err := r.cfg.Graph.PruneTip()
	if err != nil {
		switch {
		// If the graph has never been pruned, or hasn't fully been
		// created yet, then we don't treat this as an explicit error.
		case err == channeldb.ErrGraphNeverPruned:
		case err == channeldb.ErrGraphNotFound:
		default:
			return err
		}
	}

	log.Infof("Prune tip for Channel Graph: height=%v, hash=%v", pruneHeight,
		pruneHash)

	switch {

	// If the graph has never been pruned, then we can exit early as this
	// entails it's being created for the first time and hasn't seen any
	// block or created channels.
	case pruneHeight == 0 || pruneHash == nil:
		return nil

	// If the block hashes and heights match exactly, then we don't need to
	// prune the channel graph as we're already fully in sync.
	case bestHash.IsEqual(pruneHash) && uint32(bestHeight) == pruneHeight:
		return nil
	}

	// If the main chain blockhash at prune height is different from the
	// prune hash, this might indicate the database is on a stale branch.
	mainBlockHash, err := r.cfg.Chain.GetBlockHash(int64(pruneHeight))
	if err != nil {
		return err
	}

	// While we are on a stale branch of the chain, walk backwards to find
	// first common block.
	for !pruneHash.IsEqual(mainBlockHash) {
		log.Infof("channel graph is stale. Disconnecting block %v "+
			"(hash=%v)", pruneHeight, pruneHash)
		// Prune the graph for every channel that was opened at height
		// >= pruneHeight.
		_, err := r.cfg.Graph.DisconnectBlockAtHeight(pruneHeight)
		if err != nil {
			return err
		}

		pruneHash, pruneHeight, err = r.cfg.Graph.PruneTip()
		if err != nil {
			switch {
			// If at this point the graph has never been pruned, we
			// can exit as this entails we are back to the point
			// where it hasn't seen any block or created channels,
			// alas there's nothing left to prune.
			case err == channeldb.ErrGraphNeverPruned:
				return nil
			case err == channeldb.ErrGraphNotFound:
				return nil
			default:
				return err
			}
		}
		mainBlockHash, err = r.cfg.Chain.GetBlockHash(int64(pruneHeight))
		if err != nil {
			return err
		}
	}

	log.Infof("Syncing channel graph from height=%v (hash=%v) to height=%v "+
		"(hash=%v)", pruneHeight, pruneHash, bestHeight, bestHash)

	// If we're not yet caught up, then we'll walk forward in the chain
	// pruning the channel graph with each new block that hasn't yet been
	// consumed by the channel graph.
	var numChansClosed uint32
	for nextHeight := pruneHeight + 1; nextHeight <= uint32(bestHeight); nextHeight++ {
		// Break out of the rescan early if a shutdown has been
		// requested, otherwise long rescans will block the daemon from
		// shutting down promptly.
		select {
		case <-r.quit:
			return ErrRouterShuttingDown
		default:
		}

		// Using the next height, request a manual block pruning from
		// the chainview for the particular block hash.
		nextHash, err := r.cfg.Chain.GetBlockHash(int64(nextHeight))
		if err != nil {
			return err
		}
		filterBlock, err := r.cfg.ChainView.FilterBlock(nextHash)
		if err != nil {
			return err
		}

		// We're only interested in all prior outputs that have been
		// spent in the block, so collate all the referenced previous
		// outpoints within each tx and input.
		var spentOutputs []*wire.OutPoint
		for _, tx := range filterBlock.Transactions {
			for _, txIn := range tx.TxIn {
				spentOutputs = append(spentOutputs,
					&txIn.PreviousOutPoint)
			}
		}

		// With the spent outputs gathered, attempt to prune the
		// channel graph, also passing in the hash+height of the block
		// being pruned so the prune tip can be updated.
		closedChans, err := r.cfg.Graph.PruneGraph(spentOutputs,
			nextHash,
			nextHeight)
		if err != nil {
			return err
		}

		numClosed := uint32(len(closedChans))
		log.Infof("Block %v (height=%v) closed %v channels",
			nextHash, nextHeight, numClosed)

		numChansClosed += numClosed
	}

	log.Infof("Graph pruning complete: %v channels were closed since "+
		"height %v", numChansClosed, pruneHeight)
	return nil
}

// pruneZombieChans is a method that will be called periodically to prune out
// any "zombie" channels. We consider channels zombies if *both* edges haven't
// been updated since our zombie horizon. If AssumeChannelValid is present,
// we'll also consider channels zombies if *both* edges are disabled. This
// usually signals that a channel has been closed on-chain. We do this
// periodically to keep a healthy, lively routing table.
func (r *ChannelRouter) pruneZombieChans() error {
	var chansToPrune []uint64
	chanExpiry := r.cfg.ChannelPruneExpiry

	log.Infof("Examining channel graph for zombie channels")

	// First, we'll collect all the channels which are eligible for garbage
	// collection due to being zombies.
	filterPruneChans := func(info *channeldb.ChannelEdgeInfo,
		e1, e2 *channeldb.ChannelEdgePolicy) error {

		// We'll ensure that we don't attempt to prune our *own*
		// channels from the graph, as in any case this should be
		// re-advertised by the sub-system above us.
		if info.NodeKey1Bytes == r.selfNode.PubKeyBytes ||
			info.NodeKey2Bytes == r.selfNode.PubKeyBytes {

			return nil
		}

		// If *both* edges haven't been updated for a period of
		// chanExpiry, then we'll mark the channel itself as eligible
		// for graph pruning.
		var e1Zombie, e2Zombie bool
		if e1 != nil {
			e1Zombie = time.Since(e1.LastUpdate) >= chanExpiry
			if e1Zombie {
				log.Tracef("Edge #1 of ChannelID(%v) last "+
					"update: %v", info.ChannelID,
					e1.LastUpdate)
			}
		}
		if e2 != nil {
			e2Zombie = time.Since(e2.LastUpdate) >= chanExpiry
			if e2Zombie {
				log.Tracef("Edge #2 of ChannelID(%v) last "+
					"update: %v", info.ChannelID,
					e2.LastUpdate)
			}
		}

		isZombieChan := e1Zombie && e2Zombie

		// If AssumeChannelValid is present and we've determined the
		// channel is not a zombie, we'll look at the disabled bit for
		// both edges. If they're both disabled, then we can interpret
		// this as the channel being closed and can prune it from our
		// graph.
		if r.cfg.AssumeChannelValid && !isZombieChan {
			var e1Disabled, e2Disabled bool
			if e1 != nil {
				e1Disabled = e1.IsDisabled()
				log.Tracef("Edge #1 of ChannelID(%v) "+
					"disabled=%v", info.ChannelID,
					e1Disabled)
			}
			if e2 != nil {
				e2Disabled = e2.IsDisabled()
				log.Tracef("Edge #2 of ChannelID(%v) "+
					"disabled=%v", info.ChannelID,
					e2Disabled)
			}

			isZombieChan = e1Disabled && e2Disabled
		}

		// If the channel is not considered zombie, we can move on to
		// the next.
		if !isZombieChan {
			return nil
		}

		log.Debugf("ChannelID(%v) is a zombie, collecting to prune",
			info.ChannelID)

		// TODO(roasbeef): add ability to delete single directional edge
		chansToPrune = append(chansToPrune, info.ChannelID)

		return nil
	}

	err := r.cfg.Graph.ForEachChannel(filterPruneChans)
	if err != nil {
		return fmt.Errorf("unable to filter local zombie channels: "+
			"%v", err)
	}

	log.Infof("Pruning %v zombie channels", len(chansToPrune))

	// With the set of zombie-like channels obtained, we'll do another pass
	// to delete them from the channel graph.
	for _, chanID := range chansToPrune {
		log.Tracef("Pruning zombie channel with ChannelID(%v)", chanID)
	}
	if err := r.cfg.Graph.DeleteChannelEdges(chansToPrune...); err != nil {
		return fmt.Errorf("unable to delete zombie channels: %v", err)
	}

	// With the channels pruned, we'll also attempt to prune any nodes that
	// were a part of them.
	err = r.cfg.Graph.PruneGraphNodes()
	if err != nil && err != channeldb.ErrGraphNodesNotFound {
		return fmt.Errorf("unable to prune graph nodes: %v", err)
	}

	return nil
}

// networkHandler is the primary goroutine for the ChannelRouter. The roles of
// this goroutine include answering queries related to the state of the
// network, pruning the graph on new block notification, applying network
// updates, and registering new topology clients.
//
// NOTE: This MUST be run as a goroutine.
func (r *ChannelRouter) networkHandler() {
	defer r.wg.Done()

	graphPruneTicker := time.NewTicker(r.cfg.GraphPruneInterval)
	defer graphPruneTicker.Stop()

	// We'll use this validation barrier to ensure that we process all jobs
	// in the proper order during parallel validation.
	validationBarrier := NewValidationBarrier(runtime.NumCPU()*4, r.quit)

	for {
		select {
		// A new fully validated network update has just arrived. As a
		// result we'll modify the channel graph accordingly depending
		// on the exact type of the message.
		case update := <-r.networkUpdates:
			// We'll set up any dependants, and wait until a free
			// slot for this job opens up, this allow us to not
			// have thousands of goroutines active.
			validationBarrier.InitJobDependencies(update.msg)

			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				defer validationBarrier.CompleteJob()

				// If this message has an existing dependency,
				// then we'll wait until that has been fully
				// validated before we proceed.
				err := validationBarrier.WaitForDependants(
					update.msg,
				)
				if err != nil {
					if err != ErrVBarrierShuttingDown {
						log.Warnf("unexpected error "+
							"during validation "+
							"barrier shutdown: %v",
							err)
					}
					return
				}

				// Process the routing update to determine if
				// this is either a new update from our PoV or
				// an update to a prior vertex/edge we
				// previously accepted.
				err = r.processUpdate(update.msg)
				update.err <- err

				// If this message had any dependencies, then
				// we can now signal them to continue.
				validationBarrier.SignalDependants(update.msg)
				if err != nil {
					return
				}

				// Send off a new notification for the newly
				// accepted update.
				topChange := &TopologyChange{}
				err = addToTopologyChange(
					r.cfg.Graph, topChange, update.msg,
				)
				if err != nil {
					log.Errorf("unable to update topology "+
						"change notification: %v", err)
					return
				}

				if !topChange.isEmpty() {
					r.notifyTopologyChange(topChange)
				}
			}()

			// TODO(roasbeef): remove all unconnected vertexes
			// after N blocks pass with no corresponding
			// announcements.

		case chainUpdate, ok := <-r.staleBlocks:
			// If the channel has been closed, then this indicates
			// the daemon is shutting down, so we exit ourselves.
			if !ok {
				return
			}

			// Since this block is stale, we update our best height
			// to the previous block.
			blockHeight := uint32(chainUpdate.Height)
			atomic.StoreUint32(&r.bestHeight, blockHeight-1)

			// Update the channel graph to reflect that this block
			// was disconnected.
			_, err := r.cfg.Graph.DisconnectBlockAtHeight(blockHeight)
			if err != nil {
				log.Errorf("unable to prune graph with stale "+
					"block: %v", err)
				continue
			}

			// TODO(halseth): notify client about the reorg?

		// A new block has arrived, so we can prune the channel graph
		// of any channels which were closed in the block.
		case chainUpdate, ok := <-r.newBlocks:
			// If the channel has been closed, then this indicates
			// the daemon is shutting down, so we exit ourselves.
			if !ok {
				return
			}

			// We'll ensure that any new blocks received attach
			// directly to the end of our main chain. If not, then
			// we've somehow missed some blocks. We don't process
			// this block as otherwise, we may miss on-chain
			// events.
			currentHeight := atomic.LoadUint32(&r.bestHeight)
			if chainUpdate.Height != currentHeight+1 {
				log.Errorf("out of order block: expecting "+
					"height=%v, got height=%v", currentHeight+1,
					chainUpdate.Height)
				continue
			}

			// Once a new block arrives, we update our running
			// track of the height of the chain tip.
			blockHeight := uint32(chainUpdate.Height)
			atomic.StoreUint32(&r.bestHeight, blockHeight)
			log.Infof("Pruning channel graph using block %v (height=%v)",
				chainUpdate.Hash, blockHeight)

			// We're only interested in all prior outputs that have
			// been spent in the block, so collate all the
			// referenced previous outpoints within each tx and
			// input.
			var spentOutputs []*wire.OutPoint
			for _, tx := range chainUpdate.Transactions {
				for _, txIn := range tx.TxIn {
					spentOutputs = append(spentOutputs,
						&txIn.PreviousOutPoint)
				}
			}

			// With the spent outputs gathered, attempt to prune
			// the channel graph, also passing in the hash+height
			// of the block being pruned so the prune tip can be
			// updated.
			chansClosed, err := r.cfg.Graph.PruneGraph(spentOutputs,
				&chainUpdate.Hash, chainUpdate.Height)
			if err != nil {
				log.Errorf("unable to prune routing table: %v", err)
				continue
			}

			log.Infof("Block %v (height=%v) closed %v channels",
				chainUpdate.Hash, blockHeight, len(chansClosed))

			if len(chansClosed) == 0 {
				continue
			}

			// Notify all currently registered clients of the newly
			// closed channels.
			closeSummaries := createCloseSummaries(blockHeight, chansClosed...)
			r.notifyTopologyChange(&TopologyChange{
				ClosedChannels: closeSummaries,
			})

		// A new notification client update has arrived. We're either
		// gaining a new client, or cancelling notifications for an
		// existing client.
		case ntfnUpdate := <-r.ntfnClientUpdates:
			clientID := ntfnUpdate.clientID

			if ntfnUpdate.cancel {
				r.RLock()
				client, ok := r.topologyClients[ntfnUpdate.clientID]
				r.RUnlock()
				if ok {
					r.Lock()
					delete(r.topologyClients, clientID)
					r.Unlock()

					close(client.exit)
					client.wg.Wait()

					close(client.ntfnChan)
				}

				continue
			}

			r.Lock()
			r.topologyClients[ntfnUpdate.clientID] = &topologyClient{
				ntfnChan: ntfnUpdate.ntfnChan,
				exit:     make(chan struct{}),
			}
			r.Unlock()

		// The graph prune ticker has ticked, so we'll examine the
		// state of the known graph to filter out any zombie channels
		// for pruning.
		case <-graphPruneTicker.C:
			if err := r.pruneZombieChans(); err != nil {
				log.Errorf("Unable to prune zombies: %v", err)
			}

		// The router has been signalled to exit, to we exit our main
		// loop so the wait group can be decremented.
		case <-r.quit:
			return
		}
	}
}

// assertNodeAnnFreshness returns a non-nil error if we have an announcement in
// the database for the passed node with a timestamp newer than the passed
// timestamp. ErrIgnored will be returned if we already have the node, and
// ErrOutdated will be returned if we have a timestamp that's after the new
// timestamp.
func (r *ChannelRouter) assertNodeAnnFreshness(node route.Vertex,
	msgTimestamp time.Time) error {

	// If we are not already aware of this node, it means that we don't
	// know about any channel using this node. To avoid a DoS attack by
	// node announcements, we will ignore such nodes. If we do know about
	// this node, check that this update brings info newer than what we
	// already have.
	lastUpdate, exists, err := r.cfg.Graph.HasLightningNode(node)
	if err != nil {
		return errors.Errorf("unable to query for the "+
			"existence of node: %v", err)
	}
	if !exists {
		return newErrf(ErrIgnored, "Ignoring node announcement"+
			" for node not found in channel graph (%x)",
			node[:])
	}

	// If we've reached this point then we're aware of the vertex being
	// advertised. So we now check if the new message has a new time stamp,
	// if not then we won't accept the new data as it would override newer
	// data.
	if !lastUpdate.Before(msgTimestamp) {
		return newErrf(ErrOutdated, "Ignoring outdated "+
			"announcement for %x", node[:])
	}

	return nil
}

// processUpdate processes a new relate authenticated channel/edge, node or
// channel/edge update network update. If the update didn't affect the internal
// state of the draft due to either being out of date, invalid, or redundant,
// then error is returned.
func (r *ChannelRouter) processUpdate(msg interface{}) error {
	switch msg := msg.(type) {
	case *channeldb.LightningNode:
		// Before we add the node to the database, we'll check to see
		// if the announcement is "fresh" or not. If it isn't, then
		// we'll return an error.
		err := r.assertNodeAnnFreshness(msg.PubKeyBytes, msg.LastUpdate)
		if err != nil {
			return err
		}

		if err := r.cfg.Graph.AddLightningNode(msg); err != nil {
			return errors.Errorf("unable to add node %v to the "+
				"graph: %v", msg.PubKeyBytes, err)
		}

		log.Infof("Updated vertex data for node=%x", msg.PubKeyBytes)

	case *channeldb.ChannelEdgeInfo:
		// Prior to processing the announcement we first check if we
		// already know of this channel, if so, then we can exit early.
		_, _, exists, isZombie, err := r.cfg.Graph.HasChannelEdge(
			msg.ChannelID,
		)
		if err != nil && err != channeldb.ErrGraphNoEdgesFound {
			return errors.Errorf("unable to check for edge "+
				"existence: %v", err)
		}
		if isZombie {
			return newErrf(ErrIgnored, "ignoring msg for zombie "+
				"chan_id=%v", msg.ChannelID)
		}
		if exists {
			return newErrf(ErrIgnored, "ignoring msg for known "+
				"chan_id=%v", msg.ChannelID)
		}

		// If AssumeChannelValid is present, then we are unable to
		// perform any of the expensive checks below, so we'll
		// short-circuit our path straight to adding the edge to our
		// graph.
		if r.cfg.AssumeChannelValid {
			if err := r.cfg.Graph.AddChannelEdge(msg); err != nil {
				return fmt.Errorf("unable to add edge: %v", err)
			}
			log.Infof("New channel discovered! Link "+
				"connects %x and %x with ChannelID(%v)",
				msg.NodeKey1Bytes, msg.NodeKey2Bytes,
				msg.ChannelID)
			break
		}

		// Before we can add the channel to the channel graph, we need
		// to obtain the full funding outpoint that's encoded within
		// the channel ID.
		channelID := lnwire.NewShortChanIDFromInt(msg.ChannelID)
		fundingPoint, _, err := r.fetchChanPoint(&channelID)
		if err != nil {
			return errors.Errorf("unable to fetch chan point for "+
				"chan_id=%v: %v", msg.ChannelID, err)
		}

		// Recreate witness output to be sure that declared in channel
		// edge bitcoin keys and channel value corresponds to the
		// reality.
		witnessScript, err := input.GenMultiSigScript(
			msg.BitcoinKey1Bytes[:], msg.BitcoinKey2Bytes[:],
		)
		if err != nil {
			return err
		}
		fundingPkScript, err := input.WitnessScriptHash(witnessScript)
		if err != nil {
			return err
		}

		// Now that we have the funding outpoint of the channel, ensure
		// that it hasn't yet been spent. If so, then this channel has
		// been closed so we'll ignore it.
		chanUtxo, err := r.cfg.Chain.GetUtxo(
			fundingPoint, fundingPkScript, channelID.BlockHeight,
			r.quit,
		)
		if err != nil {
			return fmt.Errorf("unable to fetch utxo "+
				"for chan_id=%v, chan_point=%v: %v",
				msg.ChannelID, fundingPoint, err)
		}

		// By checking the equality of witness pkscripts we checks that
		// funding witness script is multisignature lock which contains
		// both local and remote public keys which was declared in
		// channel edge and also that the announced channel value is
		// right.
		if !bytes.Equal(fundingPkScript, chanUtxo.PkScript) {
			return errors.Errorf("pkScript mismatch: expected %x, "+
				"got %x", fundingPkScript, chanUtxo.PkScript)
		}

		// TODO(roasbeef): this is a hack, needs to be removed
		// after commitment fees are dynamic.
		msg.Capacity = btcutil.Amount(chanUtxo.Value)
		msg.ChannelPoint = *fundingPoint
		if err := r.cfg.Graph.AddChannelEdge(msg); err != nil {
			return errors.Errorf("unable to add edge: %v", err)
		}

		log.Infof("New channel discovered! Link "+
			"connects %x and %x with ChannelPoint(%v): "+
			"chan_id=%v, capacity=%v",
			msg.NodeKey1Bytes, msg.NodeKey2Bytes,
			fundingPoint, msg.ChannelID, msg.Capacity)

		// As a new edge has been added to the channel graph, we'll
		// update the current UTXO filter within our active
		// FilteredChainView so we are notified if/when this channel is
		// closed.
		filterUpdate := []channeldb.EdgePoint{
			{
				FundingPkScript: fundingPkScript,
				OutPoint:        *fundingPoint,
			},
		}
		err = r.cfg.ChainView.UpdateFilter(
			filterUpdate, atomic.LoadUint32(&r.bestHeight),
		)
		if err != nil {
			return errors.Errorf("unable to update chain "+
				"view: %v", err)
		}

	case *channeldb.ChannelEdgePolicy:
		// We make sure to hold the mutex for this channel ID,
		// such that no other goroutine is concurrently doing
		// database accesses for the same channel ID.
		r.channelEdgeMtx.Lock(msg.ChannelID)
		defer r.channelEdgeMtx.Unlock(msg.ChannelID)

		edge1Timestamp, edge2Timestamp, exists, isZombie, err :=
			r.cfg.Graph.HasChannelEdge(msg.ChannelID)
		if err != nil && err != channeldb.ErrGraphNoEdgesFound {
			return errors.Errorf("unable to check for edge "+
				"existence: %v", err)

		}

		// If the channel is marked as a zombie in our database, and
		// we consider this a stale update, then we should not apply the
		// policy.
		isStaleUpdate := time.Since(msg.LastUpdate) > r.cfg.ChannelPruneExpiry
		if isZombie && isStaleUpdate {
			return newErrf(ErrIgnored, "ignoring stale update "+
				"(flags=%v|%v) for zombie chan_id=%v",
				msg.MessageFlags, msg.ChannelFlags,
				msg.ChannelID)
		}

		// If the channel doesn't exist in our database, we cannot
		// apply the updated policy.
		if !exists {
			return newErrf(ErrIgnored, "ignoring update "+
				"(flags=%v|%v) for unknown chan_id=%v",
				msg.MessageFlags, msg.ChannelFlags,
				msg.ChannelID)
		}

		// As edges are directional edge node has a unique policy for
		// the direction of the edge they control. Therefore we first
		// check if we already have the most up to date information for
		// that edge. If this message has a timestamp not strictly
		// newer than what we already know of we can exit early.
		switch {

		// A flag set of 0 indicates this is an announcement for the
		// "first" node in the channel.
		case msg.ChannelFlags&lnwire.ChanUpdateDirection == 0:

			// Ignore outdated message.
			if !edge1Timestamp.Before(msg.LastUpdate) {
				return newErrf(ErrOutdated, "Ignoring "+
					"outdated update (flags=%v|%v) for "+
					"known chan_id=%v", msg.MessageFlags,
					msg.ChannelFlags, msg.ChannelID)
			}

		// Similarly, a flag set of 1 indicates this is an announcement
		// for the "second" node in the channel.
		case msg.ChannelFlags&lnwire.ChanUpdateDirection == 1:

			// Ignore outdated message.
			if !edge2Timestamp.Before(msg.LastUpdate) {
				return newErrf(ErrOutdated, "Ignoring "+
					"outdated update (flags=%v|%v) for "+
					"known chan_id=%v", msg.MessageFlags,
					msg.ChannelFlags, msg.ChannelID)
			}
		}

		// Now that we know this isn't a stale update, we'll apply the
		// new edge policy to the proper directional edge within the
		// channel graph.
		if err = r.cfg.Graph.UpdateEdgePolicy(msg); err != nil {
			err := errors.Errorf("unable to add channel: %v", err)
			log.Error(err)
			return err
		}

		log.Tracef("New channel update applied: %v",
			newLogClosure(func() string { return spew.Sdump(msg) }))

	default:
		return errors.Errorf("wrong routing update message type")
	}

	return nil
}

// fetchChanPoint retrieves the original outpoint which is encoded within the
// channelID. This method also return the public key script for the target
// transaction.
//
// TODO(roasbeef): replace with call to GetBlockTransaction? (would allow to
// later use getblocktxn)
func (r *ChannelRouter) fetchChanPoint(
	chanID *lnwire.ShortChannelID) (*wire.OutPoint, *wire.TxOut, error) {

	// First fetch the block hash by the block number encoded, then use
	// that hash to fetch the block itself.
	blockNum := int64(chanID.BlockHeight)
	blockHash, err := r.cfg.Chain.GetBlockHash(blockNum)
	if err != nil {
		return nil, nil, err
	}
	fundingBlock, err := r.cfg.Chain.GetBlock(blockHash)
	if err != nil {
		return nil, nil, err
	}

	// As a sanity check, ensure that the advertised transaction index is
	// within the bounds of the total number of transactions within a
	// block.
	numTxns := uint32(len(fundingBlock.Transactions))
	if chanID.TxIndex > numTxns-1 {
		return nil, nil, fmt.Errorf("tx_index=#%v is out of range "+
			"(max_index=%v), network_chan_id=%v\n", chanID.TxIndex,
			numTxns-1, spew.Sdump(chanID))
	}

	// Finally once we have the block itself, we seek to the targeted
	// transaction index to obtain the funding output and txout.
	fundingTx := fundingBlock.Transactions[chanID.TxIndex]
	outPoint := &wire.OutPoint{
		Hash:  fundingTx.TxHash(),
		Index: uint32(chanID.TxPosition),
	}
	txOut := fundingTx.TxOut[chanID.TxPosition]

	return outPoint, txOut, nil
}

// routingMsg couples a routing related routing topology update to the
// error channel.
type routingMsg struct {
	msg interface{}
	err chan error
}

// FindRoute attempts to query the ChannelRouter for the optimum path to a
// particular target destination to which it is able to send `amt` after
// factoring in channel capacities and cumulative fees along the route.
func (r *ChannelRouter) FindRoute(source, target route.Vertex,
	amt lnwire.MilliSatoshi, restrictions *RestrictParams,
	finalExpiry ...uint16) (*route.Route, error) {

	var finalCLTVDelta uint16
	if len(finalExpiry) == 0 {
		finalCLTVDelta = zpay32.DefaultFinalCLTVDelta
	} else {
		finalCLTVDelta = finalExpiry[0]
	}

	log.Debugf("Searching for path to %x, sending %v", target, amt)

	// We can short circuit the routing by opportunistically checking to
	// see if the target vertex event exists in the current graph.
	if _, exists, err := r.cfg.Graph.HasLightningNode(target); err != nil {
		return nil, err
	} else if !exists {
		log.Debugf("Target %x is not in known graph", target)
		return nil, newErrf(ErrTargetNotInNetwork, "target not found")
	}

	// We'll attempt to obtain a set of bandwidth hints that can help us
	// eliminate certain routes early on in the path finding process.
	bandwidthHints, err := generateBandwidthHints(
		r.selfNode, r.cfg.QueryBandwidth,
	)
	if err != nil {
		return nil, err
	}

	// Now that we know the destination is reachable within the graph, we'll
	// execute our path finding algorithm.
	path, err := findPath(
		&graphParams{
			graph:          r.cfg.Graph,
			bandwidthHints: bandwidthHints,
		},
		restrictions, source, target, amt,
	)
	if err != nil {
		return nil, err
	}

	// We'll fetch the current block height so we can properly calculate the
	// required HTLC time locks within the route.
	_, currentHeight, err := r.cfg.Chain.GetBestBlock()
	if err != nil {
		return nil, err
	}

	// Create the route with absolute time lock values.
	route, err := newRoute(
		amt, source, path, uint32(currentHeight), finalCLTVDelta,
	)
	if err != nil {
		return nil, err
	}

	go log.Tracef("Obtained path to send %v to %x: %v",
		amt, target, newLogClosure(func() string {
			return spew.Sdump(route)
		}),
	)

	return route, nil
}

// generateNewSessionKey generates a new ephemeral private key to be used for a
// payment attempt.
func generateNewSessionKey() (*btcec.PrivateKey, error) {
	// Generate a new random session key to ensure that we don't trigger
	// any replay.
	//
	// TODO(roasbeef): add more sources of randomness?
	return btcec.NewPrivateKey(btcec.S256())
}

// generateSphinxPacket generates then encodes a sphinx packet which encodes
// the onion route specified by the passed layer 3 route. The blob returned
// from this function can immediately be included within an HTLC add packet to
// be sent to the first hop within the route.
func generateSphinxPacket(rt *route.Route, paymentHash []byte,
	sessionKey *btcec.PrivateKey) ([]byte, *sphinx.Circuit, error) {

	// As a sanity check, we'll ensure that the set of hops has been
	// properly filled in, otherwise, we won't actually be able to
	// construct a route.
	if len(rt.Hops) == 0 {
		return nil, nil, route.ErrNoRouteHopsProvided
	}

	// Now that we know we have an actual route, we'll map the route into a
	// sphinx payument path which includes per-hop paylods for each hop
	// that give each node within the route the necessary information
	// (fees, CLTV value, etc) to properly forward the payment.
	sphinxPath, err := rt.ToSphinxPath()
	if err != nil {
		return nil, nil, err
	}

	log.Tracef("Constructed per-hop payloads for payment_hash=%x: %v",
		paymentHash[:], newLogClosure(func() string {
			path := sphinxPath[:sphinxPath.TrueRouteLength()]
			for i := range path {
				path[i].NodePub.Curve = nil
			}
			return spew.Sdump(path)
		}),
	)

	// Next generate the onion routing packet which allows us to perform
	// privacy preserving source routing across the network.
	sphinxPacket, err := sphinx.NewOnionPacket(
		sphinxPath, sessionKey, paymentHash,
	)
	if err != nil {
		return nil, nil, err
	}

	// Finally, encode Sphinx packet using its wire representation to be
	// included within the HTLC add packet.
	var onionBlob bytes.Buffer
	if err := sphinxPacket.Encode(&onionBlob); err != nil {
		return nil, nil, err
	}

	log.Tracef("Generated sphinx packet: %v",
		newLogClosure(func() string {
			// We unset the internal curve here in order to keep
			// the logs from getting noisy.
			sphinxPacket.EphemeralKey.Curve = nil
			return spew.Sdump(sphinxPacket)
		}),
	)

	return onionBlob.Bytes(), &sphinx.Circuit{
		SessionKey:  sessionKey,
		PaymentPath: sphinxPath.NodeKeys(),
	}, nil
}

// LightningPayment describes a payment to be sent through the network to the
// final destination.
type LightningPayment struct {
	// Target is the node in which the payment should be routed towards.
	Target route.Vertex

	// Amount is the value of the payment to send through the network in
	// milli-satoshis.
	Amount lnwire.MilliSatoshi

	// FeeLimit is the maximum fee in millisatoshis that the payment should
	// accept when sending it through the network. The payment will fail
	// if there isn't a route with lower fees than this limit.
	FeeLimit lnwire.MilliSatoshi

	// CltvLimit is the maximum time lock that is allowed for attempts to
	// complete this payment.
	CltvLimit *uint32

	// PaymentHash is the r-hash value to use within the HTLC extended to
	// the first hop.
	PaymentHash [32]byte

	// FinalCLTVDelta is the CTLV expiry delta to use for the _final_ hop
	// in the route. This means that the final hop will have a CLTV delta
	// of at least: currentHeight + FinalCLTVDelta.
	FinalCLTVDelta uint16

	// PayAttemptTimeout is a timeout value that we'll use to determine
	// when we should should abandon the payment attempt after consecutive
	// payment failure. This prevents us from attempting to send a payment
	// indefinitely. A zero value means the payment will never time out.
	//
	// TODO(halseth): make wallclock time to allow resume after startup.
	PayAttemptTimeout time.Duration

	// RouteHints represents the different routing hints that can be used to
	// assist a payment in reaching its destination successfully. These
	// hints will act as intermediate hops along the route.
	//
	// NOTE: This is optional unless required by the payment. When providing
	// multiple routes, ensure the hop hints within each route are chained
	// together and sorted in forward order in order to reach the
	// destination successfully.
	RouteHints [][]zpay32.HopHint

	// OutgoingChannelID is the channel that needs to be taken to the first
	// hop. If nil, any channel may be used.
	OutgoingChannelID *uint64

	// PaymentRequest is an optional payment request that this payment is
	// attempting to complete.
	PaymentRequest []byte

	// TODO(roasbeef): add e2e message?
}

// SendPayment attempts to send a payment as described within the passed
// LightningPayment. This function is blocking and will return either: when the
// payment is successful, or all candidates routes have been attempted and
// resulted in a failed payment. If the payment succeeds, then a non-nil Route
// will be returned which describes the path the successful payment traversed
// within the network to reach the destination. Additionally, the payment
// preimage will also be returned.
func (r *ChannelRouter) SendPayment(payment *LightningPayment) ([32]byte,
	*route.Route, error) {

	paySession, err := r.preparePayment(payment)
	if err != nil {
		return [32]byte{}, nil, err
	}

	// Since this is the first time this payment is being made, we pass nil
	// for the existing attempt.
	return r.sendPayment(nil, payment, paySession)
}

// SendPaymentAsync is the non-blocking version of SendPayment. The payment
// result needs to be retrieved via the control tower.
func (r *ChannelRouter) SendPaymentAsync(payment *LightningPayment) error {
	paySession, err := r.preparePayment(payment)
	if err != nil {
		return err
	}

	// Since this is the first time this payment is being made, we pass nil
	// for the existing attempt.
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()

		_, _, err := r.sendPayment(nil, payment, paySession)
		if err != nil {
			log.Errorf("Payment with hash %x failed: %v",
				payment.PaymentHash, err)
		}
	}()

	return nil
}

// preparePayment creates the payment session and registers the payment with the
// control tower.
func (r *ChannelRouter) preparePayment(payment *LightningPayment) (
	PaymentSession, error) {

	// Before starting the HTLC routing attempt, we'll create a fresh
	// payment session which will report our errors back to mission
	// control.
	paySession, err := r.cfg.MissionControl.NewPaymentSession(
		payment.RouteHints, payment.Target,
	)
	if err != nil {
		return nil, err
	}

	// Record this payment hash with the ControlTower, ensuring it is not
	// already in-flight.
	info := &channeldb.PaymentCreationInfo{
		PaymentHash:    payment.PaymentHash,
		Value:          payment.Amount,
		CreationDate:   time.Now(),
		PaymentRequest: payment.PaymentRequest,
	}

	err = r.cfg.Control.InitPayment(payment.PaymentHash, info)
	if err != nil {
		return nil, err
	}

	return paySession, nil
}

// SendToRoute attempts to send a payment with the given hash through the
// provided route. This function is blocking and will return the obtained
// preimage if the payment is successful or the full error in case of a failure.
func (r *ChannelRouter) SendToRoute(hash lntypes.Hash, route *route.Route) (
	lntypes.Preimage, error) {

	// Create a payment session for just this route.
	paySession := r.cfg.MissionControl.NewPaymentSessionForRoute(route)

	// Calculate amount paid to receiver.
	amt := route.TotalAmount - route.TotalFees()

	// Record this payment hash with the ControlTower, ensuring it is not
	// already in-flight.
	info := &channeldb.PaymentCreationInfo{
		PaymentHash:    hash,
		Value:          amt,
		CreationDate:   time.Now(),
		PaymentRequest: nil,
	}

	err := r.cfg.Control.InitPayment(hash, info)
	if err != nil {
		return [32]byte{}, err
	}

	// Create a (mostly) dummy payment, as the created payment session is
	// not going to do path finding.
	// TODO(halseth): sendPayment doesn't really need LightningPayment, make
	// it take just needed fields instead.
	//
	// PayAttemptTime doesn't need to be set, as there is only a single
	// attempt.
	payment := &LightningPayment{
		PaymentHash: hash,
	}

	// Since this is the first time this payment is being made, we pass nil
	// for the existing attempt.
	preimage, _, err := r.sendPayment(nil, payment, paySession)
	if err != nil {
		// SendToRoute should return a structured error. In case the
		// provided route fails, payment lifecycle will return a
		// noRouteError with the structured error embedded.
		if noRouteError, ok := err.(errNoRoute); ok {
			if noRouteError.lastError == nil {
				return lntypes.Preimage{},
					errors.New("failure message missing")
			}

			return lntypes.Preimage{}, noRouteError.lastError
		}

		return lntypes.Preimage{}, err
	}

	return preimage, nil
}

// sendPayment attempts to send a payment as described within the passed
// LightningPayment. This function is blocking and will return either: when the
// payment is successful, or all candidates routes have been attempted and
// resulted in a failed payment. If the payment succeeds, then a non-nil Route
// will be returned which describes the path the successful payment traversed
// within the network to reach the destination. Additionally, the payment
// preimage will also be returned.
//
// The existing attempt argument should be set to nil if this is a payment that
// haven't had any payment attempt sent to the switch yet. If it has had an
// attempt already, it should be passed such that the result can be retrieved.
//
// This method relies on the ControlTower's internal payment state machine to
// carry out its execution. After restarts it is safe, and assumed, that the
// router will call this method for every payment still in-flight according to
// the ControlTower.
func (r *ChannelRouter) sendPayment(
	existingAttempt *channeldb.PaymentAttemptInfo,
	payment *LightningPayment, paySession PaymentSession) (
	[32]byte, *route.Route, error) {

	log.Tracef("Dispatching route for lightning payment: %v",
		newLogClosure(func() string {
			for _, routeHint := range payment.RouteHints {
				for _, hopHint := range routeHint {
					hopHint.NodeID.Curve = nil
				}
			}
			return spew.Sdump(payment)
		}),
	)

	// We'll also fetch the current block height so we can properly
	// calculate the required HTLC time locks within the route.
	_, currentHeight, err := r.cfg.Chain.GetBestBlock()
	if err != nil {
		return [32]byte{}, nil, err
	}

	// Now set up a paymentLifecycle struct with these params, such that we
	// can resume the payment from the current state.
	p := &paymentLifecycle{
		router:         r,
		payment:        payment,
		paySession:     paySession,
		currentHeight:  currentHeight,
		finalCLTVDelta: uint16(payment.FinalCLTVDelta),
		attempt:        existingAttempt,
		circuit:        nil,
		lastError:      nil,
	}

	// If a timeout is specified, create a timeout channel. If no timeout is
	// specified, the channel is left nil and will never abort the payment
	// loop.
	if payment.PayAttemptTimeout != 0 {
		p.timeoutChan = time.After(payment.PayAttemptTimeout)
	}

	return p.resumePayment()

}

// processSendError analyzes the error for the payment attempt received from the
// switch and updates mission control and/or channel policies. Depending on the
// error type, this error is either the final outcome of the payment or we need
// to continue with an alternative route. This is indicated by the boolean
// return value.
func (r *ChannelRouter) processSendError(paySession PaymentSession,
	rt *route.Route, fErr *htlcswitch.ForwardingError) bool {

	errSource := fErr.ErrorSource
	errVertex := route.NewVertex(errSource)

	log.Tracef("node=%x reported failure when sending htlc", errVertex)

	// Always determine chan id ourselves, because a channel
	// update with id may not be available.
	failedEdge, failedAmt, err := getFailedEdge(
		rt, route.Vertex(errVertex),
	)
	if err != nil {
		return true
	}

	// processChannelUpdateAndRetry is a closure that
	// handles a failure message containing a channel
	// update. This function always tries to apply the
	// channel update and passes on the result to the
	// payment session to adjust its view on the reliability
	// of the network.
	//
	// As channel id, the locally determined channel id is
	// used. It does not rely on the channel id that is part
	// of the channel update message, because the remote
	// node may lie to us or the update may be corrupt.
	processChannelUpdateAndRetry := func(
		update *lnwire.ChannelUpdate,
		pubKey *btcec.PublicKey) {

		// Try to apply the channel update.
		updateOk := r.applyChannelUpdate(update, pubKey)

		// If the update could not be applied, prune the
		// edge. There is no reason to continue trying
		// this channel.
		//
		// TODO: Could even prune the node completely?
		// Or is there a valid reason for the channel
		// update to fail?
		if !updateOk {
			paySession.ReportEdgeFailure(
				failedEdge, 0,
			)
		}

		paySession.ReportEdgePolicyFailure(failedEdge)
	}

	switch onionErr := fErr.FailureMessage.(type) {

	// If the end destination didn't know the payment
	// hash or we sent the wrong payment amount to the
	// destination, then we'll terminate immediately.
	case *lnwire.FailUnknownPaymentHash:
		return true

	// If we sent the wrong amount to the destination, then
	// we'll exit early.
	case *lnwire.FailIncorrectPaymentAmount:
		return true

	// If the time-lock that was extended to the final node
	// was incorrect, then we can't proceed.
	case *lnwire.FailFinalIncorrectCltvExpiry:
		return true

	// If we crafted an invalid onion payload for the final
	// node, then we'll exit early.
	case *lnwire.FailFinalIncorrectHtlcAmount:
		return true

	// Similarly, if the HTLC expiry that we extended to
	// the final hop expires too soon, then will fail the
	// payment.
	//
	// TODO(roasbeef): can happen to to race condition, try
	// again with recent block height
	case *lnwire.FailFinalExpiryTooSoon:
		return true

	// If we erroneously attempted to cross a chain border,
	// then we'll cancel the payment.
	case *lnwire.FailInvalidRealm:
		return true

	// If we get a notice that the expiry was too soon for
	// an intermediate node, then we'll prune out the node
	// that sent us this error, as it doesn't now what the
	// correct block height is.
	case *lnwire.FailExpiryTooSoon:
		r.applyChannelUpdate(&onionErr.Update, errSource)
		paySession.ReportVertexFailure(errVertex)
		return false

	// If we hit an instance of onion payload corruption or
	// an invalid version, then we'll exit early as this
	// shouldn't happen in the typical case.
	case *lnwire.FailInvalidOnionVersion:
		return true
	case *lnwire.FailInvalidOnionHmac:
		return true
	case *lnwire.FailInvalidOnionKey:
		return true

	// If we get a failure due to violating the minimum
	// amount, we'll apply the new minimum amount and retry
	// routing.
	case *lnwire.FailAmountBelowMinimum:
		processChannelUpdateAndRetry(
			&onionErr.Update, errSource,
		)
		return false

	// If we get a failure due to a fee, we'll apply the
	// new fee update, and retry our attempt using the
	// newly updated fees.
	case *lnwire.FailFeeInsufficient:
		processChannelUpdateAndRetry(
			&onionErr.Update, errSource,
		)
		return false

	// If we get the failure for an intermediate node that
	// disagrees with our time lock values, then we'll
	// apply the new delta value and try it once more.
	case *lnwire.FailIncorrectCltvExpiry:
		processChannelUpdateAndRetry(
			&onionErr.Update, errSource,
		)
		return false

	// The outgoing channel that this node was meant to
	// forward one is currently disabled, so we'll apply
	// the update and continue.
	case *lnwire.FailChannelDisabled:
		r.applyChannelUpdate(&onionErr.Update, errSource)
		paySession.ReportEdgeFailure(failedEdge, 0)
		return false

	// It's likely that the outgoing channel didn't have
	// sufficient capacity, so we'll prune this edge for
	// now, and continue onwards with our path finding.
	case *lnwire.FailTemporaryChannelFailure:
		r.applyChannelUpdate(onionErr.Update, errSource)
		paySession.ReportEdgeFailure(failedEdge, failedAmt)
		return false

	// If the send fail due to a node not having the
	// required features, then we'll note this error and
	// continue.
	case *lnwire.FailRequiredNodeFeatureMissing:
		paySession.ReportVertexFailure(errVertex)
		return false

	// If the send fail due to a node not having the
	// required features, then we'll note this error and
	// continue.
	case *lnwire.FailRequiredChannelFeatureMissing:
		paySession.ReportVertexFailure(errVertex)
		return false

	// If the next hop in the route wasn't known or
	// offline, we'll only the channel which we attempted
	// to route over. This is conservative, and it can
	// handle faulty channels between nodes properly.
	// Additionally, this guards against routing nodes
	// returning errors in order to attempt to black list
	// another node.
	case *lnwire.FailUnknownNextPeer:
		paySession.ReportEdgeFailure(failedEdge, 0)
		return false

	// If the node wasn't able to forward for which ever
	// reason, then we'll note this and continue with the
	// routes.
	case *lnwire.FailTemporaryNodeFailure:
		paySession.ReportVertexFailure(errVertex)
		return false

	case *lnwire.FailPermanentNodeFailure:
		paySession.ReportVertexFailure(errVertex)
		return false

	// If we crafted a route that contains a too long time
	// lock for an intermediate node, we'll prune the node.
	// As there currently is no way of knowing that node's
	// maximum acceptable cltv, we cannot take this
	// constraint into account during routing.
	//
	// TODO(joostjager): Record the rejected cltv and use
	// that as a hint during future path finding through
	// that node.
	case *lnwire.FailExpiryTooFar:
		paySession.ReportVertexFailure(errVertex)
		return false

	// If we get a permanent channel or node failure, then
	// we'll prune the channel in both directions and
	// continue with the rest of the routes.
	case *lnwire.FailPermanentChannelFailure:
		paySession.ReportEdgeFailure(failedEdge, 0)
		paySession.ReportEdgeFailure(edge{
			from:    failedEdge.to,
			to:      failedEdge.from,
			channel: failedEdge.channel,
		}, 0)
		return false

	default:
		return true
	}
}

// getFailedEdge tries to locate the failing channel given a route and the
// pubkey of the node that sent the error. It will assume that the error is
// associated with the outgoing channel of the error node. As a second result,
// it returns the amount sent over the edge.
func getFailedEdge(route *route.Route, errSource route.Vertex) (edge,
	lnwire.MilliSatoshi, error) {

	hopCount := len(route.Hops)
	fromNode := route.SourcePubKey
	amt := route.TotalAmount
	for i, hop := range route.Hops {
		toNode := hop.PubKeyBytes

		// Determine if we have a failure from the final hop.
		//
		// TODO(joostjager): In this case, certain types of errors are
		// not expected. For example FailUnknownNextPeer. This could be
		// a reason to prune the node?
		finalHopFailing := i == hopCount-1 && errSource == toNode

		// As this error indicates that the target channel was unable to
		// carry this HTLC (for w/e reason), we'll return the _outgoing_
		// channel that the source of the error was meant to pass the
		// HTLC along to.
		//
		// If the errSource is the final hop, we assume that the failing
		// channel is the incoming channel.
		if errSource == fromNode || finalHopFailing {
			return edge{
				from:    fromNode,
				to:      toNode,
				channel: hop.ChannelID,
			}, amt, nil
		}

		fromNode = toNode
		amt = hop.AmtToForward
	}

	return edge{}, 0, fmt.Errorf("cannot find error source node in route")
}

// applyChannelUpdate validates a channel update and if valid, applies it to the
// database. It returns a bool indicating whether the updates was successful.
func (r *ChannelRouter) applyChannelUpdate(msg *lnwire.ChannelUpdate,
	pubKey *btcec.PublicKey) bool {
	// If we get passed a nil channel update (as it's optional with some
	// onion errors), then we'll exit early with a success result.
	if msg == nil {
		return true
	}

	ch, _, _, err := r.GetChannelByID(msg.ShortChannelID)
	if err != nil {
		log.Errorf("Unable to retrieve channel by id: %v", err)
		return false
	}

	if err := ValidateChannelUpdateAnn(pubKey, ch.Capacity, msg); err != nil {
		log.Errorf("Unable to validate channel update: %v", err)
		return false
	}

	err = r.UpdateEdge(&channeldb.ChannelEdgePolicy{
		SigBytes:                  msg.Signature.ToSignatureBytes(),
		ChannelID:                 msg.ShortChannelID.ToUint64(),
		LastUpdate:                time.Unix(int64(msg.Timestamp), 0),
		MessageFlags:              msg.MessageFlags,
		ChannelFlags:              msg.ChannelFlags,
		TimeLockDelta:             msg.TimeLockDelta,
		MinHTLC:                   msg.HtlcMinimumMsat,
		MaxHTLC:                   msg.HtlcMaximumMsat,
		FeeBaseMSat:               lnwire.MilliSatoshi(msg.BaseFee),
		FeeProportionalMillionths: lnwire.MilliSatoshi(msg.FeeRate),
	})
	if err != nil && !IsError(err, ErrIgnored, ErrOutdated) {
		log.Errorf("Unable to apply channel update: %v", err)
		return false
	}

	return true
}

// AddNode is used to add information about a node to the router database. If
// the node with this pubkey is not present in an existing channel, it will
// be ignored.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) AddNode(node *channeldb.LightningNode) error {
	rMsg := &routingMsg{
		msg: node,
		err: make(chan error, 1),
	}

	select {
	case r.networkUpdates <- rMsg:
		select {
		case err := <-rMsg.err:
			return err
		case <-r.quit:
			return ErrRouterShuttingDown
		}
	case <-r.quit:
		return ErrRouterShuttingDown
	}
}

// AddEdge is used to add edge/channel to the topology of the router, after all
// information about channel will be gathered this edge/channel might be used
// in construction of payment path.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) AddEdge(edge *channeldb.ChannelEdgeInfo) error {
	rMsg := &routingMsg{
		msg: edge,
		err: make(chan error, 1),
	}

	select {
	case r.networkUpdates <- rMsg:
		select {
		case err := <-rMsg.err:
			return err
		case <-r.quit:
			return ErrRouterShuttingDown
		}
	case <-r.quit:
		return ErrRouterShuttingDown
	}
}

// UpdateEdge is used to update edge information, without this message edge
// considered as not fully constructed.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) UpdateEdge(update *channeldb.ChannelEdgePolicy) error {
	rMsg := &routingMsg{
		msg: update,
		err: make(chan error, 1),
	}

	select {
	case r.networkUpdates <- rMsg:
		select {
		case err := <-rMsg.err:
			return err
		case <-r.quit:
			return ErrRouterShuttingDown
		}
	case <-r.quit:
		return ErrRouterShuttingDown
	}
}

// CurrentBlockHeight returns the block height from POV of the router subsystem.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) CurrentBlockHeight() (uint32, error) {
	_, height, err := r.cfg.Chain.GetBestBlock()
	return uint32(height), err
}

// GetChannelByID return the channel by the channel id.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) GetChannelByID(chanID lnwire.ShortChannelID) (
	*channeldb.ChannelEdgeInfo,
	*channeldb.ChannelEdgePolicy,
	*channeldb.ChannelEdgePolicy, error) {

	return r.cfg.Graph.FetchChannelEdgesByID(chanID.ToUint64())
}

// FetchLightningNode attempts to look up a target node by its identity public
// key. channeldb.ErrGraphNodeNotFound is returned if the node doesn't exist
// within the graph.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) FetchLightningNode(node route.Vertex) (*channeldb.LightningNode, error) {
	pubKey, err := btcec.ParsePubKey(node[:], btcec.S256())
	if err != nil {
		return nil, fmt.Errorf("unable to parse raw public key: %v", err)
	}
	return r.cfg.Graph.FetchLightningNode(pubKey)
}

// ForEachNode is used to iterate over every node in router topology.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) ForEachNode(cb func(*channeldb.LightningNode) error) error {
	return r.cfg.Graph.ForEachNode(nil, func(_ *bbolt.Tx, n *channeldb.LightningNode) error {
		return cb(n)
	})
}

// ForAllOutgoingChannels is used to iterate over all outgoing channels owned by
// the router.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) ForAllOutgoingChannels(cb func(*channeldb.ChannelEdgeInfo,
	*channeldb.ChannelEdgePolicy) error) error {

	return r.selfNode.ForEachChannel(nil, func(_ *bbolt.Tx, c *channeldb.ChannelEdgeInfo,
		e, _ *channeldb.ChannelEdgePolicy) error {

		if e == nil {
			return fmt.Errorf("Channel from self node has no policy")
		}

		return cb(c, e)
	})
}

// ForEachChannel is used to iterate over every known edge (channel) within our
// view of the channel graph.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) ForEachChannel(cb func(chanInfo *channeldb.ChannelEdgeInfo,
	e1, e2 *channeldb.ChannelEdgePolicy) error) error {

	return r.cfg.Graph.ForEachChannel(cb)
}

// AddProof updates the channel edge info with proof which is needed to
// properly announce the edge to the rest of the network.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) AddProof(chanID lnwire.ShortChannelID,
	proof *channeldb.ChannelAuthProof) error {

	info, _, _, err := r.cfg.Graph.FetchChannelEdgesByID(chanID.ToUint64())
	if err != nil {
		return err
	}

	info.AuthProof = proof
	return r.cfg.Graph.UpdateChannelEdge(info)
}

// IsStaleNode returns true if the graph source has a node announcement for the
// target node with a more recent timestamp.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) IsStaleNode(node route.Vertex, timestamp time.Time) bool {
	// If our attempt to assert that the node announcement is fresh fails,
	// then we know that this is actually a stale announcement.
	return r.assertNodeAnnFreshness(node, timestamp) != nil
}

// IsPublicNode determines whether the given vertex is seen as a public node in
// the graph from the graph's source node's point of view.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) IsPublicNode(node route.Vertex) (bool, error) {
	return r.cfg.Graph.IsPublicNode(node)
}

// IsKnownEdge returns true if the graph source already knows of the passed
// channel ID either as a live or zombie edge.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) IsKnownEdge(chanID lnwire.ShortChannelID) bool {
	_, _, exists, isZombie, _ := r.cfg.Graph.HasChannelEdge(chanID.ToUint64())
	return exists || isZombie
}

// IsStaleEdgePolicy returns true if the graph soruce has a channel edge for
// the passed channel ID (and flags) that have a more recent timestamp.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) IsStaleEdgePolicy(chanID lnwire.ShortChannelID,
	timestamp time.Time, flags lnwire.ChanUpdateChanFlags) bool {

	edge1Timestamp, edge2Timestamp, exists, isZombie, err :=
		r.cfg.Graph.HasChannelEdge(chanID.ToUint64())
	if err != nil {
		return false

	}

	// If we know of the edge as a zombie, then we'll make some additional
	// checks to determine if the new policy is fresh.
	if isZombie {
		// When running with AssumeChannelValid, we also prune channels
		// if both of their edges are disabled. We'll mark the new
		// policy as stale if it remains disabled.
		if r.cfg.AssumeChannelValid {
			isDisabled := flags&lnwire.ChanUpdateDisabled ==
				lnwire.ChanUpdateDisabled
			if isDisabled {
				return true
			}
		}

		// Otherwise, we'll fall back to our usual ChannelPruneExpiry.
		return time.Since(timestamp) > r.cfg.ChannelPruneExpiry
	}

	// If we don't know of the edge, then it means it's fresh (thus not
	// stale).
	if !exists {
		return false
	}

	// As edges are directional edge node has a unique policy for the
	// direction of the edge they control. Therefore we first check if we
	// already have the most up to date information for that edge. If so,
	// then we can exit early.
	switch {
	// A flag set of 0 indicates this is an announcement for the "first"
	// node in the channel.
	case flags&lnwire.ChanUpdateDirection == 0:
		return !edge1Timestamp.Before(timestamp)

	// Similarly, a flag set of 1 indicates this is an announcement for the
	// "second" node in the channel.
	case flags&lnwire.ChanUpdateDirection == 1:
		return !edge2Timestamp.Before(timestamp)
	}

	return false
}

// MarkEdgeLive clears an edge from our zombie index, deeming it as live.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) MarkEdgeLive(chanID lnwire.ShortChannelID) error {
	return r.cfg.Graph.MarkEdgeLive(chanID.ToUint64())
}
