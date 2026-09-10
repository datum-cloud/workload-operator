// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.datum.net/compute/internal/locations"
	"go.datum.net/compute/internal/quotaview"
	"go.datum.net/compute/internal/validation"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

// The discovery tools answer what a project MAY deploy, where the diagnostic
// tools answer what it HAS deployed.
//
// They exist because an assistant asked to write a Workload otherwise invents
// the three fields it cannot guess — a location, a network, an instance type —
// and invents them plausibly. A workload naming a location the project is not
// entitled to, or a size the API will not take, fails after the customer has
// been told it was written correctly. Every one of these is read from the
// project itself, so the answer is what that project will actually accept.
//
// All four are read-only.
const (
	ToolLocationsList     = "compute_locations_list"
	ToolNetworksList      = "compute_networks_list"
	ToolQuotaGet          = "compute_quota_get"
	ToolInstanceTypesList = "compute_instance_types_list"
)

// Discoverer reads what a project is entitled to deploy.
//
// Separate from Reader rather than folded into it: Reader is scoped to the
// compute objects a diagnosis walks, and these three reads reach other API
// groups entirely. Keeping them apart means a caller that only diagnoses need
// not be given the wiring for a locations or quota read it will never do.
//
// Like Reader, whoever constructs one decides the identity its reads run under.
type Discoverer interface {
	// ListPlacementLocations returns the locations the project may place
	// workloads at. Not namespaced: entitlement is a property of the project.
	ListPlacementLocations(ctx context.Context) ([]locations.PlacementLocation, error)
	// ListNetworks returns the Networks in the namespace.
	ListNetworks(ctx context.Context, namespace string) ([]networkingv1alpha.Network, error)
	// GetQuota returns the project's compute quota, one row per resource type.
	GetQuota(ctx context.Context) ([]quotaview.QuotaRow, error)
}

// ClientDiscoverer implements Discoverer against a controller-runtime client.
type ClientDiscoverer struct {
	// Client reads the project, with whatever credentials it carries.
	Client client.Client

	// PlatformClient supplies display metadata for quota rows, and may be nil.
	// A server that reads only as the person who asked holds no platform
	// credential of its own; the numbers are read from the project either way,
	// and only the unit labels fall back to a generic form without it.
	PlatformClient client.Client
}

var _ Discoverer = (*ClientDiscoverer)(nil)

// NewClientDiscoverer returns a Discoverer backed by c.
func NewClientDiscoverer(c client.Client) *ClientDiscoverer {
	return &ClientDiscoverer{Client: c}
}

// ListPlacementLocations reads compute's own availability records. There is no
// choice of source here: what an assistant needs is where compute is offered
// and this project can use it, and only the availability records say that. The
// manager still reads placement per its own configuration; this is the answer a
// customer is given, and it is the same one wherever they ask.
func (d *ClientDiscoverer) ListPlacementLocations(ctx context.Context) ([]locations.PlacementLocation, error) {
	found, err := locations.ListPlacementLocations(ctx, d.Client, locations.SourceServiceAvailability)
	if err != nil {
		// A project that cannot answer the question at all must not be
		// reported as a project with nowhere to run: the first is a deployment
		// to fix, the second is a wait, and an assistant told the wrong one
		// sends the customer to argue with the wrong people. The kind travels
		// as evidence inside the wrapped error, the sentence does not lean on
		// it, and the blame is placed where the fix is.
		if errors.Is(err, locations.ErrAvailabilityNotServed) {
			return nil, fmt.Errorf(
				"compute could not read where it is offered from this project, because the service "+
					"that publishes availability is not reachable here. This is a problem with how "+
					"Datum is deployed for this project, not with the workload and not with the "+
					"person who asked: nothing in a workload can be changed to fix it, and "+
					"re-authenticating will not help. Underlying detail, for whoever operates "+
					"Datum: %w", err)
		}
		return nil, fmt.Errorf("listing the locations this project may place at: %w", err)
	}
	return found, nil
}

