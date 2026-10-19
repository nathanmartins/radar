package ownership_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/skyhook-io/radar/pkg/ownership"
	"github.com/skyhook-io/radar/pkg/ownership/ownershiptest"
	"github.com/skyhook-io/radar/pkg/readreason"
	"github.com/skyhook-io/radar/pkg/resourceid"
)

// changeManager rewrites manager in w at a later version and points s at the
// result.
func changeManager(t *testing.T, s *countingSession, w ownershiptest.World, manager resourceid.Ref, change func(*ownershiptest.Object)) {
	t.Helper()
	for i := range w.Objects {
		if w.Objects[i].Ref == manager {
			change(&w.Objects[i])
			w.Objects[i].ResourceVersion += "-later"
		}
	}
	later, err := ownershiptest.Scan(w).Begin()
	if err != nil {
		t.Fatal(err)
	}
	s.now = later
}

// Asked twice about one object, the view gives one answer, from the managers
// it captured, however they changed since.
func TestRecordersAnswerEachObjectOnce(t *testing.T) {
	b := ownershiptest.NewBuilder()
	cfg := b.Add("", "ConfigMap", "alpha", "cfg")
	app := b.Manager(ownership.ArgoApplication, "argocd", "app", ownership.MembersRecorded, "alpha", cfg.Ref)
	w := b.World()
	s := newCountingSession(t, w)
	v := ownership.NewView(s, ownership.Unrestricted())
	before := v.Recorders(cfg.Ref)
	if len(before.Recorders) != 1 {
		t.Fatalf("Recorders = %+v, want %v", before, app.Ref)
	}
	changeManager(t, s, w, app.Ref, func(o *ownershiptest.Object) { o.Members = nil })
	if after := v.Recorders(cfg.Ref); len(after.Recorders) != 1 || after.Recorders[0].Ref != app.Ref {
		t.Fatalf("asked again, Recorders = %+v; want the first answer", after)
	}
	if err := v.Validate(); err != nil {
		t.Fatal(err)
	}
}

// A manager's member list, read through Members, and a search for the
// managers recording an object must agree, in either order; when the manager
// changed between them, Validate says so.
func TestRecordersAgreeWithTheMemberListsTheViewRead(t *testing.T) {
	setup := func(t *testing.T, members ...resourceid.Ref) (*ownership.View, *countingSession, ownershiptest.World, ownershiptest.Object, ownershiptest.Object) {
		b := ownershiptest.NewBuilder()
		cfg := b.Add("", "ConfigMap", "alpha", "cfg")
		app := b.Manager(ownership.ArgoApplication, "argocd", "app", ownership.MembersRecorded, "alpha", members...)
		w := b.World()
		s := newCountingSession(t, w)
		return ownership.NewView(s, ownership.Unrestricted()), s, w, cfg, app
	}
	cfgRef := resourceid.Ref{Kind: "ConfigMap", Namespace: "alpha", Name: "cfg"}
	t.Run("one version, both ways", func(t *testing.T) {
		v, _, _, cfg, app := setup(t, cfgRef)
		if got := v.Members(app.Ref); len(got.Members) != 1 {
			t.Fatalf("Members = %+v", got)
		}
		if got := v.Recorders(cfg.Ref); len(got.Recorders) != 1 {
			t.Fatalf("Recorders = %+v", got)
		}
		if err := v.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a readable list, then the manager found unreadable", func(t *testing.T) {
		v, s, w, cfg, app := setup(t)
		if got := v.Members(app.Ref); got.Membership != ownership.MembersRecorded {
			t.Fatalf("Members = %+v", got)
		}
		changeManager(t, s, w, app.Ref, func(o *ownershiptest.Object) { o.Membership = ownership.MembersUnreadable })
		v.Recorders(cfg.Ref)
		if v.Validate() == nil {
			t.Fatal("a manager read with a readable list, then found unreadable, passed Validate")
		}
	})
	t.Run("a list naming the object, then the manager not found", func(t *testing.T) {
		v, s, w, cfg, app := setup(t, cfgRef)
		v.Members(app.Ref)
		changeManager(t, s, w, app.Ref, func(o *ownershiptest.Object) { o.Members = nil })
		v.Recorders(cfg.Ref)
		if v.Validate() == nil {
			t.Fatal("a member list naming the object, then a search not finding its manager, passed Validate")
		}
	})
	t.Run("the manager not found, then its list naming the object", func(t *testing.T) {
		v, s, w, cfg, app := setup(t)
		if got := v.Recorders(cfg.Ref); len(got.Recorders) != 0 {
			t.Fatalf("Recorders = %+v", got)
		}
		changeManager(t, s, w, app.Ref, func(o *ownershiptest.Object) { o.Members = []resourceid.Ref{cfgRef} })
		v.Members(app.Ref)
		if v.Validate() == nil {
			t.Fatal("a search not finding the manager, then its member list naming the object, passed Validate")
		}
	})
	t.Run("the manager found unreadable, then its list read", func(t *testing.T) {
		v, s, w, cfg, app := setup(t)
		changeManager(t, s, w, app.Ref, func(o *ownershiptest.Object) { o.Membership = ownership.MembersUnreadable })
		if got := v.Recorders(cfg.Ref); len(got.Unreadable) != 1 {
			t.Fatalf("Recorders = %+v", got)
		}
		changeManager(t, s, w, app.Ref, func(o *ownershiptest.Object) { o.Membership = ownership.MembersRecorded })
		v.Members(app.Ref)
		if v.Validate() == nil {
			t.Fatal("a manager found unreadable, then read with a readable list, passed Validate")
		}
	})
}

