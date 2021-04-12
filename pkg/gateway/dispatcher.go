package gateway

import (
	"fmt"
	"reflect"

	v2 "github.com/datawire/ambassador/pkg/api/envoy/api/v2"
	core "github.com/datawire/ambassador/pkg/api/envoy/api/v2/core"
	route "github.com/datawire/ambassador/pkg/api/envoy/api/v2/route"
	"github.com/datawire/ambassador/pkg/envoy-control-plane/cache/types"
	"github.com/datawire/ambassador/pkg/envoy-control-plane/cache/v2"
	"github.com/datawire/ambassador/pkg/envoy-control-plane/resource/v2"
	"github.com/datawire/ambassador/pkg/envoy-control-plane/wellknown"
	"github.com/datawire/ambassador/pkg/kates"
	"github.com/golang/protobuf/ptypes/duration"
	"github.com/pkg/errors"
)

// The Dispatcher struct allows transforms to be registered for different kinds of kubernetes
// resources and invokes those transforms to produce compiled envoy configurations. It also knows
// how to assemble the compiled envoy configuration into a complete snapshot.
type Dispatcher struct {
	// Map from kind to transform function.
	transforms map[string]reflect.Value
	configs    map[string]*CompiledConfig

	version     string
	changeCount int
	snapshot    *cache.Snapshot
}

// resourceKey produces a fully qualified key for a kubernetes resource.
func resourceKey(resource kates.Object) string {
	gvk := resource.GetObjectKind().GroupVersionKind()
	return resourceKeyFromParts(gvk.Kind, resource.GetNamespace(), resource.GetName())
}

func resourceKeyFromParts(kind, namespace, name string) string {
	return fmt.Sprintf("%s:%s:%s", kind, namespace, name)
}

// NewDispatcher creates a new and empty *Dispatcher struct.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{
		transforms: map[string]reflect.Value{},
		configs:    map[string]*CompiledConfig{},
	}
}

// Register registers a transform function for the specified kubernetes resource.
func (d *Dispatcher) Register(kind string, transform interface{}) error {
	_, ok := d.transforms[kind]
	if ok {
		return errors.Errorf("duplicate transform: %+v", transform)
	}

	xform := reflect.ValueOf(transform)

	d.transforms[kind] = xform

	return nil
}

// IsRegistered returns true if the given kind can be processed by this dispatcher.
func (d *Dispatcher) IsRegistered(kind string) bool {
	_, ok := d.transforms[kind]
	return ok
}

// Upsert processes the given kubernetes resource whether it is new or just updated.
func (d *Dispatcher) Upsert(resource kates.Object) error {
	gvk := resource.GetObjectKind().GroupVersionKind()
	xform, ok := d.transforms[gvk.Kind]
	if !ok {
		return errors.Errorf("no transform for kind: %q", gvk.Kind)
	}

	key := resourceKey(resource)

	var config *CompiledConfig
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				e, ok := r.(error)
				if ok {
					err = errors.Wrapf(e, "internal error processing %s", key)
				} else {
					err = errors.Errorf("internal error processing %s: %+v", key, e)
				}
			}
		}()
		result := xform.Call([]reflect.Value{reflect.ValueOf(resource)})
		config = result[0].Interface().(*CompiledConfig)
	}()

	if err != nil {
		return err
	}

	d.configs[key] = config
	// Clear out the snapshot so we regenerate one.
	d.snapshot = nil
	return nil
}

// Delete processes the deletion of the given kubernetes resource.
func (d *Dispatcher) Delete(resource kates.Object) {
	key := resourceKey(resource)
	delete(d.configs, key)

	// Clear out the snapshot so we regenerate one.
	d.snapshot = nil
}

func (d *Dispatcher) DeleteKey(kind, namespace, name string) {
	key := resourceKeyFromParts(kind, namespace, name)
	delete(d.configs, key)
	d.snapshot = nil
}

// UpsertYaml parses the supplied yaml and invokes Upsert on the result.
func (d *Dispatcher) UpsertYaml(manifests string) error {
	objs, err := kates.ParseManifests(manifests)
	if err != nil {
		return err
	}
	for _, obj := range objs {
		err := d.Upsert(obj)
		if err != nil {
			return err
		}
	}
	return nil
}

// GetErrors returns all compiled items with errors.
func (d *Dispatcher) GetErrors() []*CompiledItem {
	var result []*CompiledItem
	for _, config := range d.configs {
		if config.Error != "" {
			result = append(result, &config.CompiledItem)
		}
		for _, l := range config.Listeners {
			if l.Error != "" {
				result = append(result, &l.CompiledItem)
			}
		}
		for _, r := range config.Routes {
			if r.Error != "" {
				result = append(result, &r.CompiledItem)
			}
			for _, cr := range r.ClusterRefs {
				if cr.Error != "" {
					result = append(result, &cr.CompiledItem)
				}
			}
		}
		for _, c := range config.Clusters {
			if c.Error != "" {
				result = append(result, &c.CompiledItem)
			}
		}
		for _, la := range config.LoadAssignments {
			if la.Error != "" {
				result = append(result, &la.CompiledItem)
			}
		}
	}
	return result
}

