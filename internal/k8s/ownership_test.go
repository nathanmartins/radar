package k8s

import (
	"errors"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/skyhook-io/radar/pkg/k8score"
	"github.com/skyhook-io/radar/pkg/ownership"
	"github.com/skyhook-io/radar/pkg/ownership/cachesource"
	"github.com/skyhook-io/radar/pkg/ownership/ownershiptest"
	"github.com/skyhook-io/radar/pkg/readreason"
	"github.com/skyhook-io/radar/pkg/resourceid"
	"github.com/skyhook-io/radar/pkg/topology"
)

// scriptedSource opens sessions over the ownership fixture whose Validate
// returns the next scripted error.
type scriptedSource struct {
	t        *testing.T
	begin    error
	validate []error
	begun    int
	calls    []string
}

type scriptedSession struct {
	ownership.Session
	validate error
	calls    *[]string
}

func (s scriptedSession) Validate() error {
	*s.calls = append(*s.calls, "validate")
	return s.validate
}

func (s *scriptedSource) Begin() (ownership.Session, error) {
	if s.begin != nil {
		return nil, s.begin
	}
	inner, err := ownershiptest.Scan(ownershiptest.Fixture()).Begin()
	if err != nil {
		s.t.Fatal(err)
	}
	var v error
	if s.begun < len(s.validate) {
		v = s.validate[s.begun]
	}
	s.begun++
	return scriptedSession{Session: inner, validate: v, calls: &s.calls}, nil
}

func TestAnswerOwnershipRetriesAReplacedStoreAndRefusesReplacedCaches(t *testing.T) {
	pod := resourceid.Ref{Kind: "Pod", Namespace: "alpha", Name: "web-1-a"}
	var fetched ownership.Observation
	for _, o := range ownershiptest.Fixture().Objects {
		if o.Ref == pod {
			fetched = ownership.Observation{Ref: o.Ref, APIVersion: o.APIVersion, UID: o.UID, OwnerReferences: o.Owners}
		}
	}
	unrestricted := func(*ownership.Catalog) ownership.Access { return ownership.Unrestricted() }
	for _, tc := range []struct {
		name        string
		source      *scriptedSource
		begun       int
		unavailable readreason.Code
	}{
		{"valid at once", &scriptedSource{}, 1, ""},
		{"a store replaced once", &scriptedSource{validate: []error{ownership.ErrStoreReplaced}}, 2, ""},
		{"a store replaced twice", &scriptedSource{validate: []error{ownership.ErrStoreReplaced, ownership.ErrStoreReplaced}}, 2, readreason.KindSyncPending},
		{"objects changed once", &scriptedSource{validate: []error{ownership.ErrObjectsChanged}}, 2, ""},
		{"objects changed twice", &scriptedSource{validate: []error{ownership.ErrObjectsChanged, ownership.ErrObjectsChanged}}, 2, readreason.ReadError},
		{"a store replaced, then objects changed", &scriptedSource{validate: []error{ownership.ErrStoreReplaced, ownership.ErrObjectsChanged}}, 2, readreason.ReadError},
		{"caches replaced", &scriptedSource{validate: []error{ownership.ErrCachesReplaced}}, 1, readreason.ConnectionChanged},
		{"no caches", &scriptedSource{begin: errors.New("not connected")}, 0, readreason.ClusterConnecting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.source.t = t
			rel := &topology.Relationships{}
			present := func(rel *topology.Relationships, _ ownership.Session) {
				tc.source.calls = append(tc.source.calls, "present")
				rel.InactiveChildren = append(rel.InactiveChildren, topology.ResourceRef{Group: "apps", Kind: "ReplicaSet", Namespace: "alpha", Name: "old"})
			}
			answerOwnership(tc.source, topology.ApplyOwnership, present, rel, fetched, unrestricted)
			if want := slices.Repeat([]string{"present", "validate"}, tc.begun); !slices.Equal(tc.source.calls, want) {
				t.Errorf("calls %v, want %v: each answer is presented before it is validated", tc.source.calls, want)
			}
			if wantInactive := map[bool]int{true: 1, false: 0}[tc.unavailable == ""]; len(rel.InactiveChildren) != wantInactive {
				t.Errorf("inactive children %v; a refused answer keeps none, a retried one only its last", rel.InactiveChildren)
			}
			if tc.source.begun != tc.begun {
				t.Errorf("began %d sessions, want %d", tc.source.begun, tc.begun)
			}
			if rel.Ownership == nil || rel.Ownership.Unavailable != string(tc.unavailable) {
				t.Fatalf("ownership = %+v, want unavailable %q", rel.Ownership, tc.unavailable)
			}
			if answered := rel.Owner != nil; answered != (tc.unavailable == "") {
				t.Fatalf("owner = %+v; an answer only when the session held", rel.Owner)
			}
		})
	}
}

