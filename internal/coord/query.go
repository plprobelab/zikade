package coord

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/benbjohnson/clock"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/exp/slog"

	"github.com/plprobelab/zikade/errs"
	"github.com/plprobelab/zikade/internal/coord/coordt"
	"github.com/plprobelab/zikade/internal/coord/query"
	"github.com/plprobelab/zikade/kadt"
	"github.com/plprobelab/zikade/pb"
	"github.com/plprobelab/zikade/tele"
)

type PooledQueryConfig struct {
	// Clock is a clock that may replaced by a mock when testing
	Clock clock.Clock

	// Logger is a structured logger that will be used when logging.
	Logger *slog.Logger

	// Tracer is the tracer that should be used to trace execution.
	Tracer trace.Tracer

	// Concurrency is the maximum number of queries that may be waiting for message responses at any one time.
	Concurrency int

	// Timeout the time to wait before terminating a query that is not making progress.
	Timeout time.Duration

	// RequestConcurrency is the maximum number of concurrent requests that each query may have in flight.
	RequestConcurrency int

	// RequestTimeout is the timeout queries should use for contacting a single node
	RequestTimeout time.Duration
}

// Validate checks the configuration options and returns an error if any have invalid values.
func (cfg *PooledQueryConfig) Validate() error {
	if cfg.Clock == nil {
		return &errs.ConfigurationError{
			Component: "PooledQueryConfig",
			Err:       fmt.Errorf("clock must not be nil"),
		}
	}

	if cfg.Logger == nil {
		return &errs.ConfigurationError{
			Component: "PooledQueryConfig",
			Err:       fmt.Errorf("logger must not be nil"),
		}
	}

	if cfg.Tracer == nil {
		return &errs.ConfigurationError{
			Component: "PooledQueryConfig",
			Err:       fmt.Errorf("tracer must not be nil"),
		}
	}

	if cfg.Concurrency < 1 {
		return &errs.ConfigurationError{
			Component: "PooledQueryConfig",
			Err:       fmt.Errorf("query concurrency must be greater than zero"),
		}
	}
	if cfg.Timeout < 1 {
		return &errs.ConfigurationError{
			Component: "PooledQueryConfig",
			Err:       fmt.Errorf("query timeout must be greater than zero"),
		}
	}

	if cfg.RequestConcurrency < 1 {
		return &errs.ConfigurationError{
			Component: "PooledQueryConfig",
			Err:       fmt.Errorf("request concurrency must be greater than zero"),
		}
	}

	if cfg.RequestTimeout < 1 {
		return &errs.ConfigurationError{
			Component: "PooledQueryConfig",
			Err:       fmt.Errorf("request timeout must be greater than zero"),
		}
	}

	return nil
}

func DefaultPooledQueryConfig() *PooledQueryConfig {
	return &PooledQueryConfig{
		Clock:              clock.New(),
		Logger:             tele.DefaultLogger("coord"),
		Tracer:             tele.NoopTracer(),
		Concurrency:        3,               // MAGIC
		Timeout:            5 * time.Minute, // MAGIC
		RequestConcurrency: 3,               // MAGIC
		RequestTimeout:     time.Minute,     // MAGIC

	}
}

// PooledQueryBehaviour holds the behaviour and state for managing a pool of queries.
type PooledQueryBehaviour struct {
	// cfg is a copy of the optional configuration supplied to the behaviour.
	cfg PooledQueryConfig

	// pool is the query pool state machine used for managing individual queries.
	pool *query.Pool[kadt.Key, kadt.PeerID, *pb.Message]

	// waiters is a map that keeps track of event notifications for each running query.
	waiters map[coordt.QueryID]NotifyCloser[BehaviourEvent]

	// pendingMu guards access to pending
	pendingMu sync.Mutex

	// pending is a queue of pending events that need to be processed.
	pending []BehaviourEvent

	// ready is a channel signaling that events are ready to be processed.
	ready chan struct{}
}

