// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	sigsyaml "sigs.k8s.io/yaml"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/compute/internal/workloadspec"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

// The write path: four tools, of which exactly two can change anything.
//
// It is one operation split into four steps because the model is not the
// person. compute_workload_render turns inputs into a manifest and touches
// nothing. compute_workload_validate asks the server for its verdict on that
// manifest without creating anything. Neither can write, so both are safe to
// run as often as it takes to get the manifest right.
//
// compute_workload_plan and compute_workload_apply are the two that matter.
// Plan returns a canonical manifest and a token that is a hash of it, together
// with the project and the version of the workload the plan saw. Apply takes
// that manifest and that token and re-derives the hash: a manifest edited after
// the plan, a token minted for another project, or a workload someone else
// changed in the meantime all fail to match, and apply refuses rather than
// writing something nobody agreed to. So the only thing that can reach the API
// is the manifest the model already put in front of the person who asked — a
// model that reads a poisoned status message cannot smuggle a different
// workload past a confirmation of this one, and a manifest nobody was shown has
// no token and cannot be applied at all.
const (
	ToolWorkloadRender   = "compute_workload_render"
	ToolWorkloadValidate = "compute_workload_validate"
	ToolWorkloadPlan     = "compute_workload_plan"
	ToolWorkloadApply    = "compute_workload_apply"
)

const (
	// planTokenTTL is how long a plan token stays good. Long enough for the
	// model to show the manifest and the person to read it and answer, short
	// enough that a token cannot outlive the conversation it belongs to.
	planTokenTTL = 15 * time.Minute

	// actionCreate and actionUpdate are the two things an apply can be.
	actionCreate = "create"
	actionUpdate = "update"

	// fieldManifest is the field a rejection names when the manifest could not
	// be read at all, rather than when the server named a path inside it.
	fieldManifest = "manifest"
)

// Writer creates and changes the objects a deployment needs.
//
// Separate from Reader for the reason the interface exists at all: a server
// built for diagnosis is given no Writer, and the write tools then say so
// rather than failing somewhere further down. Like Reader, whoever constructs
// one decides the identity its writes run under — the server builds one per
// request from the caller's own credentials, so a tool call can create nothing
// the person who asked could not create themselves.
type Writer interface {
	// DryRunCreate asks the server to check a create without performing it.
	// The error is the server's own rejection, unwrapped, because its wording
	// and the field path it names are the answer.
	DryRunCreate(ctx context.Context, w *computev1alpha.Workload) error
	// DryRunUpdate asks the server to check an update without performing it.
	DryRunUpdate(ctx context.Context, w *computev1alpha.Workload) error
	// Create creates the workload.
	Create(ctx context.Context, w *computev1alpha.Workload) error
	// Update replaces the workload. The caller sets the resource version it
	// read, so a change made in between is refused by the server.
	Update(ctx context.Context, w *computev1alpha.Workload) error
	// GetNetwork returns one network by name. A missing network is reported as
	// a not-found error, which callers distinguish with apierrors.IsNotFound.
	GetNetwork(ctx context.Context, namespace, name string) (*networkingv1alpha.Network, error)
	// CreateNetwork creates a network.
	CreateNetwork(ctx context.Context, n *networkingv1alpha.Network) error
}

// ClientWriter implements Writer against a controller-runtime client.
type ClientWriter struct {
	// Client writes the project, with whatever credentials it carries.
	Client client.Client
}

var _ Writer = (*ClientWriter)(nil)

// NewClientWriter returns a Writer backed by c. Every write is performed with
// whatever credentials c carries.
func NewClientWriter(c client.Client) *ClientWriter {
	return &ClientWriter{Client: c}
}

// DryRunCreate copies the workload before handing it over: a dry run still
// returns a populated object, and the caller's copy is what the plan token is
// computed over, so it must come back unchanged.
func (w *ClientWriter) DryRunCreate(ctx context.Context, workload *computev1alpha.Workload) error {
	return w.Client.Create(ctx, workload.DeepCopy(), client.DryRunAll)
}

func (w *ClientWriter) DryRunUpdate(ctx context.Context, workload *computev1alpha.Workload) error {
	return w.Client.Update(ctx, workload.DeepCopy(), client.DryRunAll)
}

func (w *ClientWriter) Create(ctx context.Context, workload *computev1alpha.Workload) error {
	if err := w.Client.Create(ctx, workload); err != nil {
		return fmt.Errorf("creating workload %s: %w", workload.Name, err)
	}
	return nil
}

func (w *ClientWriter) Update(ctx context.Context, workload *computev1alpha.Workload) error {
	if err := w.Client.Update(ctx, workload); err != nil {
		return fmt.Errorf("updating workload %s: %w", workload.Name, err)
	}
	return nil
}

func (w *ClientWriter) GetNetwork(
	ctx context.Context, namespace, name string,
) (*networkingv1alpha.Network, error) {
	var n networkingv1alpha.Network
	key := client.ObjectKey{Namespace: namespace, Name: name}
	if err := w.Client.Get(ctx, key, &n); err != nil {
		return nil, fmt.Errorf("getting network %s: %w", name, err)
	}
	return &n, nil
}

func (w *ClientWriter) CreateNetwork(ctx context.Context, n *networkingv1alpha.Network) error {
	if err := w.Client.Create(ctx, n); err != nil {
		return fmt.Errorf("creating network %s: %w", n.Name, err)
	}
	return nil
}

// ---------------------------------------------------------------- I/O types

