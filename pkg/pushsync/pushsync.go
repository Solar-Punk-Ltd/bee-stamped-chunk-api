// Copyright 2020 The Swarm Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package pushsync provides the pushsync protocol
// implementation.
package pushsync

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ethersphere/bee/pkg/accounting"
	"github.com/ethersphere/bee/pkg/cac"
	"github.com/ethersphere/bee/pkg/crypto"
	"github.com/ethersphere/bee/pkg/log"
	"github.com/ethersphere/bee/pkg/p2p"
	"github.com/ethersphere/bee/pkg/p2p/protobuf"
	"github.com/ethersphere/bee/pkg/postage"
	"github.com/ethersphere/bee/pkg/pricer"
	"github.com/ethersphere/bee/pkg/pushsync/pb"
	"github.com/ethersphere/bee/pkg/skippeers"
	"github.com/ethersphere/bee/pkg/soc"
	"github.com/ethersphere/bee/pkg/storage"
	"github.com/ethersphere/bee/pkg/swarm"
	"github.com/ethersphere/bee/pkg/tags"
	"github.com/ethersphere/bee/pkg/topology"
	"github.com/ethersphere/bee/pkg/tracing"
	opentracing "github.com/opentracing/opentracing-go"
)

// loggerName is the tree path name of the logger for this package.
const loggerName = "pushsync"

const (
	protocolName    = "pushsync"
	protocolVersion = "1.1.0"
	streamName      = "pushsync"
)

const (
	defaultTTL     = 30 * time.Second // request time to live
	p90TTL         = 5 * time.Second  // P90 request time to live
	sanctionWait   = 5 * time.Minute
	replicationTTL = 5 * time.Second // time to live for neighborhood replication
	waitRefresh    = 600 * time.Millisecond
)

const (
	nPeersToReplicate = 3 // number of peers to replicate to as receipt is sent upstream
	maxAttempts       = 8
	maxPeers          = 16
)

var (
	ErrNoPush            = errors.New("could not push chunk")
	ErrOutOfDepthStoring = errors.New("storing outside of the neighborhood")
	ErrWarmup            = errors.New("node warmup time not complete")
	errNotAttempted      = errors.New("peer not attempted")
)

type PushSyncer interface {
	PushChunkToClosest(ctx context.Context, ch swarm.Chunk) (*Receipt, error)
}

type Receipt struct {
	Address   swarm.Address
	Signature []byte
	Nonce     []byte
}

type PushSync struct {
	address        swarm.Address
	nonce          []byte
	streamer       p2p.StreamerDisconnecter
	storer         storage.Putter
	topologyDriver topology.Driver
	tagger         *tags.Tags
	unwrap         func(swarm.Chunk)
	logger         log.Logger
	accounting     accounting.Interface
	pricer         pricer.Interface
	metrics        metrics
	tracer         *tracing.Tracer
	validStamp     postage.ValidStampFn
	signer         crypto.Signer
	isFullNode     bool
	warmupPeriod   time.Time
	skipList       *peerSkipList
}

type receiptResult struct {
	pushTime time.Time
	peer     swarm.Address
	receipt  *pb.Receipt
	pushed   bool
	err      error
}

func New(address swarm.Address, nonce []byte, streamer p2p.StreamerDisconnecter, storer storage.Putter, topology topology.Driver, tagger *tags.Tags, isFullNode bool, unwrap func(swarm.Chunk), validStamp postage.ValidStampFn, logger log.Logger, accounting accounting.Interface, pricer pricer.Interface, signer crypto.Signer, tracer *tracing.Tracer, warmupTime time.Duration) *PushSync {
	ps := &PushSync{
		address:        address,
		nonce:          nonce,
		streamer:       streamer,
		storer:         storer,
		topologyDriver: topology,
		tagger:         tagger,
		isFullNode:     isFullNode,
		unwrap:         unwrap,
		logger:         logger.WithName(loggerName).Register(),
		accounting:     accounting,
		pricer:         pricer,
		metrics:        newMetrics(),
		tracer:         tracer,
		signer:         signer,
		skipList:       newPeerSkipList(),
		warmupPeriod:   time.Now().Add(warmupTime),
	}

	ps.validStamp = ps.validStampWrapper(validStamp)
	return ps
}

