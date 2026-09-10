// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/compute/internal/workloadspec"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

const (
	// testProject is the project a plan is bound to. Plan tokens are only good
	// in the project they were minted for, so the tests need two of them.
	testProject  = "acme-prod"
	otherProject = "acme-staging"

	testImage = "ghcr.io/acme/api:1.4.2"
	// deployedImage is what the fixture workload is already running, so an
	// update has something to change.
	deployedImage = "ghcr.io/acme/api:1.0.0"
)

// testPlanKey stands in for PLAN_TOKEN_KEY. Long enough to be a real key, and
// obviously not one that was ever deployed.
var testPlanKey = []byte("test-plan-token-key-32-bytes-long!!")

// fakeWriter records what would have reached the server, and can be told to
// reject a dry run the way the server itself would. The recording is the point:
// the strongest thing these tests assert is that nothing was written.
type fakeWriter struct {
	// dryRunErr, when set, is what both dry-run checks return.
	dryRunErr error
	// writeErr, when set, is what Create and Update return.
	writeErr error
	// networks that already exist, by name.
	networks map[string]bool
	// networkGetErr, when set, fails the network read with something other
	// than a not-found.
	networkGetErr error

	dryRuns         int
	created         []computev1alpha.Workload
	updated         []computev1alpha.Workload
	createdNetworks []networkingv1alpha.Network
}

var _ Writer = (*fakeWriter)(nil)

func (w *fakeWriter) DryRunCreate(_ context.Context, _ *computev1alpha.Workload) error {
	w.dryRuns++
	return w.dryRunErr
}

func (w *fakeWriter) DryRunUpdate(_ context.Context, _ *computev1alpha.Workload) error {
	w.dryRuns++
	return w.dryRunErr
}

func (w *fakeWriter) Create(_ context.Context, workload *computev1alpha.Workload) error {
	if w.writeErr != nil {
		return w.writeErr
	}
	w.created = append(w.created, *workload.DeepCopy())
	return nil
}

func (w *fakeWriter) Update(_ context.Context, workload *computev1alpha.Workload) error {
	if w.writeErr != nil {
		return w.writeErr
	}
	w.updated = append(w.updated, *workload.DeepCopy())
	return nil
}

func (w *fakeWriter) GetNetwork(
	_ context.Context, namespace, name string,
) (*networkingv1alpha.Network, error) {
	if w.networkGetErr != nil {
		return nil, w.networkGetErr
	}
	if !w.networks[name] {
		return nil, apierrors.NewNotFound(
			schema.GroupResource{Group: networkingv1alpha.GroupVersion.Group, Resource: "networks"}, name)
	}
	return &networkingv1alpha.Network{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
	}, nil
}

func (w *fakeWriter) CreateNetwork(_ context.Context, n *networkingv1alpha.Network) error {
	if w.networks == nil {
		w.networks = map[string]bool{}
	}
	w.networks[n.Name] = true
	w.createdNetworks = append(w.createdNetworks, *n.DeepCopy())
	return nil
}

// wrote reports whether anything at all reached the server.
func (w *fakeWriter) wrote() bool {
	return len(w.created) > 0 || len(w.updated) > 0 || len(w.createdNetworks) > 0
}

// writeReader answers "is this workload already there?" the way the API server
// does — a missing one is a not-found, not an error — because that distinction
// is what decides create versus update.
type writeReader struct {
	workloads map[string]*computev1alpha.Workload
	err       error
}

var _ Reader = (*writeReader)(nil)

func (r *writeReader) ListWorkloads(context.Context, string) ([]computev1alpha.Workload, error) {
	return nil, r.err
}

func (r *writeReader) GetWorkload(_ context.Context, _, name string) (*computev1alpha.Workload, error) {
	if r.err != nil {
		return nil, r.err
	}
	if w, ok := r.workloads[name]; ok {
		return w.DeepCopy(), nil
	}
	return nil, apierrors.NewNotFound(
		schema.GroupResource{Group: computev1alpha.GroupVersion.Group, Resource: "workloads"}, name)
}

func (r *writeReader) ListDeployments(
	context.Context, string, string,
) ([]computev1alpha.WorkloadDeployment, error) {
	return nil, r.err
}

func (r *writeReader) ListInstances(context.Context, string, string) ([]computev1alpha.Instance, error) {
	return nil, r.err
}