// RenderPlacement is one group of locations scaled together.
type RenderPlacement struct {
	Name string `json:"name,omitempty" jsonschema:"Placement name, a DNS label. Defaults to \"default\"."`
	// Locations and LocationSelector are the two ways to say where a placement
	// runs, and exactly one of them must be given. The schema says so rather
	// than leaving a model to discover it from a rejection.
	Locations        []string                `json:"locations,omitempty" jsonschema:"Location names this placement runs in, e.g. [\"us-south-dfw-1\"]. Take the names verbatim from compute_locations_list — a name that is not in that list can never be satisfied. Set exactly one of locations or locationSelector."`
	LocationSelector *RenderLocationSelector `json:"locationSelector,omitempty" jsonschema:"Place at every location whose topology matches, instead of naming them. Use this for \"every location in Dallas\" or \"every location in a region\": match on the topology keys compute_locations_list reports, such as topology.datum.net/city-code. New locations matching it are picked up automatically. Set exactly one of locations or locationSelector."`
	MinReplicas      int32                   `json:"minReplicas,omitempty" jsonschema:"Instances to run per placement. At least 1 — there is no scaling to zero — and at most 1000. Defaults to 1."`
}

// RenderLocationSelector is a label selector over location topology, in the
// two forms the API accepts. An empty selector is refused rather than read as
// matching every location.
type RenderLocationSelector struct {
	MatchLabels      map[string]string           `json:"matchLabels,omitempty" jsonschema:"Topology key/value pairs a location must carry, e.g. {\"topology.datum.net/city-code\": \"DFW\"}."`
	MatchExpressions []RenderLocationSelectorReq `json:"matchExpressions,omitempty" jsonschema:"Set-based requirements over topology keys, for cases matchLabels cannot express, such as one of several cities."`
}

// RenderLocationSelectorReq is one set-based requirement.
type RenderLocationSelectorReq struct {
	Key      string   `json:"key" jsonschema:"Topology key, e.g. topology.datum.net/city-code."`
	Operator string   `json:"operator" jsonschema:"In, NotIn, Exists or DoesNotExist."`
	Values   []string `json:"values,omitempty" jsonschema:"Values for In and NotIn. Must be empty for Exists and DoesNotExist."`
}

// RenderPort is a named port the workload serves.
type RenderPort struct {
	Name     string `json:"name" jsonschema:"Port name, e.g. \"http\". At most 15 characters, and must contain a letter."`
	Port     int32  `json:"port" jsonschema:"Port number, 1 to 65535."`
	Protocol string `json:"protocol,omitempty" jsonschema:"TCP, UDP or SCTP. Defaults to TCP."`
}

// RenderKeyRef selects one key of a ConfigMap or Secret.
type RenderKeyRef struct {
	Name string `json:"name" jsonschema:"Name of the ConfigMap or Secret, which must already exist in the project."`
	Key  string `json:"key" jsonschema:"Key within it."`
}

// RenderEnvVar is one environment variable on the container.
type RenderEnvVar struct {
	Name            string        `json:"name" jsonschema:"Variable name."`
	Value           string        `json:"value,omitempty" jsonschema:"Literal value. Set at most one of value, configMapKeyRef, secretKeyRef."`
	ConfigMapKeyRef *RenderKeyRef `json:"configMapKeyRef,omitempty" jsonschema:"Read the value from a ConfigMap key instead."`
	SecretKeyRef    *RenderKeyRef `json:"secretKeyRef,omitempty" jsonschema:"Read the value from a Secret key instead."`
}

// RenderMount projects a ConfigMap or Secret into the instance's filesystem.
type RenderMount struct {
	Name      string `json:"name,omitempty" jsonschema:"Volume name. Defaults to the ConfigMap or Secret name."`
	ConfigMap string `json:"configMap,omitempty" jsonschema:"Name of the ConfigMap to mount. Set exactly one of configMap or secret."`
	Secret    string `json:"secret,omitempty" jsonschema:"Name of the Secret to mount. Set exactly one of configMap or secret."`
	MountPath string `json:"mountPath" jsonschema:"Absolute path the contents appear at inside the instance."`
}

// RenderVM asks for a virtual machine rather than a container.
type RenderVM struct {
	SSHKeys   []string `json:"sshKeys" jsonschema:"Keys authorized to log in, each \"username:ssh-public-key\". At least one — a machine with no key is unreachable and is rejected."`
	BootImage string   `json:"bootImage,omitempty" jsonschema:"Disk image the machine boots. Defaults to datumcloud/ubuntu-2204-lts, currently the only one accepted."`
}

// WorkloadRenderInput is the flat description a manifest is rendered from. It
// mirrors workloadspec.Input field for field, so the manifest a model renders
// and the one `datumctl compute deploy` writes cannot drift apart.
type WorkloadRenderInput struct {
	Name         string            `json:"name" jsonschema:"Workload name, a DNS label, e.g. \"api-backend\". Cannot be changed later."`
	Image        string            `json:"image,omitempty" jsonschema:"Fully qualified container image, e.g. \"ghcr.io/acme/api:1.4.2\". Required unless vm is set. A bare name is the most common cause of ImageUnavailable afterwards."`
	InstanceType string            `json:"instanceType,omitempty" jsonschema:"Instance type from compute_instance_types_list. Defaults to the only one accepted today."`
	RuntimeClass string            `json:"runtimeClass,omitempty" jsonschema:"Execution tier the instances run in. Leave unset unless the person named one: the server picks its default, and the tier cannot be changed after the workload exists."`
	Network      string            `json:"network,omitempty" jsonschema:"Network the instance attaches to. Defaults to \"default\"."`
	Placements   []RenderPlacement `json:"placements" jsonschema:"Where instances run and how many. At least one is required."`
	Ports        []RenderPort      `json:"ports,omitempty" jsonschema:"Named ports the workload serves. Each is also opened to the internet, since a port nothing can reach is not useful."`
	Env          []RenderEnvVar    `json:"env,omitempty" jsonschema:"Environment variables on the container. Not accepted for a virtual machine."`
	ConfigMounts []RenderMount     `json:"configMounts,omitempty" jsonschema:"ConfigMaps and Secrets projected into the instance's filesystem."`
	PublicIPv4   bool              `json:"publicIPv4,omitempty" jsonschema:"Ask for a public IPv4 address. Settled at create: it cannot be added or removed later, so ask before rendering rather than defaulting it."`
	Labels       map[string]string `json:"labels,omitempty" jsonschema:"Labels applied to the workload and to every instance it creates."`
	VM           *RenderVM         `json:"vm,omitempty" jsonschema:"Render a virtual machine instead of a container. Only when the person needs a whole operating system to log into."`
}

