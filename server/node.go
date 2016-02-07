// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Spencer Kimball (spencer.kimball@gmail.com)

package server

import (
	"container/list"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/client"
	"github.com/cockroachdb/cockroach/gossip"
	"github.com/cockroachdb/cockroach/gossip/resolver"
	"github.com/cockroachdb/cockroach/keys"
	"github.com/cockroachdb/cockroach/kv"
	"github.com/cockroachdb/cockroach/roachpb"
	"github.com/cockroachdb/cockroach/rpc"
	"github.com/cockroachdb/cockroach/server/status"
	"github.com/cockroachdb/cockroach/sql"
	"github.com/cockroachdb/cockroach/storage"
	"github.com/cockroachdb/cockroach/storage/engine"
	"github.com/cockroachdb/cockroach/util"
	"github.com/cockroachdb/cockroach/util/hlc"
	"github.com/cockroachdb/cockroach/util/log"
	"github.com/cockroachdb/cockroach/util/metric"
	"github.com/cockroachdb/cockroach/util/stop"
	"github.com/cockroachdb/cockroach/util/tracing"
	"github.com/cockroachdb/cockroach/util/uuid"
	"github.com/gogo/protobuf/proto"
	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/standardtracer"
)

const (
	// gossipInterval is the interval for gossiping storage-related info.
	gossipInterval = 1 * time.Minute
	// publishStatusInterval is the interval for publishing periodic statistics
	// from stores to the internal event feed.
	publishStatusInterval = 10 * time.Second
)

// errNeedsBootstrap indicates the node should be used as the seed of
// a new cluster.
var errNeedsBootstrap = errors.New("node has no initialized stores and no instructions for joining an existing cluster")

// errCannotJoinSelf indicates that a node was started with no initialized
// stores but --join specifying itself; there's no way to make forward
// progress in this state.
var errCannotJoinSelf = errors.New("an uninitialized node cannot specify its own address to join a cluster")

// A Node manages a map of stores (by store ID) for which it serves
// traffic. A node is the top-level data structure. There is one node
// instance per process. A node accepts incoming RPCs and services
// them by directing the commands contained within RPCs to local
// stores, which in turn direct the commands to specific ranges. Each
// node has access to the global, monolithic Key-Value abstraction via
// its kv.DB reference. Nodes use this to allocate node and store
// IDs for bootstrapping the node itself or new stores as they're added
// on subsequent instantiations.
type Node struct {
	stopper    *stop.Stopper
	ClusterID  string                 // UUID for Cockroach cluster
	Descriptor roachpb.NodeDescriptor // Node ID, network/physical topology
	ctx        storage.StoreContext   // Context to use and pass to stores
	stores     *storage.Stores        // Access to node-local stores
	feed       status.NodeEventFeed   // Feed publisher for local events
	status     *status.NodeStatusMonitor
	startedAt  int64
}

// allocateNodeID increments the node id generator key to allocate
// a new, unique node id.
func allocateNodeID(db *client.DB) (roachpb.NodeID, error) {
	r, err := db.Inc(keys.NodeIDGenerator, 1)
	if err != nil {
		return 0, util.Errorf("unable to allocate node ID: %s", err)
	}
	return roachpb.NodeID(r.ValueInt()), nil
}

// allocateStoreIDs increments the store id generator key for the
// specified node to allocate "inc" new, unique store ids. The
// first ID in a contiguous range is returned on success.
func allocateStoreIDs(nodeID roachpb.NodeID, inc int64, db *client.DB) (roachpb.StoreID, error) {
	r, err := db.Inc(keys.StoreIDGenerator, inc)
	if err != nil {
		return 0, util.Errorf("unable to allocate %d store IDs for node %d: %s", inc, nodeID, err)
	}
	return roachpb.StoreID(r.ValueInt() - inc + 1), nil
}

// GetBootstrapSchema returns the schema which will be used to bootstrap a new
// server.
func GetBootstrapSchema() sql.MetadataSchema {
	schema := sql.MakeMetadataSchema()
	storage.AddEventLogToMetadataSchema(&schema)
	return schema
}

