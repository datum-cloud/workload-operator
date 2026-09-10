package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"go.datum.net/compute/internal/locations"
	"go.datum.net/compute/internal/quotaview"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	locationsv1alpha1 "go.miloapis.com/locations/api/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// fakeDiscoverer serves canned entitlement facts so the discovery tools can be
// exercised without a cluster.
type fakeDiscoverer struct {
	locations []locations.PlacementLocation
	networks  []networkingv1alpha.Network
	quota     []quotaview.QuotaRow
	err       error
}

var _ Discoverer = (*fakeDiscoverer)(nil)

func (f *fakeDiscoverer) ListPlacementLocations(context.Context) ([]locations.PlacementLocation, error) {
	return f.locations, f.err
}

func (f *fakeDiscoverer) ListNetworks(context.Context, string) ([]networkingv1alpha.Network, error) {
	return f.networks, f.err
}

func (f *fakeDiscoverer) GetQuota(context.Context) ([]quotaview.QuotaRow, error) {
	return f.quota, f.err
}

// fixtureDiscoverer covers the shapes worth distinguishing: a location that
// declares a city and one that does not, a ready network and one that is still
// waiting, and quota with room in one dimension and none in another.
func fixtureDiscoverer() *fakeDiscoverer {
	return &fakeDiscoverer{
		// Deliberately out of alphabetical order, so the sort is proven.
		locations: []locations.PlacementLocation{
			{Name: locDFW, Topology: map[string]string{locations.TopologyCityCodeKey: cityDFW}},
			{Name: "eu-west-ams", Topology: map[string]string{locations.TopologyCityCodeKey: cityAMS}},
			{Name: "no-city", Topology: map[string]string{"topology.datum.net/region": "unknown"}},
		},
		networks: []networkingv1alpha.Network{
			network("staging", []networkingv1alpha.IPFamily{networkingv1alpha.IPv6Protocol},
				metav1.Condition{
					Type:   networkingv1alpha.NetworkReady,
					Status: metav1.ConditionFalse,
					Reason: networkingv1alpha.NetworkReasonProjectNamespaceNotFound,
				}),
			network("default",
				[]networkingv1alpha.IPFamily{networkingv1alpha.IPv4Protocol, networkingv1alpha.IPv6Protocol},
				metav1.Condition{
					Type:   networkingv1alpha.NetworkReady,
					Status: metav1.ConditionTrue,
					Reason: networkingv1alpha.NetworkReadyReasonReady,
				}),
		},
		quota: []quotaview.QuotaRow{
			{ResourceType: "compute.datumapis.com/workloads", DisplayName: "Workloads", Unit: "workloads",
				Limit: 10, Used: 3, Available: 7},
			{ResourceType: "compute.datumapis.com/vcpus", DisplayName: "vCPUs", Unit: "vCPUs",
				Limit: 8, Used: 8, Available: 0},
		},
	}
}

func network(name string, families []networkingv1alpha.IPFamily, conditions ...metav1.Condition) networkingv1alpha.Network {
	return networkingv1alpha.Network{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       networkingv1alpha.NetworkSpec{IPFamilies: families},
		Status:     networkingv1alpha.NetworkStatus{Conditions: conditions},
	}
}

// discoveryDeps supplies both halves, since RegisterTools registers every tool
// against one DepsFor.
func discoveryDeps(d Discoverer) DepsFor {
	return func(context.Context) (ToolDeps, error) {
		return ToolDeps{Reader: fixtureReader(), Discoverer: d, Namespace: testNamespace}, nil
	}
}

func TestLocationsListReportsCityCodesAndSortsByName(t *testing.T) {
	deps := discoveryDeps(fixtureDiscoverer())

	_, out, err := locationsList(deps)(context.Background(), nil, LocationsListInput{})
	if err != nil {
		t.Fatalf("compute_locations_list: %v", err)
	}
	if len(out.Locations) != 3 {
		t.Fatalf("got %d locations, want 3", len(out.Locations))
	}

	wantOrder := []string{"eu-west-ams", "no-city", locDFW}
	for i, want := range wantOrder {
		if got := out.Locations[i].Name; got != want {
			t.Errorf("locations[%d] = %q, want %q (the list must be sorted by name)", i, got, want)
		}
	}

	byName := make(map[string]LocationView, len(out.Locations))
	for _, l := range out.Locations {
		byName[l.Name] = l
	}
	if got := byName[locDFW].CityCode; got != cityDFW {
		t.Errorf("us-south-dfw cityCode = %q, want %q", got, cityDFW)
	}
	// A location with no city is reported rather than dropped: a placement that
	// names a city cannot be satisfied by it, and the model needs to see that.
	if got := byName["no-city"].CityCode; got != "" {
		t.Errorf("no-city cityCode = %q, want empty", got)
	}
	if len(byName["no-city"].Topology) == 0 {
		t.Error("no-city lost its topology; the attributes are what is left to match on")
	}
}

