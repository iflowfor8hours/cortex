package ring

// Based on https://raw.githubusercontent.com/stathat/consistent/master/consistent.go

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/weaveworks/cortex/pkg/util"
)

const (
	unhealthy = "Unhealthy"

	// ConsulKey is the key under which we store the ring in consul.
	ConsulKey = "ring"
)

// ReadRing represents the read inferface to the ring.
type ReadRing interface {
	prometheus.Collector

	Get(key uint32, op Operation) (ReplicationSet, error)
	BatchGet(keys []uint32, op Operation) ([]ReplicationSet, error)
	GetAll() (ReplicationSet, error)
	ReplicationFactor() int
}

// ReplicationSet describes the ingesters to talk to for a given key, and how
// many errors to tolerate.
type ReplicationSet struct {
	Ingesters []*IngesterDesc
	MaxErrors int
}

// Operation can be Read or Write
type Operation int

// Values for Operation
const (
	Read Operation = iota
	Write
	Reporting // Special value for inquiring about health
)

type uint32s []uint32

func (x uint32s) Len() int           { return len(x) }
func (x uint32s) Less(i, j int) bool { return x[i] < x[j] }
func (x uint32s) Swap(i, j int)      { x[i], x[j] = x[j], x[i] }

// ErrEmptyRing is the error returned when trying to get an element when nothing has been added to hash.
var ErrEmptyRing = errors.New("empty ring")

// Config for a Ring
type Config struct {
	ConsulConfig
	store             string
	HeartbeatTimeout  time.Duration
	ReplicationFactor int
	Mock              KVClient
}

// RegisterFlags adds the flags required to config this to the given FlagSet
func (cfg *Config) RegisterFlags(f *flag.FlagSet) {
	cfg.ConsulConfig.RegisterFlags(f)

	f.StringVar(&cfg.store, "ring.store", "consul", "Backend storage to use for the ring (consul, inmemory).")
	f.DurationVar(&cfg.HeartbeatTimeout, "ring.heartbeat-timeout", time.Minute, "The heartbeat timeout after which ingesters are skipped for reads/writes.")
	f.IntVar(&cfg.ReplicationFactor, "distributor.replication-factor", 3, "The number of ingesters to write to and read from.")
}

// Ring holds the information about the members of the consistent hash ring.
type Ring struct {
	cfg      Config
	KVClient KVClient
	done     chan struct{}
	quit     context.CancelFunc

	mtx      sync.RWMutex
	ringDesc *Desc

	ingesterOwnershipDesc *prometheus.Desc
	numIngestersDesc      *prometheus.Desc
	numTokensDesc         *prometheus.Desc
}

// New creates a new Ring
func New(cfg Config) (*Ring, error) {
	if cfg.ReplicationFactor <= 0 {
		return nil, fmt.Errorf("ReplicationFactor must be greater than zero: %d", cfg.ReplicationFactor)
	}

	store := cfg.Mock
	if store == nil {
		var err error

		switch cfg.store {
		case "consul":
			codec := ProtoCodec{Factory: ProtoDescFactory}
			store, err = NewConsulClient(cfg.ConsulConfig, codec)
		case "inmemory":
			store = NewInMemoryKVClient()
		}
		if err != nil {
			return nil, err
		}
	}

	r := &Ring{
		cfg:      cfg,
		KVClient: store,
		done:     make(chan struct{}),
		ringDesc: &Desc{},
		ingesterOwnershipDesc: prometheus.NewDesc(
			"cortex_ring_ingester_ownership_percent",
			"The percent ownership of the ring by ingester",
			[]string{"ingester"}, nil,
		),
		numIngestersDesc: prometheus.NewDesc(
			"cortex_ring_ingesters",
			"Number of ingesters in the ring",
			[]string{"state"}, nil,
		),
		numTokensDesc: prometheus.NewDesc(
			"cortex_ring_tokens",
			"Number of tokens in the ring",
			nil, nil,
		),
	}
	var ctx context.Context
	ctx, r.quit = context.WithCancel(context.Background())
	go r.loop(ctx)
	return r, nil
}

// Stop the distributor.
func (r *Ring) Stop() {
	r.quit()
	<-r.done
}

func (r *Ring) loop(ctx context.Context) {
	defer close(r.done)
	r.KVClient.WatchKey(ctx, ConsulKey, func(value interface{}) bool {
		if value == nil {
			level.Info(util.Logger).Log("msg", "ring doesn't exist in consul yet")
			return true
		}

		ringDesc := value.(*Desc)
		r.mtx.Lock()
		defer r.mtx.Unlock()
		r.ringDesc = ringDesc
		return true
	})
}

// Get returns n (or more) ingesters which form the replicas for the given key.
func (r *Ring) Get(key uint32, op Operation) (ReplicationSet, error) {
	r.mtx.RLock()
	defer r.mtx.RUnlock()
	return r.getInternal(key, op)
}

// BatchGet returns ReplicationFactor (or more) ingesters which form the replicas
// for the given keys. The order of the result matches the order of the input.
func (r *Ring) BatchGet(keys []uint32, op Operation) ([]ReplicationSet, error) {
	r.mtx.RLock()
	defer r.mtx.RUnlock()

	result := make([]ReplicationSet, len(keys), len(keys))
	for i, key := range keys {
		rs, err := r.getInternal(key, op)
		if err != nil {
			return nil, err
		}
		result[i] = rs
	}
	return result, nil
}

