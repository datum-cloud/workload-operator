// SPDX-License-Identifier: AGPL-3.0-only

package workloadspec

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/compute/internal/validation"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

const (
	testSSHKey       = "user:ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILPbDbsv9fgEnam9iJ5b51Na/WieeiKCJRC0+m7fRwPk vscode@42aafaf8293e"
	testCityCode     = "DFW"
	testImage        = "ghcr.io/acme/api:1.4.2"
	testWorkload     = "api"
	testPlacement    = "us"
	testMountPath    = "/etc/app"
	testCredsPath    = "/etc/creds"
	testConfigMap    = "app-config"
	testSecretName   = "db-creds"
	testPortName     = "http"
	testEnvLiteral   = "LOG_LEVEL"
	testEnvFromSecre = "DB_PASSWORD"
	testSharedName   = "shared"
	testSSHKeysPath  = "vm.sshKeys[0]"
	testEnvValue     = "debug"
	testSecretKey    = "password"
	testLabelValue   = "frontend"
)

// validInput is the smallest Input that renders: a sandbox in one placement.
func validInput(tweaks ...func(*Input)) Input {
	in := Input{
		Name:  testWorkload,
		Image: testImage,
		Placements: []Placement{
			{Name: testPlacement, CityCodes: []string{testCityCode}, MinReplicas: 2},
		},
	}
	for _, tweak := range tweaks {
		tweak(&in)
	}
	return in
}

func mustRender(t *testing.T, in Input) *computev1alpha.Workload {
	t.Helper()

	w, err := Render(in)
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	return w
}

// vmInput turns a sandbox input into the equivalent VM input.
func vmInput(tweaks ...func(*Input)) Input {
	return validInput(append([]func(*Input){func(in *Input) {
		in.Image = ""
		in.VM = &VMInput{SSHKeys: []string{testSSHKey}}
	}}, tweaks...)...)
}

func TestRenderMinimalSandbox(t *testing.T) {
	w := mustRender(t, validInput())

	if got, want := w.APIVersion, computev1alpha.GroupVersion.String(); got != want {
		t.Errorf("apiVersion = %q, want %q", got, want)
	}
	if got, want := w.Kind, "Workload"; got != want {
		t.Errorf("kind = %q, want %q", got, want)
	}
	if got, want := w.Namespace, Namespace; got != want {
		t.Errorf("namespace = %q, want %q", got, want)
	}

	spec := w.Spec.Template.Spec
	if got, want := spec.Runtime.Resources.InstanceType, DefaultInstanceType; got != want {
		t.Errorf("instanceType = %q, want %q", got, want)
	}
	if spec.Runtime.Resources.Requests != nil {
		t.Error("runtime.resources.requests must stay unset: the webhook rejects it as not implemented")
	}
	if spec.Runtime.VirtualMachine != nil {
		t.Error("virtualMachine set on a sandbox render")
	}

	containers := spec.Runtime.Sandbox.Containers
	if len(containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(containers))
	}
	if got, want := containers[0].Image, testImage; got != want {
		t.Errorf("image = %q, want %q", got, want)
	}
	if containers[0].Resources != nil {
		t.Error("containers[0].resources must stay unset: the webhook rejects it as not implemented")
	}

	placements := w.Spec.Placements
	if len(placements) != 1 {
		t.Fatalf("placements = %d, want 1", len(placements))
	}
	if got, want := placements[0].ScaleSettings.MinReplicas, int32(2); got != want {
		t.Errorf("minReplicas = %d, want %d", got, want)
	}
	if got, want := placements[0].ScaleSettings.InstanceManagementPolicy,
		computev1alpha.OrderedReadyInstanceManagementPolicyType; got != want {
		t.Errorf("instanceManagementPolicy = %q, want %q", got, want)
	}
}

func TestRenderNetworkInterfaceLeavesImmutableFieldsDefaulted(t *testing.T) {
	spec := mustRender(t, validInput()).Spec.Template.Spec

	if len(spec.NetworkInterfaces) != 1 {
		t.Fatalf("networkInterfaces = %d, want exactly 1", len(spec.NetworkInterfaces))
	}

	iface := spec.NetworkInterfaces[0]
	if got, want := iface.Network.Name, DefaultNetwork; got != want {
		t.Errorf("network = %q, want %q", got, want)
	}
	if iface.Name != "" || iface.IPFamilies != nil || iface.Addresses != nil || iface.ReclaimPolicy != "" {
		t.Errorf("interface should leave create-time-only fields to the API server, got %+v", iface)
	}
	if iface.NetworkPolicy != nil {
		t.Error("no ports were requested, so no network policy should be rendered")
	}
}

