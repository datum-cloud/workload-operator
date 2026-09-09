package deploy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/compute/internal/cmd/compute/build"
	"go.datum.net/compute/internal/cmd/compute/util"
	"go.datum.net/compute/internal/cmd/compute/watch"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	locationsv1alpha1 "go.miloapis.com/locations/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type options struct {
	image            string
	build            string
	instanceType     string
	locations        []string
	locationSelector string
	cities           []string
	min              int32
	port             int32
	file             string
	yes              bool
}

func Command() *cobra.Command {
	opts := &options{}

	cmd := &cobra.Command{
		Use:   "deploy [workload-name]",
		Short: "Deploy or update a workload",
		Long: `Deploy a container image as a workload across one or more locations.

If no arguments are given, an interactive prompt guides you through the deployment.
Use -f to apply a workload manifest file instead of flags.

Use --build to build and push the image before deploying, instead of running
'datumctl compute build' separately. It builds from the given directory (default
Dockerfile discovery, no build-arg/target overrides — use 'datumctl compute build'
directly if you need those) and pushes to --image, which the deployed workload
then pins by digest rather than the tag you gave it. It also analyzes and
auto-fixes common compatibility issues, rewriting the Dockerfile in place when
a fix is applied — same as 'datumctl compute build --fix'.`,
		Args: cobra.MaximumNArgs(1),
		Example: `  # Deploy with flags
  datumctl compute deploy api --image=ghcr.io/acme/api:1.4.2 --location=us-east-1,eu-west-1 --min=2 --port=8080

  # Build and deploy in one step (builds ., pushes to --image, deploys that digest)
  datumctl compute deploy api --build --image=ghcr.io/acme/api:1.4.2 --location=us-east-1,eu-west-1

  # Build from another directory
  datumctl compute deploy api --build=./api --image=ghcr.io/acme/api:1.4.2 --location=us-east-1,eu-west-1

  # Deploy to every location in one or more cities
  datumctl compute deploy api --image=ghcr.io/acme/api:1.4.2 --city=DFW,IAD

  # Select locations by any topology label
  datumctl compute deploy api --image=ghcr.io/acme/api:1.4.2 --location-selector='topology.datum.net/region=us-east-1'

  # Interactive mode
  datumctl compute deploy

  # Manifest-driven
  datumctl compute deploy -f workload.yaml`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeploy(cmd, args, opts)
		},
		ValidArgsFunction: util.CompleteWorkloadNamesAndFlags,
	}

	cmd.Flags().StringVar(&opts.image, "image", "", "Container image to deploy (e.g. ghcr.io/acme/api:1.4.2); also the push destination when --build is set")
	cmd.Flags().StringVar(&opts.build, "build", "", "Build and push the image from this directory before deploying (default \".\" if given with no value)")
	cmd.Flags().Lookup("build").NoOptDefVal = "."
	cmd.Flags().StringVar(&opts.instanceType, "instance-type", "datumcloud/d1-standard-2", "Instance type (e.g. datumcloud/d1-standard-2)")
	cmd.Flags().StringSliceVar(&opts.locations, "location", nil, "One or more locations to deploy to (e.g. us-east-1,eu-west-1)")
	cmd.Flags().StringVar(&opts.locationSelector, "location-selector", "", "Select every location whose topology matches a label selector (e.g. 'topology.datum.net/city-code=DFW' or 'topology.datum.net/region in (us-east-1,eu-west-1)')")
	cmd.Flags().StringSliceVar(&opts.cities, "city", nil, "Deploy to every location in these cities (e.g. DFW,IAD); shorthand for a --location-selector on topology.datum.net/city-code")
	cmd.Flags().Int32Var(&opts.min, "min", 1, "Minimum number of instances per location")
	cmd.Flags().Int32Var(&opts.port, "port", 0, "Port to expose on the workload (optional)")
	cmd.Flags().StringVarP(&opts.file, "file", "f", "", "Path to a workload manifest file")
	cmd.Flags().BoolVarP(&opts.yes, "yes", "y", false, "Skip confirmation prompts")
	_ = cmd.RegisterFlagCompletionFunc("location", util.CompletePlacementLocations)
	_ = cmd.RegisterFlagCompletionFunc("location-selector", util.CompleteLocationSelector)
	_ = cmd.RegisterFlagCompletionFunc("city", util.CompleteCityCodes)

	return cmd
}