func (s *PushSync) Protocol() p2p.ProtocolSpec {
	return p2p.ProtocolSpec{
		Name:    protocolName,
		Version: protocolVersion,
		StreamSpecs: []p2p.StreamSpec{
			{
				Name:    streamName,
				Handler: s.handler,
			},
		},
	}
}

// handler handles chunk delivery from other node and forwards to its destination node.
// If the current node is the destination, it stores in the local store and sends a receipt.
func (ps *PushSync) handler(ctx context.Context, p p2p.Peer, stream p2p.Stream) (err error) {
	now := time.Now()
	w, r := protobuf.NewWriterAndReader(stream)
	ctx, cancel := context.WithTimeout(ctx, defaultTTL)
	defer cancel()
	defer func() {
		if err != nil {
			ps.metrics.TotalHandlerTime.WithLabelValues("failure").Observe(time.Since(now).Seconds())
			ps.metrics.TotalHandlerErrors.Inc()
			_ = stream.Reset()
		} else {
			ps.metrics.TotalHandlerTime.WithLabelValues("success").Observe(time.Since(now).Seconds())
			_ = stream.FullClose()
		}
	}()
	var ch pb.Delivery
	if err = r.ReadMsgWithContext(ctx, &ch); err != nil {
		return fmt.Errorf("pushsync read delivery: %w", err)
	}
	ps.metrics.TotalReceived.Inc()

	chunk := swarm.NewChunk(swarm.NewAddress(ch.Address), ch.Data)
	chunkAddress := chunk.Address()

	span, logger, ctx := ps.tracer.StartSpanFromContext(ctx, "pushsync-handler", ps.logger, opentracing.Tag{Key: "address", Value: chunkAddress.String()})
	defer span.Finish()

	stamp := new(postage.Stamp)
	// attaching the stamp is required becase pushToClosest expects a chunk with a stamp
	err = stamp.UnmarshalBinary(ch.Stamp)
	if err != nil {
		return fmt.Errorf("pushsync stamp unmarshall: %w", err)
	}
	chunk.WithStamp(stamp)

	if cac.Valid(chunk) {
		if ps.unwrap != nil {
			go ps.unwrap(chunk)
		}
	} else if !soc.Valid(chunk) {
		return swarm.ErrInvalidChunk
	}

	price := ps.pricer.Price(chunkAddress)

	// if the peer is closer to the chunk, AND it's a full node, we were selected for replication. Return early.
	if p.FullNode {
		if closer, _ := p.Address.Closer(chunkAddress, ps.address); closer {

			ps.metrics.HandlerReplication.Inc()

			var err error
			defer func() {
				if err != nil {
					ps.metrics.HandlerReplicationErrors.Inc()
				}
			}()

			ctxd, canceld := context.WithTimeout(context.Background(), replicationTTL)
			defer canceld()

			span, _, ctxd := ps.tracer.StartSpanFromContext(ctxd, "pushsync-replication-storage", ps.logger, opentracing.Tag{Key: "address", Value: chunkAddress.String()})
			defer span.Finish()

			realClosestPeer, err := ps.topologyDriver.ClosestPeer(chunk.Address(), false, topology.Filter{Reachable: true})
			if err == nil {
				if !realClosestPeer.Equal(p.Address) {
					ps.metrics.TotalReplicationFromDistantPeer.Inc()
				} else {
					ps.metrics.TotalReplicationFromClosestPeer.Inc()
				}
			}

			chunk, err = ps.validStamp(chunk, ch.Stamp)
			if err != nil {
				return fmt.Errorf("pushsync replication invalid stamp: %w", err)
			}

			_, err = ps.storer.Put(ctxd, storage.ModePutSync, chunk)
			if err != nil {
				return fmt.Errorf("chunk store: %w", err)
			}

			debit, err := ps.accounting.PrepareDebit(ctx, p.Address, price)
			if err != nil {
				return fmt.Errorf("prepare debit to peer %s before writeback: %w", p.Address.String(), err)
			}
			defer debit.Cleanup()

			// return back receipt
			signature, err := ps.signer.Sign(chunkAddress.Bytes())
			if err != nil {
				return fmt.Errorf("receipt signature: %w", err)
			}

			receipt := pb.Receipt{Address: chunkAddress.Bytes(), Signature: signature, Nonce: ps.nonce}
			err = w.WriteMsgWithContext(ctxd, &receipt)
			if err != nil {
				return fmt.Errorf("send receipt to peer %s: %w", p.Address.String(), err)
			}

			err = debit.Apply()
			return err
		}
	}

	// forwarding replication
	storerNode := false
	defer func() {
		if !storerNode && ps.warmedUp() && ps.topologyDriver.IsWithinDepth(chunkAddress) {
			verifiedChunk, err := ps.validStamp(chunk, ch.Stamp)
			if err != nil {
				logger.Warning("forwarder, invalid stamp for chunk", "chunk_address", chunkAddress)
				return
			}
			_, err = ps.storer.Put(ctx, storage.ModePutSync, verifiedChunk)
			if err != nil {
				logger.Warning("within depth peer's attempt to store chunk failed", "chunk_address", verifiedChunk.Address(), "error", err)
			}
		}
	}()

	receipt, err := ps.pushToClosest(ctx, chunk, false, p.Address)
	if err != nil {
		if errors.Is(err, topology.ErrWantSelf) {
			storerNode = true
			ps.metrics.Storer.Inc()
			chunk, err = ps.validStamp(chunk, ch.Stamp)
			if err != nil {
				return fmt.Errorf("pushsync storer invalid stamp: %w", err)
			}

			_, err = ps.storer.Put(ctx, storage.ModePutSync, chunk)
			if err != nil {
				return fmt.Errorf("chunk store: %w", err)
			}

			signature, err := ps.signer.Sign(ch.Address)
			if err != nil {
				return fmt.Errorf("receipt signature: %w", err)
			}

			// return back receipt
			debit, err := ps.accounting.PrepareDebit(ctx, p.Address, price)
			if err != nil {
				return fmt.Errorf("prepare debit to peer %s before writeback: %w", p.Address.String(), err)
			}
			defer debit.Cleanup()

			receipt := pb.Receipt{Address: chunkAddress.Bytes(), Signature: signature, Nonce: ps.nonce}
			if err := w.WriteMsgWithContext(ctx, &receipt); err != nil {
				return fmt.Errorf("send receipt to peer %s: %w", p.Address.String(), err)
			}

			return debit.Apply()
		}

		ps.metrics.Forwarder.Inc()

		return fmt.Errorf("handler: push to closest: %w", err)
	}

	ps.metrics.Forwarder.Inc()

	debit, err := ps.accounting.PrepareDebit(ctx, p.Address, price)
	if err != nil {
		return fmt.Errorf("prepare debit to peer %s before writeback: %w", p.Address.String(), err)
	}
	defer debit.Cleanup()

	// pass back the receipt
	if err := w.WriteMsgWithContext(ctx, receipt); err != nil {
		return fmt.Errorf("send receipt to peer %s: %w", p.Address.String(), err)
	}

	return debit.Apply()
}