func TestRenderDefaults(t *testing.T) {
	w := mustRender(t, Input{
		Name:       testWorkload,
		Image:      testImage,
		Placements: []Placement{{CityCodes: []string{testCityCode}}},
	})

	p := w.Spec.Placements[0]
	if got, want := p.Name, DefaultPlacementName; got != want {
		t.Errorf("placement name = %q, want %q", got, want)
	}
	if got, want := p.ScaleSettings.MinReplicas, DefaultMinReplicas; got != want {
		t.Errorf("minReplicas = %d, want %d", got, want)
	}
	if got, want := w.Spec.Template.Spec.NetworkInterfaces[0].Network.Name, DefaultNetwork; got != want {
		t.Errorf("network = %q, want %q", got, want)
	}
}

func TestRenderPortsOpenIngress(t *testing.T) {
	spec := mustRender(t, validInput(func(in *Input) {
		in.Ports = []Port{
			{Name: testPortName, Port: 8080},
			{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP},
		}
	})).Spec.Template.Spec

	ports := spec.Runtime.Sandbox.Containers[0].Ports
	if len(ports) != 2 {
		t.Fatalf("container ports = %d, want 2", len(ports))
	}
	if got, want := *ports[0].Protocol, corev1.ProtocolTCP; got != want {
		t.Errorf("ports[0].protocol = %q, want %q (the default)", got, want)
	}
	if got, want := *ports[1].Protocol, corev1.ProtocolUDP; got != want {
		t.Errorf("ports[1].protocol = %q, want %q", got, want)
	}

	policy := spec.NetworkInterfaces[0].NetworkPolicy
	if policy == nil {
		t.Fatal("declared ports must open matching ingress rules")
	}
	if len(policy.Ingress) != 2 {
		t.Fatalf("ingress rules = %d, want 2", len(policy.Ingress))
	}
	if got, want := policy.Ingress[0].Ports[0].Port.IntValue(), 8080; got != want {
		t.Errorf("ingress[0] port = %d, want %d", got, want)
	}
	if got, want := policy.Ingress[0].From[0].IPBlock.CIDR, anyIPv4CIDR; got != want {
		t.Errorf("ingress[0] cidr = %q, want %q", got, want)
	}
}

func TestRenderEnv(t *testing.T) {
	spec := mustRender(t, validInput(func(in *Input) {
		in.Env = []EnvVar{
			{Name: testEnvLiteral, Value: testEnvValue},
			{Name: testEnvFromSecre, SecretKeyRef: &KeyRef{Name: testSecretName, Key: testSecretKey}},
			{Name: "REGION", ConfigMapKeyRef: &KeyRef{Name: testConfigMap, Key: "region"}},
		}
	})).Spec.Template.Spec

	want := []corev1.EnvVar{
		{Name: testEnvLiteral, Value: testEnvValue},
		{
			Name: testEnvFromSecre,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: testSecretName},
					Key:                  testSecretKey,
				},
			},
		},
		{
			Name: "REGION",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: testConfigMap},
					Key:                  "region",
				},
			},
		},
	}
	if delta := cmp.Diff(want, spec.Runtime.Sandbox.Containers[0].Env); delta != "" {
		t.Errorf("env mismatch (-want +got):\n%s", delta)
	}
}