// WorkloadRenderOutput is the manifest and what rendering it settled.
type WorkloadRenderOutput struct {
	// Manifest is the complete Workload, as YAML.
	Manifest string `json:"manifest"`
	// Notes are the decisions this manifest fixes for the life of the workload
	// and the defaults that were filled in. Worth reading out: several of them
	// cannot be changed after the first apply.
	Notes []string `json:"notes,omitempty"`
}

// FieldError is one rejection, with the field it names.
type FieldError struct {
	// Field is the path the server named, e.g.
	// "spec.template.spec.volumes[1].name". Empty when the rejection is about
	// the manifest as a whole.
	Field string `json:"field,omitempty"`
	// Message is the server's own wording, kept verbatim so it can be quoted.
	Message string `json:"message"`
}

// WorkloadValidateInput is one manifest to check.
type WorkloadValidateInput struct {
	Manifest string `json:"manifest" jsonschema:"A complete Workload manifest as YAML, normally the one compute_workload_render returned."`
}

// WorkloadValidateOutput is the server's verdict.
type WorkloadValidateOutput struct {
	Valid  bool         `json:"valid"`
	Errors []FieldError `json:"errors,omitempty"`
	// Exists reports whether a workload of this name is already there, which
	// decides whether applying would create or change one.
	Exists bool `json:"exists"`
	// Diff is what applying would change about the existing workload. Empty
	// for a create, and empty for an update that changes nothing this diff
	// covers.
	Diff []string `json:"diff,omitempty"`
}

// NetworkPlan says what the plan found out about the network the interface
// names.
type NetworkPlan struct {
	Name   string `json:"name"`
	Exists bool   `json:"exists"`
	// WillCreate reports that applying this plan creates the network too. Say
	// so when showing the plan: it is a second object being created.
	WillCreate bool `json:"willCreate"`
}

// WorkloadPlanInput is the manifest to plan.
type WorkloadPlanInput struct {
	Manifest string `json:"manifest" jsonschema:"A complete Workload manifest as YAML, normally the one compute_workload_render returned and compute_workload_validate accepted."`
}

// WorkloadPlanOutput is everything the person needs to see before agreeing,
// plus the token that binds their agreement to this manifest.
type WorkloadPlanOutput struct {
	// Valid is false when the manifest was rejected. There is no token in that
	// case, and nothing can be applied until it is fixed.
	Valid  bool         `json:"valid"`
	Errors []FieldError `json:"errors,omitempty"`
	// Manifest is the canonical form of what was planned. This exact manifest is
	// what the token covers and what compute_workload_apply has to be given, and
	// it is the one to show the person who asked.
	Manifest string `json:"manifest,omitempty"`
	// Action is "create" or "update".
	Action  string       `json:"action,omitempty"`
	Diff    []string     `json:"diff,omitempty"`
	Network *NetworkPlan `json:"network,omitempty"`
	// PlanToken authorizes applying this manifest, and nothing else.
	PlanToken string `json:"planToken,omitempty"`
	// ExpiresAt is when the token stops being accepted, in RFC 3339.
	ExpiresAt string `json:"expiresAt,omitempty"`
}

// WorkloadApplyInput is the plan, handed back whole.
type WorkloadApplyInput struct {
	Manifest  string `json:"manifest" jsonschema:"The manifest compute_workload_plan returned, verbatim. One changed character and the token stops matching and nothing is created."`
	PlanToken string `json:"planToken" jsonschema:"The token compute_workload_plan returned for that manifest. Only call this after the person who asked has seen the manifest and the diff and said yes."`
}

// NetworkApplied reports whether the network had to be created alongside the
// workload.
type NetworkApplied struct {
	Name    string `json:"name,omitempty"`
	Created bool   `json:"created"`
}

// WorkloadApplyOutput is what was done.
type WorkloadApplyOutput struct {
	Action   string         `json:"action"`
	Workload string         `json:"workload"`
	Network  NetworkApplied `json:"network"`
	// Next is the step that turns an accepted request into a running workload,
	// which are not the same thing.
	Next string `json:"next"`
}

// ------------------------------------------------------------ registration