func TestNetworksListReportsReadinessAndFamilies(t *testing.T) {
	deps := discoveryDeps(fixtureDiscoverer())

	_, out, err := networksList(deps)(context.Background(), nil, NetworksListInput{})
	if err != nil {
		t.Fatalf("compute_networks_list: %v", err)
	}
	if len(out.Networks) != 2 {
		t.Fatalf("got %d networks, want 2", len(out.Networks))
	}

	// "default" is the one an assistant reaches for, and sorting puts it first.
	first := out.Networks[0]
	if first.Name != "default" {
		t.Fatalf("networks[0] = %q, want default (the list must be sorted by name)", first.Name)
	}
	if !first.Ready {
		t.Error("default Ready = false, want true")
	}
	if first.Reason != "" {
		t.Errorf("default reason = %q, want empty on a ready network", first.Reason)
	}
	if strings.Join(first.IPFamilies, ",") != "IPv4,IPv6" {
		t.Errorf("default ipFamilies = %v, want [IPv4 IPv6]", first.IPFamilies)
	}

	second := out.Networks[1]
	if second.Ready {
		t.Error("staging Ready = true, want false")
	}
	// The reason is the whole point of reporting an unready network: it says
	// what the network is waiting on.
	if second.Reason != networkingv1alpha.NetworkReasonProjectNamespaceNotFound {
		t.Errorf("staging reason = %q, want %q", second.Reason,
			networkingv1alpha.NetworkReasonProjectNamespaceNotFound)
	}
}

// TestNetworksListTreatsAnUnreportedNetworkAsNotReady pins the safe default: a
// network nothing has looked at yet must not read as usable.
func TestNetworksListTreatsAnUnreportedNetworkAsNotReady(t *testing.T) {
	d := &fakeDiscoverer{networks: []networkingv1alpha.Network{
		network("fresh", []networkingv1alpha.IPFamily{networkingv1alpha.IPv6Protocol}),
	}}

	_, out, err := networksList(discoveryDeps(d))(context.Background(), nil, NetworksListInput{})
	if err != nil {
		t.Fatalf("compute_networks_list: %v", err)
	}
	if len(out.Networks) != 1 || out.Networks[0].Ready {
		t.Errorf("networks = %+v, want one network reported not ready", out.Networks)
	}
}

func TestQuotaGetReturnsEveryResourceType(t *testing.T) {
	deps := discoveryDeps(fixtureDiscoverer())

	_, out, err := quotaGet(deps)(context.Background(), nil, QuotaGetInput{})
	if err != nil {
		t.Fatalf("compute_quota_get: %v", err)
	}
	if len(out.Resources) != 2 {
		t.Fatalf("got %d resources, want 2", len(out.Resources))
	}
	// Order is the caller's, not re-sorted here: quotaview already returns the
	// rows in the order a person reads them.
	if got := out.Resources[0].ResourceType; got != "compute.datumapis.com/workloads" {
		t.Errorf("resources[0] = %q, want the workloads row first", got)
	}
	exhausted := out.Resources[1]
	if exhausted.Available != 0 || exhausted.Used != exhausted.Limit {
		t.Errorf("vCPU row = %+v, want a row with nothing available", exhausted)
	}
}

// TestQuotaGetReturnsAnEmptyListWhenNoQuotaIsConfigured keeps "no quota is set
// up" from arriving as a null the model has to interpret.
func TestQuotaGetReturnsAnEmptyListWhenNoQuotaIsConfigured(t *testing.T) {
	_, out, err := quotaGet(discoveryDeps(&fakeDiscoverer{}))(context.Background(), nil, QuotaGetInput{})
	if err != nil {
		t.Fatalf("compute_quota_get: %v", err)
	}
	if out.Resources == nil {
		t.Fatal("Resources is nil; an empty project must return an empty list")
	}
	if len(out.Resources) != 0 {
		t.Errorf("Resources = %+v, want empty", out.Resources)
	}
}

func TestInstanceTypesListOffersOnlyWhatValidationAccepts(t *testing.T) {
	deps := discoveryDeps(fixtureDiscoverer())

	_, out, err := instanceTypesList(deps)(context.Background(), nil, InstanceTypesListInput{})
	if err != nil {
		t.Fatalf("compute_instance_types_list: %v", err)
	}
	if len(out.InstanceTypes) == 0 {
		t.Fatal("no instance types offered; a model with no catalog invents one")
	}

	var defaults int
	for _, it := range out.InstanceTypes {
		if it.Default {
			defaults++
		}
		// A type with no size is worse than useless: it invites a replica count
		// chosen against nothing.
		if it.VCPU <= 0 || it.MemoryMiB <= 0 {
			t.Errorf("%s = %g vCPU / %d MiB, want a real size", it.Name, it.VCPU, it.MemoryMiB)
		}
	}
	if defaults != 1 {
		t.Errorf("got %d default instance types, want exactly 1", defaults)
	}

	// The one supported type today, with the sizing quota is accounted against.
	first := out.InstanceTypes[0]
	if first.Name != "datumcloud/d1-standard-2" || first.VCPU != 1 || first.MemoryMiB != 2048 {
		t.Errorf("first type = %+v, want datumcloud/d1-standard-2 at 1 vCPU / 2048 MiB", first)
	}
}

