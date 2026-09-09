// SPDX-License-Identifier: AGPL-3.0-only

package open

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/compute/internal/cmd/compute/url"
	"go.datum.net/compute/internal/cmd/compute/util"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
)

const (
	testWorkload  = "api"
	testPortName  = "http"
	testCanonical = "a1b2c3d4.datumproxy.net"
	testURL       = "https://" + testCanonical
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := computev1alpha.AddToScheme(s); err != nil {
		t.Fatalf("registering compute scheme: %v", err)
	}
	if err := networkingv1alpha.AddToScheme(s); err != nil {
		t.Fatalf("registering networking scheme: %v", err)
	}
	return s
}

func newFakeClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&networkingv1alpha.HTTPProxy{}, &networkingv1alpha.NetworkService{}).
		WithObjects(objs...).
		Build()
}

func testWorkloadObject(name string) *computev1alpha.Workload {
	return &computev1alpha.Workload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: util.ResourceNamespace,
			UID:       "workload-uid",
		},
	}
}

// publishedObjects returns the two objects the platform reports for a workload
// that is serving on the managed hostname.
func publishedObjects(t *testing.T, canonical string, programmed bool) []client.Object {
	t.Helper()
	w := testWorkloadObject(testWorkload)

	proxy := url.BuildHTTPProxy(w, testPortName, nil)
	proxy.Status.CanonicalHostname = canonical
	programmedStatus := metav1.ConditionFalse
	if programmed {
		programmedStatus = metav1.ConditionTrue
	}
	proxy.Status.Conditions = []metav1.Condition{
		{Type: networkingv1alpha.HTTPProxyConditionProgrammed, Status: programmedStatus, Reason: "Programmed"},
		{Type: networkingv1alpha.HTTPProxyConditionCertificatesReady, Status: metav1.ConditionTrue, Reason: "AllCertificatesReady"},
	}

	svc := url.BuildNetworkService(w, testPortName, 8080)
	svc.Status.Summary = networkingv1alpha.NetworkServiceSummary{Locations: 1, Members: 2, Healthy: 2}

	return []client.Object{w, proxy, svc}
}

// recordingOpener stands in for the browser: it records what it was handed and
// never launches anything.
type recordingOpener struct {
	calls []string
	err   error
}

func (r *recordingOpener) open(_ context.Context, rawURL string) error {
	r.calls = append(r.calls, rawURL)
	return r.err
}