// RegisterWriteTools adds the render, validate, plan and apply tools. Called
// by RegisterTools; separate so the two mutating tools can be read on their
// own, which is what a review of this surface wants to look at.
func RegisterWriteTools(s *mcp.Server, deps DepsFor) {
	mcp.AddTool(s, &mcp.Tool{
		Name:  ToolWorkloadRender,
		Title: "Render a workload manifest",
		Description: "Turn a short description of a deployment — name, image, where, how many — into a " +
			"complete Workload manifest, and report what rendering it settled. Nothing is read and nothing " +
			"is changed, so render as often as it takes to get the manifest right. Read the manifest that " +
			"comes back rather than assuming it says what was asked for, and read the notes: they name the " +
			"choices that cannot be changed once the workload exists, the interface's address families and " +
			"a public IPv4 address among them. Gather the inputs from the person rather than inventing " +
			"them, and take location names from compute_locations_list and the instance type from " +
			"compute_instance_types_list. A placement either names locations or selects them by topology; use a " +
			"locationSelector for \"every location in a city or region\", which also picks up locations added later. " +
			"Load the workload-create skill before using this. Writes nothing.",
	}, workloadRender(deps))

	mcp.AddTool(s, &mcp.Tool{
		Name:  ToolWorkloadValidate,
		Title: "Validate a workload manifest",
		Description: "Ask the server whether a manifest would be accepted, without creating anything. Returns " +
			"the exact rejection with the field path it names, whether a workload of that name already " +
			"exists, and — when it does — what applying this manifest would change about it. Every " +
			"rejection reported here is one that would otherwise arrive after the person was told the " +
			"workload was written correctly. Quote the field path verbatim and say in plain words what it " +
			"means, fix the manifest, render it again, and validate again. Never plan or apply a manifest " +
			"that failed validation. Writes nothing.",
	}, workloadValidate(deps))

	mcp.AddTool(s, &mcp.Tool{
		Name:  ToolWorkloadPlan,
		Title: "Plan a workload change",
		Description: "Settle everything that has to be true before a workload can be created or changed, and " +
			"mint the token that authorizes exactly that. Validates the manifest, resolves whether this is " +
			"a create or an update, reports what would change, says whether the network the interface names " +
			"is already there or would be created alongside the workload, and returns a canonical manifest " +
			"with a plan token that is a hash of it. The manifest in this output is the one the token " +
			"covers: show that manifest in full, and the diff, to the person who asked, say what will exist " +
			"and where and how many, and get an explicit yes before calling compute_workload_apply. A question " +
			"about the plan is not a yes. If anything changes, plan again — a token minted for the earlier " +
			"manifest will be refused, and applying it because it was close is exactly what this prevents. " +
			"Tokens are good for 15 minutes. Planning by itself creates and changes nothing.",
	}, workloadPlan(deps))

	mcp.AddTool(s, &mcp.Tool{
		Name:  ToolWorkloadApply,
		Title: "Apply a planned workload",
		Description: "Create or change the workload a plan token was minted for, and nothing else. Takes the " +
			"manifest compute_workload_plan returned and that plan's token: the token is a hash of that manifest, " +
			"the project, and the version of the workload the plan saw, so a manifest edited after the " +
			"plan, a token from another project, or a workload someone else changed in the meantime is " +
			"refused rather than applied. Call this only once the person who asked has been shown the " +
			"plan's manifest and diff and has said yes; if they asked for a change instead, go back and " +
			"plan again. When the plan said the network was missing, it is created first. A workload being " +
			"created means the request was accepted, not that anything is running — call compute_workload_diagnose " +
			"next and say that plainly rather than reporting a deployment.",
	}, workloadApply(deps))
}

// ---------------------------------------------------------------- handlers

func workloadRender(deps DepsFor) mcp.ToolHandlerFor[WorkloadRenderInput, WorkloadRenderOutput] {
	return func(
		ctx context.Context, _ *mcp.CallToolRequest, in WorkloadRenderInput,
	) (*mcp.CallToolResult, WorkloadRenderOutput, error) {
		// Rendering reads nothing, but an unauthenticated caller must not be
		// able to use it as a probe, the same rule compute_reason_explain follows.
		if _, err := deps(ctx); err != nil {
			return nil, WorkloadRenderOutput{}, err
		}

		spec := toSpecInput(in)
		workload, err := workloadspec.Render(spec)
		if err != nil {
			return nil, WorkloadRenderOutput{}, err
		}
		manifest, err := workloadspec.MarshalYAML(workload)
		if err != nil {
			return nil, WorkloadRenderOutput{}, err
		}

		return nil, WorkloadRenderOutput{
			Manifest: string(manifest),
			Notes:    renderNotes(spec),
		}, nil
	}
}

func workloadValidate(deps DepsFor) mcp.ToolHandlerFor[WorkloadValidateInput, WorkloadValidateOutput] {
	return func(
		ctx context.Context, _ *mcp.CallToolRequest, in WorkloadValidateInput,
	) (*mcp.CallToolResult, WorkloadValidateOutput, error) {
		d, err := deps(ctx)
		if err != nil {
			return nil, WorkloadValidateOutput{}, err
		}

		checked, err := check(ctx, d, in.Manifest)
		if err != nil {
			return nil, WorkloadValidateOutput{}, err
		}
		return nil, WorkloadValidateOutput{
			Valid:  checked.errors == nil,
			Errors: checked.errors,
			Exists: checked.existing != nil,
			Diff:   checked.diff,
		}, nil
	}
}