func runDeploy(cmd *cobra.Command, args []string, opts *options) error {
	// Determine path.
	if opts.file != "" {
		if opts.build != "" {
			return fmt.Errorf("--build cannot be combined with -f; a manifest already specifies its own image")
		}
		return deployFromFile(cmd, opts)
	}

	if len(args) > 0 && opts.image != "" {
		if opts.build != "" {
			if kraftfile := build.FindKraftfile(opts.build); kraftfile != "" {
				return fmt.Errorf(
					"found %s: Kraftfile-based builds aren't supported by --build, since they delegate entirely to the "+
						"unikraft CLI. Run the build and deploy steps separately instead:\n"+
						"  datumctl compute build --push --output <ref> .\n"+
						"  datumctl compute deploy --image <ref>", kraftfile)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Building %s and pushing to %s...\n", opts.build, opts.image)
			digest, err := build.Run(cmd.Context(), &build.Options{
				ContextDir: opts.build,
				Dockerfile: "Dockerfile",
				Output:     opts.image,
				Push:       true, // a combined build+deploy step always pushes: there's nothing to confirm
				Fix:        true, // deploying a broken image is worse than auto-fixing and rebuilding
			})
			if err != nil {
				return err
			}
			opts.image = digest
		}
		return deployFromFlags(cmd, args[0], opts)
	}

	return fmt.Errorf("workload name and --image are required, or use -f to specify a manifest file")
}

// deployFromFlags implements Path A: deploy a workload using CLI flags.
func deployFromFlags(cmd *cobra.Command, workloadName string, opts *options) error {
	project := util.ProjectFromCmd(cmd)
	if project == "" {
		return fmt.Errorf("no project set — pass --project or run 'datumctl config set project <name>'")
	}
	if opts.image == "" {
		return fmt.Errorf("--image is required")
	}
	placementFlags := 0
	for _, set := range []bool{len(opts.locations) > 0, len(opts.cities) > 0, opts.locationSelector != ""} {
		if set {
			placementFlags++
		}
	}
	if placementFlags == 0 {
		return fmt.Errorf("--location is required (e.g. --location=us-east-1,eu-west-1); or use --city to deploy to every location in a city, or --location-selector to select locations by topology")
	}
	if placementFlags > 1 {
		return fmt.Errorf("--location, --city, and --location-selector are mutually exclusive")
	}
	var locationSelector *metav1.LabelSelector
	if opts.locationSelector != "" {
		parsed, err := metav1.ParseToLabelSelector(opts.locationSelector)
		if err != nil {
			return fmt.Errorf("invalid --location-selector %q: %w", opts.locationSelector, err)
		}
		locationSelector = parsed
	}
	if len(opts.cities) > 0 {
		locationSelector = citySelector(opts.cities)
	}
	instanceType := opts.instanceType
	if instanceType == "" {
		instanceType = "datumcloud/d1-standard-2"
	}

	c, err := util.NewClient(project)
	if err != nil {
		return err
	}

	ctx := context.Background()
	out := cmd.OutOrStdout()
	locations := opts.locations

	if err := ensureNetwork(ctx, cmd, c, "default", project, opts); err != nil {
		return err
	}

	fmt.Fprintf(out, "Resolving workload %q in project %s...\n", workloadName, project)

	var workload computev1alpha.Workload
	creating := false
	if err := c.Get(ctx, types.NamespacedName{Namespace: util.ResourceNamespace, Name: workloadName}, &workload); err != nil {
		if k8serrors.IsNotFound(err) {
			creating = true
			workload = computev1alpha.Workload{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: util.ResourceNamespace,
					Name:      workloadName,
				},
			}
		} else {
			return fmt.Errorf("getting workload: %w", err)
		}
	}

	// Build spec.
	tcp := corev1.ProtocolTCP
	container := computev1alpha.SandboxContainer{
		Name:  "app",
		Image: opts.image,
	}
	if opts.port > 0 {
		container.Ports = []computev1alpha.NamedPort{
			{Name: "http", Port: opts.port, Protocol: &tcp},
		}
	}

	locationRefs := make([]locationsv1alpha1.LocationReference, 0, len(locations))
	for _, name := range locations {
		locationRefs = append(locationRefs, locationsv1alpha1.LocationReference{Name: name})
	}
	// All locations go into one "default" placement.
	placement := computev1alpha.WorkloadPlacement{
		Name:             "default",
		Locations:        locationRefs,
		LocationSelector: locationSelector,
		ScaleSettings: computev1alpha.HorizontalScaleSettings{
			MinReplicas:              opts.min,
			InstanceManagementPolicy: computev1alpha.OrderedReadyInstanceManagementPolicyType,
		},
	}

	workload.Spec = computev1alpha.WorkloadSpec{
		Template: computev1alpha.InstanceTemplateSpec{
			Spec: computev1alpha.InstanceSpec{
				Runtime: computev1alpha.InstanceRuntimeSpec{
					Resources: computev1alpha.InstanceRuntimeResources{
						InstanceType: instanceType,
					},
					Sandbox: &computev1alpha.SandboxRuntime{
						Containers: []computev1alpha.SandboxContainer{container},
					},
				},
				NetworkInterfaces: []computev1alpha.InstanceNetworkInterface{
					{
						// TODO: "default" network name is a convention; confirm with platform team.
						Network: networkingv1alpha.NetworkRef{Name: "default"},
					},
				},
			},
		},
		Placements: []computev1alpha.WorkloadPlacement{placement},
	}

	fmt.Fprintf(out, "  Placement \"default\": %s, min=%d\n", describePlacementLocations(placement), opts.min)

	// Prompt unless --yes or non-interactive.
	if !opts.yes && term.IsTerminal(int(os.Stdin.Fd())) {
		_, _ = fmt.Fprint(out, "Apply? (Y/n): ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return fmt.Errorf("reading confirmation: %w", err)
		}
		line = strings.TrimSpace(line)
		if line == "n" || line == "N" {
			_, _ = fmt.Fprintln(out, "Aborted.")
			return nil
		}
	}

	if creating {
		workload.Namespace = util.ResourceNamespace
		if err := c.Create(ctx, &workload); err != nil {
			return fmt.Errorf("creating workload: %w", err)
		}
		fmt.Fprintf(out, "  workload/%s created\n", workloadName)
	} else {
		if err := c.Update(ctx, &workload); err != nil {
			return fmt.Errorf("updating workload: %w", err)
		}
		fmt.Fprintf(out, "  workload/%s updated\n", workloadName)
	}

	// Save workload.yaml.
	if err := saveWorkloadYAML(workloadName, &workload); err != nil {
		fmt.Fprintf(out, "  warning: could not save workload.yaml: %v\n", err)
	} else {
		_, _ = fmt.Fprintln(out, "Saved workload.yaml")
	}

	fmt.Fprintf(out, "Waiting for rollout. Ctrl-C to detach (rollout continues in background).\n\n")

	watchCtx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer cancel()
	return watch.Rollout(watchCtx, c, out, project, workload.UID)
}

