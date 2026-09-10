// SPDX-License-Identifier: AGPL-3.0-only

package locations

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	locationsv1alpha1 "go.miloapis.com/locations/api/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	testCityCode      = "DFW"
	testOtherCityCode = "ORD"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	require.NoError(t, networkingv1alpha.AddToScheme(s))
	require.NoError(t, locationsv1alpha1.AddToScheme(s))
	require.NoError(t, servicesv1alpha1.AddToScheme(s))
	return s
}

func newBinding(name, cityCode string) *networkingv1alpha.LocationBinding {
	return &networkingv1alpha.LocationBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: networkingv1alpha.LocationBindingSpec{
			LocationRef: corev1.LocalObjectReference{Name: name},
			Topology:    map[string]string{TopologyCityCodeKey: cityCode},
		},
	}
}

func newLocation(name, cityCode string) *locationsv1alpha1.Location {
	return &locationsv1alpha1.Location{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: locationsv1alpha1.LocationSpec{
			LocationClassRef: locationsv1alpha1.LocationClassReference{Name: "datum-managed"},
			Topology:         map[string]string{TopologyCityCodeKey: cityCode},
		},
	}
}

// newAvailability records serviceName as available, or not, at location.
func newAvailability(name, serviceName, location string, available bool) *servicesv1alpha1.ServiceAvailability {
	status := metav1.ConditionFalse
	if available {
		status = metav1.ConditionTrue
	}
	return &servicesv1alpha1.ServiceAvailability{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: servicesv1alpha1.ServiceAvailabilitySpec{
			ServiceRef:  servicesv1alpha1.ServiceRef{Name: serviceName},
			LocationRef: servicesv1alpha1.LocationRef{Name: location},
		},
		Status: servicesv1alpha1.ServiceAvailabilityStatus{
			Conditions: []metav1.Condition{{
				Type:               conditionAvailable,
				Status:             status,
				Reason:             "Reported",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

// TestTopologyKeysAgreeAcrossSources guards the migration's central assumption:
// a city code means the same thing whichever source served it. If the two
// groups ever disagree, switching sources would silently repoint every
// placement.
func TestTopologyKeysAgreeAcrossSources(t *testing.T) {
	t.Parallel()

	assert.Equal(t, networkingv1alpha.TopologyCityCodeKey, TopologyCityCodeKey)
	assert.Equal(t, networkingv1alpha.ServingLocationTopologyLabel, ServingLocationTopologyLabel)
}

func TestSourceResolve(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		source Source
		want   Source
		wantOK bool
	}{
		{source: "", want: SourceNetworkServices, wantOK: true},
		{source: SourceNetworkServices, want: SourceNetworkServices, wantOK: true},
		{source: SourceLocations, want: SourceLocations, wantOK: true},
		{source: SourceServiceAvailability, want: SourceServiceAvailability, wantOK: true},
		{source: "Nonsense"},
	} {
		resolved, err := tc.source.Resolve()
		if !tc.wantOK {
			require.Error(t, err)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, tc.want, resolved)
	}
}

func TestListPlacementLocations_NetworkServices(t *testing.T) {
	t.Parallel()

	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			newBinding("dfw", testCityCode),
			newBinding("ord", testOtherCityCode),
			// A binding with no city code contributes no placement city.
			&networkingv1alpha.LocationBinding{ObjectMeta: metav1.ObjectMeta{Name: "nowhere"}},
			// The locations service must not be read when network services is
			// selected.
			newLocation("lhr", "LHR"),
		).
		Build()

	found, err := ListPlacementLocations(context.Background(), cl, SourceNetworkServices)
	require.NoError(t, err)
	require.Len(t, found, 3)
	assert.ElementsMatch(t, []string{testCityCode, testOtherCityCode}, CityCodes(found).UnsortedList())
}

func TestListPlacementLocations_Locations(t *testing.T) {
	t.Parallel()

	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			newLocation("dfw", testCityCode),
			newLocation("ord", testOtherCityCode),
			// The network services source must not be read when the locations
			// service is selected.
			newBinding("lhr", "LHR"),
		).
		Build()

	found, err := ListPlacementLocations(context.Background(), cl, SourceLocations)
	require.NoError(t, err)
	require.Len(t, found, 2)
	assert.ElementsMatch(t, []string{testCityCode, testOtherCityCode}, CityCodes(found).UnsortedList())
}

// TestListPlacementLocations_ServiceAvailability covers the four shapes a
// control plane actually serves: compute available, compute not available, a
// location another service is available at, and a record whose Location is not
// there. Only the first is placeable.
func TestListPlacementLocations_ServiceAvailability(t *testing.T) {
	t.Parallel()

	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			newLocation("dfw", testCityCode),
			newLocation("ord", testOtherCityCode),
			newLocation("lhr", "LHR"),
			newAvailability("compute-dfw", DefaultServiceName, "dfw", true),
			// Deployed but not yet validated: not somewhere to place.
			newAvailability("compute-ord", DefaultServiceName, "ord", false),
			// Another service is available at lhr; compute is not offered
			// there, and a control plane serves every service's records.
			newAvailability("dns-lhr", "dns", "lhr", true),
			// A record whose Location is gone is skipped, not failed: it
			// carries no topology to place against.
			newAvailability("compute-nowhere", DefaultServiceName, "atl", true),
		).
		Build()

	found, err := ListPlacementLocations(context.Background(), cl, SourceServiceAvailability)
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "dfw", found[0].Name)
	assert.Equal(t, []string{testCityCode}, CityCodes(found).UnsortedList())
}