// A Deployment's history moves out of Children only for ReplicaSets the cache
// holds with no Pod naming them, read through the session's own cache.
func TestSplitHistoryReadsTheSessionsCache(t *testing.T) {
	const revision = "deployment.kubernetes.io/revision"
	replicas := func(n int32) *int32 { return &n }
	controller := true
	replicaSet := func(name, rev string, desired, running int32) *appsv1.ReplicaSet {
		return &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name, UID: types.UID("uid-" + name), Annotations: map[string]string{revision: rev}},
			Spec:       appsv1.ReplicaSetSpec{Replicas: replicas(desired)},
			Status:     appsv1.ReplicaSetStatus{Replicas: running},
		}
	}
	podOf := func(name, rs string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name, OwnerReferences: []metav1.OwnerReference{
			{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs, UID: types.UID("uid-" + rs), Controller: &controller},
		}}}
	}
	client := fake.NewClientset(
		replicaSet("web-3", "3", 1, 1), podOf("web-3-a", "web-3"),
		replicaSet("web-2", "2", 0, 0),
		// Scaled down with a Pod still shutting down, on a cluster that
		// doesn't report terminatingReplicas.
		replicaSet("web-1", "1", 0, 0), podOf("web-1-a", "web-1"),
	)
	typed, err := k8score.NewResourceCache(k8score.CacheConfig{Client: client, ResourceTypes: map[string]bool{k8score.Pods: true, k8score.ReplicaSets: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(typed.Stop)
	session, err := cachesource.New(func() cachesource.Caches { return cachesource.Caches{Typed: typed} }).Begin()
	if err != nil {
		t.Fatal(err)
	}
	child := func(name string) topology.ResourceRef {
		return topology.ResourceRef{Group: "apps", Kind: "ReplicaSet", Namespace: "shop", Name: name}
	}
	deployment := &metav1.ObjectMeta{Namespace: "shop", Name: "web", Annotations: map[string]string{revision: "3"}}
	kind := resourceid.GroupKind{Group: "apps", Kind: "Deployment"}

	rel := &topology.Relationships{Children: []topology.ResourceRef{child("web-3"), child("web-2"), child("web-1"), child("web-0")}}
	splitHistory(kind, deployment)(rel, session)
	if want := []topology.ResourceRef{child("web-3"), child("web-1"), child("web-0")}; !slices.Equal(rel.Children, want) {
		t.Errorf("children %v, want %v: the live revision, one still holding a Pod and one the cache lacks", rel.Children, want)
	}
	if want := []topology.ResourceRef{child("web-2")}; !slices.Equal(rel.InactiveChildren, want) {
		t.Errorf("inactive children %v, want %v", rel.InactiveChildren, want)
	}

	scripted, err := ownershiptest.Scan(ownershiptest.Fixture()).Begin()
	if err != nil {
		t.Fatal(err)
	}
	rel = &topology.Relationships{Children: []topology.ResourceRef{child("web-2")}}
	splitHistory(kind, deployment)(rel, scripted)
	if len(rel.InactiveChildren) != 0 {
		t.Errorf("a session over other caches moved %v; only the typed cache it reads can say", rel.InactiveChildren)
	}
}