func (d *ClientDiscoverer) ListNetworks(ctx context.Context, namespace string) ([]networkingv1alpha.Network, error) {
	var list networkingv1alpha.NetworkList
	if err := d.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing networks in %s: %w", namespace, err)
	}
	return list.Items, nil
}

func (d *ClientDiscoverer) GetQuota(ctx context.Context) ([]quotaview.QuotaRow, error) {
	rows, err := quotaview.ListComputeQuota(ctx, d.Client, d.PlatformClient)
	if err != nil {
		return nil, fmt.Errorf("reading this project's compute quota: %w", err)
	}
	return rows, nil
}

// ---------------------------------------------------------------- I/O types

// LocationView is one location a project may place at.
type LocationView struct {
	Name string `json:"name"`
	// CityCode is the city the location serves, e.g. "DFW". Empty when the
	// location declares none, which is worth reporting rather than hiding: a
	// placement that names a city cannot be satisfied by such a location.
	CityCode string `json:"cityCode,omitempty"`
	// DisplayName is the human-readable label, when the location carries one.
	DisplayName string `json:"displayName,omitempty"`
	// Topology is the full set of attributes the location declares, city code
	// included, so a placement can be matched on more than the city once more
	// attributes are published.
	Topology map[string]string `json:"topology,omitempty"`
}

// NetworkView is one network a workload's instances can attach to.
type NetworkView struct {
	Name       string   `json:"name"`
	IPFamilies []string `json:"ipFamilies,omitempty"`
	// Ready reports whether the network holds everything it needs to be used.
	Ready bool `json:"ready"`
	// Reason says why, when it is not ready.
	Reason string `json:"reason,omitempty"`
}

// InstanceTypeView is one instance type a Workload may ask for.
type InstanceTypeView struct {
	Name string `json:"name"`
	// VCPU is how many virtual CPUs the type provides. Fractional, because the
	// size is stored in thousandths and a future type need not be a whole one.
	VCPU float64 `json:"vcpu"`
	// MemoryMiB is the RAM the type provides, in mebibytes.
	MemoryMiB int64 `json:"memoryMiB"`
	// Default marks the type to use when the customer expressed no preference.
	Default bool `json:"default"`
}

// LocationsListInput takes no arguments: the project is fixed by the request.
type LocationsListInput struct{}

// LocationsListOutput is every location the project may place at.
type LocationsListOutput struct {
	Locations []LocationView `json:"locations"`
}

// NetworksListInput takes no arguments.
type NetworksListInput struct{}

// NetworksListOutput is every network in the project.
type NetworksListOutput struct {
	Networks []NetworkView `json:"networks"`
}

// QuotaGetInput takes no arguments.
type QuotaGetInput struct{}

// QuotaGetOutput is the project's compute quota, one row per resource type.
// The rows are quotaview's own, so what an assistant reports and what
// `datumctl compute quota` prints cannot drift apart.
type QuotaGetOutput struct {
	Resources []quotaview.QuotaRow `json:"resources"`
}

// InstanceTypesListInput takes no arguments.
type InstanceTypesListInput struct{}

// InstanceTypesListOutput is the catalog of instance types.
type InstanceTypesListOutput struct {
	InstanceTypes []InstanceTypeView `json:"instanceTypes"`
}

// ------------------------------------------------------------ registration