// existingWorkload is the fixture workload as it is already deployed, at a
// known resource version, so a plan can be made against it and then
// invalidated by moving it.
func existingWorkload(image, resourceVersion string) *computev1alpha.Workload {
	w, err := workloadspec.Render(workloadspec.Input{
		Name:  wlAPIBackend,
		Image: image,
		Placements: []workloadspec.Placement{
			{CityCodes: []string{cityDFW}, MinReplicas: 1},
		},
	})
	if err != nil {
		panic(err)
	}
	w.ResourceVersion = resourceVersion
	return w
}

// writeDeps supplies everything the write path needs, in testProject.
func writeDeps(r Reader, w Writer) DepsFor {
	return writeDepsFor(testProject, r, w)
}

func writeDepsFor(project string, r Reader, w Writer) DepsFor {
	return func(context.Context) (ToolDeps, error) {
		return ToolDeps{
			Reader:       r,
			Writer:       w,
			Namespace:    testNamespace,
			Project:      project,
			PlanTokenKey: testPlanKey,
		}, nil
	}
}

// renderInput is the everyday case: one container, one city, one port.
func renderInput() WorkloadRenderInput {
	return WorkloadRenderInput{
		Name:       wlAPIBackend,
		Image:      testImage,
		Placements: []RenderPlacement{{CityCodes: []string{cityDFW}, MinReplicas: 2}},
		Ports:      []RenderPort{{Name: "http", Port: 8080}},
	}
}

func mustRender(t *testing.T, deps DepsFor, in WorkloadRenderInput) string {
	t.Helper()
	_, out, err := workloadRender(deps)(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("compute_workload_render: %v", err)
	}
	return out.Manifest
}

func mustPlan(t *testing.T, deps DepsFor, manifest string) WorkloadPlanOutput {
	t.Helper()
	_, out, err := workloadPlan(deps)(context.Background(), nil, WorkloadPlanInput{Manifest: manifest})
	if err != nil {
		t.Fatalf("compute_workload_plan: %v", err)
	}
	if !out.Valid {
		t.Fatalf("compute_workload_plan rejected the manifest: %+v", out.Errors)
	}
	return out
}

// rejection is a server rejection shaped the way the API server sends one: a
// message, and a cause naming the field. Parsing it back into field/message
// pairs is what lets the model quote the field path.
func rejection(field, message string) error {
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		Status:  metav1.StatusFailure,
		Code:    422,
		Reason:  metav1.StatusReasonInvalid,
		Message: "Workload.compute.datumapis.com \"api-backend\" is invalid: " + field + ": " + message,
		Details: &metav1.StatusDetails{
			Causes: []metav1.StatusCause{{
				Type:    metav1.CauseTypeFieldValueInvalid,
				Field:   field,
				Message: message,
			}},
		},
	}}
}

// ------------------------------------------------------------------ render

// TestWorkloadRenderProducesAManifestAndSaysWhatIsSettled: the manifest is only
// half the answer. The notes carry the decisions that cannot be corrected by a
// later render, and a model that does not read them out lets a person agree to
// something they would have to recreate the workload to change.
func TestWorkloadRenderProducesAManifestAndSaysWhatIsSettled(t *testing.T) {
	deps := writeDeps(&writeReader{}, &fakeWriter{})

	_, out, err := workloadRender(deps)(context.Background(), nil, renderInput())
	if err != nil {
		t.Fatalf("compute_workload_render: %v", err)
	}

	for _, want := range []string{
		"kind: Workload",
		"name: " + wlAPIBackend,
		testImage,
		"minReplicas: 2",
		"- " + cityDFW,
	} {
		if !strings.Contains(out.Manifest, want) {
			t.Errorf("manifest is missing %q:\n%s", want, out.Manifest)
		}
	}
	// Rendering reaches nothing, so the manifest has to be readable straight
	// back into the workload the plan token would be computed over.
	if _, ferr := decodeManifest(out.Manifest, testNamespace); ferr != nil {
		t.Errorf("the rendered manifest does not read back: %+v", ferr)
	}

	notes := strings.Join(out.Notes, "\n")
	// The interface is settled at create, and the instance type and network
	// were defaulted rather than chosen — both are things to say out loud
	// while the workload can still be changed.
	for _, want := range []string{
		"cannot be changed once the workload exists",
		"IPv6 only",
		workloadspec.DefaultInstanceType,
		"\"" + workloadspec.DefaultNetwork + "\"",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes do not mention %q:\n%s", want, notes)
		}
	}
}