func TestRenderConfigMountsAreAttached(t *testing.T) {
	spec := mustRender(t, validInput(func(in *Input) {
		in.ConfigMounts = []Mount{
			{ConfigMap: testConfigMap, MountPath: testMountPath},
			{Secret: testSecretName, MountPath: testCredsPath},
		}
	})).Spec.Template.Spec

	if len(spec.Volumes) != 2 {
		t.Fatalf("volumes = %d, want 2", len(spec.Volumes))
	}
	// A configMap volume names its source with `name`, a secret volume with
	// `secretName`.
	if got, want := spec.Volumes[0].ConfigMap.Name, testConfigMap; got != want {
		t.Errorf("configMap volume name = %q, want %q", got, want)
	}
	if got, want := spec.Volumes[1].Secret.SecretName, testSecretName; got != want {
		t.Errorf("secret volume secretName = %q, want %q", got, want)
	}

	attachments := spec.Runtime.Sandbox.Containers[0].VolumeAttachments
	if len(attachments) != 2 {
		t.Fatalf("volumeAttachments = %d, want 2", len(attachments))
	}
	for i, a := range attachments {
		if a.Name != spec.Volumes[i].Name {
			t.Errorf("attachment %d = %q, does not match volume %q", i, a.Name, spec.Volumes[i].Name)
		}
		if a.MountPath == nil {
			t.Fatalf("attachment %d has no mount path", i)
		}
	}
	if got, want := *attachments[0].MountPath, testMountPath; got != want {
		t.Errorf("mountPath = %q, want %q", got, want)
	}
}

func TestRenderExplicitMountNameResolvesCollision(t *testing.T) {
	volumes := mustRender(t, validInput(func(in *Input) {
		in.ConfigMounts = []Mount{
			{ConfigMap: testSharedName, MountPath: testMountPath},
			{Name: testSharedName + "-secret", Secret: testSharedName, MountPath: testCredsPath},
		}
	})).Spec.Template.Spec.Volumes

	if got, want := volumes[0].Name, testSharedName; got != want {
		t.Errorf("volumes[0].name = %q, want %q", got, want)
	}
	if got, want := volumes[1].Name, testSharedName+"-secret"; got != want {
		t.Errorf("volumes[1].name = %q, want %q", got, want)
	}
}

func TestRenderPublicIPv4(t *testing.T) {
	iface := mustRender(t, validInput(func(in *Input) {
		in.PublicIPv4 = true
	})).Spec.Template.Spec.NetworkInterfaces[0]

	wantFamilies := []networkingv1alpha.IPFamily{
		networkingv1alpha.IPv4Protocol,
		networkingv1alpha.IPv6Protocol,
	}
	if delta := cmp.Diff(wantFamilies, iface.IPFamilies); delta != "" {
		t.Errorf("ipFamilies mismatch (-want +got):\n%s", delta)
	}

	wantAddresses := []computev1alpha.InstanceNetworkInterfaceAddressRequest{{Class: PublicIPv4Class}}
	if delta := cmp.Diff(wantAddresses, iface.Addresses); delta != "" {
		t.Errorf("addresses mismatch (-want +got):\n%s", delta)
	}
}

func TestRenderLabelsReachTheInstanceTemplate(t *testing.T) {
	w := mustRender(t, validInput(func(in *Input) {
		in.Labels = map[string]string{"tier": testLabelValue}
	}))

	if got, want := w.Labels["tier"], testLabelValue; got != want {
		t.Errorf("workload label = %q, want %q", got, want)
	}
	if got, want := w.Spec.Template.Labels["tier"], testLabelValue; got != want {
		t.Errorf("template label = %q, want %q", got, want)
	}
}

func TestRenderVM(t *testing.T) {
	template := mustRender(t, vmInput(func(in *Input) {
		in.Ports = []Port{{Name: "ssh", Port: 22}}
		in.ConfigMounts = []Mount{{Secret: testSecretName, MountPath: testCredsPath}}
	})).Spec.Template

	if got, want := template.Annotations[computev1alpha.SSHKeysAnnotation], testSSHKey; got != want {
		t.Errorf("ssh-keys annotation = %q, want %q", got, want)
	}

	spec := template.Spec
	if spec.Runtime.Sandbox != nil {
		t.Error("sandbox set on a VM render")
	}
	vm := spec.Runtime.VirtualMachine
	if vm == nil {
		t.Fatal("virtualMachine not rendered")
	}
	if len(vm.Ports) != 1 {
		t.Errorf("vm ports = %d, want 1", len(vm.Ports))
	}

	// The webhook requires the first attachment to be a bootable volume: a
	// disk with an image populator.
	if len(vm.VolumeAttachments) != 2 {
		t.Fatalf("volumeAttachments = %d, want 2", len(vm.VolumeAttachments))
	}
	boot := vm.VolumeAttachments[0]
	if got, want := boot.Name, BootVolumeName; got != want {
		t.Errorf("first attachment = %q, want %q", got, want)
	}
	if boot.MountPath != nil {
		t.Error("the boot disk must be attached as a device, not mounted")
	}

	bootVolume := spec.Volumes[0]
	if bootVolume.Name != BootVolumeName {
		t.Fatalf("volumes[0] = %q, want %q", bootVolume.Name, BootVolumeName)
	}
	if got, want := bootVolume.Disk.Template.Spec.Type, diskTypePDStandard; got != want {
		t.Errorf("boot disk type = %q, want %q", got, want)
	}
	if got, want := bootVolume.Disk.Template.Spec.Populator.Image.Name, DefaultBootImage; got != want {
		t.Errorf("boot image = %q, want %q", got, want)
	}
	// The image populator carries the size, so a storage request would be
	// redundant.
	if bootVolume.Disk.Template.Spec.Resources != nil {
		t.Error("boot disk should take its size from the image populator")
	}
}

