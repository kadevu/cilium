// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package legacy

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	envoy_config_core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	envoy_config_endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"

	"github.com/cilium/cilium/pkg/envoy"
	"github.com/cilium/cilium/pkg/loadbalancer"
	"github.com/cilium/cilium/pkg/loadbalancer/legacy/service"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/slices"
)

const anyPort = "*"

// envoyServiceBackendSyncer syncs the backends of a Service as Endpoints to the Envoy L7 proxy.
type envoyServiceBackendSyncer struct {
	logger *slog.Logger

	envoyXdsServer envoy.XDSServer

	l7lbSvcsMutex lock.RWMutex
	l7lbSvcs      map[loadbalancer.ServiceName]*backendSyncInfo
}

var _ service.BackendSyncer = &envoyServiceBackendSyncer{}

func (*envoyServiceBackendSyncer) ProxyName() string {
	return "Envoy"
}

func newEnvoyServiceBackendSyncer(logger *slog.Logger, envoyXdsServer envoy.XDSServer) *envoyServiceBackendSyncer {
	return &envoyServiceBackendSyncer{
		logger:         logger,
		envoyXdsServer: envoyXdsServer,
		l7lbSvcs:       map[loadbalancer.ServiceName]*backendSyncInfo{},
	}
}

func (r *envoyServiceBackendSyncer) Sync(svc *loadbalancer.LegacySVC) error {
	r.l7lbSvcsMutex.RLock()
	l7lbInfo, exists := r.l7lbSvcs[svc.Name]
	if !exists {
		r.l7lbSvcsMutex.RUnlock()
		return nil
	}
	frontendPorts := l7lbInfo.GetAllFrontendPorts()
	r.l7lbSvcsMutex.RUnlock()

	// Filter backend based on list of port numbers, then upsert backends
	// as Envoy endpoints
	be := filterServiceBackends(svc, frontendPorts)

	r.logger.Debug("Upsert envoy endpoints",
		logfields.L7LBFrontendPorts, frontendPorts,
		logfields.ServiceNamespace, svc.Name.Namespace,
		logfields.ServiceName, svc.Name.Name,
	)
	if err := r.upsertEnvoyEndpoints(svc.Name, be); err != nil {
		return fmt.Errorf("failed to update backends in Envoy: %w", err)
	}

	return nil
}

func (r *envoyServiceBackendSyncer) RegisterServiceUsageInCEC(svcName loadbalancer.ServiceName, resourceName service.L7LBResourceName, frontendPorts []string) {
	r.l7lbSvcsMutex.Lock()
	defer r.l7lbSvcsMutex.Unlock()

	l7lbInfo, exists := r.l7lbSvcs[svcName]

	if !exists {
		l7lbInfo = &backendSyncInfo{}
	}

	if l7lbInfo.backendRefs == nil {
		l7lbInfo.backendRefs = make(map[service.L7LBResourceName]backendSyncCECInfo, 1)
	}

	l7lbInfo.backendRefs[resourceName] = backendSyncCECInfo{
		frontendPorts: frontendPorts,
	}

	r.l7lbSvcs[svcName] = l7lbInfo
}

func (r *envoyServiceBackendSyncer) DeregisterServiceUsageInCEC(svcName loadbalancer.ServiceName, resourceName service.L7LBResourceName) bool {
	r.l7lbSvcsMutex.Lock()
	defer r.l7lbSvcsMutex.Unlock()

	l7lbInfo, exists := r.l7lbSvcs[svcName]

	if !exists {
		return false
	}

	if l7lbInfo.backendRefs != nil {
		delete(l7lbInfo.backendRefs, resourceName)
	}

	// Cleanup service if it's no longer used by any CEC
	if len(l7lbInfo.backendRefs) == 0 {
		delete(r.l7lbSvcs, svcName)
		return true
	}

	r.l7lbSvcs[svcName] = l7lbInfo

	return false
}

func (r *envoyServiceBackendSyncer) upsertEnvoyEndpoints(serviceName loadbalancer.ServiceName, backendMap map[string][]*loadbalancer.LegacyBackend) error {
	var resources envoy.Resources

	resources.Endpoints = getEndpointsForLBBackends(serviceName, backendMap)

	// Using context.TODO() is fine as we do not upsert listener resources here - the
	// context ends up being used only if listener(s) are included in 'resources'.
	return r.envoyXdsServer.UpsertEnvoyResources(context.TODO(), resources)
}