// TestWorkloadRenderReportsAPublicAddressAsFinal: asking for IPv4 fixes the
// address families for the life of the workload, so the note has to change
// with the input rather than always saying the same thing.
func TestWorkloadRenderReportsAPublicAddressAsFinal(t *testing.T) {
	deps := writeDeps(&writeReader{}, &fakeWriter{})
	in := renderInput()
	in.PublicIPv4 = true

	_, out, err := workloadRender(deps)(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("compute_workload_render: %v", err)
	}
	notes := strings.Join(out.Notes, "\n")
	if !strings.Contains(notes, "public IPv4 address was asked for") {
		t.Errorf("notes do not report the public address as settled:\n%s", notes)
	}
	if strings.Contains(notes, "IPv6 only") {
		t.Errorf("notes still claim IPv6 only after IPv4 was asked for:\n%s", notes)
	}
}

// TestWorkloadRenderRefusesAnIncompleteInput: a missing image is the caller's
// to supply, and rendering something plausible around a name nobody pushed is
// the failure this prevents.
func TestWorkloadRenderRefusesAnIncompleteInput(t *testing.T) {
	deps := writeDeps(&writeReader{}, &fakeWriter{})
	in := renderInput()
	in.Image = ""

	if _, _, err := workloadRender(deps)(context.Background(), nil, in); err == nil {
		t.Error("compute_workload_render accepted an input with no image")
	}
}

// ---------------------------------------------------------------- validate

// TestWorkloadValidateReportsARejectionAsFieldErrors: the server's answer is
// the whole value of this tool, so the field path it named has to survive as a
// field path rather than being flattened into prose.
func TestWorkloadValidateReportsARejectionAsFieldErrors(t *testing.T) {
	const field = "spec.template.spec.volumes[1].name"
	writer := &fakeWriter{dryRunErr: rejection(field, "volume must be attached at least 1 time")}
	deps := writeDeps(&writeReader{}, writer)
	manifest := mustRender(t, deps, renderInput())

	_, out, err := workloadValidate(deps)(context.Background(), nil,
		WorkloadValidateInput{Manifest: manifest})
	if err != nil {
		t.Fatalf("compute_workload_validate: %v", err)
	}

	if out.Valid {
		t.Fatal("valid = true after the server rejected the manifest")
	}
	if out.Exists {
		t.Error("exists = true for a workload that is not there")
	}
	fields := make([]string, 0, len(out.Errors))
	messages := make([]string, 0, len(out.Errors))
	for _, e := range out.Errors {
		fields = append(fields, e.Field)
		messages = append(messages, e.Message)
	}
	if !contains(fields, field) {
		t.Errorf("errors do not name the field the server named: %+v", out.Errors)
	}
	// The whole message travels too: it is what a person quotes when the field
	// path alone does not tell them what to change.
	if !strings.Contains(strings.Join(messages, "\n"), "is invalid") {
		t.Errorf("errors dropped the server's own message: %+v", out.Errors)
	}
	if writer.wrote() {
		t.Error("compute_workload_validate wrote something")
	}
}

// TestWorkloadValidateReportsAnExistingWorkloadAsADiff: validate is where the
// model learns it is about to change something rather than create it, and the
// diff is what the person has to be shown.
func TestWorkloadValidateReportsAnExistingWorkloadAsADiff(t *testing.T) {
	reader := &writeReader{workloads: map[string]*computev1alpha.Workload{
		wlAPIBackend: existingWorkload(deployedImage, "7"),
	}}
	writer := &fakeWriter{}
	deps := writeDeps(reader, writer)
	manifest := mustRender(t, deps, renderInput())

	_, out, err := workloadValidate(deps)(context.Background(), nil,
		WorkloadValidateInput{Manifest: manifest})
	if err != nil {
		t.Fatalf("compute_workload_validate: %v", err)
	}

	if !out.Valid || !out.Exists {
		t.Fatalf("valid = %v, exists = %v; want both true", out.Valid, out.Exists)
	}
	diff := strings.Join(out.Diff, "\n")
	if !strings.Contains(diff, testImage) {
		t.Errorf("diff does not report the image change: %q", diff)
	}
	if !strings.Contains(diff, "min replicas: 1 → 2") {
		t.Errorf("diff does not report the replica change: %q", diff)
	}
	if writer.wrote() {
		t.Error("compute_workload_validate wrote something")
	}
}