// A manager one search found unreadable and the next found readable, listing
// that search's object or not, changed between them.
func TestRecordersSeeEachManagerInOneState(t *testing.T) {
	for name, members := range map[string]func(other resourceid.Ref) []resourceid.Ref{
		"listing the next object": func(other resourceid.Ref) []resourceid.Ref { return []resourceid.Ref{other} },
		"listing neither object":  func(resourceid.Ref) []resourceid.Ref { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			b := ownershiptest.NewBuilder()
			cfg := b.Add("", "ConfigMap", "alpha", "cfg")
			other := b.Add("", "ConfigMap", "alpha", "other")
			app := b.Manager(ownership.ArgoApplication, "argocd", "app", ownership.MembersUnreadable, "alpha")
			w := b.World()
			s := newCountingSession(t, w)
			v := ownership.NewView(s, ownership.Unrestricted())
			if got := v.Recorders(cfg.Ref); len(got.Unreadable) != 1 {
				t.Fatalf("Recorders = %+v", got)
			}
			changeManager(t, s, w, app.Ref, func(o *ownershiptest.Object) {
				o.Membership, o.Members = ownership.MembersRecorded, members(other.Ref)
			})
			v.Recorders(other.Ref)
			if v.Validate() == nil {
				t.Fatal("a manager unreadable for one search and readable for the next passed Validate")
			}
		})
	}
}

// A caller who may read no manager of a kind learns nothing of how Radar
// collects it.
func TestRecordersHideGapsOfKindsTheCallerCantRead(t *testing.T) {
	w := ownershiptest.Fixture()
	w.Collection[ownership.ArgoApplication] = ownershiptest.Collection{State: ownershiptest.NotCollected}
	v := view(t, w, access{denied: []resourceid.GroupKind{ownership.ArgoApplication}})
	for _, g := range v.Recorders(at("", "ConfigMap", "alpha", "cfg")).Gaps {
		if g.Kind == ownership.ArgoApplication {
			t.Errorf("a kind the caller can't read shows in a gap: %+v", g)
		}
	}
}

// fixedRecorders answers every Recorders search with answer.
type fixedRecorders struct {
	ownership.Session
	answer ownership.RecorderRead
}

func (s fixedRecorders) Recorders(resourceid.Ref) ownership.RecorderRead { return s.answer }