func TestRun(t *testing.T) {
	tests := []struct {
		name string
		// objects seeded into the control plane
		objects func(t *testing.T) []client.Object
		// arguments
		workload string
		urlOnly  bool
		openErr  error

		wantErr      string
		wantOut      string
		wantContains []string
		wantOpened   []string
	}{
		{
			name:       "opens the URL in the browser",
			objects:    func(t *testing.T) []client.Object { return publishedObjects(t, testCanonical, true) },
			workload:   testWorkload,
			wantOut:    "Opening " + testURL + "\n",
			wantOpened: []string{testURL},
		},
		{
			name:       "--url prints the bare URL and opens nothing",
			objects:    func(t *testing.T) []client.Object { return publishedObjects(t, testCanonical, true) },
			workload:   testWorkload,
			urlOnly:    true,
			wantOut:    testURL + "\n",
			wantOpened: nil,
		},
		{
			name:       "--url output has no decoration to break a pipe",
			objects:    func(t *testing.T) []client.Object { return publishedObjects(t, testCanonical, false) },
			workload:   testWorkload,
			urlOnly:    true,
			wantOut:    testURL + "\n",
			wantOpened: nil,
		},
		{
			name:     "a URL that is not live yet still opens, with a note",
			objects:  func(t *testing.T) []client.Object { return publishedObjects(t, testCanonical, false) },
			workload: testWorkload,
			wantContains: []string{
				"not fully live yet",
				"Opening " + testURL,
			},
			wantOpened: []string{testURL},
		},
		{
			name: "workload that is not published names the command that publishes it",
			objects: func(t *testing.T) []client.Object {
				return []client.Object{testWorkloadObject(testWorkload)}
			},
			workload:   testWorkload,
			wantErr:    "datumctl compute deploy api --http-port",
			wantOpened: nil,
		},
		{
			name: "workload that does not exist is a different error",
			objects: func(t *testing.T) []client.Object {
				return []client.Object{testWorkloadObject("other")}
			},
			workload:   testWorkload,
			wantErr:    `workload "api" not found`,
			wantOpened: nil,
		},
		{
			name: "published but no hostname assigned yet",
			objects: func(t *testing.T) []client.Object {
				return publishedObjects(t, "", true)
			},
			workload:   testWorkload,
			wantErr:    "does not have a URL yet",
			wantOpened: nil,
		},
		{
			name: "a hostname carrying credentials is never opened",
			objects: func(t *testing.T) []client.Object {
				return publishedObjects(t, "user:password@evil.example.com", true)
			},
			workload:   testWorkload,
			wantErr:    wantCredentials,
			wantOpened: nil,
		},
		{
			name: "a malformed hostname is never opened",
			objects: func(t *testing.T) []client.Object {
				return publishedObjects(t, "not a host", true)
			},
			workload:   testWorkload,
			wantErr:    wantMalformed,
			wantOpened: nil,
		},
		{
			name: "a malformed hostname is not printed by --url either",
			objects: func(t *testing.T) []client.Object {
				return publishedObjects(t, "not a host", true)
			},
			workload:   testWorkload,
			urlOnly:    true,
			wantErr:    wantMalformed,
			wantOpened: nil,
		},
		{
			name:       "a browser failure is reported",
			objects:    func(t *testing.T) []client.Object { return publishedObjects(t, testCanonical, true) },
			workload:   testWorkload,
			openErr:    errors.New("no browser"),
			wantErr:    "no browser",
			wantOpened: []string{testURL},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeClient(t, tc.objects(t)...)
			opener := &recordingOpener{err: tc.openErr}
			var out strings.Builder

			err := run(context.Background(), &out, c, tc.workload, tc.urlOnly, opener.open)

			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("run() error = %v, want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("run() error = nil, want one containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("run() error = %q, want it to contain %q", err, tc.wantErr)
			}

			if tc.wantOut != "" && out.String() != tc.wantOut {
				t.Errorf("output = %q, want %q", out.String(), tc.wantOut)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output = %q, want it to contain %q", out.String(), want)
				}
			}
			if got, want := strings.Join(opener.calls, ","), strings.Join(tc.wantOpened, ","); got != want {
				t.Errorf("opened %q, want %q", got, want)
			}
		})
	}
}

// A failing lookup is the platform's problem to report, not something to
// translate into "this workload has no URL".
func TestRunPropagatesLookupErrors(t *testing.T) {
	boom := errors.New("connection refused")
	c := interceptor.NewClient(newFakeClient(t, testWorkloadObject(testWorkload)), interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return boom
		},
	})

	opener := &recordingOpener{}
	var out strings.Builder
	err := run(context.Background(), &out, c, testWorkload, false, opener.open)
	if !errors.Is(err, boom) {
		t.Fatalf("run() error = %v, want the transport error to propagate", err)
	}
	if len(opener.calls) != 0 {
		t.Fatalf("opened %v, want nothing", opener.calls)
	}
	if out.String() != "" {
		t.Errorf("output = %q, want nothing", out.String())
	}
}

func TestCommand(t *testing.T) {
	cmd := Command()

	if cmd.Use != "open <workload-name>" {
		t.Errorf("Use = %q", cmd.Use)
	}
	if cmd.ValidArgsFunction == nil {
		t.Error("ValidArgsFunction is nil, want util.CompleteWorkloadNames")
	}
	if cmd.Flags().Lookup("url") == nil {
		t.Error("--url flag is not registered")
	}
	if err := cmd.Args(cmd, []string{}); err == nil {
		t.Error("Args accepted no workload name, want exactly one")
	}
	if err := cmd.Args(cmd, []string{"a", "b"}); err == nil {
		t.Error("Args accepted two workload names, want exactly one")
	}
}