// TestWorkloadValidateRejectsAnUnreadableManifest: a manifest the model
// invented has to come back as something it can fix, not as a tool failure.
func TestWorkloadValidateRejectsAnUnreadableManifest(t *testing.T) {
	deps := writeDeps(&writeReader{}, &fakeWriter{})

	for name, manifest := range map[string]string{
		"empty":        "",
		"not yaml":     "this is not: a: manifest:",
		"no name":      "apiVersion: compute.datumapis.com/v1alpha\nkind: Workload\nspec: {}\n",
		"another kind": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: config\n",
		"typo":         "kind: Workload\nmetadata:\n  name: api\nspce: {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, out, err := workloadValidate(deps)(context.Background(), nil,
				WorkloadValidateInput{Manifest: manifest})
			if err != nil {
				t.Fatalf("compute_workload_validate: %v", err)
			}
			if out.Valid {
				t.Errorf("valid = true for %s", name)
			}
			if len(out.Errors) == 0 {
				t.Error("no errors reported, so there is nothing to fix")
			}
		})
	}
}

// -------------------------------------------------------------------- plan

// TestWorkloadPlanMintsATokenAndReportsAMissingNetwork: the network is a second
// object the apply would create, and a person agreeing to a workload has not
// agreed to that unless the plan says so.
func TestWorkloadPlanMintsATokenAndReportsAMissingNetwork(t *testing.T) {
	writer := &fakeWriter{}
	deps := writeDeps(&writeReader{}, writer)
	manifest := mustRender(t, deps, renderInput())

	out := mustPlan(t, deps, manifest)

	if out.Action != actionCreate {
		t.Errorf("action = %q, want %q", out.Action, actionCreate)
	}
	if out.Network == nil {
		t.Fatal("plan did not report on the network")
	}
	if out.Network.Name != workloadspec.DefaultNetwork || out.Network.Exists || !out.Network.WillCreate {
		t.Errorf("network = %+v, want the default network reported as missing and to be created", *out.Network)
	}
	if out.PlanToken == "" {
		t.Fatal("plan minted no token")
	}
	// The manifest in the output is the one the token covers, which is why the
	// description tells the model to show that one and not its own draft.
	if _, ferr := decodeManifest(out.Manifest, testNamespace); ferr != nil {
		t.Errorf("the planned manifest does not read back: %+v", ferr)
	}
	expires, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if err != nil {
		t.Fatalf("expiresAt = %q, want RFC 3339: %v", out.ExpiresAt, err)
	}
	if until := time.Until(expires); until <= 0 || until > planTokenTTL {
		t.Errorf("token expires in %s, want inside %s", until, planTokenTTL)
	}
	if writer.wrote() {
		t.Error("compute_workload_plan wrote something")
	}
}

// TestWorkloadPlanReportsANetworkThatIsAlreadyThere is the other half: nothing
// extra is created, and the plan must not say it would be.
func TestWorkloadPlanReportsANetworkThatIsAlreadyThere(t *testing.T) {
	writer := &fakeWriter{networks: map[string]bool{workloadspec.DefaultNetwork: true}}
	deps := writeDeps(&writeReader{}, writer)

	out := mustPlan(t, deps, mustRender(t, deps, renderInput()))

	if out.Network == nil || !out.Network.Exists || out.Network.WillCreate {
		t.Errorf("network = %+v, want it reported as already there", out.Network)
	}
}

// TestWorkloadPlanMintsNoTokenForARejectedManifest: a token is authority to
// write. A manifest the server would refuse must never carry one, or a later
// apply spends it on a rejection.
func TestWorkloadPlanMintsNoTokenForARejectedManifest(t *testing.T) {
	writer := &fakeWriter{dryRunErr: rejection("spec.template.spec.runtime.resources.instanceType",
		"Unsupported value: \"datumcloud/d1-huge-64\"")}
	deps := writeDeps(&writeReader{}, writer)
	manifest := mustRender(t, deps, renderInput())

	_, out, err := workloadPlan(deps)(context.Background(), nil, WorkloadPlanInput{Manifest: manifest})
	if err != nil {
		t.Fatalf("compute_workload_plan: %v", err)
	}
	if out.Valid {
		t.Fatal("valid = true after the server rejected the manifest")
	}
	if out.PlanToken != "" {
		t.Error("plan minted a token for a manifest the server rejected")
	}
	if len(out.Errors) == 0 {
		t.Error("no errors reported, so there is nothing to fix")
	}
}