func workloadPlan(deps DepsFor) mcp.ToolHandlerFor[WorkloadPlanInput, WorkloadPlanOutput] {
	return func(
		ctx context.Context, _ *mcp.CallToolRequest, in WorkloadPlanInput,
	) (*mcp.CallToolResult, WorkloadPlanOutput, error) {
		d, err := deps(ctx)
		if err != nil {
			return nil, WorkloadPlanOutput{}, err
		}
		// Resolved before any work, so a server that cannot mint a token says
		// so rather than validating a manifest it could never let through.
		key, err := d.planTokenKey()
		if err != nil {
			return nil, WorkloadPlanOutput{}, err
		}

		checked, err := check(ctx, d, in.Manifest)
		if err != nil {
			return nil, WorkloadPlanOutput{}, err
		}
		if checked.errors != nil {
			return nil, WorkloadPlanOutput{Valid: false, Errors: checked.errors}, nil
		}

		network, err := planNetwork(ctx, d, checked.desired)
		if err != nil {
			return nil, WorkloadPlanOutput{}, err
		}

		manifest, err := workloadspec.MarshalYAML(checked.desired)
		if err != nil {
			return nil, WorkloadPlanOutput{}, err
		}
		canonical, err := canonicalJSON(checked.desired)
		if err != nil {
			return nil, WorkloadPlanOutput{}, err
		}

		action := actionCreate
		if checked.existing != nil {
			action = actionUpdate
		}
		expiry := time.Now().Add(planTokenTTL)

		return nil, WorkloadPlanOutput{
			Valid:     true,
			Manifest:  string(manifest),
			Action:    action,
			Diff:      checked.diff,
			Network:   network,
			PlanToken: mintPlanToken(key, canonical, d.Project, resourceVersionOf(checked.existing), expiry),
			ExpiresAt: expiry.UTC().Format(time.RFC3339),
		}, nil
	}
}

func workloadApply(deps DepsFor) mcp.ToolHandlerFor[WorkloadApplyInput, WorkloadApplyOutput] {
	return func(
		ctx context.Context, _ *mcp.CallToolRequest, in WorkloadApplyInput,
	) (*mcp.CallToolResult, WorkloadApplyOutput, error) {
		d, err := deps(ctx)
		if err != nil {
			return nil, WorkloadApplyOutput{}, err
		}
		key, err := d.planTokenKey()
		if err != nil {
			return nil, WorkloadApplyOutput{}, err
		}
		w, err := d.writer()
		if err != nil {
			return nil, WorkloadApplyOutput{}, err
		}

		desired, ferr := decodeManifest(in.Manifest, d.Namespace)
		if ferr != nil {
			return nil, WorkloadApplyOutput{}, fmt.Errorf(
				"this manifest could not be read, so nothing was created. %s: %s. Render the workload "+
					"again, plan it, and show the person who asked what came back",
				ferr.Field, ferr.Message)
		}
		existing, err := getExisting(ctx, d, desired.Name)
		if err != nil {
			return nil, WorkloadApplyOutput{}, err
		}
		canonical, err := canonicalJSON(desired)
		if err != nil {
			return nil, WorkloadApplyOutput{}, err
		}

		// The token is checked before anything is read further or written: a
		// manifest nobody agreed to must not reach the server at all, not even
		// as a dry run.
		if err := verifyPlanToken(
			key, in.PlanToken, canonical, d.Project, resourceVersionOf(existing), time.Now(),
		); err != nil {
			return nil, WorkloadApplyOutput{}, err
		}

		// Checked once more against the live server, because the plan may have
		// been made minutes ago and quota, references and the catalogs all move
		// underneath it. A rejection here writes nothing.
		attempt := desired.DeepCopy()
		if existing != nil {
			attempt.ResourceVersion = existing.ResourceVersion
			err = w.DryRunUpdate(ctx, attempt)
		} else {
			err = w.DryRunCreate(ctx, attempt)
		}
		if err != nil {
			return nil, WorkloadApplyOutput{}, fmt.Errorf(
				"the server rejected this workload when it was checked again just before creating it, so "+
					"nothing was created: %w. Something changed since the plan was made. Fix the manifest, "+
					"plan again, and show the person who asked what came back", err)
		}

		// The network the interface names has to exist for instances to be
		// published, and creating it is part of what the plan promised.
		applied, err := applyNetwork(ctx, d, w, desired)
		if err != nil {
			return nil, WorkloadApplyOutput{}, err
		}

		action := actionCreate
		if existing != nil {
			action = actionUpdate
			desired.ResourceVersion = existing.ResourceVersion
			err = w.Update(ctx, desired)
		} else {
			err = w.Create(ctx, desired)
		}
		if err != nil {
			return nil, WorkloadApplyOutput{}, err
		}

		return nil, WorkloadApplyOutput{
			Action:   action,
			Workload: desired.Name,
			Network:  applied,
			Next: fmt.Sprintf(
				"call %s with name %q. The request was accepted, which is not the same as anything "+
					"running yet: instances appear, then start, and the first pull of a large image takes "+
					"a while",
				ToolWorkloadDiagnose, desired.Name),
		}, nil
	}
}

// ----------------------------------------------------------------- helpers

// checked is the shared result of the validate step, which plan and apply both
// begin with.
type checked struct {
	// desired is the manifest, normalized. Nil when errors is set.
	desired *computev1alpha.Workload
	// existing is the workload of that name today, or nil when there is none.
	existing *computev1alpha.Workload
	// errors is nil when the server accepted the manifest.
	errors []FieldError
	diff   []string
}