// TestListPlacementLocations_ServiceAvailabilityNamesTheService proves the
// filter is the service's name and not "whatever is available": the same
// control plane answers differently for a different service.
func TestListPlacementLocations_ServiceAvailabilityNamesTheService(t *testing.T) {
	t.Parallel()

	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			newLocation("dfw", testCityCode),
			newLocation("ord", testOtherCityCode),
			newAvailability("compute-dfw", DefaultServiceName, "dfw", true),
			newAvailability("dns-ord", "dns", "ord", true),
		).
		Build()

	ctx := context.Background()

	found, err := ListPlacementLocationsForService(ctx, cl, SourceServiceAvailability, "dns")
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "ord", found[0].Name)

	found, err = ListPlacementLocationsForService(ctx, cl, SourceServiceAvailability, DefaultServiceName)
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "dfw", found[0].Name)
}

// TestListPlacementLocations_ServiceAvailabilityIgnoresLocationBindings keeps
// the sources apart: an availability read must never fall back to the bindings
// a control plane happens to still carry.
func TestListPlacementLocations_ServiceAvailabilityIgnoresLocationBindings(t *testing.T) {
	t.Parallel()

	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(newBinding("lhr", "LHR"), newLocation("lhr", "LHR")).
		Build()

	found, err := ListPlacementLocations(context.Background(), cl, SourceServiceAvailability)
	require.NoError(t, err)
	assert.Empty(t, found)
}

// TestListPlacementLocations_ServiceAvailabilityFailsWhenNotServed is the
// difference between "compute is offered nowhere" and "nothing looked". Either
// kind missing must fail, and fail identifiably, rather than answer with an
// empty list that reads exactly like a project waiting on Datum.
func TestListPlacementLocations_ServiceAvailabilityFailsWhenNotServed(t *testing.T) {
	t.Parallel()

	// The list object carries no GVK until the client fills it in, so the kind
	// to withhold is matched on the Go type the caller asked for.
	noMatchFor := func(kinds ...string) interceptor.Funcs {
		missing := sets.New(kinds...)
		return interceptor.Funcs{
			List: func(
				ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption,
			) error {
				var kind string
				switch list.(type) {
				case *servicesv1alpha1.ServiceAvailabilityList:
					kind = kindServiceAvailability
				case *locationsv1alpha1.LocationList:
					kind = kindLocation
				}
				if kind != "" && missing.Has(kind) {
					return &apimeta.NoKindMatchError{
						GroupKind: schema.GroupKind{Kind: kind},
					}
				}
				return c.List(ctx, list, opts...)
			},
		}
	}

	for name, missing := range map[string][]string{
		"availability records are not served": {kindServiceAvailability},
		"locations are not served":            {kindLocation},
		"neither is served":                   {kindServiceAvailability, kindLocation},
	} {
		t.Run(name, func(t *testing.T) {
			cl := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithObjects(
					newLocation("dfw", testCityCode),
					newAvailability("compute-dfw", DefaultServiceName, "dfw", true),
				).
				WithInterceptorFuncs(noMatchFor(missing...)).
				Build()

			found, err := ListPlacementLocations(context.Background(), cl, SourceServiceAvailability)
			require.Error(t, err, "a kind nobody serves must never read as no locations")
			assert.ErrorIs(t, err, ErrAvailabilityNotServed)
			assert.Empty(t, found)
		})
	}
}

func TestListPlacementLocations_UnknownSource(t *testing.T) {
	t.Parallel()

	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()

	_, err := ListPlacementLocations(context.Background(), cl, "Nonsense")
	require.Error(t, err)
}