// TestDiscoveryToolsFailWhenDepsAreUnavailable covers the path every handler
// shares: an unauthenticated or misconfigured caller must be turned away before
// any read, so no tool can be used to probe the server.
func TestDiscoveryToolsFailWhenDepsAreUnavailable(t *testing.T) {
	wantErr := errors.New("no credentials on this request")
	deps := DepsFor(func(context.Context) (ToolDeps, error) { return ToolDeps{}, wantErr })
	ctx := context.Background()

	calls := map[string]func() error{
		ToolLocationsList: func() error {
			_, _, err := locationsList(deps)(ctx, nil, LocationsListInput{})
			return err
		},
		ToolNetworksList: func() error {
			_, _, err := networksList(deps)(ctx, nil, NetworksListInput{})
			return err
		},
		ToolQuotaGet: func() error {
			_, _, err := quotaGet(deps)(ctx, nil, QuotaGetInput{})
			return err
		},
		// Answerable from the catalog alone, and still refused.
		ToolInstanceTypesList: func() error {
			_, _, err := instanceTypesList(deps)(ctx, nil, InstanceTypesListInput{})
			return err
		},
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, wantErr) {
			t.Errorf("%s error = %v, want the deps error to surface unchanged", name, err)
		}
	}
}

// TestDiscoveryToolsExplainAMissingDiscoverer covers a server wired for
// diagnosis only: the tools must name the wiring gap rather than panic.
func TestDiscoveryToolsExplainAMissingDiscoverer(t *testing.T) {
	deps := func(context.Context) (ToolDeps, error) {
		return ToolDeps{Reader: fixtureReader(), Namespace: testNamespace}, nil
	}
	ctx := context.Background()

	if _, _, err := locationsList(deps)(ctx, nil, LocationsListInput{}); err == nil {
		t.Error("compute_locations_list succeeded with no Discoverer, want an error")
	}
	if _, _, err := networksList(deps)(ctx, nil, NetworksListInput{}); err == nil {
		t.Error("compute_networks_list succeeded with no Discoverer, want an error")
	}
	if _, _, err := quotaGet(deps)(ctx, nil, QuotaGetInput{}); err == nil {
		t.Error("compute_quota_get succeeded with no Discoverer, want an error")
	}
	// compute_instance_types_list reads no project state, so it still answers.
	if _, _, err := instanceTypesList(deps)(ctx, nil, InstanceTypesListInput{}); err != nil {
		t.Errorf("compute_instance_types_list: %v, want the catalog to answer without a Discoverer", err)
	}
}

// TestDiscoveryToolsAnswerOverTheWire proves registration, not just the
// handlers: a tool that is never wired into RegisterTools passes every unit
// test above and is uncallable in production.
func TestDiscoveryToolsAnswerOverTheWire(t *testing.T) {
	ctx := context.Background()

	server := mcp.NewServer(&mcp.Implementation{Name: testServerName, Version: testImplVersion}, nil)
	RegisterTools(server, discoveryDeps(fixtureDiscoverer()))

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connecting server: %v", err)
	}
	defer func() { _ = serverSession.Close() }()

	client := mcp.NewClient(&mcp.Implementation{Name: testClientName, Version: testImplVersion}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connecting client: %v", err)
	}
	defer func() { _ = clientSession.Close() }()

	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      ToolLocationsList,
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("calling %s: %v", ToolLocationsList, err)
	}
	if res.IsError {
		t.Fatalf("%s returned an error result: %+v", ToolLocationsList, res.Content)
	}

	// Round-tripped through the wire's JSON, so the output schema is exercised
	// as the model would receive it.
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshalling structured content: %v", err)
	}
	var out LocationsListOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding %s output: %v", ToolLocationsList, err)
	}
	if len(out.Locations) != 3 {
		t.Fatalf("got %d locations over the wire, want 3: %s", len(out.Locations), raw)
	}
	if out.Locations[0].CityCode != cityAMS {
		t.Errorf("locations[0].cityCode = %q, want the city the location declares", out.Locations[0].CityCode)
	}
}