// check decodes a manifest and asks the server for its verdict, without
// persisting anything. A rejection is a result, not an error: the field paths
// are what the model has to act on.
func check(ctx context.Context, d ToolDeps, manifest string) (checked, error) {
	w, err := d.writer()
	if err != nil {
		return checked{}, err
	}

	desired, ferr := decodeManifest(manifest, d.Namespace)
	if ferr != nil {
		return checked{errors: []FieldError{*ferr}}, nil
	}

	existing, err := getExisting(ctx, d, desired.Name)
	if err != nil {
		return checked{}, err
	}

	attempt := desired.DeepCopy()
	if existing != nil {
		// An update is checked at the version that was read, so the check is
		// of the change that would actually be made.
		attempt.ResourceVersion = existing.ResourceVersion
		err = w.DryRunUpdate(ctx, attempt)
	} else {
		err = w.DryRunCreate(ctx, attempt)
	}
	if err != nil {
		return checked{existing: existing, errors: fieldErrors(err)}, nil
	}

	out := checked{desired: desired, existing: existing}
	if existing != nil {
		out.diff = workloadspec.Diff(existing, desired)
	}
	return out, nil
}

// planNetwork reports on the network the interface names.
func planNetwork(ctx context.Context, d ToolDeps, desired *computev1alpha.Workload) (*NetworkPlan, error) {
	name := networkNameOf(desired)
	if name == "" {
		return nil, nil
	}

	w, err := d.writer()
	if err != nil {
		return nil, err
	}
	if _, err := w.GetNetwork(ctx, d.Namespace, name); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
		return &NetworkPlan{Name: name, WillCreate: true}, nil
	}
	return &NetworkPlan{Name: name, Exists: true}, nil
}

// applyNetwork creates the network the interface names when it is still
// missing, mirroring what `datumctl compute deploy` does: a minimal network
// with automatic address management.
func applyNetwork(
	ctx context.Context, d ToolDeps, w Writer, desired *computev1alpha.Workload,
) (NetworkApplied, error) {
	name := networkNameOf(desired)
	if name == "" {
		return NetworkApplied{}, nil
	}

	if _, err := w.GetNetwork(ctx, d.Namespace, name); err == nil {
		return NetworkApplied{Name: name}, nil
	} else if !apierrors.IsNotFound(err) {
		return NetworkApplied{}, err
	}

	network := &networkingv1alpha.Network{
		ObjectMeta: metav1.ObjectMeta{Namespace: d.Namespace, Name: name},
		Spec: networkingv1alpha.NetworkSpec{
			IPAM: networkingv1alpha.NetworkIPAM{Mode: networkingv1alpha.NetworkIPAMModeAuto},
		},
	}
	if err := w.CreateNetwork(ctx, network); err != nil {
		return NetworkApplied{}, fmt.Errorf(
			"the workload was not created: the network %q it attaches to is missing and could not be "+
				"created either: %w", name, err)
	}
	return NetworkApplied{Name: name, Created: true}, nil
}

// networkNameOf returns the network the workload's interface attaches to. A
// rendered workload has exactly one interface; a hand-written one that has
// none is left to the server to reject.
func networkNameOf(w *computev1alpha.Workload) string {
	interfaces := w.Spec.Template.Spec.NetworkInterfaces
	if len(interfaces) == 0 {
		return ""
	}
	return interfaces[0].Network.Name
}

// getExisting returns the workload of that name, or nil when there is none.
func getExisting(ctx context.Context, d ToolDeps, name string) (*computev1alpha.Workload, error) {
	if d.Reader == nil {
		return nil, fmt.Errorf(
			"this server was built without the ability to read the project's workloads, so this tool " +
				"cannot answer. The person who asked did nothing wrong: whoever operates this server " +
				"needs to configure it")
	}
	w, err := d.Reader.GetWorkload(ctx, d.Namespace, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return w, nil
}

func resourceVersionOf(w *computev1alpha.Workload) string {
	if w == nil {
		return ""
	}
	return w.ResourceVersion
}

// decodeManifest reads a manifest into a Workload and normalizes it: the
// namespace is forced to the project's, the type is stamped, and everything
// the server owns is dropped. What comes back is what the plan token is
// computed over, so two manifests that mean the same thing hash the same.
func decodeManifest(manifest, namespace string) (*computev1alpha.Workload, *FieldError) {
	if strings.TrimSpace(manifest) == "" {
		return nil, &FieldError{Field: fieldManifest, Message: "a workload manifest is required"}
	}

	var w computev1alpha.Workload
	// Strict, so a misspelled field is reported rather than silently dropped
	// and then missing from a workload that was said to have it.
	if err := sigsyaml.UnmarshalStrict([]byte(manifest), &w); err != nil {
		return nil, &FieldError{
			Field:   fieldManifest,
			Message: fmt.Sprintf("this is not a readable Workload manifest: %v", err),
		}
	}

	if w.Kind != "" && w.Kind != "Workload" {
		return nil, &FieldError{
			Field:   "kind",
			Message: fmt.Sprintf("these tools create Workloads, not %s", w.Kind),
		}
	}
	if w.Name == "" {
		return nil, &FieldError{Field: "metadata.name", Message: "a workload name is required"}
	}

	w.TypeMeta = metav1.TypeMeta{
		APIVersion: computev1alpha.GroupVersion.String(),
		Kind:       "Workload",
	}
	// The project decides the namespace, never the manifest: a manifest that
	// named another one would be asking to write somewhere this request does
	// not reach.
	w.Namespace = namespace
	w.ResourceVersion = ""
	w.UID = ""
	w.Generation = 0
	w.CreationTimestamp = metav1.Time{}
	w.ManagedFields = nil
	w.Status = computev1alpha.WorkloadStatus{}

	return &w, nil
}

// canonicalJSON renders the manifest the token is computed over. The workload
// is normalized first, and Go marshals struct fields in declaration order and
// map keys in sorted order, so the same manifest always produces the same
// bytes.
func canonicalJSON(w *computev1alpha.Workload) ([]byte, error) {
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("normalizing the manifest: %w", err)
	}
	return raw, nil
}