// RegisterDiscoveryTools adds the tools that answer what a project may deploy.
// Called by RegisterTools; separate so the set can be read on its own.
func RegisterDiscoveryTools(s *mcp.Server, deps DepsFor) {
	mcp.AddTool(s, &mcp.Tool{
		Name:  ToolLocationsList,
		Title: "List locations",
		Description: "List the locations where compute is offered and this project can use it, each with " +
			"its city code (e.g. \"DFW\") and the attributes it declares. The list is derived from " +
			"compute's own availability records, so it is where compute is actually running, not where " +
			"it might be: a location missing from this list is one compute is not offered in, and a " +
			"placement naming it will never come up. Call this before writing a Workload's placements " +
			"rather than guessing a city. Read-only.",
	}, locationsList(deps))

	mcp.AddTool(s, &mcp.Tool{
		Name:  ToolNetworksList,
		Title: "List networks",
		Description: "List the networks in this project, each with its IP families and whether it is ready " +
			"to use. Every Workload attaches its instances to a network by name, so a draft needs one; " +
			"\"default\" is the conventional name and is what a project normally has. A network that is " +
			"not ready will hold new instances back, and its reason says what it is waiting on. Read-only.",
	}, networksList(deps))

	mcp.AddTool(s, &mcp.Tool{
		Name:  ToolQuotaGet,
		Title: "Get compute quota",
		Description: "Report this project's compute quota: for each resource type — workloads, instances, " +
			"vCPUs, memory — the limit, how much is already in use, and how much is left. Call it before " +
			"proposing a replica count or an instance size, so the workload fits in what the project has, " +
			"and call it when something reports QuotaExceeded to see how much room there actually is. " +
			"Read-only.",
	}, quotaGet(deps))

	mcp.AddTool(s, &mcp.Tool{
		Name:  ToolInstanceTypesList,
		Title: "List instance types",
		Description: "List the instance types a Workload may ask for, with the vCPU and memory each one " +
			"provides and which is the default. Only these names are accepted — a Workload naming any " +
			"other is rejected the moment it is submitted, so never invent a size. Read-only.",
	}, instanceTypesList(deps))
}

// ---------------------------------------------------------------- handlers

func locationsList(deps DepsFor) mcp.ToolHandlerFor[LocationsListInput, LocationsListOutput] {
	return func(
		ctx context.Context, _ *mcp.CallToolRequest, _ LocationsListInput,
	) (*mcp.CallToolResult, LocationsListOutput, error) {
		d, err := deps(ctx)
		if err != nil {
			return nil, LocationsListOutput{}, err
		}
		disc, err := d.discoverer()
		if err != nil {
			return nil, LocationsListOutput{}, err
		}

		found, err := disc.ListPlacementLocations(ctx)
		if err != nil {
			return nil, LocationsListOutput{}, err
		}

		out := LocationsListOutput{Locations: make([]LocationView, 0, len(found))}
		for _, location := range found {
			code, _ := location.CityCode()
			out.Locations = append(out.Locations, LocationView{
				Name:     location.Name,
				CityCode: code,
				Topology: location.Topology,
			})
		}
		// By name, so two calls in one conversation read the same way.
		sort.Slice(out.Locations, func(i, j int) bool {
			return out.Locations[i].Name < out.Locations[j].Name
		})
		return nil, out, nil
	}
}

func networksList(deps DepsFor) mcp.ToolHandlerFor[NetworksListInput, NetworksListOutput] {
	return func(
		ctx context.Context, _ *mcp.CallToolRequest, _ NetworksListInput,
	) (*mcp.CallToolResult, NetworksListOutput, error) {
		d, err := deps(ctx)
		if err != nil {
			return nil, NetworksListOutput{}, err
		}
		disc, err := d.discoverer()
		if err != nil {
			return nil, NetworksListOutput{}, err
		}

		found, err := disc.ListNetworks(ctx, d.Namespace)
		if err != nil {
			return nil, NetworksListOutput{}, err
		}

		out := NetworksListOutput{Networks: make([]NetworkView, 0, len(found))}
		for i := range found {
			n := &found[i]
			view := NetworkView{Name: n.Name}
			for _, family := range n.Spec.IPFamilies {
				view.IPFamilies = append(view.IPFamilies, string(family))
			}
			// A network with no Ready condition at all has not been looked at
			// yet, which reads as not ready with nothing to say about why.
			if ready := apimeta.FindStatusCondition(n.Status.Conditions, networkingv1alpha.NetworkReady); ready != nil {
				view.Ready = ready.Status == metav1.ConditionTrue
				if !view.Ready {
					view.Reason = ready.Reason
				}
			}
			out.Networks = append(out.Networks, view)
		}
		sort.Slice(out.Networks, func(i, j int) bool {
			return out.Networks[i].Name < out.Networks[j].Name
		})
		return nil, out, nil
	}
}