// PushChunkToClosest sends chunk to the closest peer by opening a stream. It then waits for
// a receipt from that peer and returns error or nil based on the receiving and
// the validity of the receipt.
func (ps *PushSync) PushChunkToClosest(ctx context.Context, ch swarm.Chunk) (*Receipt, error) {
	ps.metrics.TotalOutgoing.Inc()
	r, err := ps.pushToClosest(ctx, ch, true, swarm.ZeroAddress)
	if err != nil {
		ps.metrics.TotalOutgoingErrors.Inc()
		return nil, err
	}
	return &Receipt{
		Address:   swarm.NewAddress(r.Address),
		Signature: r.Signature,
		Nonce:     r.Nonce,
	}, nil
}

func (ps *PushSync) pushToClosest(ctx context.Context, ch swarm.Chunk, origin bool, originAddr swarm.Address) (*pb.Receipt, error) {
	span, logger, ctx := ps.tracer.StartSpanFromContext(ctx, "push-closest", ps.logger, opentracing.Tag{Key: "address", Value: ch.Address().String()})
	defer span.Finish()
	defer ps.skipList.PruneExpired()

	var (
		// limits "attempted" requests, see pushPeer when a request becomes attempted
		allowedPushes = 1
		// limits total requests, irregardless of "attempted"
		allowedRetries = maxPeers
	)

	if origin {
		allowedPushes = maxAttempts
	}

	var (
		includeSelf = ps.isFullNode
		skipPeers   = new(skippeers.List)
	)

	resultChan := make(chan receiptResult)
	doneChan := make(chan struct{})
	defer close(doneChan)

	timer := time.NewTimer(0)
	defer timer.Stop()

	// nextPeer attempts to lookup the next peer to push the chunk to, if there are overdrafted peers the boolean would signal a re-attempt
	nextPeer := func() (peer swarm.Address, retry bool, err error) {

		fullSkipList := append(ps.skipList.ChunkSkipPeers(ch.Address()), skipPeers.All()...)

		peer, err = ps.topologyDriver.ClosestPeer(ch.Address(), includeSelf, topology.Filter{Reachable: true}, fullSkipList...)
		if err != nil {
			// ClosestPeer can return ErrNotFound in case we are not connected to any peers
			// in which case we should return immediately.
			// if ErrWantSelf is returned, it means we are the closest peer.
			if errors.Is(err, topology.ErrWantSelf) {

				if !ps.warmedUp() {
					return swarm.ZeroAddress, false, ErrWarmup
				}

				if skipPeers.OverdraftListEmpty() { // no peers in skip list means we can be confident that we are the closest peer
					// we don't act on ErrWantSelf unless there are no overdraft peers
					if !ps.topologyDriver.IsWithinDepth(ch.Address()) {
						return swarm.ZeroAddress, false, ErrOutOfDepthStoring
					}
					ps.pushToNeighbourhood(ctx, fullSkipList, ch, origin, originAddr)
					return swarm.ZeroAddress, false, err
				}

				ps.logger.Debug("pushsync: continue iteration and reset overdraft skiplist")

				skipPeers.ResetOverdraft() // reset the overdraft list and retry (in case the closest peer was there)
				return swarm.ZeroAddress, true, nil
			}

			return swarm.ZeroAddress, false, fmt.Errorf("closest peer: %w", err)
		}

		return peer, false, nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ErrNoPush
		case <-timer.C:

			allowedRetries--
			// decrement here to limit inflight requests, if the request is not "attempted", we will increment below

			peer, retry, err := nextPeer()
			if err != nil {
				return nil, err
			}

			if retry {
				if allowedRetries <= 0 {
					return nil, ErrNoPush
				}
				timer.Reset(waitRefresh)
				continue
			}

			ps.metrics.TotalSendAttempts.Inc()

			skipPeers.Add(peer)

			allowedPushes--

			go func() {
				ctxd, cancel := context.WithTimeout(ctx, defaultTTL)
				defer cancel()
				ps.pushPeer(ctxd, resultChan, doneChan, peer, ch, origin)
			}()

			// reached the limit, do not set timer to retry
			if allowedRetries <= 0 || allowedPushes <= 0 {
				continue
			}

			// retry
			timer.Reset(p90TTL)

		case result := <-resultChan:

			ps.measurePushPeer(result.pushTime, result.err, origin)

			if errors.Is(result.err, errNotAttempted) {
				logger.Debug("not attempted: adding overdraft peer to skiplist", "peer_address", result.peer)
				skipPeers.AddOverdraft(result.peer)
			}

			if ps.warmedUp() && !errors.Is(result.err, errNotAttempted) {
				ps.skipList.Add(ch.Address(), result.peer, sanctionWait)
				ps.metrics.TotalSkippedPeers.Inc()
				logger.Debug("adding peer to skiplist", "peer_address", result.peer)
			}

			if result.err == nil {
				return result.receipt, nil
			}

			ps.metrics.TotalFailedSendAttempts.Inc()
			logger.Debug("could not push to peer", "peer_address", result.peer, "error", result.err)

			// pushPeer returned early, do not count as an attempt
			if !result.pushed {
				allowedPushes++
			}

			if allowedRetries <= 0 || allowedPushes <= 0 {
				return nil, ErrNoPush
			}

			// retry immediately
			timer.Reset(0)
		}
	}
}

