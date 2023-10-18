package brdcst

import (
	"context"
	"fmt"

	"github.com/plprobelab/go-libdht/kad"
	"go.opentelemetry.io/otel/trace"

	"github.com/plprobelab/zikade/internal/coord/coordt"
	"github.com/plprobelab/zikade/internal/coord/query"
	"github.com/plprobelab/zikade/tele"
)

// ConfigFollowUp specifies the configuration for the [FollowUp] state machine.
type ConfigFollowUp[K kad.Key[K]] struct {
	Target K
}

// Validate checks the configuration options and returns an error if any have
// invalid values.
func (c *ConfigFollowUp[K]) Validate() error {
	return nil
}

// DefaultConfigFollowUp returns the default configuration options for the
// [FollowUp] state machine.
func DefaultConfigFollowUp[K kad.Key[K]](target K) *ConfigFollowUp[K] {
	return &ConfigFollowUp[K]{
		Target: target,
	}
}

// FollowUp is a [Broadcast] state machine and encapsulates the logic around
// doing a "classic" put operation. This mimics the algorithm employed in the
// original go-libp2p-kad-dht v1 code base. It first queries the closest nodes
// to a certain target key, and after they were discovered, it "follows up" with
// storing the record with these closest nodes.
type FollowUp[K kad.Key[K], N kad.NodeID[K], M coordt.Message] struct {
	// the unique ID for this broadcast operation
	queryID coordt.QueryID

	// a struct holding configuration options
	cfg *ConfigFollowUp[K]

	// a reference to the query pool in which the "get closer nodes" queries
	// will be spawned. This pool is governed by the broadcast [Pool].
	// Unfortunately, having a reference here breaks the hierarchy but it makes
	// the logic much easier to implement.
	pool *query.Pool[K, N, M]

	// started indicates that this state machine has sent out the first
	// message to a node. Even after this state machine has returned a finished
	// state, this flag will stay true.
	started bool

	// the message generator that takes a target key and will return the message
	// that we will send to the closest nodes in the follow-up phase
	msgFunc func(K) M

	// seed holds the nodes from where we should start our query to find closer
	// nodes to the target key (held by [ConfigFollowUp]).
	seed []N

	// the closest nodes to the target key. This will be filled after the query
	// for the closest nodes has finished (when the query pool emits a
	// [query.StatePoolQueryFinished] event).
	closest []N

	// nodes we still need to store records with. This map will be filled with
	// all the closest nodes after the query has finished.
	todo map[string]N

	// nodes we have contacted to store the record but haven't heard a response yet
	waiting map[string]N

	// nodes that successfully hold the record for us
	success map[string]N

	// nodes that failed to hold the record for us
	failed map[string]struct {
		Node N
		Err  error
	}
}

// NewFollowUp initializes a new [FollowUp] struct.
func NewFollowUp[K kad.Key[K], N kad.NodeID[K], M coordt.Message](qid coordt.QueryID, msgFunc func(K) M, pool *query.Pool[K, N, M], seed []N, cfg *ConfigFollowUp[K]) *FollowUp[K, N, M] {
	f := &FollowUp[K, N, M]{
		queryID: qid,
		cfg:     cfg,
		pool:    pool,
		started: false,
		msgFunc: msgFunc,
		seed:    seed,
		todo:    map[string]N{},
		waiting: map[string]N{},
		success: map[string]N{},
		failed: map[string]struct {
			Node N
			Err  error
		}{},
	}

	return f
}

// Advance advances the state of the [FollowUp] [Broadcast] state machine. It
// first handles the event by mapping it to a potential event for the query
// pool. If the [BroadcastEvent] maps to a [query.PoolEvent], it gets forwarded
// to the query pool and handled in [FollowUp.advancePool]. If it doesn't map to
// a query pool event, we check if there are any nodes we should contact to hold
// the record for us and emit that instruction instead. Similarly, if we're
// waiting on responses or are completely finished, we return that as well.
func (f *FollowUp[K, N, M]) Advance(ctx context.Context, ev BroadcastEvent) (out BroadcastState) {
	ctx, span := tele.StartSpan(ctx, "FollowUp.Advance", trace.WithAttributes(tele.AttrInEvent(ev)))
	defer func() {
		span.SetAttributes(tele.AttrOutEvent(out))
		span.End()
	}()

	pev := f.handleEvent(ctx, ev)
	if pev != nil {
		if state, terminal := f.advancePool(ctx, pev); terminal {
			return state
		}
	}

	_, isStopEvent := ev.(*EventBroadcastStop)
	if isStopEvent {
		for _, n := range f.todo {
			delete(f.todo, n.String())
			f.failed[n.String()] = struct {
				Node N
				Err  error
			}{Node: n, Err: fmt.Errorf("cancelled")}
		}

		for _, n := range f.waiting {
			delete(f.waiting, n.String())
			f.failed[n.String()] = struct {
				Node N
				Err  error
			}{Node: n, Err: fmt.Errorf("cancelled")}
		}
	}

	for k, n := range f.todo {
		delete(f.todo, k)
		f.waiting[k] = n
		return &StateBroadcastStoreRecord[K, N, M]{
			QueryID: f.queryID,
			NodeID:  n,
			Target:  f.cfg.Target,
			Message: f.msgFunc(f.cfg.Target),
		}
	}

	if len(f.waiting) > 0 {
		return &StateBroadcastWaiting{}
	}

	if isStopEvent || (len(f.todo) == 0 && len(f.closest) != 0) {
		return &StateBroadcastFinished[K, N]{
			QueryID:   f.queryID,
			Contacted: f.closest,
			Errors:    f.failed,
		}
	}

	return &StateBroadcastIdle{}
}