func TestRenderErrors(t *testing.T) {
	cases := map[string]struct {
		input    Input
		wantPath string
	}{
		"no name": {
			input:    validInput(func(in *Input) { in.Name = "" }),
			wantPath: "name",
		},
		"invalid name": {
			input:    validInput(func(in *Input) { in.Name = "Not A Name" }),
			wantPath: "name",
		},
		"no image for a sandbox": {
			input:    validInput(func(in *Input) { in.Image = "" }),
			wantPath: "image",
		},
		"image on a vm": {
			input:    vmInput(func(in *Input) { in.Image = testImage }),
			wantPath: "image",
		},
		"no placements": {
			input:    validInput(func(in *Input) { in.Placements = nil }),
			wantPath: "placements",
		},
		"placement without city codes": {
			input:    validInput(func(in *Input) { in.Placements = []Placement{{Name: testPlacement}} }),
			wantPath: "placements[0].cityCodes",
		},
		"placement name is not a DNS label": {
			input:    validInput(func(in *Input) { in.Placements[0].Name = "US East" }),
			wantPath: "placements[0].name",
		},
		"too many replicas": {
			input:    validInput(func(in *Input) { in.Placements[0].MinReplicas = 1001 }),
			wantPath: "placements[0].minReplicas",
		},
		"negative replicas": {
			input:    validInput(func(in *Input) { in.Placements[0].MinReplicas = -1 }),
			wantPath: "placements[0].minReplicas",
		},
		"vm without ssh keys": {
			input:    vmInput(func(in *Input) { in.VM.SSHKeys = nil }),
			wantPath: "vm.sshKeys",
		},
		"ssh key without a username": {
			input: vmInput(func(in *Input) {
				_, key, _ := strings.Cut(testSSHKey, ":")
				in.VM.SSHKeys = []string{":" + key}
			}),
			wantPath: testSSHKeysPath,
		},
		"ssh key without a username separator": {
			input:    vmInput(func(in *Input) { in.VM.SSHKeys = []string{"ssh-ed25519 AAAA"} }),
			wantPath: testSSHKeysPath,
		},
		"unparseable ssh key": {
			input:    vmInput(func(in *Input) { in.VM.SSHKeys = []string{"user:not-a-key"} }),
			wantPath: testSSHKeysPath,
		},
		"duplicate port name": {
			input: validInput(func(in *Input) {
				in.Ports = []Port{{Name: testPortName, Port: 80}, {Name: testPortName, Port: 8080}}
			}),
			wantPath: "ports[1].name",
		},
		"port out of range": {
			input:    validInput(func(in *Input) { in.Ports = []Port{{Name: testPortName, Port: 70000}} }),
			wantPath: "ports[0].port",
		},
		"mount with neither configMap nor secret": {
			input: validInput(func(in *Input) {
				in.ConfigMounts = []Mount{{Name: "cfg", MountPath: testMountPath}}
			}),
			wantPath: "configMounts[0]",
		},
		"mount without a mount path": {
			input:    validInput(func(in *Input) { in.ConfigMounts = []Mount{{ConfigMap: testConfigMap}} }),
			wantPath: "configMounts[0].mountPath",
		},
		"colliding volume names": {
			input: validInput(func(in *Input) {
				in.ConfigMounts = []Mount{
					{ConfigMap: testSharedName, MountPath: testMountPath},
					{Secret: testSharedName, MountPath: testCredsPath},
				}
			}),
			wantPath: "configMounts[1].name",
		},
		"duplicate mount paths": {
			input: validInput(func(in *Input) {
				in.ConfigMounts = []Mount{
					{ConfigMap: testConfigMap, MountPath: testMountPath},
					{Secret: testSecretName, MountPath: testMountPath},
				}
			}),
			wantPath: "configMounts[1].mountPath",
		},
		"env var with two sources": {
			input: validInput(func(in *Input) {
				in.Env = []EnvVar{{
					Name:         testEnvLiteral,
					Value:        "literal",
					SecretKeyRef: &KeyRef{Name: testSecretName, Key: "k"},
				}}
			}),
			wantPath: "env[0]",
		},
		"env on a vm": {
			input:    vmInput(func(in *Input) { in.Env = []EnvVar{{Name: testEnvLiteral, Value: "b"}} }),
			wantPath: "env",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w, err := Render(tc.input)
			if err == nil {
				t.Fatalf("Render() succeeded, want an error mentioning %q", tc.wantPath)
			}
			if w != nil {
				t.Error("Render() returned a workload alongside an error")
			}
			if !strings.Contains(err.Error(), tc.wantPath) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.wantPath)
			}
		})
	}
}