// deployFromFile implements Path C: deploy from a manifest file.
func deployFromFile(cmd *cobra.Command, opts *options) error {
	project := util.ProjectFromCmd(cmd)
	if project == "" {
		return fmt.Errorf("no project set — pass --project or run 'datumctl config set project <name>'")
	}

	data, err := os.ReadFile(opts.file)
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}

	var workload computev1alpha.Workload
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	if err := decoder.Decode(&workload); err != nil {
		return fmt.Errorf("decoding manifest: %w", err)
	}

	workload.Namespace = util.ResourceNamespace

	c, err := util.NewClient(project)
	if err != nil {
		return err
	}

	ctx := context.Background()
	out := cmd.OutOrStdout()

	for _, iface := range workload.Spec.Template.Spec.NetworkInterfaces {
		if err := ensureNetwork(ctx, cmd, c, iface.Network.Name, project, opts); err != nil {
			return err
		}
	}

	var existing computev1alpha.Workload
	creating := false
	if err := c.Get(ctx, types.NamespacedName{Namespace: util.ResourceNamespace, Name: workload.Name}, &existing); err != nil {
		if k8serrors.IsNotFound(err) {
			creating = true
		} else {
			return fmt.Errorf("getting workload: %w", err)
		}
	}

	var diffLines []string
	if !creating {
		diffLines = manifestDiff(existing, workload)
		for _, l := range diffLines {
			_, _ = fmt.Fprintln(out, l)
		}
		if len(diffLines) == 0 {
			_, _ = fmt.Fprintln(out, "No changes detected.")
		}
	}

	// Prompt unless --yes or non-interactive.
	if !opts.yes && term.IsTerminal(int(os.Stdin.Fd())) {
		_, _ = fmt.Fprint(out, "Apply? (Y/n): ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return fmt.Errorf("reading confirmation: %w", err)
		}
		line = strings.TrimSpace(line)
		if line == "n" || line == "N" {
			_, _ = fmt.Fprintln(out, "Aborted.")
			return nil
		}
	}

	if creating {
		if err := c.Create(ctx, &workload); err != nil {
			return fmt.Errorf("creating workload: %w", err)
		}
		fmt.Fprintf(out, "  workload/%s created\n", workload.Name)
	} else {
		workload.ResourceVersion = existing.ResourceVersion
		if err := c.Update(ctx, &workload); err != nil {
			return fmt.Errorf("updating workload: %w", err)
		}
		fmt.Fprintf(out, "  workload/%s updated\n", workload.Name)
	}

	fmt.Fprintf(out, "Waiting for rollout. Ctrl-C to detach (rollout continues in background).\n\n")

	watchCtx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer cancel()
	return watch.Rollout(watchCtx, c, out, project, workload.UID)
}

