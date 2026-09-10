package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.datum.net/compute/internal/locations"
	"go.datum.net/compute/internal/quotaview"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
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
			{Name: "us-south-dfw", Topology: map[string]string{locations.TopologyCityCodeKey: cityDFW}},
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

	wantOrder := []string{"eu-west-ams", "no-city", "us-south-dfw"}
	for i, want := range wantOrder {
		if got := out.Locations[i].Name; got != want {
			t.Errorf("locations[%d] = %q, want %q (the list must be sorted by name)", i, got, want)
		}
	}

	byName := make(map[string]LocationView, len(out.Locations))
	for _, l := range out.Locations {
		byName[l.Name] = l
	}
	if got := byName["us-south-dfw"].CityCode; got != cityDFW {
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