// TestRenderedManifestsPassAdmission runs rendered manifests through the real
// admission validation, so this package is held to the webhook's rules rather
// than to a copy of them.
func TestRenderedManifestsPassAdmission(t *testing.T) {
	inputs := map[string]Input{
		"minimal sandbox": validInput(),
		"sandbox with ports, env, mounts and a public address": validInput(func(in *Input) {
			in.Ports = []Port{{Name: testPortName, Port: 8080}}
			in.Env = []EnvVar{
				{Name: testEnvLiteral, Value: testEnvValue},
				{Name: testEnvFromSecre, SecretKeyRef: &KeyRef{Name: testSecretName, Key: testSecretKey}},
			}
			in.ConfigMounts = []Mount{
				{ConfigMap: testConfigMap, MountPath: testMountPath},
				{Secret: testSecretName, MountPath: testCredsPath},
			}
			in.PublicIPv4 = true
			in.Labels = map[string]string{"tier": testLabelValue}
		}),
		"vm with ssh keys, a boot disk and mounts": vmInput(func(in *Input) {
			in.Ports = []Port{{Name: "ssh", Port: 22}}
			in.ConfigMounts = []Mount{{Secret: testSecretName, MountPath: testCredsPath}}
		}),
		"multiple placements at the replica limits": validInput(func(in *Input) {
			in.Placements = []Placement{
				{Name: testPlacement, CityCodes: []string{testCityCode}, MinReplicas: 1},
				{Name: testPlacement + "-east", CityCodes: []string{testCityCode}, MinReplicas: 1000},
			}
		}),
	}

	for name, in := range inputs {
		t.Run(name, func(t *testing.T) {
			w := mustRender(t, in)

			opts := validation.WorkloadValidationOptions{
				Context:        context.Background(),
				Client:         allowAllClient(t),
				Workload:       w,
				ValidCityCodes: []string{testCityCode},
			}

			if errs := validation.ValidateWorkloadCreate(w, opts); len(errs) > 0 {
				t.Errorf("rendered manifest rejected by admission validation: %v", errs)
			}
		})
	}
}