// TestKindNotInstalled_ScopedToLocationsSource pins the degrade to the source
// that needs it. A no-match reads as no locations for the locations service,
// which may not be installed yet, and still fails for network services, whose
// kinds every control plane already serves.
func TestKindNotInstalled_ScopedToLocationsSource(t *testing.T) {
	t.Parallel()

	noMatch := func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
		gvk := list.GetObjectKind().GroupVersionKind()
		return &apimeta.NoKindMatchError{GroupKind: gvk.GroupKind().WithVersion("").GroupKind()}
	}

	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{List: noMatch}).
		Build()

	ctx := context.Background()

	found, err := ListPlacementLocations(ctx, cl, SourceLocations)
	require.NoError(t, err)
	assert.Empty(t, found)

	serving, err := ListServingLocations(ctx, cl, SourceLocations)
	require.NoError(t, err)
	assert.Empty(t, serving)

	_, err = ListPlacementLocations(ctx, cl, SourceNetworkServices)
	require.Error(t, err)

	_, err = ListServingLocations(ctx, cl, SourceNetworkServices)
	require.Error(t, err)
}

// TestListLocations_SchemeMissingStillFails separates a control plane that does
// not serve the kind from a binary that forgot to register it. The first reads
// as empty; the second is a wiring mistake and must keep surfacing.
func TestListLocations_SchemeMissingStillFails(t *testing.T) {
	t.Parallel()

	bare := runtime.NewScheme()
	require.NoError(t, networkingv1alpha.AddToScheme(bare))

	cl := fake.NewClientBuilder().WithScheme(bare).Build()

	_, err := ListPlacementLocations(context.Background(), cl, SourceLocations)
	require.Error(t, err, "an unregistered type is a wiring mistake, not an empty control plane")
}

func TestListServingLocations(t *testing.T) {
	t.Parallel()

	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			&networkingv1alpha.ServingLocation{
				ObjectMeta: metav1.ObjectMeta{Name: "nso-dfw"},
				Spec: networkingv1alpha.ServingLocationSpec{
					Topology: map[string]string{TopologyCityCodeKey: testCityCode},
				},
			},
			&locationsv1alpha1.ServingLocation{
				ObjectMeta: metav1.ObjectMeta{Name: "locations-ord"},
				Spec: locationsv1alpha1.ServingLocationSpec{
					Topology: map[string]string{TopologyCityCodeKey: testOtherCityCode},
				},
			},
		).
		Build()

	ctx := context.Background()

	fromNetworkServices, err := ListServingLocations(ctx, cl, SourceNetworkServices)
	require.NoError(t, err)
	require.Len(t, fromNetworkServices, 1)
	assert.Equal(t, "nso-dfw", fromNetworkServices[0].Name)
	assert.Equal(t, testCityCode, fromNetworkServices[0].CityCode())

	fromLocations, err := ListServingLocations(ctx, cl, SourceLocations)
	require.NoError(t, err)
	require.Len(t, fromLocations, 1)
	assert.Equal(t, "locations-ord", fromLocations[0].Name)
	assert.Equal(t, testOtherCityCode, fromLocations[0].CityCode())

	// An unset source reads what every deployment reads today.
	fromDefault, err := ListServingLocations(ctx, cl, "")
	require.NoError(t, err)
	assert.Equal(t, fromNetworkServices, fromDefault)
}

func TestServingLocationObject(t *testing.T) {
	t.Parallel()

	for _, source := range []Source{"", SourceNetworkServices} {
		object, err := ServingLocationObject(source)
		require.NoError(t, err)
		assert.IsType(t, &networkingv1alpha.ServingLocation{}, object)
	}

	object, err := ServingLocationObject(SourceLocations)
	require.NoError(t, err)
	assert.IsType(t, &locationsv1alpha1.ServingLocation{}, object)

	_, err = ServingLocationObject("Nonsense")
	require.Error(t, err)
}

// TestEnsureServingLocationKind_UnknownSource keeps an unreadable config from
// reaching the REST mapper at all.
func TestEnsureServingLocationKind_UnknownSource(t *testing.T) {
	t.Parallel()

	require.Error(t, EnsureServingLocationKind(apimeta.NewDefaultRESTMapper(nil), "Nonsense"))
}

func TestServingLocationGVK(t *testing.T) {
	t.Parallel()

	for _, source := range []Source{"", SourceNetworkServices} {
		gvk, err := ServingLocationGVK(source)
		require.NoError(t, err)
		assert.Equal(t, networkingv1alpha.GroupVersion.WithKind("ServingLocation"), gvk)
		assert.Equal(t, "servinglocations.networking.datumapis.com", crdName(gvk))
	}

	gvk, err := ServingLocationGVK(SourceLocations)
	require.NoError(t, err)
	assert.Equal(t, locationsv1alpha1.GroupVersion.WithKind("ServingLocation"), gvk)
	assert.Equal(t, "servinglocations.locations.miloapis.com", crdName(gvk))

	_, err = ServingLocationGVK("Nonsense")
	require.Error(t, err)
}

// Kinds the not-served tests withhold from the fake client.
const (
	kindServiceAvailability = "ServiceAvailability"
	kindLocation            = "Location"
)
