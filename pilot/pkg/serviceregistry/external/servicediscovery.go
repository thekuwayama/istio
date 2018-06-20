// Copyright 2017 Istio Authors
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

package external

import (
	"fmt"
	"sync"
	"time"

	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/model"
)

// TODO: move this out of 'external' package. Either 'serviceentry' package or
// merge with aggregate (caching, events), and possibly merge both into the
// config directory, for a single top-level cache and event system.

type serviceHandler func(*model.Service, model.Event)
type instanceHandler func(*model.ServiceInstance, model.Event)

// ServiceEntryStore communicates with ServiceEntry CRDs and monitors for changes
type ServiceEntryStore struct {
	serviceHandlers  []serviceHandler
	instanceHandlers []instanceHandler
	store            model.IstioConfigStore

	// storeCache has callbacks. Some tests use mock store.
	// Pilot 0.8 implementation only invalidates the v1 cache.
	// Post 0.8 we want to remove the v1 cache and directly interface with ads, to
	// simplify and optimize the code, this abstraction is not helping.
	callbacks model.ConfigStoreCache

	storeMutex sync.RWMutex

	ip2instance map[string][]*model.ServiceInstance
	// Endpoints table. Key is the fqdn of the service, ':', port
	instances map[string][]*model.ServiceInstance

	lastChange   time.Time
	updateNeeded bool
}

// NewServiceDiscovery creates a new ServiceEntry discovery service
func NewServiceDiscovery(callbacks model.ConfigStoreCache, store model.IstioConfigStore) *ServiceEntryStore {
	c := &ServiceEntryStore{
		serviceHandlers:  make([]serviceHandler, 0),
		instanceHandlers: make([]instanceHandler, 0),
		store:            store,
		callbacks:        callbacks,
		ip2instance:      map[string][]*model.ServiceInstance{},
		instances:        map[string][]*model.ServiceInstance{},
		updateNeeded:     true,
	}
	if callbacks != nil {
		callbacks.RegisterEventHandler(model.ServiceEntry.Type, func(config model.Config, event model.Event) {
			serviceEntry := config.Spec.(*networking.ServiceEntry)

			// Recomputing the index here is too expensive.
			c.storeMutex.Lock()
			c.lastChange = time.Now()
			c.updateNeeded = true
			c.storeMutex.Unlock()

			services := convertServices(serviceEntry)
			for _, handler := range c.serviceHandlers {
				for _, service := range services {
					go handler(service, event)
				}
			}

			instances := convertInstances(serviceEntry)
			for _, handler := range c.instanceHandlers {
				for _, instance := range instances {
					go handler(instance, event)
				}
			}
		})
	}

	return c
}

// AppendServiceHandler is an over-complicated way to add the v1 cache invalidation.
// In <0.8 pilot it is not usingthe event or service param.
// Deprecated: post 0.8 we're planning to use direct interface
func (d *ServiceEntryStore) AppendServiceHandler(f func(*model.Service, model.Event)) error {
	d.serviceHandlers = append(d.serviceHandlers, f)
	return nil
}

// AppendInstanceHandler is an over-complicated way to add the v1 cache invalidation.
// In <0.8 pilot it is not usingthe event or service param.
// Deprecated: post 0.8 we're planning to use direct interface
func (d *ServiceEntryStore) AppendInstanceHandler(f func(*model.ServiceInstance, model.Event)) error {
	d.instanceHandlers = append(d.instanceHandlers, f)
	return nil
}

// Run is used by some controllers to execute background jobs after init is done.
func (d *ServiceEntryStore) Run(stop <-chan struct{}) {}

// Services list declarations of all services in the system
func (d *ServiceEntryStore) Services() ([]*model.Service, error) {
	services := make([]*model.Service, 0)
	for _, config := range d.store.ServiceEntries() {
		serviceEntry := config.Spec.(*networking.ServiceEntry)
		services = append(services, convertServices(serviceEntry)...)
	}

	return services, nil
}

// GetService retrieves a service by host name if it exists
func (d *ServiceEntryStore) GetService(hostname model.Hostname) (*model.Service, error) {
	for _, service := range d.getServices() {
		if service.Hostname == hostname {
			return service, nil
		}
	}

	return nil, nil
}

// GetServiceAttributes retrieves the custom attributes of a service if it exists.
func (d *ServiceEntryStore) GetServiceAttributes(hostname model.Hostname) (*model.ServiceAttributes, error) {
	for _, config := range d.store.ServiceEntries() {
		serviceEntry := config.Spec.(*networking.ServiceEntry)
		svcs := convertServices(serviceEntry)
		for _, s := range svcs {
			if s.Hostname == hostname {
				return &model.ServiceAttributes{
					Name:      hostname.String(),
					Namespace: config.Namespace}, nil
			}
		}
	}
	return nil, fmt.Errorf("service not found")
}