// bootstrapCluster bootstraps a multiple stores using the provided
// engines and cluster ID. The first bootstrapped store contains a
// single range spanning all keys. Initial range lookup metadata is
// populated for the range. Returns the cluster ID.
func bootstrapCluster(engines []engine.Engine) (string, error) {
	clusterID := uuid.NewUUID4().String()
	stopper := stop.NewStopper()
	defer stopper.Stop()

	ctx := storage.StoreContext{}
	ctx.ScanInterval = 10 * time.Minute
	ctx.Clock = hlc.NewClock(hlc.UnixNano)
	ctx.Tracer = tracing.NewTracer()
	// Create a KV DB with a local sender.
	stores := storage.NewStores(ctx.Clock)
	sender := kv.NewTxnCoordSender(stores, ctx.Clock, false, ctx.Tracer, stopper)
	ctx.DB = client.NewDB(sender)
	ctx.Transport = storage.NewLocalRPCTransport(stopper)
	for i, eng := range engines {
		sIdent := roachpb.StoreIdent{
			ClusterID: clusterID,
			NodeID:    1,
			StoreID:   roachpb.StoreID(i + 1),
		}

		// The bootstrapping store will not connect to other nodes so its
		// StoreConfig doesn't really matter.
		s := storage.NewStore(ctx, eng, &roachpb.NodeDescriptor{NodeID: 1})

		// Verify the store isn't already part of a cluster.
		if len(s.Ident.ClusterID) > 0 {
			return "", util.Errorf("storage engine already belongs to a cluster (%s)", s.Ident.ClusterID)
		}

		// Bootstrap store to persist the store ident.
		if err := s.Bootstrap(sIdent, stopper); err != nil {
			return "", err
		}
		// Create first range, writing directly to engine. Note this does
		// not create the range, just its data.  Only do this if this is the
		// first store.
		if i == 0 {
			initialValues := GetBootstrapSchema().GetInitialValues()
			if err := s.BootstrapRange(initialValues); err != nil {
				return "", err
			}
		}
		if err := s.Start(stopper); err != nil {
			return "", err
		}

		stores.AddStore(s)

		// Initialize node and store ids.  Only initialize the node once.
		if i == 0 {
			if nodeID, err := allocateNodeID(ctx.DB); nodeID != sIdent.NodeID || err != nil {
				return "", util.Errorf("expected to initialize node id allocator to %d, got %d: %s",
					sIdent.NodeID, nodeID, err)
			}
		}
		if storeID, err := allocateStoreIDs(sIdent.NodeID, 1, ctx.DB); storeID != sIdent.StoreID || err != nil {
			return "", util.Errorf("expected to initialize store id allocator to %d, got %d: %s",
				sIdent.StoreID, storeID, err)
		}
	}
	return clusterID, nil
}

// NewNode returns a new instance of Node.
func NewNode(ctx storage.StoreContext, registry *metric.Registry, stopper *stop.Stopper, subRegistries []status.NodeSubregistry) *Node {
	return &Node{
		ctx:     ctx,
		stopper: stopper,
		status:  status.NewNodeStatusMonitor(registry, subRegistries),
		stores:  storage.NewStores(ctx.Clock),
	}
}

// context returns a context encapsulating the NodeID.
func (n *Node) context() context.Context {
	return log.Add(context.Background(), log.NodeID, n.Descriptor.NodeID)
}

// initDescriptor initializes the node descriptor with the server
// address and the node attributes.
func (n *Node) initDescriptor(addr net.Addr, attrs roachpb.Attributes) {
	n.Descriptor.Address = util.MakeUnresolvedAddr(addr.Network(), addr.String())
	n.Descriptor.Attrs = attrs
}

// initNodeID updates the internal NodeDescriptor with the given ID. If zero is
// supplied, a new NodeID is allocated with the first invocation. For all other
// values, the supplied ID is stored into the descriptor (unless one has been
// set previously, in which case a fatal error occurs).
//
// Upon setting a new NodeID, the descriptor is gossiped and the NodeID is
// stored into the gossip instance.
func (n *Node) initNodeID(id roachpb.NodeID) {
	if id < 0 {
		log.Fatalf("NodeID must not be negative")
	}

	if o := n.Descriptor.NodeID; o > 0 {
		if id == 0 {
			return
		}
		log.Fatalf("cannot initialize NodeID to %d, already have %d", id, o)
	}
	var err error
	if id == 0 {
		id, err = allocateNodeID(n.ctx.DB)
		log.Infof("new node allocated ID %d", id)
		if err != nil {
			log.Fatal(err)
		}
		if id == 0 {
			log.Fatal("new node allocated illegal ID 0")
		}
		n.ctx.Gossip.SetNodeID(id)
	} else {
		log.Infof("node ID %d initialized", id)
	}
	// Gossip the node descriptor to make this node addressable by node ID.
	n.Descriptor.NodeID = id
	if err = n.ctx.Gossip.SetNodeDescriptor(&n.Descriptor); err != nil {
		log.Fatalf("couldn't gossip descriptor for node %d: %s", n.Descriptor.NodeID, err)
	}
}

