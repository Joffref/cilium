// Copyright 2016-2019 Authors of Cilium
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package endpointmanager

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/cilium/cilium/pkg/completion"
	"github.com/cilium/cilium/pkg/controller"
	"github.com/cilium/cilium/pkg/endpoint"
	endpointid "github.com/cilium/cilium/pkg/endpoint/id"
	"github.com/cilium/cilium/pkg/endpoint/regeneration"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/metrics"
	monitorAPI "github.com/cilium/cilium/pkg/monitor/api"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/policy"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sirupsen/logrus"
)

var (
	log         = logging.DefaultLogger.WithField(logfields.LogSubsys, "endpoint-manager")
	metricsOnce sync.Once
)

// EndpointManager is a structure designed for containing state about the
// collection of locally running endpoints.
type EndpointManager struct {
	// mutex protects endpoints and endpointsAux
	mutex lock.RWMutex

	// endpoints is the global list of endpoints indexed by ID. mutex must
	// be held to read and write.
	endpoints    map[uint16]*endpoint.Endpoint
	endpointsAux map[string]*endpoint.Endpoint

	// EndpointSynchronizer updates external resources (e.g., Kubernetes) with
	// up-to-date information about endpoints managed by the endpoint manager.
	EndpointResourceSynchronizer

	// checker supports endpoint garbage collection by verifying the health
	// of an endpoint.
	checker EndpointChecker

	// A mark-and-sweep garbage collector may operate on the endpoint list.
	// This is configured via WithPeriodicEndpointGC() and will mark
	// endpoints for removal on one run of the controller, then in the
	// subsequent controller run will remove the endpoints.
	markedEndpoints []uint16
}

// EndpointResourceSynchronizer is an interface which synchronizes CiliumEndpoint
// resources with Kubernetes.
type EndpointResourceSynchronizer interface {
	RunK8sCiliumEndpointSync(ep *endpoint.Endpoint, conf endpoint.EndpointStatusConfiguration)
}

// NewEndpointManager creates a new EndpointManager.
func NewEndpointManager(epSynchronizer EndpointResourceSynchronizer) *EndpointManager {
	mgr := EndpointManager{
		endpoints:                    make(map[uint16]*endpoint.Endpoint),
		endpointsAux:                 make(map[string]*endpoint.Endpoint),
		EndpointResourceSynchronizer: epSynchronizer,
	}

	return &mgr
}

// EndpointChecker can verify whether an endpoint is currently healthy.
type EndpointChecker interface {
	Check(*endpoint.Endpoint) error
	DeleteEndpoint(*endpoint.Endpoint) int
}

// WithPeriodicEndpointGC runs a controller to periodically garbage collect
// endpoints that match the specified checker.
func (mgr *EndpointManager) WithPeriodicEndpointGC(ctx context.Context, checker EndpointChecker, interval time.Duration) *EndpointManager {
	mgr.checker = checker
	controller.NewManager().UpdateController("endpoint-gc",
		controller.ControllerParams{
			DoFunc:      mgr.markAndSweep,
			RunInterval: interval,
			Context:     ctx,
		})
	return mgr
}

// markAndSweep performs a two-phase garbage collection of endpoints using the
// configured EndpointChecker.
//
// 1) Mark all endpoints that require GC. Do not GC these endpoints this round.
// 2) Sweep all endpoints marked as requiring GC during the previous iteration.
//
// This way, if there is a temporary condition that will be resolved by other
// components in the system, then we will not flag warnings about the system
// getting out-of-sync.
func (mgr *EndpointManager) markAndSweep(ctx context.Context) error {
	marked := mgr.markEndpoints()

	mgr.mutex.Lock()
	toSweep := mgr.markedEndpoints
	mgr.markedEndpoints = marked
	mgr.mutex.Unlock()

	// Avoid returning an error which would cause the calling controller to
	// re-run the garbage collection more frequently than the RunInterval.
	mgr.sweepEndpoints(toSweep)
	return nil
}