// NewPooledQueryBehaviour initialises a new PooledQueryBehaviour, setting up the query
// pool and other internal state.
func NewPooledQueryBehaviour(self kadt.PeerID, cfg *PooledQueryConfig) (*PooledQueryBehaviour, error) {
	if cfg == nil {
		cfg = DefaultPooledQueryConfig()
	} else if err := cfg.Validate(); err != nil {
		return nil, err
	}

	qpCfg := query.DefaultPoolConfig()
	qpCfg.Clock = cfg.Clock
	qpCfg.Concurrency = cfg.Concurrency
	qpCfg.Timeout = cfg.Timeout
	qpCfg.QueryConcurrency = cfg.RequestConcurrency
	qpCfg.RequestTimeout = cfg.RequestTimeout

	pool, err := query.NewPool[kadt.Key, kadt.PeerID, *pb.Message](self, qpCfg)
	if err != nil {
		return nil, fmt.Errorf("query pool: %w", err)
	}

	h := &PooledQueryBehaviour{
		cfg:     *cfg,
		pool:    pool,
		waiters: make(map[coordt.QueryID]NotifyCloser[BehaviourEvent]),
		ready:   make(chan struct{}, 1),
	}
	return h, err
}

// Notify receives a behaviour event and takes appropriate actions such as starting,
// stopping, or updating queries. It also queues events for later processing and
// triggers the advancement of the query pool if applicable.
func (p *PooledQueryBehaviour) Notify(ctx context.Context, ev BehaviourEvent) {
	ctx, span := p.cfg.Tracer.Start(ctx, "PooledQueryBehaviour.Notify")
	defer span.End()

	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()

	var cmd query.PoolEvent
	switch ev := ev.(type) {
	case *EventStartFindCloserQuery:
		cmd = &query.EventPoolAddFindCloserQuery[kadt.Key, kadt.PeerID]{
			QueryID: ev.QueryID,
			Target:  ev.Target,
			Seed:    ev.KnownClosestNodes,
		}
		if ev.Notify != nil {
			p.waiters[ev.QueryID] = ev.Notify
		}
	case *EventStartMessageQuery:
		cmd = &query.EventPoolAddQuery[kadt.Key, kadt.PeerID, *pb.Message]{
			QueryID: ev.QueryID,
			Target:  ev.Target,
			Message: ev.Message,
			Seed:    ev.KnownClosestNodes,
		}
		if ev.Notify != nil {
			p.waiters[ev.QueryID] = ev.Notify
		}
	case *EventStopQuery:
		cmd = &query.EventPoolStopQuery{
			QueryID: ev.QueryID,
		}
	case *EventGetCloserNodesSuccess:
		for _, info := range ev.CloserNodes {
			// TODO: do this after advancing pool
			p.pending = append(p.pending, &EventAddNode{
				NodeID: info,
			})
		}
		waiter, ok := p.waiters[ev.QueryID]
		if ok {
			waiter.Notify(ctx, &EventQueryProgressed{
				NodeID:  ev.To,
				QueryID: ev.QueryID,
				// CloserNodes: CloserNodeIDs(ev.CloserNodes),
				// Stats:    stats,
			})
		}
		cmd = &query.EventPoolNodeResponse[kadt.Key, kadt.PeerID]{
			NodeID:      ev.To,
			QueryID:     ev.QueryID,
			CloserNodes: ev.CloserNodes,
		}
	case *EventGetCloserNodesFailure:
		// queue an event that will notify the routing behaviour of a failed node
		p.cfg.Logger.Debug("peer has no connectivity", tele.LogAttrPeerID(ev.To), "source", "query")
		p.pending = append(p.pending, &EventNotifyNonConnectivity{
			ev.To,
		})

		cmd = &query.EventPoolNodeFailure[kadt.Key, kadt.PeerID]{
			NodeID:  ev.To,
			QueryID: ev.QueryID,
			Error:   ev.Err,
		}
	case *EventSendMessageSuccess:
		for _, info := range ev.CloserNodes {
			// TODO: do this after advancing pool
			p.pending = append(p.pending, &EventAddNode{
				NodeID: info,
			})
		}
		waiter, ok := p.waiters[ev.QueryID]
		if ok {
			waiter.Notify(ctx, &EventQueryProgressed{
				NodeID:   ev.To,
				QueryID:  ev.QueryID,
				Response: ev.Response,
			})
		}
		cmd = &query.EventPoolNodeResponse[kadt.Key, kadt.PeerID]{
			NodeID:      ev.To,
			QueryID:     ev.QueryID,
			CloserNodes: ev.CloserNodes,
		}
	case *EventSendMessageFailure:
		// queue an event that will notify the routing behaviour of a failed node
		p.cfg.Logger.Debug("peer has no connectivity", tele.LogAttrPeerID(ev.To), "source", "query")
		p.pending = append(p.pending, &EventNotifyNonConnectivity{
			ev.To,
		})

		cmd = &query.EventPoolNodeFailure[kadt.Key, kadt.PeerID]{
			NodeID:  ev.To,
			QueryID: ev.QueryID,
			Error:   ev.Err,
		}
	default:
		panic(fmt.Sprintf("unexpected dht event: %T", ev))
	}

	// attempt to advance the query pool
	ev, ok := p.advancePool(ctx, cmd)
	if ok {
		p.pending = append(p.pending, ev)
	}
	if len(p.pending) > 0 {
		select {
		case p.ready <- struct{}{}:
		default:
		}
	}
}