// start starts the node by registering the storage instance for the
// RPC service "Node" and initializing stores for each specified
// engine. Launches periodic store gossiping in a goroutine.
func (n *Node) start(rpcServer *rpc.Server, addr net.Addr, engines []engine.Engine, attrs roachpb.Attributes) error {
	n.initDescriptor(addr, attrs)

	// Start status monitor.
	n.status.StartMonitorFeed(n.ctx.EventFeed)

	// Initialize stores, including bootstrapping new ones.
	if err := n.initStores(engines, n.stopper); err != nil {
		if err == errNeedsBootstrap {
			// This node has no initialized stores and no way to connect to
			// an existing cluster, so we bootstrap it.
			clusterID, err := bootstrapCluster(engines)
			if err != nil {
				return err
			}
			log.Infof("**** cluster %s has been created", clusterID)
			log.Infof("**** add additional nodes by specifying --join=%s", addr)
			// Make sure we add the node as a resolver.
			selfResolver, err := resolver.NewResolverFromAddress(addr)
			if err != nil {
				return err
			}
			n.ctx.Gossip.SetResolvers([]resolver.Resolver{selfResolver})
			// After bootstrapping, try again to initialize the stores.
			if err := n.initStores(engines, n.stopper); err != nil {
				return err
			}
		} else {
			return err
		}
	}

	n.startedAt = n.ctx.Clock.Now().WallTime

	// Initialize publisher for Node Events. This requires the NodeID, which is
	// initialized by initStores(); because of this, some Store initialization
	// events will precede the StartNodeEvent on the feed.
	n.feed = status.NewNodeEventFeed(n.Descriptor.NodeID, n.ctx.EventFeed)
	n.feed.StartNode(n.Descriptor, n.startedAt)

	n.startPublishStatuses(n.stopper)
	n.startGossip(n.stopper)

	// Register the RPC methods we support last as doing so allows RPCs to be
	// received which may access state initialized above without locks.
	const method = "Node.Batch"
	if err := rpcServer.Register(method, n.executeCmd, &roachpb.BatchRequest{}); err != nil {
		log.Fatalf("unable to register node service with RPC server: %s", err)
	}

	log.Infoc(n.context(), "Started node with %v engine(s) and attributes %v", engines, attrs.Attrs)
	return nil
}