// markEndpoints runs all endpoints in the manager against the configured
// EndpointChecker and returns a slice of endpoint ids that require garbage
// collection.
func (mgr *EndpointManager) markEndpoints() []uint16 {
	mgr.mutex.RLock()
	defer mgr.mutex.RUnlock()

	// TODO: Consider exposing visibility via Endpoint.SetState().
	needsGC := make([]uint16, 0, len(mgr.endpoints))
	for eid, ep := range mgr.endpoints {
		if err := mgr.checker.Check(ep); err != nil {
			needsGC = append(needsGC, eid)
		}
	}
	return needsGC
}

// sweepEndpoints iterates through the specified list of endpoints marked for
// deletion and attempts to garbage-collect them if they still exist.
func (mgr *EndpointManager) sweepEndpoints(markedEndpoints []uint16) {
	toSweep := make([]*endpoint.Endpoint, 0, len(markedEndpoints))

	// 'markedEndpoints' were marked during the previous mark round, so
	// they may no longer be valid endpoints. Narrow the list to only the
	// endpoints that remain. Then, release the lock so DeleteEndpoint()
	// below can independently grab it.
	mgr.mutex.RLock()
	for _, id := range markedEndpoints {
		if ep, ok := mgr.endpoints[id]; ok {
			toSweep = append(toSweep, ep)
		}
	}
	mgr.mutex.RUnlock()

	for _, ep := range toSweep {
		log.WithFields(logrus.Fields{
			logfields.EndpointID:  ep.StringID(),
			logfields.ContainerID: ep.GetShortContainerID(),
			logfields.K8sPodName:  ep.GetK8sNamespaceAndPodName(),
			logfields.URL:         "https://github.com/kubernetes/kubernetes/issues/86944",
		}).Warning("Stray endpoint found. You may be affected by upstream Kubernetes issue #86944.")
		// Callee handles the errors which we ignore.
		_ = mgr.checker.DeleteEndpoint(ep)
	}
}

// waitForProxyCompletions blocks until all proxy changes have been completed.
func waitForProxyCompletions(proxyWaitGroup *completion.WaitGroup) error {
	err := proxyWaitGroup.Context().Err()
	if err != nil {
		return fmt.Errorf("context cancelled before waiting for proxy updates: %s", err)
	}

	start := time.Now()
	log.Debug("Waiting for proxy updates to complete...")
	err = proxyWaitGroup.Wait()
	if err != nil {
		return fmt.Errorf("proxy updates failed: %s", err)
	}
	log.Debug("Wait time for proxy updates: ", time.Since(start))

	return nil
}

// UpdatePolicyMaps returns a WaitGroup which is signaled upon once all endpoints
// have had their PolicyMaps updated against the Endpoint's desired policy state.
func (mgr *EndpointManager) UpdatePolicyMaps(ctx context.Context) *sync.WaitGroup {
	var epWG sync.WaitGroup
	var wg sync.WaitGroup

	proxyWaitGroup := completion.NewWaitGroup(ctx)

	eps := mgr.GetEndpoints()
	epWG.Add(len(eps))
	wg.Add(1)

	// This is in a goroutine to allow the caller to proceed with other tasks before waiting for the ACKs to complete
	go func() {
		// Wait for all the eps to have applied policy map
		// changes before waiting for the changes to be ACKed
		epWG.Wait()
		if err := waitForProxyCompletions(proxyWaitGroup); err != nil {
			log.WithError(err).Warning("Failed to apply L7 proxy policy changes. These will be re-applied in future updates.")
		}
		wg.Done()
	}()

	// TODO: bound by number of CPUs?
	for _, ep := range eps {
		go func(ep *endpoint.Endpoint) {
			if err := ep.ApplyPolicyMapChanges(proxyWaitGroup); err != nil {
				ep.Logger("endpointmanager").WithError(err).Warning("Failed to apply policy map changes. These will be re-applied in future updates.")
			}
			epWG.Done()
		}(ep)
	}

	return &wg
}

// InitMetrics hooks the EndpointManager into the metrics subsystem. This can
// only be done once, globally, otherwise the metrics library will panic.
func (mgr *EndpointManager) InitMetrics() {
	if option.Config.DryMode {
		return
	}
	metricsOnce.Do(func() { // EndpointCount is a function used to collect this metric. We cannot
		// increment/decrement a gauge since we invoke Remove gratuitiously and that
		// would result in negative counts.
		// It must be thread-safe.
		metrics.EndpointCount = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: metrics.Namespace,
			Name:      "endpoint_count",
			Help:      "Number of endpoints managed by this agent",
		},
			func() float64 { return float64(len(mgr.GetEndpoints())) },
		)
		metrics.MustRegister(metrics.EndpointCount)
	})
}