func (ps *PushSync) measurePushPeer(t time.Time, err error, origin bool) {
	var status string
	if err != nil {
		status = "failure"
	} else {
		status = "success"
		if origin {
			ps.metrics.OriginPushTime.Observe(time.Since(t).Seconds())
		}
	}
	ps.metrics.PushToPeerTime.WithLabelValues(status).Observe(time.Since(t).Seconds())
}

func (ps *PushSync) pushPeer(ctx context.Context, resultChan chan<- receiptResult, doneChan <-chan struct{}, peer swarm.Address, ch swarm.Chunk, origin bool) {

	var (
		err     error
		receipt pb.Receipt
		pushed  bool
		now     = time.Now()
	)

	defer func() {
		select {
		case resultChan <- receiptResult{pushTime: now, peer: peer, err: err, pushed: pushed, receipt: &receipt}:
		case <-doneChan:
			ps.metrics.DuplicateReceipt.Inc()
		}
	}()

	// compute the price we pay for this receipt and reserve it for the rest of this function
	receiptPrice := ps.pricer.PeerPrice(peer, ch.Address())

	creditCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// Reserve to see whether we can make the request
	creditAction, err := ps.accounting.PrepareCredit(creditCtx, peer, receiptPrice, origin)
	if err != nil {
		if errors.Is(err, accounting.ErrOverdraft) || errors.Is(err, accounting.ErrFailToLock) {
			err = fmt.Errorf("pushsync: prepare credit: %v: %w", err, errNotAttempted)
			return
		}
		err = fmt.Errorf("reserve balance for peer %s: %w", peer, err)
		return
	}
	defer creditAction.Cleanup()

	stamp, err := ch.Stamp().MarshalBinary()
	if err != nil {
		return
	}

	streamer, err := ps.streamer.NewStream(ctx, peer, nil, protocolName, protocolVersion, streamName)
	if err != nil {
		err = fmt.Errorf("new stream for peer %s: %w", peer, err)
		return
	}
	defer streamer.Close()

	w, r := protobuf.NewWriterAndReader(streamer)
	err = w.WriteMsgWithContext(ctx, &pb.Delivery{
		Address: ch.Address().Bytes(),
		Data:    ch.Data(),
		Stamp:   stamp,
	})
	if err != nil {
		_ = streamer.Reset()
		err = fmt.Errorf("chunk %s deliver to peer %s: %w", ch.Address(), peer, err)
		return
	}

	ps.metrics.TotalSent.Inc()

	pushed = true

	// if you manage to get a tag, just increment the respective counter
	t, err := ps.tagger.Get(ch.TagID())
	if err == nil && t != nil {
		err = t.Inc(tags.StateSent)
		if err != nil {
			err = fmt.Errorf("tag %d increment: %w", ch.TagID(), err)
			return
		}
	}

	err = r.ReadMsgWithContext(ctx, &receipt)
	if err != nil {
		_ = streamer.Reset()
		err = fmt.Errorf("chunk %s receive receipt from peer %s: %w", ch.Address(), peer, err)
		return
	}

	if !ch.Address().Equal(swarm.NewAddress(receipt.Address)) {
		// if the receipt is invalid, try to push to the next peer
		err = fmt.Errorf("invalid receipt. chunk %s, peer %s", ch.Address(), peer)
		return
	}

	err = creditAction.Apply()
}