// fieldErrors turns a server rejection into field/message pairs. A structured
// rejection carries a cause per field; the whole message is returned alongside
// them, because it is the server's own wording and is what a person quotes
// when escalating.
func fieldErrors(err error) []FieldError {
	var status *apierrors.StatusError
	if !errors.As(err, &status) {
		return []FieldError{{Message: err.Error()}}
	}

	out := []FieldError{}
	if details := status.ErrStatus.Details; details != nil {
		for _, cause := range details.Causes {
			out = append(out, FieldError{Field: cause.Field, Message: cause.Message})
		}
	}
	return append(out, FieldError{Message: status.ErrStatus.Message})
}

// writer returns the Writer for this call, or an error naming the wiring
// mistake, the same way discovery's does.
func (d ToolDeps) writer() (Writer, error) {
	if d.Writer == nil {
		return nil, fmt.Errorf(
			"this server was built without the ability to create or change a workload, so this tool " +
				"cannot answer. The person who asked did nothing wrong: whoever operates this server " +
				"needs to configure it")
	}
	return d.Writer, nil
}

// planTokenKey returns the key plan tokens are minted and checked with, or an
// error. Both halves are refused without one: a server that cannot check a
// token must not issue something that looks like one.
func (d ToolDeps) planTokenKey() ([]byte, error) {
	if len(d.PlanTokenKey) == 0 {
		return nil, fmt.Errorf(
			"this server was built without a plan token key, so it cannot authorize creating or " +
				"changing a workload. The person who asked did nothing wrong: whoever operates this " +
				"server needs to configure it")
	}
	if d.Project == "" {
		return nil, fmt.Errorf(
			"this request did not say which project it is for, so a plan cannot be bound to one. The " +
				"person who asked did nothing wrong: whoever operates the client that called this tool " +
				"needs to configure it")
	}
	return d.PlanTokenKey, nil
}

// ------------------------------------------------------------- plan tokens

// mintPlanToken returns the token that authorizes applying exactly this
// manifest, in this project, against this version of the workload, until
// expiry. The form is base64(HMAC-SHA256(payload)) + "." + expiry in seconds:
// the expiry travels in the clear because it is also covered by the hash, so
// moving it invalidates the token.
func mintPlanToken(key, canonical []byte, project, resourceVersion string, expiry time.Time) string {
	unix := expiry.Unix()
	mac := planTokenMAC(key, canonical, project, resourceVersion, unix)
	return base64.RawURLEncoding.EncodeToString(mac) + "." + strconv.FormatInt(unix, 10)
}

// verifyPlanToken refuses anything that is not a token minted for exactly this
// manifest, project and workload version, and still inside its window. The
// refusals are what a person reads, so each one says what happened, that
// nothing was created, and what to do instead.
func verifyPlanToken(
	key []byte, token string, canonical []byte, project, resourceVersion string, now time.Time,
) error {
	encoded, expiryText, found := strings.Cut(token, ".")
	if !found {
		return fmt.Errorf(
			"this is not a plan token in the form %s issues, so nothing was created. Call %s with the "+
				"manifest to apply and use the token it returns",
			ToolWorkloadPlan, ToolWorkloadPlan)
	}
	unix, err := strconv.ParseInt(expiryText, 10, 64)
	if err != nil {
		return fmt.Errorf(
			"this plan token does not carry a readable expiry, so nothing was created. Call %s again "+
				"and use the token it returns", ToolWorkloadPlan)
	}
	if expiry := time.Unix(unix, 0); now.After(expiry) {
		return fmt.Errorf(
			"this plan expired at %s and nothing was created. A plan is good for %s, so that what is "+
				"created is still what was agreed to. Call %s again, show the person who asked the "+
				"manifest and the diff it returns, and ask again before applying",
			expiry.UTC().Format(time.RFC3339), planTokenTTL, ToolWorkloadPlan)
	}

	presented, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		presented = nil
	}
	if !hmac.Equal(presented, planTokenMAC(key, canonical, project, resourceVersion, unix)) {
		return fmt.Errorf(
			"this plan token does not cover the manifest it was given, so nothing was created. That "+
				"happens when the manifest changed after the plan was made — one character is enough — "+
				"when the plan was made for a different project, or when the workload was changed by "+
				"someone else in the meantime. This refusal is the check working. Call %s again with "+
				"the manifest to apply, show the person who asked the manifest and the diff it returns, "+
				"and ask again", ToolWorkloadPlan)
	}
	return nil
}

// planTokenMAC hashes the manifest together with everything the plan assumed,
// each part separated by a newline so no two payloads can be built out of the
// same bytes.
func planTokenMAC(key, canonical []byte, project, resourceVersion string, expiryUnix int64) []byte {
	var payload bytes.Buffer
	payload.Write(canonical)
	payload.WriteByte('\n')
	payload.WriteString(project)
	payload.WriteByte('\n')
	payload.WriteString(resourceVersion)
	payload.WriteByte('\n')
	payload.WriteString(strconv.FormatInt(expiryUnix, 10))

	mac := hmac.New(sha256.New, key)
	mac.Write(payload.Bytes())
	return mac.Sum(nil)
}

// ---------------------------------------------------------------- rendering