// saveWorkloadYAML marshals the workload and writes it to workload.yaml in the
// current directory.
func saveWorkloadYAML(_ string, workload *computev1alpha.Workload) error {
	workload.TypeMeta = metav1.TypeMeta{
		APIVersion: "compute.datumapis.com/v1alpha",
		Kind:       "Workload",
	}

	data, err := sigsyaml.Marshal(workload)
	if err != nil {
		return fmt.Errorf("marshalling workload: %w", err)
	}

	header := "# Managed by datumctl compute deploy. Commit this file to manage your workload declaratively.\n" +
		"# Apply changes with: datumctl compute deploy -f workload.yaml\n"

	return os.WriteFile("workload.yaml", append([]byte(header), data...), 0o644)
}

// imageFromWorkload returns the first container image found in a workload, or empty string.
func imageFromWorkload(w computev1alpha.Workload) string {
	sb := w.Spec.Template.Spec.Runtime.Sandbox
	if sb != nil && len(sb.Containers) > 0 {
		return sb.Containers[0].Image
	}
	return ""
}

// ensureNetwork checks if the named network exists and, if not, offers to create it.
// It creates a minimal auto-IPAM IPv4 network on behalf of the user.
func ensureNetwork(ctx context.Context, cmd *cobra.Command, c client.Client, networkName, project string, opts *options) error {
	var network networkingv1alpha.Network
	err := c.Get(ctx, types.NamespacedName{Namespace: util.ResourceNamespace, Name: networkName}, &network)
	if err == nil {
		return nil
	}
	if !k8serrors.IsNotFound(err) {
		return fmt.Errorf("checking network %q: %w", networkName, err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "  Network %q does not exist in project %s.\n", networkName, project)

	if !opts.yes && term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintf(out, "  Create it now? (Y/n): ")
		line, readErr := bufio.NewReader(os.Stdin).ReadString('\n')
		if readErr != nil {
			return fmt.Errorf("reading confirmation: %w", readErr)
		}
		line = strings.TrimSpace(line)
		if line == "n" || line == "N" {
			return fmt.Errorf("network %q is required — create it with: datumctl apply -f network.yaml --project %s", networkName, project)
		}
	} else if !opts.yes {
		return fmt.Errorf("network %q not found in project %s — use --yes to auto-create or create it first", networkName, project)
	}

	ipv4Mode := networkingv1alpha.NetworkIPAMModeAuto
	newNetwork := networkingv1alpha.Network{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: util.ResourceNamespace,
			Name:      networkName,
		},
		Spec: networkingv1alpha.NetworkSpec{
			IPAM: networkingv1alpha.NetworkIPAM{
				Mode: ipv4Mode,
			},
		},
	}
	if err := c.Create(ctx, &newNetwork); err != nil {
		return fmt.Errorf("creating network %q: %w", networkName, err)
	}
	fmt.Fprintf(out, "  network/%s created\n", networkName)
	return nil
}