// AllocateID checks if the ID can be reused. If it cannot, returns an error.
// If an ID of 0 is provided, a new ID is allocated. If a new ID cannot be
// allocated, returns an error.
func (mgr *EndpointManager) AllocateID(currID uint16) (uint16, error) {
	var newID uint16
	if currID != 0 {
		if err := endpointid.Reuse(currID); err != nil {
			return 0, fmt.Errorf("unable to reuse endpoint ID: %s", err)
		}
		newID = currID
	} else {
		id := endpointid.Allocate()
		if id == uint16(0) {
			return 0, fmt.Errorf("no more endpoint IDs available")
		}
		newID = id
	}

	return newID, nil
}

// RemoveID removes the id from the endpoints map in the EndpointManager.
func (mgr *EndpointManager) RemoveID(currID uint16) {
	mgr.mutex.Lock()
	defer mgr.mutex.Unlock()
	delete(mgr.endpoints, currID)
}

// Lookup looks up the endpoint by prefix id
func (mgr *EndpointManager) Lookup(id string) (*endpoint.Endpoint, error) {
	mgr.mutex.RLock()
	defer mgr.mutex.RUnlock()

	prefix, eid, err := endpointid.Parse(id)
	if err != nil {
		return nil, err
	}

	switch prefix {
	case endpointid.CiliumLocalIdPrefix:
		n, err := endpointid.ParseCiliumID(id)
		if err != nil {
			return nil, err
		}
		return mgr.lookupCiliumID(uint16(n)), nil

	case endpointid.CiliumGlobalIdPrefix:
		return nil, ErrUnsupportedID

	case endpointid.ContainerIdPrefix:
		return mgr.lookupContainerID(eid), nil

	case endpointid.DockerEndpointPrefix:
		return mgr.lookupDockerEndpoint(eid), nil

	case endpointid.ContainerNamePrefix:
		return mgr.lookupDockerContainerName(eid), nil

	case endpointid.PodNamePrefix:
		return mgr.lookupPodNameLocked(eid), nil

	case endpointid.IPv4Prefix:
		return mgr.lookupIPv4(eid), nil

	case endpointid.IPv6Prefix:
		return mgr.lookupIPv6(eid), nil

	default:
		return nil, ErrInvalidPrefix{InvalidPrefix: prefix.String()}
	}
}

// LookupCiliumID looks up endpoint by endpoint ID
func (mgr *EndpointManager) LookupCiliumID(id uint16) *endpoint.Endpoint {
	mgr.mutex.RLock()
	ep := mgr.lookupCiliumID(id)
	mgr.mutex.RUnlock()
	return ep
}

// LookupContainerID looks up endpoint by Docker ID
func (mgr *EndpointManager) LookupContainerID(id string) *endpoint.Endpoint {
	mgr.mutex.RLock()
	ep := mgr.lookupContainerID(id)
	mgr.mutex.RUnlock()
	return ep
}

// LookupIPv4 looks up endpoint by IPv4 address
func (mgr *EndpointManager) LookupIPv4(ipv4 string) *endpoint.Endpoint {
	mgr.mutex.RLock()
	ep := mgr.lookupIPv4(ipv4)
	mgr.mutex.RUnlock()
	return ep
}

// LookupIPv6 looks up endpoint by IPv6 address
func (mgr *EndpointManager) LookupIPv6(ipv6 string) *endpoint.Endpoint {
	mgr.mutex.RLock()
	ep := mgr.lookupIPv6(ipv6)
	mgr.mutex.RUnlock()
	return ep
}

// LookupIP looks up endpoint by IP address
func (mgr *EndpointManager) LookupIP(ip net.IP) (ep *endpoint.Endpoint) {
	addr := ip.String()
	mgr.mutex.RLock()
	if ip.To4() != nil {
		ep = mgr.lookupIPv4(addr)
	} else {
		ep = mgr.lookupIPv6(addr)
	}
	mgr.mutex.RUnlock()
	return ep
}

