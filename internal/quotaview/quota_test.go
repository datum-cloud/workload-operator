package quotaview

import (
	"context"
	"testing"

	quotav1alpha1 "go.miloapis.com/milo/pkg/apis/quota/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := quotav1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("building scheme: %v", err)
	}
	return s
}

func bucket(resourceType string, limit, allocated, available int64) *quotav1alpha1.AllowanceBucket {
	return &quotav1alpha1.AllowanceBucket{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceType,
			Namespace: quotaNamespace,
			Labels:    map[string]string{consumerKindLabel: consumerKindProject},
		},
		Spec:   quotav1alpha1.AllowanceBucketSpec{ResourceType: resourceType},
		Status: quotav1alpha1.AllowanceBucketStatus{Limit: limit, Allocated: allocated, Available: available},
	}
}

func projectClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
}

// TestListComputeQuotaOrdersRowsAndConvertsUnits pins the two things a caller
// depends on: the explicit order, and vCPUs arriving as vCPUs rather than as
// the thousandths they are stored in.
func TestListComputeQuotaOrdersRowsAndConvertsUnits(t *testing.T) {
	c := projectClient(t,
		// Inserted out of display order, so the ordering is proven.
		bucket("compute.datumapis.com/vcpus", 8000, 3000, 5000),
		bucket("compute.datumapis.com/workloads", 10, 3, 7),
		bucket("compute.datumapis.com/memory", 16384, 4096, 12288),
	)

	// No platform client: the server that reads as the person who asked has
	// none, and the numbers must still arrive.
	rows, err := ListComputeQuota(context.Background(), c, nil)
	if err != nil {
		t.Fatalf("ListComputeQuota: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(rows), rows)
	}

	wantOrder := []string{
		"compute.datumapis.com/workloads",
		"compute.datumapis.com/vcpus",
		"compute.datumapis.com/memory",
	}
	for i, want := range wantOrder {
		if rows[i].ResourceType != want {
			t.Errorf("rows[%d] = %q, want %q", i, rows[i].ResourceType, want)
		}
	}

	vcpus := rows[1]
	if vcpus.Unit != "vCPUs" || vcpus.Limit != 8 || vcpus.Used != 3 || vcpus.Available != 5 {
		t.Errorf("vCPU row = %+v, want 8/3/5 vCPUs (divided down from millicores)", vcpus)
	}
}

// TestListServiceQuotaIgnoresOtherServices keeps another service's quota out of
// compute's answer, and sorts whatever compute owns but did not order.
func TestListServiceQuotaIgnoresOtherServices(t *testing.T) {
	c := projectClient(t,
		bucket("networking.datumapis.com/networks", 5, 1, 4),
		bucket("compute.datumapis.com/zzz-new", 2, 0, 2),
		bucket("compute.datumapis.com/aaa-new", 2, 0, 2),
		bucket("compute.datumapis.com/workloads", 10, 3, 7),
	)

	rows, err := ListComputeQuota(context.Background(), c, nil)
	if err != nil {
		t.Fatalf("ListComputeQuota: %v", err)
	}

	want := []string{
		"compute.datumapis.com/workloads", // explicitly ordered, so first
		"compute.datumapis.com/aaa-new",   // the rest alphabetically, so a new
		"compute.datumapis.com/zzz-new",   // resource type lands reproducibly
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for i, rt := range want {
		if rows[i].ResourceType != rt {
			t.Errorf("rows[%d] = %q, want %q", i, rows[i].ResourceType, rt)
		}
	}
	// A type with no display metadata falls back to its last segment.
	if rows[1].DisplayName != "aaa-new" || rows[1].Unit != "units" {
		t.Errorf("unregistered row = %+v, want the suffix as its name and generic units", rows[1])
	}
}

// TestListServiceQuotaReturnsNothingWhenNoQuotaIsConfigured distinguishes "no
// quota" from a failure: a project with none is not an error.
func TestListServiceQuotaReturnsNothingWhenNoQuotaIsConfigured(t *testing.T) {
	rows, err := ListComputeQuota(context.Background(), projectClient(t), nil)
	if err != nil {
		t.Fatalf("ListComputeQuota: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %+v, want none", rows)
	}
}