// handleEvent receives a [BroadcastEvent] and returns the corresponding query
// pool event ([query.PoolEvent]). Some [BroadcastEvent] events don't map to
// a query pool event, in which case this method handles that event and returns
// nil.
func (f *FollowUp[K, N, M]) handleEvent(ctx context.Context, ev BroadcastEvent) (out query.PoolEvent) {
	_, span := tele.StartSpan(ctx, "FollowUp.handleEvent", trace.WithAttributes(tele.AttrInEvent(ev)))
	defer func() {
		span.SetAttributes(tele.AttrOutEvent(out))
		span.End()
	}()

	switch ev := ev.(type) {
	case *EventBroadcastStop:
		if f.isQueryDone() {
			return nil
		}

		return &query.EventPoolStopQuery{
			QueryID: f.queryID,
		}
	case *EventBroadcastNodeResponse[K, N]:
		return &query.EventPoolNodeResponse[K, N]{
			QueryID:     f.queryID,
			NodeID:      ev.NodeID,
			CloserNodes: ev.CloserNodes,
		}
	case *EventBroadcastNodeFailure[K, N]:
		return &query.EventPoolNodeFailure[K, N]{
			QueryID: f.queryID,
			NodeID:  ev.NodeID,
			Error:   ev.Error,
		}
	case *EventBroadcastStoreRecordSuccess[K, N, M]:
		delete(f.waiting, ev.NodeID.String())
		f.success[ev.NodeID.String()] = ev.NodeID
	case *EventBroadcastStoreRecordFailure[K, N, M]:
		delete(f.waiting, ev.NodeID.String())
		f.failed[ev.NodeID.String()] = struct {
			Node N
			Err  error
		}{Node: ev.NodeID, Err: ev.Error}
	case *EventBroadcastPoll:
		if !f.started {
			f.started = true
			return &query.EventPoolAddFindCloserQuery[K, N]{
				QueryID: f.queryID,
				Target:  f.cfg.Target,
				Seed:    f.seed,
			}
		}
		return &query.EventPoolPoll{}
	default:
		panic(fmt.Sprintf("unexpected event: %T", ev))
	}

	return nil
}

// advancePool advances the query pool with the given query pool event that was
// returned by [FollowUp.handleEvent]. The additional boolean value indicates
// whether the returned [BroadcastState] should be ignored.
func (f *FollowUp[K, N, M]) advancePool(ctx context.Context, ev query.PoolEvent) (out BroadcastState, term bool) {
	ctx, span := tele.StartSpan(ctx, "FollowUp.advanceQuery", trace.WithAttributes(tele.AttrInEvent(ev)))
	defer func() {
		span.SetAttributes(tele.AttrOutEvent(out))
		span.End()
	}()

	state := f.pool.Advance(ctx, ev)
	switch st := state.(type) {
	case *query.StatePoolFindCloser[K, N]:
		return &StateBroadcastFindCloser[K, N]{
			QueryID: st.QueryID,
			NodeID:  st.NodeID,
			Target:  st.Target,
		}, true
	case *query.StatePoolWaitingAtCapacity:
		return &StateBroadcastWaiting{
			QueryID: f.queryID,
		}, true
	case *query.StatePoolWaitingWithCapacity:
		return &StateBroadcastWaiting{
			QueryID: f.queryID,
		}, true
	case *query.StatePoolQueryFinished[K, N]:
		if len(st.ClosestNodes) == 0 {
			return &StateBroadcastFinished[K, N]{
				QueryID:   f.queryID,
				Contacted: make([]N, 0),
				Errors: map[string]struct {
					Node N
					Err  error
				}{},
			}, true
		}

		f.closest = st.ClosestNodes

		for _, n := range st.ClosestNodes {
			f.todo[n.String()] = n
		}

	case *query.StatePoolQueryTimeout:
		return &StateBroadcastFinished[K, N]{
			QueryID:   f.queryID,
			Contacted: make([]N, 0),
			Errors: map[string]struct {
				Node N
				Err  error
			}{},
		}, true
	case *query.StatePoolIdle:
		// nothing to do
	default:
		panic(fmt.Sprintf("unexpected pool state: %T", st))
	}

	return nil, false
}

// isQueryDone returns true if the DHT walk/ query phase has finished.
// This is indicated by the fact that the [FollowUp.closest] slice is filled.
func (f *FollowUp[K, N, M]) isQueryDone() bool {
	return len(f.closest) != 0
}