// LookupPodName looks up endpoint by namespace + pod name
func (mgr *EndpointManager) LookupPodName(name string) *endpoint.Endpoint {
	mgr.mutex.RLock()
	ep := mgr.lookupPodNameLocked(name)
	mgr.mutex.RUnlock()
	return ep
}

// ReleaseID releases the ID of the specified endpoint from the EndpointManager.
// Returns an error if the ID cannot be released.
func (mgr *EndpointManager) ReleaseID(ep *endpoint.Endpoint) error {
	return endpointid.Release(ep.ID)
}

// WaitEndpointRemoved waits until all operations associated with Remove of
// the endpoint have been completed.
// Note: only used for unit tests
func (mgr *EndpointManager) WaitEndpointRemoved(ep *endpoint.Endpoint) {
	<-ep.Unexpose(mgr)
}

// RemoveAll removes all endpoints from the global maps.
func (mgr *EndpointManager) RemoveAll() {
	mgr.mutex.Lock()
	defer mgr.mutex.Unlock()
	endpointid.ReallocatePool()
	mgr.endpoints = map[uint16]*endpoint.Endpoint{}
	mgr.endpointsAux = map[string]*endpoint.Endpoint{}
}

// lookupCiliumID looks up endpoint by endpoint ID
func (mgr *EndpointManager) lookupCiliumID(id uint16) *endpoint.Endpoint {
	if ep, ok := mgr.endpoints[id]; ok {
		return ep
	}
	return nil
}

func (mgr *EndpointManager) lookupDockerEndpoint(id string) *endpoint.Endpoint {
	if ep, ok := mgr.endpointsAux[endpointid.NewID(endpointid.DockerEndpointPrefix, id)]; ok {
		return ep
	}
	return nil
}

func (mgr *EndpointManager) lookupPodNameLocked(name string) *endpoint.Endpoint {
	if ep, ok := mgr.endpointsAux[endpointid.NewID(endpointid.PodNamePrefix, name)]; ok {
		return ep
	}
	return nil
}

func (mgr *EndpointManager) lookupDockerContainerName(name string) *endpoint.Endpoint {
	if ep, ok := mgr.endpointsAux[endpointid.NewID(endpointid.ContainerNamePrefix, name)]; ok {
		return ep
	}
	return nil
}

func (mgr *EndpointManager) lookupIPv4(ipv4 string) *endpoint.Endpoint {
	if ep, ok := mgr.endpointsAux[endpointid.NewID(endpointid.IPv4Prefix, ipv4)]; ok {
		return ep
	}
	return nil
}

func (mgr *EndpointManager) lookupIPv6(ipv6 string) *endpoint.Endpoint {
	if ep, ok := mgr.endpointsAux[endpointid.NewID(endpointid.IPv6Prefix, ipv6)]; ok {
		return ep
	}
	return nil
}

func (mgr *EndpointManager) lookupContainerID(id string) *endpoint.Endpoint {
	if ep, ok := mgr.endpointsAux[endpointid.NewID(endpointid.ContainerIdPrefix, id)]; ok {
		return ep
	}
	return nil
}

// UpdateIDReference updates the endpoints map in the EndpointManager for
// the given Endpoint.
func (mgr *EndpointManager) UpdateIDReference(ep *endpoint.Endpoint) {
	mgr.mutex.Lock()
	defer mgr.mutex.Unlock()
	if ep == nil {
		return
	}
	mgr.endpoints[ep.ID] = ep
}

// UpdateReferences updates maps the contents of mappings to the specified
// endpoint.
func (mgr *EndpointManager) UpdateReferences(mappings map[endpointid.PrefixType]string, ep *endpoint.Endpoint) {
	mgr.mutex.Lock()
	defer mgr.mutex.Unlock()
	for k := range mappings {
		id := endpointid.NewID(k, mappings[k])
		mgr.endpointsAux[id] = ep

	}
}

// RemoveReferences removes the mappings from the endpointmanager.
func (mgr *EndpointManager) RemoveReferences(mappings map[endpointid.PrefixType]string) {
	mgr.mutex.Lock()
	defer mgr.mutex.Unlock()
	for prefix := range mappings {
		id := endpointid.NewID(prefix, mappings[prefix])
		delete(mgr.endpointsAux, id)
	}
}