// Ready returns a channel that signals when the pooled query behaviour is ready to
// perform work.
func (p *PooledQueryBehaviour) Ready() <-chan struct{} {
	return p.ready
}

// Perform executes the next available task from the queue of pending events or advances
// the query pool. Returns the executed event and a boolean indicating whether work was
// performed.
func (p *PooledQueryBehaviour) Perform(ctx context.Context) (BehaviourEvent, bool) {
	ctx, span := p.cfg.Tracer.Start(ctx, "PooledQueryBehaviour.Perform")
	defer span.End()

	// No inbound work can be done until Perform is complete
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()

	for {
		// drain queued events first.
		if len(p.pending) > 0 {
			var ev BehaviourEvent
			ev, p.pending = p.pending[0], p.pending[1:]

			if len(p.pending) > 0 {
				select {
				case p.ready <- struct{}{}:
				default:
				}
			}
			return ev, true
		}

		// attempt to advance the query pool
		ev, ok := p.advancePool(ctx, &query.EventPoolPoll{})
		if ok {
			return ev, true
		}

		if len(p.pending) == 0 {
			return nil, false
		}
	}
}

// advancePool advances the query pool state machine and returns an outbound event if
// there is work to be performed. Also notifies waiters of query completion or
// progress.
func (p *PooledQueryBehaviour) advancePool(ctx context.Context, ev query.PoolEvent) (out BehaviourEvent, term bool) {
	ctx, span := p.cfg.Tracer.Start(ctx, "PooledQueryBehaviour.advancePool", trace.WithAttributes(tele.AttrInEvent(ev)))
	defer func() {
		span.SetAttributes(tele.AttrOutEvent(out))
		span.End()
	}()

	pstate := p.pool.Advance(ctx, ev)
	switch st := pstate.(type) {
	case *query.StatePoolFindCloser[kadt.Key, kadt.PeerID]:
		return &EventOutboundGetCloserNodes{
			QueryID: st.QueryID,
			To:      st.NodeID,
			Target:  st.Target,
			Notify:  p,
		}, true
	case *query.StatePoolSendMessage[kadt.Key, kadt.PeerID, *pb.Message]:
		return &EventOutboundSendMessage{
			QueryID: st.QueryID,
			To:      st.NodeID,
			Message: st.Message,
			Notify:  p,
		}, true
	case *query.StatePoolWaitingAtCapacity:
		// nothing to do except wait for message response or timeout
	case *query.StatePoolWaitingWithCapacity:
		// nothing to do except wait for message response or timeout
	case *query.StatePoolQueryFinished[kadt.Key, kadt.PeerID]:
		waiter, ok := p.waiters[st.QueryID]
		if ok {
			waiter.Notify(ctx, &EventQueryFinished{
				QueryID:      st.QueryID,
				Stats:        st.Stats,
				ClosestNodes: st.ClosestNodes,
			})
			waiter.Close()
		}
	case *query.StatePoolQueryTimeout:
		// TODO
	case *query.StatePoolIdle:
		// nothing to do
	default:
		panic(fmt.Sprintf("unexpected pool state: %T", st))
	}

	return nil, false
}