// toSpecInput converts the tool's input to workloadspec's. A straight mapping,
// kept explicit so the tool schema can be worded for a model without that
// wording leaking into the package the CLI also renders through.
func toSpecInput(in WorkloadRenderInput) workloadspec.Input {
	out := workloadspec.Input{
		Name:         in.Name,
		Image:        in.Image,
		InstanceType: in.InstanceType,
		RuntimeClass: in.RuntimeClass,
		Network:      in.Network,
		PublicIPv4:   in.PublicIPv4,
		Labels:       in.Labels,
	}

	for _, p := range in.Placements {
		out.Placements = append(out.Placements, workloadspec.Placement{
			Name:             p.Name,
			Locations:        p.Locations,
			LocationSelector: toLabelSelector(p.LocationSelector),
			MinReplicas:      p.MinReplicas,
		})
	}
	for _, p := range in.Ports {
		out.Ports = append(out.Ports, workloadspec.Port{
			Name:     p.Name,
			Port:     p.Port,
			Protocol: corev1.Protocol(p.Protocol),
		})
	}
	for _, e := range in.Env {
		out.Env = append(out.Env, workloadspec.EnvVar{
			Name:            e.Name,
			Value:           e.Value,
			ConfigMapKeyRef: toKeyRef(e.ConfigMapKeyRef),
			SecretKeyRef:    toKeyRef(e.SecretKeyRef),
		})
	}
	for _, m := range in.ConfigMounts {
		out.ConfigMounts = append(out.ConfigMounts, workloadspec.Mount{
			Name:      m.Name,
			ConfigMap: m.ConfigMap,
			Secret:    m.Secret,
			MountPath: m.MountPath,
		})
	}
	if in.VM != nil {
		out.VM = &workloadspec.VMInput{
			SSHKeys:   in.VM.SSHKeys,
			BootImage: in.VM.BootImage,
		}
	}

	return out
}

// toLabelSelector converts the tool's selector to the API's. The operator is
// passed through verbatim: an unrecognized one is refused by the render's own
// validation with the field path, which is more useful than silently dropping
// the requirement here.
func toLabelSelector(sel *RenderLocationSelector) *metav1.LabelSelector {
	if sel == nil {
		return nil
	}
	out := &metav1.LabelSelector{MatchLabels: sel.MatchLabels}
	for _, req := range sel.MatchExpressions {
		out.MatchExpressions = append(out.MatchExpressions, metav1.LabelSelectorRequirement{
			Key:      req.Key,
			Operator: metav1.LabelSelectorOperator(req.Operator),
			Values:   req.Values,
		})
	}
	return out
}

func toKeyRef(ref *RenderKeyRef) *workloadspec.KeyRef {
	if ref == nil {
		return nil
	}
	return &workloadspec.KeyRef{Name: ref.Name, Key: ref.Key}
}

// renderNotes says what this manifest settled that a later render cannot
// correct, and which values were filled in for a caller who did not name them.
//
// It is written from the input as given, before defaults are applied, so
// "defaulted to" means the person did not choose it — which is the thing they
// need to be asked about while the workload can still be changed.
func renderNotes(in workloadspec.Input) []string {
	notes := []string{
		"The instance's single network interface is settled by this manifest and cannot be changed " +
			"once the workload exists: its name, the address families it carries, any extra addresses, " +
			"and what becomes of those addresses when an instance goes away. Getting one of them wrong " +
			"means creating a new workload, not editing this one.",
	}

	if in.PublicIPv4 {
		notes = append(notes, "A public IPv4 address was asked for, so the interface carries both IPv4 "+
			"and IPv6. Neither the address nor the families can be removed later.")
	} else {
		notes = append(notes, "The interface carries IPv6 only, which is the default. If this workload "+
			"has to answer on IPv4, say so before it is applied: IPv4 cannot be added afterwards.")
	}

	notes = append(notes, "Addresses are given back when an instance goes away. Keeping one — an "+
		"address published in DNS, or allowed through someone's firewall — means editing this manifest "+
		"before the first apply.")

	if in.InstanceType == "" {
		notes = append(notes, fmt.Sprintf(
			"No instance type was given, so every instance is %s. Per-container CPU and memory are not "+
				"accepted: the instance type is what decides the size.", workloadspec.DefaultInstanceType))
	}
	if in.Network == "" {
		notes = append(notes, fmt.Sprintf(
			"No network was named, so the interface attaches to %q. If the project does not have "+
				"one, %s says so and %s creates it alongside the workload.",
			workloadspec.DefaultNetwork, ToolWorkloadPlan, ToolWorkloadApply))
	}
	for _, p := range in.Placements {
		if p.Name == "" {
			notes = append(notes, fmt.Sprintf("A placement was not named, so it is called %q.",
				workloadspec.DefaultPlacementName))
		}
		if p.MinReplicas == 0 {
			notes = append(notes, fmt.Sprintf(
				"Placement %q did not say how many instances to run, so it runs %d. There is no "+
					"scaling to zero.", placementName(p), workloadspec.DefaultMinReplicas))
		}
		if p.LocationSelector != nil {
			notes = append(notes, fmt.Sprintf(
				"Placement %q selects its locations by topology rather than naming them, so it runs "+
					"wherever the selector matches — including locations added later, which will "+
					"start instances without this manifest changing. %s shows which locations match "+
					"today.", placementName(p), ToolLocationsList))
		}
	}
	if in.VM != nil && in.VM.BootImage == "" {
		notes = append(notes, fmt.Sprintf(
			"No boot image was given, so the machine boots %s, currently the only one accepted.",
			workloadspec.DefaultBootImage))
	}

	return notes
}

func placementName(p workloadspec.Placement) string {
	if p.Name == "" {
		return workloadspec.DefaultPlacementName
	}
	return p.Name
}
