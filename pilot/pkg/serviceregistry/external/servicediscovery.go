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
	"sync"
	"time"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/labels"
	"istio.io/istio/pkg/config/schemas"
)

// TODO: move this out of 'external' package. Either 'serviceentry' package or
// merge with aggregate (caching, events), and possibly merge both into the
// config directory, for a single top-level cache and event system.

var _ serviceregistry.Instance = &ServiceEntryStore{}

type serviceHandler func(*model.Service, model.Event)
type instanceHandler func(*model.ServiceInstance, model.Event)

// ServiceEntryStore communicates with ServiceEntry CRDs and monitors for changes
type ServiceEntryStore struct {
	serviceHandlers  []serviceHandler
	instanceHandlers []instanceHandler
	store            model.IstioConfigStore

	storeMutex sync.RWMutex

	ip2instance map[string][]*model.ServiceInstance
	// Endpoints table. Key is the fqdn hostname and namespace
	instances map[host.Name]map[string][]*model.ServiceInstance

	changeMutex  sync.RWMutex
	lastChange   time.Time
	updateNeeded bool
}

// NewServiceDiscovery creates a new ServiceEntry discovery service
func NewServiceDiscovery(configController model.ConfigStoreCache, store model.IstioConfigStore) *ServiceEntryStore {
	c := &ServiceEntryStore{
		serviceHandlers:  make([]serviceHandler, 0),
		instanceHandlers: make([]instanceHandler, 0),
		store:            store,
		ip2instance:      map[string][]*model.ServiceInstance{},
		instances:        map[host.Name]map[string][]*model.ServiceInstance{},
		updateNeeded:     true,
	}
	if configController != nil {
		configController.RegisterEventHandler(schemas.ServiceEntry.Type, func(config model.Config, event model.Event) {
			// Recomputing the index here is too expensive.
			c.changeMutex.Lock()
			c.lastChange = time.Now()
			c.updateNeeded = true
			c.changeMutex.Unlock()

			// TODO : Currently any update to ServiceEntry triggers a full push. We need to identify what has actually
			// changed and call appropriate handlers - for example call only service handler for the changed services
			// and call instance handlers only when instance update happens. This requires us to rework the handlers to
			// have both old and new objects so that they can compare and be smart.
			services := convertServices(config)
			for _, handler := range c.serviceHandlers {
				for _, service := range services {
					go handler(service, event)
				}
			}
		})
	}

	return c
}

func (d *ServiceEntryStore) Provider() serviceregistry.ProviderID {
	return serviceregistry.External
}

func (d *ServiceEntryStore) Cluster() string {
	return ""
}

// AppendServiceHandler adds service resource event handler
func (d *ServiceEntryStore) AppendServiceHandler(f func(*model.Service, model.Event)) error {
	d.serviceHandlers = append(d.serviceHandlers, f)
	return nil
}

// AppendInstanceHandler adds instance event handler.
func (d *ServiceEntryStore) AppendInstanceHandler(f func(*model.ServiceInstance, model.Event)) error {
	d.instanceHandlers = append(d.instanceHandlers, f)
	return nil
}

// Run is used by some controllers to execute background jobs after init is done.
func (d *ServiceEntryStore) Run(stop <-chan struct{}) {}

// Services list declarations of all services in the system
func (d *ServiceEntryStore) Services() ([]*model.Service, error) {
	services := make([]*model.Service, 0)
	for _, cfg := range d.store.ServiceEntries() {
		services = append(services, convertServices(cfg)...)
	}

	return services, nil
}

// GetService retrieves a service by host name if it exists
// THIS IS A LINEAR SEARCH WHICH CAUSES ALL SERVICE ENTRIES TO BE RECONVERTED -
// DO NOT USE
func (d *ServiceEntryStore) GetService(hostname host.Name) (*model.Service, error) {
	for _, service := range d.getServices() {
		if service.Hostname == hostname {
			return service, nil
		}
	}

	return nil, nil
}