// initStores initializes the Stores map from ID to Store. Stores are
// added to the local sender if already bootstrapped. A bootstrapped
// Store has a valid ident with cluster, node and Store IDs set. If
// the Store doesn't yet have a valid ident, it's added to the
// bootstraps list for initialization once the cluster and node IDs
// have been determined.
func (n *Node) initStores(engines []engine.Engine, stopper *stop.Stopper) error {
	bootstraps := list.New()

	if len(engines) == 0 {
		return util.Errorf("no engines")
	}
	for _, e := range engines {
		s := storage.NewStore(n.ctx, e, &n.Descriptor)
		// Initialize each store in turn, handling un-bootstrapped errors by
		// adding the store to the bootstraps list.
		if err := s.Start(stopper); err != nil {
			if _, ok := err.(*storage.NotBootstrappedError); ok {
				log.Infof("store %s not bootstrapped", s)
				bootstraps.PushBack(s)
				continue
			}
			return util.Errorf("failed to start store: %s", err)
		}
		if s.Ident.ClusterID == "" || s.Ident.NodeID == 0 {
			return util.Errorf("unidentified store: %s", s)
		}
		capacity, err := s.Capacity()
		if err != nil {
			return util.Errorf("could not query store capacity: %s", err)
		}
		log.Infof("initialized store %s: %+v", s, capacity)
		n.stores.AddStore(s)
	}

	// If there are no initialized stores and no gossip resolvers,
	// bootstrap this node as the seed of a new cluster.
	if n.stores.GetStoreCount() == 0 {
		resolvers := n.ctx.Gossip.GetResolvers()
		// Check for the case of uninitialized node having only itself specified as join host.
		switch len(resolvers) {
		case 0:
			return errNeedsBootstrap
		case 1:
			if addr, err := resolvers[0].GetAddress(); err == nil {
				if uaddr, ok := addr.(*util.UnresolvedAddr); ok && *uaddr == n.Descriptor.Address {
					return errCannotJoinSelf
				}
			}
		}
	}

	// Verify all initialized stores agree on cluster and node IDs.
	if err := n.validateStores(); err != nil {
		return err
	}

	// Set the stores map as the gossip persistent storage, so that
	// gossip can bootstrap using the most recently persisted set of
	// node addresses.
	if err := n.ctx.Gossip.SetStorage(n.stores); err != nil {
		return fmt.Errorf("failed to initialize the gossip interface: %s", err)
	}

	// Connect gossip before starting bootstrap. For new nodes, connecting
	// to the gossip network is necessary to get the cluster ID.
	n.connectGossip()

	// If no NodeID has been assigned yet, allocate a new node ID by
	// supplying 0 to initNodeID.
	if n.Descriptor.NodeID == 0 {
		n.initNodeID(0)
	}

	// Bootstrap any uninitialized stores asynchronously.
	if bootstraps.Len() > 0 {
		stopper.RunAsyncTask(func() {
			n.bootstrapStores(bootstraps, stopper)
		})
	}

	return nil
}

// validateStores iterates over all stores, verifying they agree on
// cluster ID and node ID. The node's ident is initialized based on
// the agreed-upon cluster and node IDs.
func (n *Node) validateStores() error {
	return n.stores.VisitStores(func(s *storage.Store) error {
		if n.ClusterID == "" {
			n.ClusterID = s.Ident.ClusterID
			n.initNodeID(s.Ident.NodeID)
		} else if n.ClusterID != s.Ident.ClusterID {
			return util.Errorf("store %s cluster ID doesn't match node cluster %q", s, n.ClusterID)
		} else if n.Descriptor.NodeID != s.Ident.NodeID {
			return util.Errorf("store %s node ID doesn't match node ID: %d", s, n.Descriptor.NodeID)
		}
		return nil
	})
}

// bootstrapStores bootstraps uninitialized stores once the cluster
// and node IDs have been established for this node. Store IDs are
// allocated via a sequence id generator stored at a system key per
// node.
func (n *Node) bootstrapStores(bootstraps *list.List, stopper *stop.Stopper) {
	if n.ClusterID == "" {
		panic("ClusterID missing during store bootstrap of auxiliary store")
	}

	// Bootstrap all waiting stores by allocating a new store id for
	// each and invoking store.Bootstrap() to persist.
	inc := int64(bootstraps.Len())
	firstID, err := allocateStoreIDs(n.Descriptor.NodeID, inc, n.ctx.DB)
	if err != nil {
		log.Fatal(err)
	}
	sIdent := roachpb.StoreIdent{
		ClusterID: n.ClusterID,
		NodeID:    n.Descriptor.NodeID,
		StoreID:   firstID,
	}
	for e := bootstraps.Front(); e != nil; e = e.Next() {
		s := e.Value.(*storage.Store)
		if err := s.Bootstrap(sIdent, stopper); err != nil {
			log.Fatal(err)
		}
		if err := s.Start(stopper); err != nil {
			log.Fatal(err)
		}
		n.stores.AddStore(s)
		sIdent.StoreID++
		log.Infof("bootstrapped store %s", s)
		// Done regularly in Node.startGossip, but this cuts down the time
		// until this store is used for range allocations.
		s.GossipStore()
	}
}

