package cachesource

import (
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	clientfeatures "k8s.io/client-go/features"
	clientfeaturestesting "k8s.io/client-go/features/testing"

	"github.com/skyhook-io/radar/pkg/ownership"
	"github.com/skyhook-io/radar/pkg/ownership/ownershiptest"
	"github.com/skyhook-io/radar/pkg/resourceid"
)

// A manager that changes twice while Recorders reads the index, each time
// between two reads, can be in none of the lists those reads find although it
// belongs in the answer throughout: filed under its destination reading, then
// under the namespace it records, then unreadable. Reads of one state of the
// stores still find it.
func TestRecordersFindAManagerChangingTwiceMidSearch(t *testing.T) {
	widget := resourceid.Ref{Group: "example.io", Kind: "Widget", Namespace: "alpha", Name: "w"}
	other := resourceid.Ref{Kind: "ConfigMap", Namespace: "alpha", Name: "other"}
	app := ownershiptest.Object{
		Ref:        resourceid.Ref{Group: ownership.ArgoApplication.Group, Kind: ownership.ArgoApplication.Kind, Namespace: "argocd", Name: "shop"},
		APIVersion: "argoproj.io/v1alpha1", UID: "uid-shop", ResourceVersion: "10",
		// The renderer writes the second entry, in the destination namespace,
		// without its namespace.
		Membership: ownership.MembersRecorded, Members: []resourceid.Ref{other, widget}, Destination: "alpha",
	}
	w := ownershiptest.World{Resources: ownershiptest.Resources(), Collection: map[resourceid.GroupKind]ownershiptest.Collection{}, Objects: []ownershiptest.Object{app}}
	for _, k := range ownershiptest.Kinds {
		w.Collection[k.GroupKind] = ownershiptest.Collection{OnDemand: !k.Typed}
	}
	r := render(t, w)
	s := begin(t, r.source()).(*session)
	recorded := memberKey(asRecorded, widget.Group, widget.Kind, widget.Namespace, widget.Name)
	unreadReads, changes := 0, 0
	s.recordersRead = func(key string) {
		switch {
		case key == recorded && changes == 0:
			// The entry comes to record its namespace.
			app.ResourceVersion, app.Members = "11", []resourceid.Ref{widget, other}
			r.update(t, app, recorded)
			changes++
		case key == unreadableKey:
			unreadReads++
			if unreadReads == 2 && changes == 1 {
				app.ResourceVersion, app.Membership = "12", ownership.MembersUnreadable
				r.update(t, app, unreadableKey)
				changes++
			}
		}
	}
	got := s.Recorders(widget)
	if changes != 2 {
		t.Fatalf("the manager changed %d times mid-search, want 2", changes)
	}
	recorder := slices.ContainsFunc(got.Recorders, func(o ownership.Observation) bool { return o.Ref == app.Ref })
	if !recorder && !slices.Contains(got.Unreadable, app.Ref) && len(got.Gaps) == 0 {
		t.Fatalf("Recorders(%v) = %+v, complete %v: %v is in neither list", widget, got, got.Complete(), app.Ref)
	}
}

// With client-go's AtomicFIFO feature off, stores keep no version to compare;
// Recorders then reads everything twice, in turn, which still finds a manager
// that changes once mid-search.
func TestRecordersWithoutStoreVersionsReadTwice(t *testing.T) {
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.AtomicFIFO, false)
	widget := resourceid.Ref{Group: "example.io", Kind: "Widget", Namespace: "alpha", Name: "w"}
	other := resourceid.Ref{Kind: "ConfigMap", Namespace: "alpha", Name: "other"}
	app := ownershiptest.Object{
		Ref:        resourceid.Ref{Group: ownership.ArgoApplication.Group, Kind: ownership.ArgoApplication.Kind, Namespace: "argocd", Name: "shop"},
		APIVersion: "argoproj.io/v1alpha1", UID: "uid-shop", ResourceVersion: "10",
		Membership: ownership.MembersRecorded, Members: []resourceid.Ref{other, widget}, Destination: "alpha",
	}
	w := ownershiptest.World{Resources: ownershiptest.Resources(), Collection: map[resourceid.GroupKind]ownershiptest.Collection{}, Objects: []ownershiptest.Object{app}}
	for _, k := range ownershiptest.Kinds {
		w.Collection[k.GroupKind] = ownershiptest.Collection{OnDemand: !k.Typed}
	}
	r := render(t, w)
	if _, fenced := r.caches.Dynamic.StoreVersion(schema.GroupResource{Group: "argoproj.io", Resource: "applications"}); fenced {
		t.Fatal("stores keep a version with AtomicFIFO off; this test no longer reaches the fallback")
	}
	s := begin(t, r.source()).(*session)
	recorded := memberKey(asRecorded, widget.Group, widget.Kind, widget.Namespace, widget.Name)
	changed := false
	s.recordersRead = func(key string) {
		if key == recorded && !changed {
			app.ResourceVersion, app.Members = "11", []resourceid.Ref{widget, other}
			r.update(t, app, recorded)
			changed = true
		}
	}
	got := s.Recorders(widget)
	if !changed || !slices.ContainsFunc(got.Recorders, func(o ownership.Observation) bool { return o.Ref == app.Ref }) {
		t.Fatalf("Recorders(%v) = %+v, changed %v: want %v found", widget, got, changed, app.Ref)
	}
}