func (ps *PushSync) pushToNeighbourhood(ctx context.Context, skiplist []swarm.Address, ch swarm.Chunk, origin bool, originAddr swarm.Address) {
	count := 0
	// Push the chunk to some peers in the neighborhood in parallel for replication.
	// Any errors here should NOT impact the rest of the handler.
	_ = ps.topologyDriver.EachPeer(func(peer swarm.Address, po uint8) (bool, bool, error) {
		// skip forwarding peer
		if peer.Equal(originAddr) {
			return false, false, nil
		}

		// skip skiplisted peers
		for _, s := range skiplist {
			if peer.Equal(s) {
				return false, false, nil
			}
		}

		// here we skip the peer if the peer is closer to the chunk than us
		// we replicate with peers that are further away than us because we are the storer
		if closer, _ := peer.Closer(ch.Address(), ps.address); closer {
			return false, false, nil
		}

		if count == nPeersToReplicate {
			return true, false, nil
		}
		count++
		go ps.pushToNeighbour(ctx, peer, ch, origin)
		return false, false, nil
	}, topology.Filter{Reachable: true})
}

// pushToNeighbour handles in-neighborhood replication for a single peer.
func (ps *PushSync) pushToNeighbour(ctx context.Context, peer swarm.Address, ch swarm.Chunk, origin bool) {
	var err error
	ps.metrics.TotalReplicatedAttempts.Inc()

	// price for neighborhood replication
	receiptPrice := ps.pricer.PeerPrice(peer, ch.Address())

	// decouple the span data from the original context so it doesn't get
	// cancelled, then glue the stuff on the new context
	span := tracing.FromContext(ctx)

	ctx, cancel := context.WithTimeout(context.Background(), replicationTTL)
	defer cancel()

	// now bring in the span data to the new context
	ctx = tracing.WithContext(ctx, span)
	spanInner, logger, ctx := ps.tracer.StartSpanFromContext(ctx, "pushsync-replication", ps.logger, opentracing.Tag{Key: "address", Value: ch.Address().String()})
	loggerV1 := logger.V(1).Build()
	defer spanInner.Finish()
	defer func() {
		if err != nil {
			loggerV1.Debug("pushsync replication failed", "error", err)
			ps.metrics.TotalReplicatedError.Inc()
		}
	}()

	creditCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	creditAction, err := ps.accounting.PrepareCredit(creditCtx, peer, receiptPrice, origin)
	if err != nil {
		err = fmt.Errorf("reserve balance for peer %s: %w", peer.String(), err)
		return
	}
	defer creditAction.Cleanup()

	streamer, err := ps.streamer.NewStream(ctx, peer, nil, protocolName, protocolVersion, streamName)
	if err != nil {
		err = fmt.Errorf("new stream for peer %s: %w", peer.String(), err)
		return
	}

	defer func() {
		if err != nil {
			_ = streamer.Reset()
		} else {
			_ = streamer.FullClose()
		}
	}()

	w, r := protobuf.NewWriterAndReader(streamer)
	stamp, err := ch.Stamp().MarshalBinary()
	if err != nil {
		return
	}
	err = w.WriteMsgWithContext(ctx, &pb.Delivery{
		Address: ch.Address().Bytes(),
		Data:    ch.Data(),
		Stamp:   stamp,
	})
	if err != nil {
		return
	}

	var receipt pb.Receipt
	if err = r.ReadMsgWithContext(ctx, &receipt); err != nil {
		return
	}

	if !ch.Address().Equal(swarm.NewAddress(receipt.Address)) {
		// if the receipt is invalid, give up
		return
	}

	if err = creditAction.Apply(); err != nil {
		return
	}
}