// TestWorkloadPlanResolvesAnUpdate: the same manifest is a create or an update
// depending only on what is already there, and the model has to be told which.
func TestWorkloadPlanResolvesAnUpdate(t *testing.T) {
	reader := &writeReader{workloads: map[string]*computev1alpha.Workload{
		wlAPIBackend: existingWorkload(deployedImage, "7"),
	}}
	deps := writeDeps(reader, &fakeWriter{})

	out := mustPlan(t, deps, mustRender(t, deps, renderInput()))

	if out.Action != actionUpdate {
		t.Errorf("action = %q, want %q", out.Action, actionUpdate)
	}
	if len(out.Diff) == 0 {
		t.Error("an update was planned with no diff to show")
	}
}

// ------------------------------------------------------------------- apply

// TestWorkloadApplyCreatesWhatWasPlanned covers the whole point of the split:
// the manifest that was planned, and only that one, reaches the server — along
// with the network the plan said would have to be created with it.
func TestWorkloadApplyCreatesWhatWasPlanned(t *testing.T) {
	writer := &fakeWriter{}
	deps := writeDeps(&writeReader{}, writer)
	plan := mustPlan(t, deps, mustRender(t, deps, renderInput()))

	_, out, err := workloadApply(deps)(context.Background(), nil, WorkloadApplyInput{
		Manifest:  plan.Manifest,
		PlanToken: plan.PlanToken,
	})
	if err != nil {
		t.Fatalf("compute_workload_apply: %v", err)
	}

	if out.Action != actionCreate || out.Workload != wlAPIBackend {
		t.Errorf("apply reported %q of %q, want a create of %q", out.Action, out.Workload, wlAPIBackend)
	}
	if len(writer.created) != 1 {
		t.Fatalf("created %d workloads, want exactly 1", len(writer.created))
	}
	created := writer.created[0]
	if created.Namespace != testNamespace {
		t.Errorf("created in namespace %q, want the project's %q", created.Namespace, testNamespace)
	}
	if got := created.Spec.Template.Spec.Runtime.Sandbox.Containers[0].Image; got != testImage {
		t.Errorf("created image = %q, want the planned %q", got, testImage)
	}
	// The plan said the network would be created, so it was.
	if len(writer.createdNetworks) != 1 || writer.createdNetworks[0].Name != workloadspec.DefaultNetwork {
		t.Fatalf("created networks = %+v, want the default network", writer.createdNetworks)
	}
	if got := writer.createdNetworks[0].Spec.IPAM.Mode; got != networkingv1alpha.NetworkIPAMModeAuto {
		t.Errorf("network IPAM mode = %q, want %q", got, networkingv1alpha.NetworkIPAMModeAuto)
	}
	if !out.Network.Created {
		t.Error("apply did not report that the network was created; it is a second object")
	}
	// A created workload is not a running one, and the next step has to say so.
	if !strings.Contains(out.Next, ToolWorkloadDiagnose) {
		t.Errorf("next = %q, want it to name %s", out.Next, ToolWorkloadDiagnose)
	}
}

// TestWorkloadApplyLeavesAnExistingNetworkAlone: creating one that is already
// there would fail, and reporting one that was not created as created would
// tell the person something untrue about their project.
func TestWorkloadApplyLeavesAnExistingNetworkAlone(t *testing.T) {
	writer := &fakeWriter{networks: map[string]bool{workloadspec.DefaultNetwork: true}}
	deps := writeDeps(&writeReader{}, writer)
	plan := mustPlan(t, deps, mustRender(t, deps, renderInput()))

	_, out, err := workloadApply(deps)(context.Background(), nil, WorkloadApplyInput{
		Manifest:  plan.Manifest,
		PlanToken: plan.PlanToken,
	})
	if err != nil {
		t.Fatalf("compute_workload_apply: %v", err)
	}
	if len(writer.createdNetworks) != 0 || out.Network.Created {
		t.Errorf("apply created a network that already existed: %+v", writer.createdNetworks)
	}
}

// TestWorkloadApplyUpdatesAtTheVersionThePlanSaw: an update carries the
// resource version that was read, so a change that lands in between is refused
// by the server rather than silently overwritten.
func TestWorkloadApplyUpdatesAtTheVersionThePlanSaw(t *testing.T) {
	reader := &writeReader{workloads: map[string]*computev1alpha.Workload{
		wlAPIBackend: existingWorkload(deployedImage, "7"),
	}}
	writer := &fakeWriter{networks: map[string]bool{workloadspec.DefaultNetwork: true}}
	deps := writeDeps(reader, writer)
	plan := mustPlan(t, deps, mustRender(t, deps, renderInput()))

	_, out, err := workloadApply(deps)(context.Background(), nil, WorkloadApplyInput{
		Manifest:  plan.Manifest,
		PlanToken: plan.PlanToken,
	})
	if err != nil {
		t.Fatalf("compute_workload_apply: %v", err)
	}
	if out.Action != actionUpdate {
		t.Errorf("action = %q, want %q", out.Action, actionUpdate)
	}
	if len(writer.updated) != 1 {
		t.Fatalf("updated %d workloads, want exactly 1", len(writer.updated))
	}
	if got := writer.updated[0].ResourceVersion; got != "7" {
		t.Errorf("updated at resource version %q, want the %q the plan read", got, "7")
	}
	if len(writer.created) != 0 {
		t.Error("apply created a workload that already existed")
	}
}