func (d *ServiceEntryStore) getServices() []*model.Service {
	services := make([]*model.Service, 0)
	for _, config := range d.store.ServiceEntries() {
		serviceEntry := config.Spec.(*networking.ServiceEntry)
		services = append(services, convertServices(serviceEntry)...)
	}
	return services
}

// ManagementPorts retries set of health check ports by instance IP.
// This does not apply to Service Entry registry, as Service entries do not
// manage the service instances.
func (d *ServiceEntryStore) ManagementPorts(addr string) model.PortList {
	return nil
}

// Instances retrieves instances for a service and its ports that match
// any of the supplied labels. All instances match an empty tag list.
// This is only called from v1 code paths - which don't support ServiceEntry,
// so it production it should never happen in v1/alpha1 case
// However, since we implement this method, v1 users will still get the ServiceEntry.
// This contradicts the general migration policy of keeping alpha3 separated, but
// may help in cases where mesh expansion is moved with some workloads still using
// v1.
func (d *ServiceEntryStore) Instances(hostname model.Hostname, ports []string,
	labels model.LabelsCollection) ([]*model.ServiceInstance, error) {
	portMap := make(map[string]bool)
	for _, port := range ports {
		portMap[port] = true
	}

	out := []*model.ServiceInstance{}
	for _, config := range d.store.ServiceEntries() {
		serviceEntry := config.Spec.(*networking.ServiceEntry)
		for _, instance := range convertInstances(serviceEntry) {
			if instance.Service.Hostname == hostname &&
				labels.HasSubsetOf(instance.Labels) &&
				portMatchEnvoyV1(instance, portMap) {
				out = append(out, instance)
			}
		}
	}

	return out, nil
}

// InstancesByPort retrieves instances for a service on the given ports with labels that
// match any of the supplied labels. All instances match an empty tag list.
func (d *ServiceEntryStore) InstancesByPort(hostname model.Hostname, port int,
	labels model.LabelsCollection) ([]*model.ServiceInstance, error) {
	d.update()

	d.storeMutex.RLock()
	defer d.storeMutex.RUnlock()
	out := []*model.ServiceInstance{}

	instances, found := d.instances[hostname.String()]
	if found {
		for _, instance := range instances {
			if instance.Service.Hostname == hostname &&
				labels.HasSubsetOf(instance.Labels) &&
				portMatchSingle(instance, port) {
				out = append(out, instance)
			}
		}
	}

	return out, nil
}

// update will iterate all ServiceEntries, convert to ServiceInstance (expensive),
// and populate the 'by host' and 'by ip' maps.
func (d *ServiceEntryStore) update() {
	d.storeMutex.RLock()
	if !d.updateNeeded {
		return
	}
	d.storeMutex.RUnlock()

	d.storeMutex.Lock()
	defer d.storeMutex.Unlock()
	d.instances = map[string][]*model.ServiceInstance{}
	d.ip2instance = map[string][]*model.ServiceInstance{}

	for _, config := range d.store.ServiceEntries() {
		serviceEntry := config.Spec.(*networking.ServiceEntry)
		for _, instance := range convertInstances(serviceEntry) {
			key := instance.Service.Hostname.String()
			out, found := d.instances[key]
			if !found {
				out = []*model.ServiceInstance{}
			}
			out = append(out, instance)
			d.instances[key] = out

			byip, found := d.instances[instance.Endpoint.Address]
			if !found {
				byip = []*model.ServiceInstance{}
			}
			byip = append(byip, instance)
			d.ip2instance[instance.Endpoint.Address] = byip
		}
	}
}

// returns true if an instance's port matches with any in the provided list
func portMatchEnvoyV1(instance *model.ServiceInstance, portMap map[string]bool) bool {
	return len(portMap) == 0 || portMap[instance.Endpoint.ServicePort.Name]
}

// returns true if an instance's port matches with any in the provided list
func portMatchSingle(instance *model.ServiceInstance, port int) bool {
	return port == 0 || port == instance.Endpoint.ServicePort.Port
}

// GetProxyServiceInstances lists service instances co-located with a given proxy
func (d *ServiceEntryStore) GetProxyServiceInstances(node *model.Proxy) ([]*model.ServiceInstance, error) {
	d.update()
	d.storeMutex.RLock()
	defer d.storeMutex.RUnlock()

	instances, found := d.ip2instance[node.IPAddress]
	if found {
		return instances, nil
	}
	return []*model.ServiceInstance{}, nil
}

// GetIstioServiceAccounts implements model.ServiceAccounts operation TODOg
func (d *ServiceEntryStore) GetIstioServiceAccounts(hostname model.Hostname, ports []string) []string {
	//for service entries, there is no istio auth, no service accounts, etc. It is just a
	// service, with service instances, and dns.
	return nil
}