func getEndpointsForLBBackends(serviceName loadbalancer.ServiceName, backendMap map[string][]*loadbalancer.LegacyBackend) []*envoy_config_endpoint.ClusterLoadAssignment {
	var endpoints []*envoy_config_endpoint.ClusterLoadAssignment

	for port, bes := range backendMap {
		var lbEndpoints []*envoy_config_endpoint.LbEndpoint
		for _, be := range bes {
			// The below is to make sure that UDP and SCTP are not allowed instead of comparing with lb.TCP
			// The reason is to avoid extra dependencies with ongoing work to differentiate protocols in datapath,
			// which might add more values such as lb.Any, lb.None, etc.
			if be.Protocol == loadbalancer.UDP || be.Protocol == loadbalancer.SCTP {
				continue
			}

			lbEndpoints = append(lbEndpoints, &envoy_config_endpoint.LbEndpoint{
				HostIdentifier: &envoy_config_endpoint.LbEndpoint_Endpoint{
					Endpoint: &envoy_config_endpoint.Endpoint{
						Address: &envoy_config_core.Address{
							Address: &envoy_config_core.Address_SocketAddress{
								SocketAddress: &envoy_config_core.SocketAddress{
									Address: be.AddrCluster.String(),
									PortSpecifier: &envoy_config_core.SocketAddress_PortValue{
										PortValue: uint32(be.Port),
									},
								},
							},
						},
					},
				},
			})
		}

		endpoint := &envoy_config_endpoint.ClusterLoadAssignment{
			ClusterName: fmt.Sprintf("%s:%s", serviceName.String(), port),
			Endpoints: []*envoy_config_endpoint.LocalityLbEndpoints{
				{
					LbEndpoints: lbEndpoints,
				},
			},
		}
		endpoints = append(endpoints, endpoint)

		// for backward compatibility, if any port is allowed, publish one more
		// endpoint having cluster name as service name.
		if port == anyPort {
			endpoints = append(endpoints, &envoy_config_endpoint.ClusterLoadAssignment{
				ClusterName: serviceName.String(),
				Endpoints: []*envoy_config_endpoint.LocalityLbEndpoints{
					{
						LbEndpoints: lbEndpoints,
					},
				},
			})
		}
	}

	return endpoints
}

// filterServiceBackends returns the list of backends based on given front end ports.
// The returned map will have key as port name/number, and value as list of respective backends.
func filterServiceBackends(svc *loadbalancer.LegacySVC, onlyPorts []string) map[string][]*loadbalancer.LegacyBackend {
	preferredBackends := filterPreferredBackends(svc.Backends)

	if len(onlyPorts) == 0 {
		return map[string][]*loadbalancer.LegacyBackend{
			"*": preferredBackends,
		}
	}

	res := map[string][]*loadbalancer.LegacyBackend{}
	for _, port := range onlyPorts {
		// check for port number
		if port == strconv.Itoa(int(svc.Frontend.Port)) {
			res[port] = preferredBackends
		}

		// Continue checking for either named port as the same service
		// can be used with multiple port types together
		for _, backend := range preferredBackends {
			if port == backend.FEPortName {
				res[port] = append(res[port], backend)
			}
		}
	}

	return res
}

// filterPreferredBackends returns the slice of backends which are preferred for the given service.
// If there is no preferred backend, it returns the slice of all backends.
func filterPreferredBackends(backends []*loadbalancer.LegacyBackend) []*loadbalancer.LegacyBackend {
	var res []*loadbalancer.LegacyBackend
	for _, backend := range backends {
		if backend.Preferred {
			res = append(res, backend)
		}
	}
	if len(res) > 0 {
		return res
	}

	return backends
}

type backendSyncInfo struct {
	// Names of the L7 LB resources (e.g. CEC) that need this service's backends to be
	// synced to an L7 Loadbalancer.
	backendRefs map[service.L7LBResourceName]backendSyncCECInfo
}

func (r *backendSyncInfo) GetAllFrontendPorts() []string {
	allPorts := []string{}

	for _, info := range r.backendRefs {
		allPorts = append(allPorts, info.frontendPorts...)
	}

	return slices.SortedUnique(allPorts)
}

type backendSyncCECInfo struct {
	// List of front-end ports of upstream service/cluster, which will be used for
	// filtering applicable endpoints.
	//
	// If nil, all the available backends will be used.
	frontendPorts []string
}