// TestWorkloadApplyRefusesATamperedManifest is the property the whole design
// rests on: what is created is what was shown, or nothing. A model that read a
// poisoned status message and changed one field cannot spend a token minted
// for the manifest the person actually agreed to.
func TestWorkloadApplyRefusesATamperedManifest(t *testing.T) {
	writer := &fakeWriter{}
	deps := writeDeps(&writeReader{}, writer)
	plan := mustPlan(t, deps, mustRender(t, deps, renderInput()))

	tampered := strings.Replace(plan.Manifest, testImage, "ghcr.io/attacker/miner:latest", 1)
	if tampered == plan.Manifest {
		t.Fatal("the manifest was not actually changed; the test proves nothing")
	}

	_, _, err := workloadApply(deps)(context.Background(), nil, WorkloadApplyInput{
		Manifest:  tampered,
		PlanToken: plan.PlanToken,
	})
	if err == nil {
		t.Fatal("apply accepted a manifest the token was not minted for")
	}
	assertRefusalIsReadable(t, err)
	if writer.wrote() {
		t.Errorf("apply wrote something after refusing: %+v %+v", writer.created, writer.createdNetworks)
	}
}

// TestWorkloadApplyRefusesAnExpiredToken: a plan is an agreement about a moment.
// Fifteen minutes later the quota, the images and the workload itself may all
// have moved, so consent has to be asked for again rather than assumed.
func TestWorkloadApplyRefusesAnExpiredToken(t *testing.T) {
	writer := &fakeWriter{}
	deps := writeDeps(&writeReader{}, writer)
	manifest := mustRender(t, deps, renderInput())

	desired, ferr := decodeManifest(manifest, testNamespace)
	if ferr != nil {
		t.Fatalf("decoding the rendered manifest: %+v", ferr)
	}
	canonical, err := canonicalJSON(desired)
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	stale := mintPlanToken(testPlanKey, canonical, testProject, "", time.Now().Add(-time.Minute))

	_, _, err = workloadApply(deps)(context.Background(), nil, WorkloadApplyInput{
		Manifest:  manifest,
		PlanToken: stale,
	})
	if err == nil {
		t.Fatal("apply accepted an expired token")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("error = %q, want it to say the plan expired", err)
	}
	assertRefusalIsReadable(t, err)
	if writer.wrote() {
		t.Error("apply wrote something after refusing an expired token")
	}
}

// TestWorkloadApplyRefusesATokenFromAnotherProject: the project is fixed by the
// request, so a token that travelled between conversations must not spend.
func TestWorkloadApplyRefusesATokenFromAnotherProject(t *testing.T) {
	writer := &fakeWriter{}
	planned := writeDepsFor(otherProject, &writeReader{}, &fakeWriter{})
	manifest := mustRender(t, planned, renderInput())
	plan := mustPlan(t, planned, manifest)

	applying := writeDepsFor(testProject, &writeReader{}, writer)
	_, _, err := workloadApply(applying)(context.Background(), nil, WorkloadApplyInput{
		Manifest:  plan.Manifest,
		PlanToken: plan.PlanToken,
	})
	if err == nil {
		t.Fatal("apply accepted a token minted for another project")
	}
	assertRefusalIsReadable(t, err)
	if writer.wrote() {
		t.Error("apply wrote something after refusing a token from another project")
	}
}