// RegenerateAllEndpoints calls a setState for each endpoint and
// regenerates if state transaction is valid. During this process, the endpoint
// list is locked and cannot be modified.
// Returns a waiting group that can be used to know when all the endpoints are
// regenerated.
func (mgr *EndpointManager) RegenerateAllEndpoints(regenMetadata *regeneration.ExternalRegenerationMetadata) *sync.WaitGroup {
	var wg sync.WaitGroup

	eps := mgr.GetEndpoints()
	wg.Add(len(eps))

	// Dereference "reason" field outside of logging statement; see
	// https://github.com/sirupsen/logrus/issues/1003.
	reason := regenMetadata.Reason
	log.WithFields(logrus.Fields{"reason": reason}).Info("regenerating all endpoints")
	for _, ep := range eps {
		go func(ep *endpoint.Endpoint) {
			<-ep.RegenerateIfAlive(regenMetadata)
			wg.Done()
		}(ep)
	}

	return &wg
}

// HasGlobalCT returns true if the endpoints have a global CT, false otherwise.
func (mgr *EndpointManager) HasGlobalCT() bool {
	eps := mgr.GetEndpoints()
	for _, e := range eps {
		if !e.Options.IsEnabled(option.ConntrackLocal) {
			return true
		}
	}
	return false
}

// GetEndpoints returns a slice of all endpoints present in endpoint manager.
func (mgr *EndpointManager) GetEndpoints() []*endpoint.Endpoint {
	mgr.mutex.RLock()
	eps := make([]*endpoint.Endpoint, 0, len(mgr.endpoints))
	for _, ep := range mgr.endpoints {
		eps = append(eps, ep)
	}
	mgr.mutex.RUnlock()
	return eps
}

// GetPolicyEndpoints returns a map of all endpoints present in endpoint
// manager as policy.Endpoint interface set for the map key.
func (mgr *EndpointManager) GetPolicyEndpoints() map[policy.Endpoint]struct{} {
	mgr.mutex.RLock()
	eps := make(map[policy.Endpoint]struct{}, len(mgr.endpoints))
	for _, ep := range mgr.endpoints {
		eps[ep] = struct{}{}
	}
	mgr.mutex.RUnlock()
	return eps
}

// AddEndpoint takes the prepared endpoint object and starts managing it.
func (mgr *EndpointManager) AddEndpoint(owner regeneration.Owner, ep *endpoint.Endpoint, reason string) (err error) {
	ep.SetDefaultConfiguration(false)

	if ep.ID != 0 {
		return fmt.Errorf("Endpoint ID is already set to %d", ep.ID)
	}
	err = ep.Expose(mgr)
	if err != nil {
		return err
	}

	repr, err := monitorAPI.EndpointCreateRepr(ep)
	// Ignore endpoint creation if EndpointCreateRepr != nil
	if err == nil {
		owner.SendNotification(monitorAPI.AgentNotifyEndpointCreated, repr)
	}
	return nil
}

// WaitForEndpointsAtPolicyRev waits for all endpoints which existed at the time
// this function is called to be at a given policy revision.
// New endpoints appearing while waiting are ignored.
func (mgr *EndpointManager) WaitForEndpointsAtPolicyRev(ctx context.Context, rev uint64) error {
	eps := mgr.GetEndpoints()
	for i := range eps {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-eps[i].WaitForPolicyRevision(ctx, rev, nil):
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	return nil
}

// CallbackForEndpointsAtPolicyRev registers a callback on all endpoints that
// exist when invoked. It is similar to WaitForEndpointsAtPolicyRevision but
// each endpoint that reaches the desired revision calls 'done' independently.
// The provided callback should not block and generally be lightweight.
func (mgr *EndpointManager) CallbackForEndpointsAtPolicyRev(ctx context.Context, rev uint64, done func(time.Time)) error {
	eps := mgr.GetEndpoints()
	for i := range eps {
		eps[i].WaitForPolicyRevision(ctx, rev, done)
	}
	return nil
}

// EndpointExists returns whether the endpoint with id exists.
func (mgr *EndpointManager) EndpointExists(id uint16) bool {
	return mgr.LookupCiliumID(id) != nil
}
