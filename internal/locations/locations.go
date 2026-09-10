// SPDX-License-Identifier: AGPL-3.0-only

// Package locations reads the two location facts compute depends on: which
// cities a project may place workloads in, and which location a cell serves.
//
// Both are served today by network-services-operator and are moving to the
// locations service. Which one is read is selected per deployment by Source,
// so a control plane that has not been migrated keeps reading the types it
// already has.
//
// Placement can also be read from the service catalog: a ServiceAvailability
// records that a service is deployed and operational at a Location, which is
// the fact "may this project place compute here?" actually rests on.
package locations

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	locationsv1alpha1 "go.miloapis.com/locations/api/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	// TopologyCityCodeKey is the topology key holding a location's city.
	TopologyCityCodeKey = locationsv1alpha1.TopologyCityCodeKey

	// ServingLocationTopologyLabel is the cluster label a cell carries to claim
	// the location it serves.
	ServingLocationTopologyLabel = locationsv1alpha1.ServingLocationTopologyLabel

	// DefaultServiceName is the service whose availability records name the
	// locations a project may place at. Compute reads its own.
	DefaultServiceName = "compute"

	// conditionAvailable is the ServiceAvailability condition that reports the
	// service deployed and validated at the location. The service catalog keeps
	// the constant inside its controller package, so the string is repeated
	// here rather than importing an internal package.
	conditionAvailable = "Available"
)

// Source names the API group locations are read from.
type Source string

const (
	// SourceNetworkServices reads networking.datumapis.com LocationBindings and
	// ServingLocations. This is what every deployment reads today.
	SourceNetworkServices Source = "NetworkServices"

	// SourceLocations reads locations.miloapis.com Locations and
	// ServingLocations, served by the locations service.
	SourceLocations Source = "Locations"

	// SourceServiceAvailability reads services.miloapis.com
	// ServiceAvailability records: a location is placeable when the service
	// reports itself available there. The topology still comes from the
	// locations.miloapis.com Location each record points at, so this source
	// reads both kinds. Serving locations are unaffected — availability is a
	// statement about a location, not about a cell — and are read from the
	// locations service.
	SourceServiceAvailability Source = "ServiceAvailability"
)

// Resolve reports which source to read. An unset source reads network
// services, matching the config default.
func (s Source) Resolve() (Source, error) {
	switch s {
	case "", SourceNetworkServices:
		return SourceNetworkServices, nil
	case SourceLocations:
		return SourceLocations, nil
	case SourceServiceAvailability:
		return SourceServiceAvailability, nil
	default:
		return "", fmt.Errorf("unknown location source %q, want %q, %q or %q",
			s, SourceNetworkServices, SourceLocations, SourceServiceAvailability)
	}
}

// ErrAvailabilityNotServed reports that a project does not serve one of the two
// kinds SourceServiceAvailability reads, so where compute is offered cannot be
// answered at all.
//
// This does NOT degrade to no locations, where the other sources do. An empty
// list is a real answer — compute is offered nowhere this project may use —
// and returning it for a kind nobody is serving tells a customer their project
// has no locations when the truth is that nothing looked. The two are opposite
// actions: one waits for Datum to add a location, the other is a deployment
// that needs fixing, so they must never arrive as the same answer.
//
// Wrapped with the kind that was missing, and matched with errors.Is.
var ErrAvailabilityNotServed = errors.New("where compute is offered cannot be read from this project")

// PlacementLocation is a location a project may place workloads at.
type PlacementLocation struct {
	Name     string
	Topology map[string]string
}

// CityCode returns the city the location serves, and whether it declares one.
func (l PlacementLocation) CityCode() (string, bool) {
	code, ok := l.Topology[TopologyCityCodeKey]
	return code, ok
}

// ServingLocation is the location a cell has been told it serves.
type ServingLocation struct {
	Name     string
	Topology map[string]string
}

// CityCode returns the city the cell sits in.
func (l ServingLocation) CityCode() string {
	return l.Topology[TopologyCityCodeKey]
}

// ListPlacementLocations returns the locations a project may place workloads
// at, read from the project's control plane. Availability records are read for
// DefaultServiceName; ListPlacementLocationsForService names another service.
func ListPlacementLocations(ctx context.Context, c client.Client, source Source) ([]PlacementLocation, error) {
	return ListPlacementLocationsForService(ctx, c, source, DefaultServiceName)
}