// TestWorkloadApplyRefusesAWorkloadThatMovedSinceThePlan: someone else changed
// it in between, so the diff the person was shown is no longer the change that
// would be made. Re-plan and ask again.
func TestWorkloadApplyRefusesAWorkloadThatMovedSinceThePlan(t *testing.T) {
	reader := &writeReader{workloads: map[string]*computev1alpha.Workload{
		wlAPIBackend: existingWorkload(deployedImage, "7"),
	}}
	writer := &fakeWriter{networks: map[string]bool{workloadspec.DefaultNetwork: true}}
	deps := writeDeps(reader, writer)
	plan := mustPlan(t, deps, mustRender(t, deps, renderInput()))

	// Someone else edits the workload between the plan and the apply.
	reader.workloads[wlAPIBackend] = existingWorkload("ghcr.io/acme/api:1.2.0", "8")

	_, _, err := workloadApply(deps)(context.Background(), nil, WorkloadApplyInput{
		Manifest:  plan.Manifest,
		PlanToken: plan.PlanToken,
	})
	if err == nil {
		t.Fatal("apply accepted a plan made against an older version of the workload")
	}
	assertRefusalIsReadable(t, err)
	if writer.wrote() {
		t.Error("apply wrote something after the workload moved underneath the plan")
	}
}

// TestWorkloadApplyWritesNothingWhenTheServerRejectsIt: the plan may be minutes
// old and quota, references and the catalogs all move. The check runs again,
// and a rejection stops the apply before the network is created too — a
// half-applied plan is worse than a refused one.
func TestWorkloadApplyWritesNothingWhenTheServerRejectsIt(t *testing.T) {
	writer := &fakeWriter{}
	deps := writeDeps(&writeReader{}, writer)
	plan := mustPlan(t, deps, mustRender(t, deps, renderInput()))

	// Accepted at plan time, refused now.
	writer.dryRunErr = rejection("spec.template.spec.volumes[0].name",
		"volume must be attached at least 1 time")

	_, _, err := workloadApply(deps)(context.Background(), nil, WorkloadApplyInput{
		Manifest:  plan.Manifest,
		PlanToken: plan.PlanToken,
	})
	if err == nil {
		t.Fatal("apply proceeded after the server rejected the workload")
	}
	if writer.wrote() {
		t.Errorf("apply wrote something after a rejected check: workloads %+v, networks %+v",
			writer.created, writer.createdNetworks)
	}
}

// TestWorkloadApplyRefusesAMalformedToken: a token the model made up, or one
// truncated in transit, is refused the same way — and the message says which
// tool issues real ones.
func TestWorkloadApplyRefusesAMalformedToken(t *testing.T) {
	writer := &fakeWriter{}
	deps := writeDeps(&writeReader{}, writer)
	manifest := mustRender(t, deps, renderInput())

	for name, token := range map[string]string{
		"empty":        "",
		"no expiry":    "bm90LWEtdG9rZW4",
		"bad expiry":   "bm90LWEtdG9rZW4.soon",
		"not base64":   "!!!!.99999999999",
		"wrong length": "AAAA.99999999999",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := workloadApply(deps)(context.Background(), nil, WorkloadApplyInput{
				Manifest:  manifest,
				PlanToken: token,
			})
			if err == nil {
				t.Fatalf("apply accepted a %s token", name)
			}
			assertRefusalIsReadable(t, err)
		})
	}
	if writer.wrote() {
		t.Error("apply wrote something while refusing made-up tokens")
	}
}

// ------------------------------------------------------------ wiring, wire

// TestWriteToolsFailWhenDepsAreUnavailable: an unauthenticated request must
// fail every write tool, render included. Rendering reads nothing, but it must
// not be a probe an unauthenticated caller can use either.
func TestWriteToolsFailWhenDepsAreUnavailable(t *testing.T) {
	denied := func(context.Context) (ToolDeps, error) {
		return ToolDeps{}, errors.New("no bearer token on the request")
	}
	ctx := context.Background()

	if _, _, err := workloadRender(denied)(ctx, nil, renderInput()); err == nil {
		t.Error("compute_workload_render should fail without deps")
	}
	if _, _, err := workloadValidate(denied)(ctx, nil, WorkloadValidateInput{Manifest: "x"}); err == nil {
		t.Error("compute_workload_validate should fail without deps")
	}
	if _, _, err := workloadPlan(denied)(ctx, nil, WorkloadPlanInput{Manifest: "x"}); err == nil {
		t.Error("compute_workload_plan should fail without deps")
	}
	if _, _, err := workloadApply(denied)(ctx, nil, WorkloadApplyInput{Manifest: "x"}); err == nil {
		t.Error("compute_workload_apply should fail without deps")
	}
}