// GetSnapshot returns a version and a snapshot.
func (d *Dispatcher) GetSnapshot() (string, *cache.Snapshot) {
	if d.snapshot == nil {
		d.buildSnapshot()
	}
	return d.version, d.snapshot
}

// GetListener returns a *v2.Listener with the specified name or nil if none exists.
func (d *Dispatcher) GetListener(name string) *v2.Listener {
	_, snap := d.GetSnapshot()
	for _, rsrc := range snap.Resources[types.Listener].Items {
		l := rsrc.(*v2.Listener)
		if l.Name == name {
			return l
		}
	}
	return nil

}

// GetRouteConfiguration returns a *v2.RouteConfiguration with the specified name or nil if none
// exists.
func (d *Dispatcher) GetRouteConfiguration(name string) *v2.RouteConfiguration {
	_, snap := d.GetSnapshot()
	for _, rsrc := range snap.Resources[types.Route].Items {
		r := rsrc.(*v2.RouteConfiguration)
		if r.Name == name {
			return r
		}
	}
	return nil
}

func (d *Dispatcher) buildClusterMap() map[string][]*ClusterRef {
	refs := map[string][]*ClusterRef{}
	for _, config := range d.configs {
		for _, route := range config.Routes {
			for _, ref := range route.ClusterRefs {
				refs[ref.Name] = append(refs[ref.Name], ref)
			}
		}
	}
	return refs
}

func (d *Dispatcher) buildEndpointMap() map[string]*v2.ClusterLoadAssignment {
	endpoints := map[string]*v2.ClusterLoadAssignment{}
	for _, config := range d.configs {
		for _, la := range config.LoadAssignments {
			endpoints[la.LoadAssignment.ClusterName] = la.LoadAssignment
		}
	}
	return endpoints
}

func (d *Dispatcher) buildRouteConfigurations() ([]types.Resource, []types.Resource) {
	listeners := []types.Resource{}
	routes := []types.Resource{}
	for _, config := range d.configs {
		for _, lst := range config.Listeners {
			listeners = append(listeners, lst.Listener)
			r := d.buildRouteConfiguration(lst)
			if r != nil {
				routes = append(routes, r)
			}
		}
	}
	return listeners, routes
}

func (d *Dispatcher) buildRouteConfiguration(lst *CompiledListener) *v2.RouteConfiguration {
	rdsName, isRds := getRdsName(lst.Listener)
	if !isRds {
		return nil
	}

	var routes []*route.Route
	for _, config := range d.configs {
		for _, route := range config.Routes {
			if lst.Predicate(route) {
				routes = append(routes, route.Routes...)
			}
		}
	}

	return &v2.RouteConfiguration{
		Name: rdsName,
		VirtualHosts: []*route.VirtualHost{
			{
				Name:    rdsName,
				Domains: lst.Domains,
				Routes:  routes,
			},
		},
	}
}

// getRdsName returns the RDS route configuration name configured for the listener and a flag
// indicating whether the listener uses Rds.
func getRdsName(l *v2.Listener) (string, bool) {
	for _, fc := range l.FilterChains {
		for _, f := range fc.Filters {
			if f.Name != wellknown.HTTPConnectionManager {
				continue
			}

			hcm := resource.GetHTTPConnectionManager(f)
			if hcm != nil {
				rds := hcm.GetRds()
				if rds != nil {
					return rds.RouteConfigName, true
				}
			}
		}
	}
	return "", false
}

func (d *Dispatcher) buildSnapshot() {
	d.changeCount++
	d.version = fmt.Sprintf("v%d", d.changeCount)

	endpointMap := d.buildEndpointMap()
	clusterMap := d.buildClusterMap()

	clusters := []types.Resource{}
	endpoints := []types.Resource{}
	for name := range clusterMap {
		clusters = append(clusters, makeCluster(name))
		endpoints = append(endpoints, endpointMap[name])
	}

	listeners, routes := d.buildRouteConfigurations()

	snapshot := cache.NewSnapshot(d.version, endpoints, clusters, routes, listeners, nil)
	if err := snapshot.Consistent(); err != nil {
		panic(errors.Wrapf(err, "Snapshot inconsistency: %s", d.version))
	} else {
		d.snapshot = &snapshot
	}
}

func makeCluster(name string) *v2.Cluster {
	return &v2.Cluster{
		Name:                 name,
		ConnectTimeout:       &duration.Duration{Seconds: 10},
		ClusterDiscoveryType: &v2.Cluster_Type{Type: v2.Cluster_EDS},
		EdsClusterConfig:     &v2.Cluster_EdsClusterConfig{EdsConfig: &core.ConfigSource{ConfigSourceSpecifier: &core.ConfigSource_Ads{}}},
	}
}