func (ps *PushSync) validStampWrapper(f postage.ValidStampFn) postage.ValidStampFn {
	return func(c swarm.Chunk, s []byte) (swarm.Chunk, error) {

		t := time.Now()

		chunk, err := f(c, s)
		if err != nil {
			ps.metrics.InvalidStampErrors.Inc()
			ps.metrics.StampValidationTime.WithLabelValues("failure").Observe(time.Since(t).Seconds())
		} else {
			ps.metrics.StampValidationTime.WithLabelValues("success").Observe(time.Since(t).Seconds())
		}

		return chunk, err
	}
}

func (ps *PushSync) warmedUp() bool {
	return time.Now().After(ps.warmupPeriod)
}

type peerSkipList struct {
	sync.Mutex

	// key is chunk address, value is map of peer address to expiration
	skip map[string]map[string]time.Time
}

func newPeerSkipList() *peerSkipList {
	return &peerSkipList{
		skip: make(map[string]map[string]time.Time),
	}
}

func (l *peerSkipList) Add(chunk, peer swarm.Address, expire time.Duration) {
	l.Lock()
	defer l.Unlock()

	if _, ok := l.skip[chunk.ByteString()]; !ok {
		l.skip[chunk.ByteString()] = make(map[string]time.Time)
	}
	l.skip[chunk.ByteString()][peer.ByteString()] = time.Now().Add(expire)
}

func (l *peerSkipList) ChunkSkipPeers(ch swarm.Address) (peers []swarm.Address) {
	l.Lock()
	defer l.Unlock()

	if p, ok := l.skip[ch.ByteString()]; ok {
		for peer, exp := range p {
			if time.Now().Before(exp) {
				peers = append(peers, swarm.NewAddress([]byte(peer)))
			}
		}
	}
	return peers
}

func (l *peerSkipList) PruneExpired() {
	l.Lock()
	defer l.Unlock()

	now := time.Now()

	for k, v := range l.skip {
		kc := len(v)
		for kk, vv := range v {
			if vv.Before(now) {
				delete(v, kk)
				kc--
			}
		}
		if kc == 0 {
			// prune the chunk too
			delete(l.skip, k)
		}
	}
}