// TestWriteToolsExplainAMissingWriter: a server built for diagnosis only has no
// Writer and no plan key. Both are wiring mistakes, and saying so beats a nil
// dereference in a handler or a token nothing can check.
func TestWriteToolsExplainAMissingWriter(t *testing.T) {
	ctx := context.Background()

	diagnoseOnly := func(context.Context) (ToolDeps, error) {
		return ToolDeps{Reader: &writeReader{}, Namespace: testNamespace, Project: testProject}, nil
	}
	_, _, err := workloadValidate(diagnoseOnly)(ctx, nil, WorkloadValidateInput{Manifest: "x"})
	if err == nil || !strings.Contains(err.Error(), "whoever operates this server") {
		t.Errorf("compute_workload_validate error = %v, want it to name the wiring mistake", err)
	}

	noKey := func(context.Context) (ToolDeps, error) {
		return ToolDeps{
			Reader: &writeReader{}, Writer: &fakeWriter{},
			Namespace: testNamespace, Project: testProject,
		}, nil
	}
	if _, _, err := workloadPlan(noKey)(ctx, nil, WorkloadPlanInput{Manifest: "x"}); err == nil {
		t.Error("compute_workload_plan minted a token with no key to sign it")
	}
	if _, _, err := workloadApply(noKey)(ctx, nil, WorkloadApplyInput{Manifest: "x"}); err == nil {
		t.Error("compute_workload_apply accepted a token with no key to check it")
	}

	// A request that never said which project it is for cannot bind a plan to
	// one, and a token that binds to "" would spend anywhere.
	noProject := func(context.Context) (ToolDeps, error) {
		return ToolDeps{
			Reader: &writeReader{}, Writer: &fakeWriter{},
			Namespace: testNamespace, PlanTokenKey: testPlanKey,
		}, nil
	}
	if _, _, err := workloadPlan(noProject)(ctx, nil, WorkloadPlanInput{Manifest: "x"}); err == nil {
		t.Error("compute_workload_plan minted a token bound to no project")
	}
}

// TestPlanToApplyOverTheWire proves registration and the schemas, not just the
// handlers: a tool that is never wired into RegisterTools passes every test
// above and is uncallable in production, and an output the SDK cannot encode
// reaches the model as nothing at all.
func TestPlanToApplyOverTheWire(t *testing.T) {
	ctx := context.Background()
	writer := &fakeWriter{}
	deps := writeDeps(&writeReader{}, writer)

	server := mcp.NewServer(&mcp.Implementation{Name: testServerName, Version: testImplVersion}, nil)
	RegisterTools(server, deps)

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

	call := func(name string, args map[string]any, out any) {
		t.Helper()
		res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("calling %s: %v", name, err)
		}
		if res.IsError {
			t.Fatalf("%s returned an error result: %+v", name, res.Content)
		}
		// Round-tripped through the wire's JSON, so the output schema is
		// exercised as the model would receive it.
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("marshalling %s output: %v", name, err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decoding %s output: %v", name, err)
		}
	}

	var rendered WorkloadRenderOutput
	call(ToolWorkloadRender, map[string]any{
		"name":  wlAPIBackend,
		"image": testImage,
		"placements": []map[string]any{
			{"cityCodes": []string{cityDFW}, "minReplicas": 2},
		},
	}, &rendered)
	if rendered.Manifest == "" {
		t.Fatal("render returned no manifest over the wire")
	}

	var planned WorkloadPlanOutput
	call(ToolWorkloadPlan, map[string]any{"manifest": rendered.Manifest}, &planned)
	if !planned.Valid || planned.PlanToken == "" {
		t.Fatalf("plan over the wire returned %+v, want a token", planned)
	}

	var applied WorkloadApplyOutput
	call(ToolWorkloadApply, map[string]any{
		"manifest":  planned.Manifest,
		"planToken": planned.PlanToken,
	}, &applied)

	if applied.Action != actionCreate || applied.Workload != wlAPIBackend {
		t.Errorf("apply over the wire reported %+v, want a create of %q", applied, wlAPIBackend)
	}
	if len(writer.created) != 1 {
		t.Fatalf("created %d workloads over the wire, want exactly 1", len(writer.created))
	}
}

// assertRefusalIsReadable pins what every refusal owes the person reading it:
// that nothing happened, and what to do next. A refusal they cannot act on
// reads as a broken tool, and the next thing they try is the CLI.
func assertRefusalIsReadable(t *testing.T, err error) {
	t.Helper()
	msg := err.Error()
	if !strings.Contains(msg, "nothing was created") {
		t.Errorf("refusal = %q, want it to say plainly that nothing was created", msg)
	}
	if !strings.Contains(msg, ToolWorkloadPlan) {
		t.Errorf("refusal = %q, want it to name %s as the way forward", msg, ToolWorkloadPlan)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