// manifestDiff computes diff lines between an existing and desired workload.
func manifestDiff(existing, desired computev1alpha.Workload) []string {
	var lines []string

	oldImage := imageFromWorkload(existing)
	newImage := imageFromWorkload(desired)
	if oldImage != newImage {
		lines = append(lines, fmt.Sprintf("  image: %s → %s", oldImage, newImage))
	}

	// Compare placements by name.
	oldPlacements := make(map[string]computev1alpha.WorkloadPlacement)
	for _, p := range existing.Spec.Placements {
		oldPlacements[p.Name] = p
	}
	newPlacements := make(map[string]computev1alpha.WorkloadPlacement)
	for _, p := range desired.Spec.Placements {
		newPlacements[p.Name] = p
	}

	for name, np := range newPlacements {
		if op, ok := oldPlacements[name]; ok {
			if op.ScaleSettings.MinReplicas != np.ScaleSettings.MinReplicas {
				lines = append(lines, fmt.Sprintf("  placement %q min replicas: %d → %d",
					name, op.ScaleSettings.MinReplicas, np.ScaleSettings.MinReplicas))
			}
			if before, after := describePlacementLocations(op), describePlacementLocations(np); before != after {
				lines = append(lines, fmt.Sprintf("  placement %q: %s → %s", name, before, after))
			}
		} else {
			lines = append(lines, fmt.Sprintf("  + new placement %q: %s", name, describePlacementLocations(np)))
		}
	}
	for name := range oldPlacements {
		if _, ok := newPlacements[name]; !ok {
			lines = append(lines, fmt.Sprintf("  - removed placement %q", name))
		}
	}

	return lines
}

// describePlacementLocations says where a placement runs the way the CLI
// prints it: the locations it names, or the selector it resolves through.
func describePlacementLocations(p computev1alpha.WorkloadPlacement) string {
	if p.LocationSelector != nil {
		return fmt.Sprintf("selector=[%s]", metav1.FormatLabelSelector(p.LocationSelector))
	}
	names := make([]string, 0, len(p.Locations))
	for _, ref := range p.Locations {
		names = append(names, ref.Name)
	}
	return fmt.Sprintf("locations=[%s]", strings.Join(names, ", "))
}

// citySelector is the selector --city stands for: every location whose
// topology places it in one of the given cities. One city is a plain
// equality; several become an In expression.
func citySelector(cities []string) *metav1.LabelSelector {
	if len(cities) == 1 {
		return &metav1.LabelSelector{
			MatchLabels: map[string]string{locationsv1alpha1.TopologyCityCodeKey: cities[0]},
		}
	}
	return &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key:      locationsv1alpha1.TopologyCityCodeKey,
			Operator: metav1.LabelSelectorOpIn,
			Values:   cities,
		}},
	}
}