// A manager's list was searched unless the object couldn't be named, or a gap
// leaves the manager's kind unread where the manager is.
func TestRecordersAnswerSearched(t *testing.T) {
	scan, err := ownershiptest.Scan(ownershiptest.Fixture()).Begin()
	if err != nil {
		t.Fatal(err)
	}
	app := at(ownership.ArgoApplication.Group, ownership.ArgoApplication.Kind, "alpha", "app")
	gap := func(namespace string, reason readreason.Code) ownership.Gap {
		return ownership.Gap{Kind: ownership.ArgoApplication, Namespace: namespace, Reason: reason}
	}
	for _, tc := range []struct {
		name   string
		answer ownership.RecorderRead
		want   bool
	}{
		{"no gaps", ownership.RecorderRead{}, true},
		{"the object can't be named", ownership.RecorderRead{Unnamed: true}, false},
		{"its namespace loading", ownership.RecorderRead{Gaps: []ownership.Gap{gap("alpha", readreason.KindSyncPending)}}, false},
		{"another namespace loading", ownership.RecorderRead{Gaps: []ownership.Gap{gap("beta", readreason.KindSyncPending)}}, true},
		{"the namespaces Radar doesn't collect", ownership.RecorderRead{Gaps: []ownership.Gap{gap("", readreason.RadarNamespaceNotCollected)}}, true},
		{"the whole kind loading", ownership.RecorderRead{Gaps: []ownership.Gap{gap("", readreason.KindSyncPending)}}, false},
		{"another kind loading", ownership.RecorderRead{Gaps: []ownership.Gap{{Kind: ownership.FluxKustomization, Reason: readreason.KindSyncPending}}}, true},
	} {
		v := ownership.NewView(fixedRecorders{scan, tc.answer}, ownership.Unrestricted())
		if got := v.Recorders(at("", "ConfigMap", "alpha", "cfg")).Searched(app); got != tc.want {
			t.Errorf("%s: Searched = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// wholeKindUndecided can't decide whether the caller may read Applications in
// some namespace, though it allows them in alpha, as a bounded check across
// namespaces may leave it.
type wholeKindUndecided struct{ ownership.Access }

func (a wholeKindUndecided) Read(gk resourceid.GroupKind, namespace string) ownership.Decision {
	if gk == ownership.ArgoApplication && namespace == "" {
		return ownership.Undecided
	}
	return a.Access.Read(gk, namespace)
}

// A gap the caller may not read is left out of the answer, but still means the
// search didn't read what it covers.
func TestRecordersSearchedCountsGapsTheCallerCantRead(t *testing.T) {
	scan, err := ownershiptest.Scan(ownershiptest.Fixture()).Begin()
	if err != nil {
		t.Fatal(err)
	}
	found := ownership.RecorderRead{Gaps: []ownership.Gap{{Kind: ownership.ArgoApplication, Reason: readreason.KindSyncPending}}}
	v := ownership.NewView(fixedRecorders{scan, found}, wholeKindUndecided{ownership.Unrestricted()})
	got := v.Recorders(at("", "ConfigMap", "alpha", "cfg"))
	if len(got.Gaps) != 0 || got.Searched(at(ownership.ArgoApplication.Group, ownership.ArgoApplication.Kind, "alpha", "app")) {
		t.Fatalf("gaps %v, searched alpha/app = %v; want no gap shown and the list unsearched", got.Gaps, got.Searched(at(ownership.ArgoApplication.Group, ownership.ArgoApplication.Kind, "alpha", "app")))
	}
}

// A manager the caller may not read takes no part in the view's answers, so
// however it changes between two searches, Validate holds.
func TestRecordersIgnoreManagersTheCallerCantRead(t *testing.T) {
	for decision, hidden := range map[string]access{
		"denied":    {denied: []resourceid.GroupKind{ownership.ArgoApplication}},
		"undecided": {undecided: []resourceid.GroupKind{ownership.ArgoApplication}},
	} {
		for _, next := range []string{"recording", "remote", "readable empty", "deleted"} {
			t.Run(decision+"/"+next, func(t *testing.T) {
				b := ownershiptest.NewBuilder()
				first := b.Add("", "ConfigMap", "alpha", "first")
				second := b.Add("", "ConfigMap", "alpha", "second")
				app := b.Manager(ownership.ArgoApplication, "hidden", "app", ownership.MembersUnreadable, "alpha")
				w := b.World()
				s := newCountingSession(t, w)
				v := ownership.NewView(s, hidden)
				if got := v.Recorders(first.Ref); len(got.Recorders)+len(got.Unreadable) != 0 {
					t.Fatalf("Recorders = %+v; want the manager hidden", got)
				}
				if next == "deleted" {
					w.Objects = slices.DeleteFunc(w.Objects, func(o ownershiptest.Object) bool { return o.Ref == app.Ref })
					later, err := ownershiptest.Scan(w).Begin()
					if err != nil {
						t.Fatal(err)
					}
					s.now = later
				} else {
					changeManager(t, s, w, app.Ref, func(o *ownershiptest.Object) {
						o.Membership = ownership.MembersRecorded
						switch next {
						case "recording":
							o.Members = []resourceid.Ref{second.Ref}
						case "remote":
							o.Membership, o.Members = ownership.MembersRemote, []resourceid.Ref{second.Ref}
						}
					})
				}
				if got := v.Recorders(second.Ref); len(got.Recorders)+len(got.Unreadable) != 0 {
					t.Fatalf("Recorders = %+v; want the manager hidden", got)
				}
				if err := v.Validate(); err != nil {
					t.Errorf("only a manager the caller can't read changed, and Validate = %v", err)
				}
			})
		}
	}
}

// BenchmarkRecordersOfManyUnreadableManagers is one search returning n
// managers whose lists can't be read, answered by a fresh view: work linear
// in n.
func BenchmarkRecordersOfManyUnreadableManagers(b *testing.B) {
	scan, err := ownershiptest.Scan(ownershiptest.Fixture()).Begin()
	if err != nil {
		b.Fatal(err)
	}
	for _, n := range []int{1000, 10000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			var found ownership.RecorderRead
			for i := range n {
				found.Unreadable = append(found.Unreadable, at(ownership.ArgoApplication.Group, ownership.ArgoApplication.Kind, "argocd", fmt.Sprintf("app-%06d", i)))
			}
			cfg := at("", "ConfigMap", "alpha", "cfg")
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if got := ownership.NewView(fixedRecorders{scan, found}, ownership.Unrestricted()).Recorders(cfg); len(got.Unreadable) != n {
					b.Fatal(len(got.Unreadable))
				}
			}
		})
	}
}