// TestClientDiscovererReadsComputeAvailability covers the one Discoverer that
// talks to a control plane. The tools above run against a fake, so nothing else
// proves that the locations an assistant is shown are the ones compute reports
// itself available at — not every location the platform has, and not another
// service's.
func TestClientDiscovererReadsComputeAvailability(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := locationsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering locations: %v", err)
	}
	if err := servicesv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering service availability: %v", err)
	}

	location := func(name, cityCode string) *locationsv1alpha1.Location {
		return &locationsv1alpha1.Location{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: locationsv1alpha1.LocationSpec{
				LocationClassRef: locationsv1alpha1.LocationClassReference{Name: "datum-managed"},
				Topology:         map[string]string{locations.TopologyCityCodeKey: cityCode},
			},
		}
	}
	availability := func(name, service, at string, status metav1.ConditionStatus) *servicesv1alpha1.ServiceAvailability {
		return &servicesv1alpha1.ServiceAvailability{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: servicesv1alpha1.ServiceAvailabilitySpec{
				ServiceRef:  servicesv1alpha1.ServiceRef{Name: service},
				LocationRef: servicesv1alpha1.LocationRef{Name: at},
			},
			Status: servicesv1alpha1.ServiceAvailabilityStatus{
				Conditions: []metav1.Condition{{
					Type:               "Available",
					Status:             status,
					Reason:             "Reported",
					LastTransitionTime: metav1.Now(),
				}},
			},
		}
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			location(locDFW, cityDFW),
			location("eu-west-ams", cityAMS),
			location("us-east-iad", "IAD"),
			availability("compute-dfw", "compute", locDFW, metav1.ConditionTrue),
			// Compute is not up here yet, so it is not somewhere to place.
			availability("compute-ams", "compute", "eu-west-ams", metav1.ConditionFalse),
			// Another service is available at IAD. Compute is not, and the
			// project's control plane carries every service's records.
			availability("dns-iad", "dns", "us-east-iad", metav1.ConditionTrue),
		).
		Build()

	found, err := NewClientDiscoverer(cl).ListPlacementLocations(context.Background())
	if err != nil {
		t.Fatalf("ListPlacementLocations: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("got %d locations %+v, want only the one compute is available at", len(found), found)
	}
	if found[0].Name != locDFW {
		t.Errorf("location = %q, want us-south-dfw", found[0].Name)
	}
	if code, ok := found[0].CityCode(); !ok || code != cityDFW {
		t.Errorf("cityCode = %q (declared %v), want %q from the location itself", code, ok, cityDFW)
	}
}

// TestLocationsListBlamesTheDeploymentWhenAvailabilityIsNotServed is the case
// the empty list must never be given for. A project that cannot be asked where
// compute is offered has to say so: told "no locations", a customer waits for
// Datum to add one, and nobody ever looks at the deployment that is actually
// broken.
func TestLocationsListBlamesTheDeploymentWhenAvailabilityIsNotServed(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := locationsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering locations: %v", err)
	}
	if err := servicesv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering service availability: %v", err)
	}

	// Nothing here serves the availability records, the way a project the
	// service was never installed for behaves.
	notServed := interceptor.Funcs{
		List: func(
			ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption,
		) error {
			if _, ok := list.(*servicesv1alpha1.ServiceAvailabilityList); ok {
				return &apimeta.NoKindMatchError{GroupKind: schema.GroupKind{Kind: "ServiceAvailability"}}
			}
			return c.List(ctx, list, opts...)
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(notServed).Build()
	disc := NewClientDiscoverer(cl)

	found, err := disc.ListPlacementLocations(context.Background())
	if err == nil {
		t.Fatalf("ListPlacementLocations returned %+v and no error; a kind nobody serves must not "+
			"read as a project with nowhere to run", found)
	}
	if !errors.Is(err, locations.ErrAvailabilityNotServed) {
		t.Errorf("error = %v, want it to stay identifiable as the availability read failing", err)
	}

	// The tool has to relay it, not swallow it into an empty list.
	_, out, err := locationsList(discoveryDeps(disc))(context.Background(), nil, LocationsListInput{})
	if err == nil {
		t.Fatalf("%s returned %+v and no error", ToolLocationsList, out)
	}

	// What the customer is told: the deployment is at fault, they are not, and
	// nothing in the wording is Datum's internal vocabulary.
	msg := err.Error()
	for _, want := range []string{"deployed", "not with the person who asked", "re-authenticating will not help"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not say %q; the customer must not be sent to fix their workload", msg, want)
		}
	}
	terms := append(internalVocabulary(), customerFacingOnly()...)
	// "ServiceAvailability" travels as an identifier, as a reason code does.
	checkCopy(t, ToolLocationsList+" not-served error", msg, terms, "ServiceAvailability")
}

// locDFW is the location name the discovery tests place in DFW.
const locDFW = "us-south-dfw"