// connectGossip connects to gossip network and reads cluster ID. If
// this node is already part of a cluster, the cluster ID is verified
// for a match. If not part of a cluster, the cluster ID is set. The
// node's address is gossiped with node ID as the gossip key.
func (n *Node) connectGossip() {
	log.Infof("connecting to gossip network to verify cluster ID...")
	// No timeout or stop condition is needed here. Log statements should be
	// sufficient for diagnosing this type of condition.
	<-n.ctx.Gossip.Connected

	bytes, err := n.ctx.Gossip.GetInfo(gossip.KeyClusterID)
	if err != nil {
		log.Fatalf("unable to ascertain cluster ID from gossip network: %s", err)
	}
	gossipClusterID := string(bytes)

	if n.ClusterID == "" {
		n.ClusterID = gossipClusterID
	} else if n.ClusterID != gossipClusterID {
		log.Fatalf("node %d belongs to cluster %q but is attempting to connect to a gossip network for cluster %q",
			n.Descriptor.NodeID, n.ClusterID, gossipClusterID)
	}
	log.Infof("node connected via gossip and verified as part of cluster %q", gossipClusterID)
}

// startGossip loops on a periodic ticker to gossip node-related
// information. Starts a goroutine to loop until the node is closed.
func (n *Node) startGossip(stopper *stop.Stopper) {
	stopper.RunWorker(func() {
		ticker := time.NewTicker(gossipInterval)
		defer ticker.Stop()
		n.gossipStores() // one-off run before going to sleep
		for {
			select {
			case <-ticker.C:
				n.gossipStores()
			case <-stopper.ShouldStop():
				return
			}
		}
	})
}

// gossipStores broadcasts each store to the gossip network.
func (n *Node) gossipStores() {
	if err := n.stores.VisitStores(func(s *storage.Store) error {
		s.GossipStore()
		return nil
	}); err != nil {
		panic(err)
	}
}

// startPublishStatuses starts a loop which periodically instructs each store to
// publish its current status to the event feed.
func (n *Node) startPublishStatuses(stopper *stop.Stopper) {
	stopper.RunWorker(func() {
		// Publish status at the same frequency as metrics are collected.
		ticker := time.NewTicker(publishStatusInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				err := n.publishStoreStatuses()
				if err != nil {
					log.Error(err)
				}
			case <-stopper.ShouldStop():
				return
			}
		}
	})
}

// publishStoreStatuses calls publishStatus on each store on the node.
func (n *Node) publishStoreStatuses() error {
	return n.stores.VisitStores(func(store *storage.Store) error {
		return store.PublishStatus()
	})
}

// executeCmd interprets the given message as a *roachpb.BatchRequest and sends it
// via the local sender.
func (n *Node) executeCmd(argsI proto.Message) (proto.Message, error) {
	ba := argsI.(*roachpb.BatchRequest)
	var br *roachpb.BatchResponse

	sp := tracing.NoopSpan
	if ba.Trace != nil {
		var tracingCB tracing.CallbackRecorder = func(rawSpan *standardtracer.RawSpan) {
			encSp, err := tracing.EncodeRawSpan(rawSpan, nil)
			if err != nil {
				panic(err)
			}
			br.CollectedSpans = append(br.CollectedSpans, encSp)
		}

		// Create a special tracer for this request (note that we're not
		// using n.ctx.Tracer) so that we can grab the span when it's done.
		tracer := standardtracer.New(tracingCB)

		var err error
		sp, err = tracer.JoinTraceFromBinary("node", ba.Trace.Context, ba.Trace.Attributes)
		if err != nil {
			return nil, fmt.Errorf("cannot unmarshal trace: %v", err)
		}
		defer sp.Finish()
	}

	f := func() {
		sp.LogEvent("node " + strconv.Itoa(int(n.Descriptor.NodeID)))
		ctx, _ := opentracing.ContextWithSpan((*Node)(n).context(), sp)

		tStart := time.Now()
		var pErr *roachpb.Error
		br, pErr = n.stores.Send(ctx, *ba)
		if pErr != nil {
			br = &roachpb.BatchResponse{}
			sp.LogEvent(fmt.Sprintf("error: %T", pErr.GetDetail()))
		}
		if br.Error != nil {
			panic(roachpb.ErrorUnexpectedlySet(n.stores, br))
		}
		n.feed.CallComplete(*ba, time.Now().Sub(tStart), pErr)
		br.Error = pErr
	}

	if !n.stopper.RunTask(f) {
		return nil, util.Errorf("node %d stopped", n.Descriptor.NodeID)
	}
	return br, nil
}