func (r *Ring) getInternal(key uint32, op Operation) (ReplicationSet, error) {
	if r.ringDesc == nil || len(r.ringDesc.Tokens) == 0 {
		return ReplicationSet{}, ErrEmptyRing
	}

	var (
		n             = r.cfg.ReplicationFactor
		ingesters     = make([]*IngesterDesc, 0, n)
		distinctHosts = map[string]struct{}{}
		start         = r.search(key)
		iterations    = 0
	)
	for i := start; len(distinctHosts) < n && iterations < len(r.ringDesc.Tokens); i++ {
		iterations++
		// Wrap i around in the ring.
		i %= len(r.ringDesc.Tokens)

		// We want n *distinct* ingesters.
		token := r.ringDesc.Tokens[i]
		if _, ok := distinctHosts[token.Ingester]; ok {
			continue
		}
		distinctHosts[token.Ingester] = struct{}{}
		ingester := r.ringDesc.Ingesters[token.Ingester]

		// We do not want to Write to Ingesters that are not ACTIVE, but we do want
		// to write the extra replica somewhere.  So we increase the size of the set
		// of replicas for the key. This means we have to also increase the
		// size of the replica set for read, but we can read from Leaving ingesters,
		// so don't skip it in this case.
		// NB dead ingester will be filtered later (by replication_strategy.go).
		if op == Write && ingester.State != ACTIVE {
			n++
		} else if op == Read && (ingester.State != ACTIVE && ingester.State != LEAVING) {
			n++
		}

		ingesters = append(ingesters, ingester)
	}

	liveIngesters, maxFailure, err := r.replicationStrategy(ingesters, op)
	if err != nil {
		return ReplicationSet{}, err
	}

	return ReplicationSet{
		Ingesters: liveIngesters,
		MaxErrors: maxFailure,
	}, nil
}

// GetAll returns all available ingesters in the ring.
func (r *Ring) GetAll() (ReplicationSet, error) {
	r.mtx.RLock()
	defer r.mtx.RUnlock()

	if r.ringDesc == nil || len(r.ringDesc.Tokens) == 0 {
		return ReplicationSet{}, ErrEmptyRing
	}

	ingesters := make([]*IngesterDesc, 0, len(r.ringDesc.Ingesters))
	maxErrors := r.cfg.ReplicationFactor / 2

	for _, ingester := range r.ringDesc.Ingesters {
		if !r.IsHealthy(ingester, Read) {
			maxErrors--
			continue
		}
		ingesters = append(ingesters, ingester)
	}

	if maxErrors < 0 {
		return ReplicationSet{}, fmt.Errorf("too many failed ingesters")
	}

	return ReplicationSet{
		Ingesters: ingesters,
		MaxErrors: maxErrors,
	}, nil
}

func (r *Ring) search(key uint32) int {
	i := sort.Search(len(r.ringDesc.Tokens), func(x int) bool {
		return r.ringDesc.Tokens[x].Token > key
	})
	if i >= len(r.ringDesc.Tokens) {
		i = 0
	}
	return i
}

// Describe implements prometheus.Collector.
func (r *Ring) Describe(ch chan<- *prometheus.Desc) {
	ch <- r.ingesterOwnershipDesc
	ch <- r.numIngestersDesc
	ch <- r.numTokensDesc
}

func countTokens(tokens []*TokenDesc) (map[string]uint32, map[string]uint32) {
	owned := map[string]uint32{}
	numTokens := map[string]uint32{}
	for i, token := range tokens {
		var diff uint32
		if i+1 == len(tokens) {
			diff = (math.MaxUint32 - token.Token) + tokens[0].Token
		} else {
			diff = tokens[i+1].Token - token.Token
		}
		numTokens[token.Ingester] = numTokens[token.Ingester] + 1
		owned[token.Ingester] = owned[token.Ingester] + diff
	}
	return numTokens, owned
}

// Collect implements prometheus.Collector.
func (r *Ring) Collect(ch chan<- prometheus.Metric) {
	r.mtx.RLock()
	defer r.mtx.RUnlock()

	_, owned := countTokens(r.ringDesc.Tokens)
	for id, totalOwned := range owned {
		ch <- prometheus.MustNewConstMetric(
			r.ingesterOwnershipDesc,
			prometheus.GaugeValue,
			float64(totalOwned)/float64(math.MaxUint32),
			id,
		)
	}

	// Initialised to zero so we emit zero-metrics (instead of not emitting anything)
	byState := map[string]int{
		unhealthy:        0,
		ACTIVE.String():  0,
		LEAVING.String(): 0,
		PENDING.String(): 0,
		JOINING.String(): 0,
	}
	for _, ingester := range r.ringDesc.Ingesters {
		if !r.IsHealthy(ingester, Reporting) {
			byState[unhealthy]++
		} else {
			byState[ingester.State.String()]++
		}
	}

	for state, count := range byState {
		ch <- prometheus.MustNewConstMetric(
			r.numIngestersDesc,
			prometheus.GaugeValue,
			float64(count),
			state,
		)
	}
	ch <- prometheus.MustNewConstMetric(
		r.numTokensDesc,
		prometheus.GaugeValue,
		float64(len(r.ringDesc.Tokens)),
	)
}