func (d *ServiceEntryStore) getServices() []*model.Service {
	services := make([]*model.Service, 0)
	for _, cfg := range d.store.ServiceEntries() {
		services = append(services, convertServices(cfg)...)
	}
	return services
}

// ManagementPorts retrieves set of health check ports by instance IP.
// This does not apply to Service Entry registry, as Service entries do not
// manage the service instances.
func (d *ServiceEntryStore) ManagementPorts(addr string) model.PortList {
	return nil
}

// WorkloadHealthCheckInfo retrieves set of health check info by instance IP.
// This does not apply to Service Entry registry, as Service entries do not
// manage the service instances.
func (d *ServiceEntryStore) WorkloadHealthCheckInfo(addr string) model.ProbeList {
	return nil
}

// InstancesByPort retrieves instances for a service on the given ports with labels that
// match any of the supplied labels. All instances match an empty tag list.
func (d *ServiceEntryStore) InstancesByPort(svc *model.Service, port int,
	labels labels.Collection) ([]*model.ServiceInstance, error) {
	d.update()

	d.storeMutex.RLock()
	defer d.storeMutex.RUnlock()
	out := make([]*model.ServiceInstance, 0)

	instances, found := d.instances[svc.Hostname][svc.Attributes.Namespace]
	if found {
		for _, instance := range instances {
			if instance.Service.Hostname == svc.Hostname &&
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
	d.changeMutex.RLock()
	if !d.updateNeeded {
		d.changeMutex.RUnlock()
		return
	}
	d.changeMutex.RUnlock()

	di := map[host.Name]map[string][]*model.ServiceInstance{}
	dip := map[string][]*model.ServiceInstance{}

	for _, cfg := range d.store.ServiceEntries() {
		for _, instance := range convertInstances(cfg, nil) {

			out, found := di[instance.Service.Hostname][instance.Service.Attributes.Namespace]
			if !found {
				out = []*model.ServiceInstance{}
			}
			out = append(out, instance)
			if _, f := di[instance.Service.Hostname]; !f {
				di[instance.Service.Hostname] = map[string][]*model.ServiceInstance{}
			}
			di[instance.Service.Hostname][instance.Service.Attributes.Namespace] = out

			byip, found := dip[instance.Endpoint.Address]
			if !found {
				byip = []*model.ServiceInstance{}
			}
			byip = append(byip, instance)
			dip[instance.Endpoint.Address] = byip
		}
	}

	d.storeMutex.Lock()
	d.instances = di
	d.ip2instance = dip
	d.storeMutex.Unlock()

	// Without this pilot will become very unstable even with few 100 ServiceEntry
	// objects - the N_clusters * N_update generates too much garbage
	// ( yaml to proto)
	// This is reset on any change in ServiceEntries
	d.changeMutex.Lock()
	d.updateNeeded = false
	d.changeMutex.Unlock()
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

	out := make([]*model.ServiceInstance, 0)

	for _, ip := range node.IPAddresses {
		instances, found := d.ip2instance[ip]
		if found {
			out = append(out, instances...)
		}
	}
	return out, nil
}

func (d *ServiceEntryStore) GetProxyWorkloadLabels(proxy *model.Proxy) (labels.Collection, error) {
	d.update()
	d.storeMutex.RLock()
	defer d.storeMutex.RUnlock()

	out := make(labels.Collection, 0)

	for _, ip := range proxy.IPAddresses {
		instances, found := d.ip2instance[ip]
		if found {
			for _, instance := range instances {
				out = append(out, instance.Labels)
			}
		}
	}
	return out, nil
}

// GetIstioServiceAccounts implements model.ServiceAccounts operation TODOg
func (d *ServiceEntryStore) GetIstioServiceAccounts(svc *model.Service, ports []int) []string {
	//for service entries, there is no istio auth, no service accounts, etc. It is just a
	// service, with service instances, and dns.
	return nil
}