// allowAllClient returns a client that approves every SubjectAccessReview, the
// way the validation package's own tests stub authorization.
func allowAllClient(t *testing.T) client.Client {
	t.Helper()

	scheme := k8sruntime.NewScheme()
	utilruntime.Must(computev1alpha.AddToScheme(scheme))
	utilruntime.Must(networkingv1alpha.AddToScheme(scheme))
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if sar, ok := obj.(*authorizationv1.SubjectAccessReview); ok {
					// The fake client only accepts a create without a name when
					// it can generate one.
					sar.GenerateName = "sar-"
					sar.Status.Allowed = true
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		WithObjects(&networkingv1alpha.Network{
			ObjectMeta: metav1.ObjectMeta{Namespace: Namespace, Name: DefaultNetwork},
		}).
		Build()
}

// TestDeployFromFlagsParity pins the manifest the CLI's flag path produces to
// the one it produced before spec building moved into this package. The
// literal below is the previous deployFromFlags construction, verbatim.
func TestDeployFromFlagsParity(t *testing.T) {
	const (
		instanceType = DefaultInstanceType
		minReplicas  = int32(2)
		port         = int32(8080)
	)
	cities := []string{testCityCode, "IAD"}

	previous := func(withPort bool) computev1alpha.WorkloadSpec {
		tcp := corev1.ProtocolTCP
		container := computev1alpha.SandboxContainer{
			Name:  "app",
			Image: testImage,
		}
		if withPort {
			container.Ports = []computev1alpha.NamedPort{
				{Name: testPortName, Port: port, Protocol: &tcp},
			}
		}

		return computev1alpha.WorkloadSpec{
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
						{Network: networkingv1alpha.NetworkRef{Name: "default"}},
					},
				},
			},
			Placements: []computev1alpha.WorkloadPlacement{{
				Name:      "default",
				CityCodes: cities,
				ScaleSettings: computev1alpha.HorizontalScaleSettings{
					MinReplicas:              minReplicas,
					InstanceManagementPolicy: computev1alpha.OrderedReadyInstanceManagementPolicyType,
				},
			}},
		}
	}

	// What deployFromFlags builds now.
	in := Input{
		Name:         testWorkload,
		Image:        testImage,
		InstanceType: instanceType,
		Network:      DefaultNetwork,
		Placements: []Placement{{
			Name:        DefaultPlacementName,
			CityCodes:   cities,
			MinReplicas: minReplicas,
		}},
	}

	t.Run("without a port", func(t *testing.T) {
		got := mustRender(t, in).Spec
		if delta := cmp.Diff(previous(false), got); delta != "" {
			t.Errorf("spec differs from the pre-refactor manifest (-want +got):\n%s", delta)
		}
	})

	// With a port the only intended difference is the ingress rule that makes
	// the port reachable, which the flag path did not emit before.
	t.Run("with a port", func(t *testing.T) {
		withPort := in
		withPort.Ports = []Port{{Name: testPortName, Port: port}}

		got := mustRender(t, withPort).Spec
		if got.Template.Spec.NetworkInterfaces[0].NetworkPolicy == nil {
			t.Fatal("expected an ingress rule for the exposed port")
		}

		got = *got.DeepCopy()
		got.Template.Spec.NetworkInterfaces[0].NetworkPolicy = nil
		if delta := cmp.Diff(previous(true), got); delta != "" {
			t.Errorf("spec differs from the pre-refactor manifest beyond the network policy (-want +got):\n%s", delta)
		}
	})
}

func TestDefaults(t *testing.T) {
	d := Defaults()
	if d.InstanceType != DefaultInstanceType || d.Network != DefaultNetwork {
		t.Errorf("Defaults() = %+v, want the advertised instance type and network", d)
	}
	if len(d.Placements) != 1 || d.Placements[0].Name != DefaultPlacementName ||
		d.Placements[0].MinReplicas != DefaultMinReplicas {
		t.Errorf("Defaults().Placements = %+v, want one default placement with one replica", d.Placements)
	}

	// Defaults() is a starting point, not a renderable input on its own.
	if _, err := Render(d); err == nil {
		t.Error("Render(Defaults()) succeeded, want errors for the fields the caller must supply")
	}
}

func TestMarshalYAML(t *testing.T) {
	w := mustRender(t, validInput(func(in *Input) {
		in.Ports = []Port{{Name: testPortName, Port: 8080}}
	}))

	data, err := MarshalYAML(w)
	if err != nil {
		t.Fatalf("MarshalYAML() error: %v", err)
	}
	out := string(data)

	for _, want := range []string{
		"apiVersion: compute.datumapis.com/v1alpha",
		"kind: Workload",
		"name: " + testWorkload,
		"instanceType: " + DefaultInstanceType,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered YAML is missing %q:\n%s", want, out)
		}
	}

	for _, unwanted := range []string{"status:", "creationTimestamp"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("rendered YAML should not contain %q:\n%s", unwanted, out)
		}
	}

	if _, err := MarshalYAML(nil); err == nil {
		t.Error("MarshalYAML(nil) succeeded, want an error")
	}
}