// ListPlacementLocationsForService is ListPlacementLocations for a named
// service. The name is only read by SourceServiceAvailability, which is the
// only source that knows which service a location is offered for; the other
// two sources have already been filtered to one service by whoever wrote them.
func ListPlacementLocationsForService(
	ctx context.Context, c client.Client, source Source, serviceName string,
) ([]PlacementLocation, error) {
	resolved, err := source.Resolve()
	if err != nil {
		return nil, err
	}

	switch resolved {
	case SourceNetworkServices:
		var bindings networkingv1alpha.LocationBindingList
		if err := c.List(ctx, &bindings); err != nil {
			return nil, fmt.Errorf("failed to list location bindings: %w", err)
		}

		found := make([]PlacementLocation, 0, len(bindings.Items))
		for _, binding := range bindings.Items {
			found = append(found, PlacementLocation{
				Name:     binding.Name,
				Topology: binding.Spec.Topology,
			})
		}
		return found, nil

	case SourceServiceAvailability:
		return listAvailableLocations(ctx, c, serviceName)
	}

	var list locationsv1alpha1.LocationList
	if err := c.List(ctx, &list); err != nil {
		if kindNotInstalled(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to list locations: %w", err)
	}

	found := make([]PlacementLocation, 0, len(list.Items))
	for _, location := range list.Items {
		found = append(found, PlacementLocation{
			Name:     location.Name,
			Topology: location.Spec.Topology,
		})
	}
	return found, nil
}

// listAvailableLocations returns the locations serviceName reports itself
// available at, with the topology of the Location each record names.
//
// A control plane serves availability records for every service, so filtering
// on the service is what makes the answer compute's rather than the platform's.
//
// The Locations are listed once and indexed rather than fetched one at a time:
// a record per service per location makes the per-record read the expensive
// shape. A record naming a Location that is not there is skipped, not failed —
// the two objects are written by different services, and a project that can
// read one but not the other must still see the locations it can. A KIND that
// is not served is the opposite case and fails: see ErrAvailabilityNotServed.
func listAvailableLocations(ctx context.Context, c client.Client, serviceName string) ([]PlacementLocation, error) {
	var availability servicesv1alpha1.ServiceAvailabilityList
	if err := c.List(ctx, &availability); err != nil {
		if kindNotInstalled(err) {
			return nil, fmt.Errorf("%w: %s is not served here: %w",
				ErrAvailabilityNotServed, "ServiceAvailability", err)
		}
		return nil, fmt.Errorf("failed to list service availability: %w", err)
	}

	var list locationsv1alpha1.LocationList
	if err := c.List(ctx, &list); err != nil {
		if kindNotInstalled(err) {
			return nil, fmt.Errorf("%w: %s is not served here: %w",
				ErrAvailabilityNotServed, "Location", err)
		}
		return nil, fmt.Errorf("failed to list locations: %w", err)
	}

	byName := make(map[string]*locationsv1alpha1.Location, len(list.Items))
	for i := range list.Items {
		byName[list.Items[i].Name] = &list.Items[i]
	}

	found := make([]PlacementLocation, 0, len(availability.Items))
	seen := sets.Set[string]{}
	for i := range availability.Items {
		record := &availability.Items[i]
		if record.Spec.ServiceRef.Name != serviceName {
			continue
		}
		if !apimeta.IsStatusConditionTrue(record.Status.Conditions, conditionAvailable) {
			continue
		}
		location, ok := byName[record.Spec.LocationRef.Name]
		if !ok || seen.Has(location.Name) {
			continue
		}
		seen.Insert(location.Name)
		found = append(found, PlacementLocation{
			Name:     location.Name,
			Topology: location.Spec.Topology,
		})
	}
	return found, nil
}

// ListServingLocations returns the locations delivered to a cell.
func ListServingLocations(ctx context.Context, c client.Client, source Source) ([]ServingLocation, error) {
	resolved, err := source.Resolve()
	if err != nil {
		return nil, err
	}

	if resolved == SourceNetworkServices {
		var list networkingv1alpha.ServingLocationList
		if err := c.List(ctx, &list); err != nil {
			return nil, fmt.Errorf("failed to list serving locations: %w", err)
		}

		found := make([]ServingLocation, 0, len(list.Items))
		for _, servingLocation := range list.Items {
			found = append(found, ServingLocation{
				Name:     servingLocation.Name,
				Topology: servingLocation.Spec.Topology,
			})
		}
		return found, nil
	}

	var list locationsv1alpha1.ServingLocationList
	if err := c.List(ctx, &list); err != nil {
		if kindNotInstalled(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to list serving locations: %w", err)
	}

	found := make([]ServingLocation, 0, len(list.Items))
	for _, servingLocation := range list.Items {
		found = append(found, ServingLocation{
			Name:     servingLocation.Name,
			Topology: servingLocation.Spec.Topology,
		})
	}
	return found, nil
}

// ServingLocationObject returns the object a controller watches to learn that
// a cell has been told where it sits. Availability records say nothing about
// cells, so that source watches the locations service's kind.
func ServingLocationObject(source Source) (client.Object, error) {
	resolved, err := source.Resolve()
	if err != nil {
		return nil, err
	}

	if resolved == SourceNetworkServices {
		return &networkingv1alpha.ServingLocation{}, nil
	}
	return &locationsv1alpha1.ServingLocation{}, nil
}

// ServingLocationGVK returns the kind a controller watches for the source.
func ServingLocationGVK(source Source) (schema.GroupVersionKind, error) {
	resolved, err := source.Resolve()
	if err != nil {
		return schema.GroupVersionKind{}, err
	}

	if resolved == SourceNetworkServices {
		return networkingv1alpha.GroupVersion.WithKind("ServingLocation"), nil
	}
	return locationsv1alpha1.GroupVersion.WithKind("ServingLocation"), nil
}

// EnsureServingLocationKind fails unless the control plane serves the kind the
// source watches.
//
// A watch is not a read: a list against a kind the control plane does not serve
// degrades to no locations, but a watch cannot, and registering one wedges the
// manager during cache sync. Refusing to start says which CRD is missing, where
// a wedged manager says nothing.
//
// This gates only the selected source. A deployment reading network services
// must not be made to depend on the locations service being installed.
func EnsureServingLocationKind(mapper apimeta.RESTMapper, source Source) error {
	gvk, err := ServingLocationGVK(source)
	if err != nil {
		return err
	}

	if _, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		if kindNotInstalled(err) {
			resolved, _ := source.Resolve()
			return fmt.Errorf(
				"locationSource %q watches %s, which this control plane does not serve: "+
					"install the %s CustomResourceDefinition, or set locationSource to %q",
				resolved, gvk, crdName(gvk), otherSource(resolved))
		}
		return fmt.Errorf("failed to determine whether %s is served: %w", gvk, err)
	}

	return nil
}

func crdName(gvk schema.GroupVersionKind) string {
	return fmt.Sprintf("%ss.%s", strings.ToLower(gvk.Kind), gvk.Group)
}

// otherSource names a source that watches a different ServingLocation kind, so
// the error can suggest one worth trying. Only network services serves its own
// kind; every other source reads the locations service's.
func otherSource(source Source) Source {
	if source == SourceNetworkServices {
		return SourceLocations
	}
	return SourceNetworkServices
}

// CityCodes returns the cities the given locations serve.
func CityCodes(found []PlacementLocation) sets.Set[string] {
	codes := sets.Set[string]{}
	for _, location := range found {
		if code, ok := location.CityCode(); ok {
			codes.Insert(code)
		}
	}
	return codes
}

// kindNotInstalled reports whether a list failed because the control plane
// does not serve the kind. A control plane is only expected to serve the kinds
// its consumers read, so a kind that is not there reads as empty rather than
// failing the caller.
//
// The REST mapper answers first. A typed client reaches it through discovery,
// so the no-match arrives wrapped in an ErrResourceDiscoveryFailed rather than
// bare, and only errors.Is unwrapping finds it. A mapper still holding the kind
// from before the CRD went away leaves the API server to answer, with a 404. A
// type missing from the scheme is neither of these: it is a wiring mistake, and
// must keep surfacing as one.
func kindNotInstalled(err error) bool {
	return apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err)
}