func quotaGet(deps DepsFor) mcp.ToolHandlerFor[QuotaGetInput, QuotaGetOutput] {
	return func(
		ctx context.Context, _ *mcp.CallToolRequest, _ QuotaGetInput,
	) (*mcp.CallToolResult, QuotaGetOutput, error) {
		d, err := deps(ctx)
		if err != nil {
			return nil, QuotaGetOutput{}, err
		}
		disc, err := d.discoverer()
		if err != nil {
			return nil, QuotaGetOutput{}, err
		}

		rows, err := disc.GetQuota(ctx)
		if err != nil {
			return nil, QuotaGetOutput{}, err
		}
		// An empty result is "no quota is configured", not an error: the tool
		// says so by returning an empty list rather than a null one.
		if rows == nil {
			rows = []quotaview.QuotaRow{}
		}
		return nil, QuotaGetOutput{Resources: rows}, nil
	}
}

// instanceTypesList reads only the catalog, but still resolves deps for the
// same reason reasonExplain does: an unauthenticated caller must not be able to
// use it to probe the server.
func instanceTypesList(deps DepsFor) mcp.ToolHandlerFor[InstanceTypesListInput, InstanceTypesListOutput] {
	return func(
		ctx context.Context, _ *mcp.CallToolRequest, _ InstanceTypesListInput,
	) (*mcp.CallToolResult, InstanceTypesListOutput, error) {
		if _, err := deps(ctx); err != nil {
			return nil, InstanceTypesListOutput{}, err
		}
		return nil, InstanceTypesListOutput{InstanceTypes: Catalog()}, nil
	}
}

// ----------------------------------------------------------------- helpers

// discoverer returns the Discoverer for this call, or an error naming the
// wiring mistake. A nil one is a server that was built without discovery, and
// saying so beats a nil dereference in a handler.
func (d ToolDeps) discoverer() (Discoverer, error) {
	if d.Discoverer == nil {
		return nil, fmt.Errorf(
			"this server was built without the ability to read what the project may deploy, so this " +
				"tool cannot answer. The person who asked did nothing wrong: whoever operates this " +
				"server needs to configure it")
	}
	return d.Discoverer, nil
}

// instanceTypeSize is the vCPU and memory one instance type provides.
type instanceTypeSize struct {
	// CPUMillicores is thousandths of a vCPU: 1000 is one.
	CPUMillicores int64
	MemoryMiB     int64
}

// instanceTypeSizes gives the size behind each supported instance type name.
//
// The names come from internal/validation, which is what actually accepts or
// rejects a Workload, so this table can never offer a type the API would turn
// down. The sizes are the platform-declared ones, duplicated here from the
// instance controller's own accounting table.
//
// TODO(#137): both halves belong in one served catalog. Until there is one,
// a new instance type has to be added in three places — validation, the
// controller's accounting, and here — and a type missing from this table is
// reported with no size rather than being silently dropped.
var instanceTypeSizes = map[string]instanceTypeSize{
	"datumcloud/d1-standard-2": {CPUMillicores: 1000, MemoryMiB: 2048},
}

// Catalog returns the instance types a Workload may ask for, in offer order.
// The first is the default: validation accepts exactly one type today, and the
// order it lists them in is the order to prefer them.
func Catalog() []InstanceTypeView {
	supported := validation.SupportedInstanceTypes()
	out := make([]InstanceTypeView, 0, len(supported))
	for i, name := range supported {
		size := instanceTypeSizes[name]
		out = append(out, InstanceTypeView{
			Name:      name,
			VCPU:      float64(size.CPUMillicores) / 1000,
			MemoryMiB: size.MemoryMiB,
			Default:   i == 0,
		})
	}
	return out
}
