package ownership

import (
	"cmp"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/skyhook-io/radar/pkg/readreason"
	"github.com/skyhook-io/radar/pkg/resourceid"
)

// View answers ownership questions for one caller from one session.
//
// It captures each object once, whether it read the object or found it
// searching an owner's dependents, and answers every question from that
// capture, so its answers agree with each other even while the cluster
// changes. It searches each owner's dependents and reads each manager's
// member list at most once. An object it first reads after a search that
// should have found it means the cluster changed in a way one capture can't
// reconcile, and Validate says so. It never reads an object the caller may
// not read, and says nothing of whether there is one. Its answers hold only
// if Validate passes once they are built. They share the view's captures, so
// they must not be modified. A View is not safe for concurrent use.
type View struct {
	session Session
	access  Access
	reads   map[resourceid.Ref]accessRead
	// named holds the objects the view captured under each UID their owner
	// references name.
	named       map[types.UID][]resourceid.Ref
	searches    map[resourceid.Ref]CandidateRead
	searched    map[types.UID]searchScope
	inventories map[resourceid.Ref]InventoryRead
	changed     bool
}

// searchScope is where the view answered for an owner's children, and the
// gaps that answer reported.
type searchScope struct {
	domain Domain
	gaps   []Gap
}

type accessRead struct {
	read ObjectRead
	may  Decision
}

// NewView answers for the caller access describes, from s.
func NewView(s Session, access Access) *View {
	return &View{
		session:     s,
		access:      access,
		reads:       map[resourceid.Ref]accessRead{},
		named:       map[types.UID][]resourceid.Ref{},
		searches:    map[resourceid.Ref]CandidateRead{},
		searched:    map[types.UID]searchScope{},
		inventories: map[resourceid.Ref]InventoryRead{},
	}
}

// Catalog is the served API the view's session reads against.
func (v *View) Catalog() *Catalog { return v.session.Catalog() }

// Validate reports whether every store the view's answers came from is still
// the one serving them (see Session.Validate), and returns ErrObjectsChanged
// if an object the view read contradicts a search it made.
func (v *View) Validate() error {
	if err := v.session.Validate(); err != nil {
		return err
	}
	if v.changed {
		return ErrObjectsChanged
	}
	return nil
}

// read reads ref for the caller, once.
func (v *View) read(ref resourceid.Ref) (ObjectRead, Decision) {
	if r, ok := v.reads[ref]; ok {
		return r.read, r.may
	}
	r := accessRead{may: v.access.Read(ref.GroupKind(), ref.Namespace)}
	if r.may == Allowed {
		r.read = v.session.Object(ref)
		if r.read.State == Present {
			v.captured(r.read.Object, true)
		}
	}
	v.reads[ref] = r
	return r.read, r.may
}

// captured records an object the view has just captured under the UIDs it
// names, and checks it against the owners whose children it already answered
// for. An answer counts only objects the view had captured, so one it names,
// whose answer covered its kind and place, was answered for before the object
// named it. collected says the session holds o's kind in o's namespace.
func (v *View) captured(o Observation, collected bool) {
	for i, r := range o.OwnerReferences {
		if slices.ContainsFunc(o.OwnerReferences[:i], func(p metav1.OwnerReference) bool { return p.UID == r.UID }) {
			continue
		}
		v.named[r.UID] = append(v.named[r.UID], o.Ref)
		s, ok := v.searched[r.UID]
		if !ok || !v.searchedIn(o.Ref, s.domain) {
			continue
		}
		if slices.ContainsFunc(s.gaps, func(g Gap) bool { return covers(g, o.Ref, collected) }) {
			continue
		}
		v.changed = true
	}
}

// covers reports whether a search's gap left ref's kind and place unread. A
// gap across all namespaces that stands for the namespaces Radar doesn't
// collect leaves the ones it does collect read.
func covers(g Gap, ref resourceid.Ref, collected bool) bool {
	switch {
	case g.Kind != ref.GroupKind():
		return false
	case g.Namespace == ref.Namespace:
		return true
	case g.Namespace != "":
		return false
	}
	return !collected || g.Reason != readreason.RadarNamespaceNotCollected
}

// Seed reads o as its object when the session can't, as when the caller
// fetched it from the API and Radar doesn't collect its kind. A read that
// holds the object stands, as does an excluded one or one the caller may not
// make. Seed an object before asking about it: replacing a read an earlier
// answer used makes Validate fail. The view keeps o, which must not be
// modified afterwards.
func (v *View) Seed(o Observation) {
	_, answered := v.reads[o.Ref]
	if read, may := v.read(o.Ref); may == Allowed && (read.State == Unavailable || read.State == Absent) {
		v.reads[o.Ref] = accessRead{read: ObjectRead{State: Present, Object: o}, may: Allowed}
		v.changed = v.changed || answered
		v.captured(o, read.State == Absent)
	}
}

// Subject is the object an answer is about, as the caller read it.
type Subject struct {
	Read ObjectRead
	// Access is whether the caller may read the object. When it may not,
	// Read is unset and the answer says nothing more.
	Access Decision
}

// Present reports whether the caller read the object, so the answer has an
// object to be about.
func (s Subject) Present() bool {
	return s.Access == Allowed && s.Read.State == Present
}

func (v *View) subject(ref resourceid.Ref) Subject {
	read, may := v.read(ref)
	return Subject{Read: read, Access: may}
}

func (v *View) resolve(dependent Observation, ref metav1.OwnerReference) OwnerResolution {
	return resolveOwner(v.session.Catalog(), dependent, ref, v.read)
}

// References resolves each of ref's owner references for the caller, in the
// object's order.
func (v *View) References(ref resourceid.Ref) (Subject, []OwnerResolution) {
	subject := v.subject(ref)
	if !subject.Present() {
		return subject, nil
	}
	refs := subject.Read.Object.OwnerReferences
	out := make([]OwnerResolution, len(refs))
	for i, r := range refs {
		out[i] = v.resolve(subject.Read.Object, r)
	}
	return subject, out
}

// OwnerAnswer is an object's owner reference: its controller reference, or
// its only reference when it has no controller.
type OwnerAnswer struct {
	Subject Subject
	// Index is the reference's position among the object's owner references,
	// or -1 when it has no such reference: none at all, or several without a
	// controller (Several).
	Index      int
	Resolution OwnerResolution
	Several    bool
}

// Owner resolves ref's controller reference, or its only reference. The
// reference answers in whatever state it resolves to: a withheld or replaced
// controller never gives way to a weaker owner.
func (v *View) Owner(ref resourceid.Ref) OwnerAnswer {
	subject, resolutions := v.References(ref)
	a := OwnerAnswer{Subject: subject, Index: -1}
	if !subject.Present() {
		return a
	}
	refs := subject.Read.Object.OwnerReferences
	i := slices.IndexFunc(refs, isController)
	if i < 0 && len(refs) == 1 {
		i = 0
	}
	if i < 0 {
		a.Several = len(refs) > 1
		return a
	}
	a.Index, a.Resolution = i, resolutions[i]
	return a
}

func isController(ref metav1.OwnerReference) bool {
	return ref.Controller != nil && *ref.Controller
}

// Child is an object whose owner reference names its owner's current
// incarnation.
type Child struct {
	Observation
	// Controller: the reference is the child's controller reference.
	Controller bool
}

// ChildrenAnswer is what an object owns, as one caller may see it.
type ChildrenAnswer struct {
	Subject Subject
	// Children are ordered by group, kind, namespace and name.
	Children []Child
	// Gaps are the parts of the search the source couldn't read; see
	// CandidateRead.Gaps.
	Gaps []Gap
	// Truncated: the search stopped at its limit, so there may be more.
	Truncated bool
	// Narrowed: the caller may read only some of the places children could
	// be, so Children are the ones it may read, and whether there are others
	// isn't said.
	Narrowed bool
}

// Complete reports whether Children are every child the caller may read. An
// answer about an object the caller didn't read lists none and is never
// complete.
func (a ChildrenAnswer) Complete() bool {
	return a.Subject.Present() && len(a.Gaps) == 0 && !a.Truncated
}

// Children finds the objects with an owner reference that resolves to ref's
// current incarnation, as Owner resolves it: ref's group, kind and name, a
// served version, and ref's UID. A namespaced owner's children are in its
// namespace, where Kubernetes resolves their references; a cluster-scoped
// owner's can be anywhere. A reference with the UID that names other
// coordinates is not one Kubernetes resolves to ref. limit caps the children
// returned, 0 for no limit; it counts only children the caller may read.
func (v *View) Children(ref resourceid.Ref, limit int) ChildrenAnswer {
	subject := v.subject(ref)
	a := ChildrenAnswer{Subject: subject}
	if !subject.Present() {
		return a
	}
	owner := subject.Read.Object
	namespaces, all := []string{ref.Namespace}, false
	if ref.Namespace == "" {
		namespaces, all = v.access.Namespaces()
	}
	domain := Domain{AllNamespaces: all, Namespaces: namespaces, ClusterScoped: ref.Namespace == ""}
	a.Narrowed = v.narrows(ref)
	found, searched := v.searches[ref]
	if !searched {
		found = v.session.OwnerCandidates(owner.UID, domain, 0)
		v.searches[ref] = found
	}
	candidates, unread, undecided := v.candidates(found, owner.UID, domain)
	// Saying a check failed would say a child may be there.
	a.Narrowed = a.Narrowed || undecided
	// unresolved are the gaps of references to the owner's UID that Radar
	// can't resolve: the collector retries them, so their objects may be
	// children.
	var unresolved []Gap
	for _, c := range candidates {
		child, names := Child{Observation: c}, false
		var unknown []Gap
		for _, r := range c.OwnerReferences {
			if r.UID != owner.UID {
				continue
			}
			switch res := v.resolve(c, r); {
			case res.State == OwnerFound && res.Ref == ref:
				names = true
				child.Controller = child.Controller || isController(r)
			case res.State == OwnerUnknown:
				unknown = append(unknown, res.Gap)
			}
		}
		if !names {
			unresolved = append(unresolved, unknown...)
			continue
		}
		if limit > 0 && len(a.Children) == limit {
			a.Truncated = true
			break
		}
		a.Children = append(a.Children, child)
	}
	for _, g := range found.Gaps {
		if v.coversReadable(g, domain) {
			a.Gaps = append(a.Gaps, g)
		}
	}
	// The caller may read these: the view read them for it.
	a.Gaps = append(a.Gaps, unread...)
	if !searched {
		v.searched[owner.UID] = searchScope{domain: domain, gaps: slices.Clone(a.Gaps)}
	}
	// An unresolved reference doesn't leave a kind unsearched, so it excuses
	// no object the search should have found.
	a.Gaps = append(a.Gaps, unresolved...)
	slices.SortFunc(a.Gaps, compareGaps)
	a.Gaps = slices.Compact(a.Gaps)
	return a
}

// candidates are the objects the caller may read that name uid in domain,
// each as the view captured it: an object it already read answers as it was
// read, whatever the search found since, and one the search found is
// captured as found. Captured objects that still name uid are kept even if
// the search no longer finds them, so every answer reflects one capture of
// each object. unread are the gaps of found objects the view read before it
// could tell whether they exist: the search says they name uid, so leaving
// them out says nothing of whether they are children. undecided reports a
// candidate whose access check failed.
func (v *View) candidates(found CandidateRead, uid types.UID, domain Domain) (out []Observation, unread []Gap, undecided bool) {
	for _, c := range found.Candidates {
		r, read := v.reads[c.Ref]
		if !read {
			r = accessRead{may: v.access.Read(c.Ref.GroupKind(), c.Ref.Namespace)}
			if r.may == Allowed {
				r.read = ObjectRead{State: Present, Object: c}
				v.captured(c, true)
			}
			v.reads[c.Ref] = r
		}
		undecided = undecided || r.may == Undecided
		if r.may == Allowed && r.read.State == Unavailable {
			unread = append(unread, r.read.Gap)
		}
	}
	for _, ref := range v.named[uid] {
		if v.searchedIn(ref, domain) {
			out = append(out, v.reads[ref].read.Object)
		}
	}
	slices.SortFunc(out, func(a, b Observation) int { return compareRefs(a.Ref, b.Ref) })
	return out, unread, undecided
}

// searchedIn reports whether a dependents search of domain covers ref's kind
// and place.
func (v *View) searchedIn(ref resourceid.Ref, domain Domain) bool {
	k, known := v.session.Catalog().Kind(ref.GroupKind())
	if !known || !k.GarbageCollected || IsEvent(ref.GroupKind()) {
		return false
	}
	if !k.Namespaced {
		return domain.ClusterScoped
	}
	return domain.AllNamespaces || slices.Contains(domain.Namespaces, ref.Namespace)
}

// coversReadable reports whether gap covers something the caller may read
// within domain, so leaving it unread may have hidden a child. A gap on a
// namespaced kind without a namespace covers every namespace of the domain,
// and counts if the caller may read the kind in any of them.
func (v *View) coversReadable(g Gap, domain Domain) bool {
	if g.Kind.Kind == "" {
		return true
	}
	if g.Namespace != "" {
		return v.access.Read(g.Kind, g.Namespace) != Denied
	}
	k, known := v.session.Catalog().Kind(g.Kind)
	if !known || !k.Namespaced {
		return v.access.Read(g.Kind, "") != Denied
	}
	if domain.AllNamespaces {
		return v.access.Read(g.Kind, "") != Denied
	}
	return slices.ContainsFunc(domain.Namespaces, func(ns string) bool { return v.access.Read(g.Kind, ns) != Denied })
}

// narrows reports whether the caller may read less than every place ref's
// children could be: anywhere for a cluster-scoped owner, its namespace for a
// namespaced one.
func (v *View) narrows(ref resourceid.Ref) bool {
	if v.access.Everything() {
		return false
	}
	return ref.Namespace == "" || !v.access.AllKindsIn(ref.Namespace)
}

// RootAnswer is where an object's chain of controller references ends.
type RootAnswer struct {
	Subject Subject
	// Root is the last object the walk reached: the subject itself when its
	// controller reference can't be followed.
	Root resourceid.Ref
	// Chain is every object the walk reached, from the subject to Root.
	Chain []resourceid.Ref
	// Stop is the controller reference the walk couldn't follow, or nil when
	// Root has no controller reference. A Node controller ends the walk
	// (Node), and is Stop only when it isn't found.
	Stop *OwnerResolution
	Node bool
	// Cycle: the references loop. Root is the smallest object in the loop
	// itself, so every object on the loop or feeding it has the same root.
	Cycle bool
	// Truncated: the chain is longer than maxControllerDepth references,
	// which only corrupt references produce; Root is where the walk gave up.
	Truncated bool
}

// maxControllerDepth bounds the walk; real chains are a few hops deep.
const maxControllerDepth = 16

// ControllerRoot follows ref's controller references to the first it can't
// follow, and says why it stopped.
func (v *View) ControllerRoot(ref resourceid.Ref) RootAnswer {
	subject := v.subject(ref)
	a := RootAnswer{Subject: subject, Root: ref}
	if !subject.Present() {
		return a
	}
	a.Chain = []resourceid.Ref{ref}
	current := subject.Read.Object
	for {
		i := slices.IndexFunc(current.OwnerReferences, isController)
		if i < 0 {
			return a
		}
		r := v.resolve(current, current.OwnerReferences[i])
		if r.Ref.Group == "" && r.Ref.Kind == "Node" {
			a.Node = true
			if r.State != OwnerFound {
				a.Stop = &r
			}
			return a
		}
		if r.State != OwnerFound {
			a.Stop = &r
			return a
		}
		if at := slices.Index(a.Chain, r.Ref); at >= 0 {
			a.Root, a.Cycle = slices.MinFunc(a.Chain[at:], compareRefs), true
			return a
		}
		if len(a.Chain) > maxControllerDepth {
			a.Truncated = true
			return a
		}
		a.Chain = append(a.Chain, r.Ref)
		a.Root, current = r.Ref, r.Owner
	}
}

func compareRefs(a, b resourceid.Ref) int {
	return cmp.Or(cmp.Compare(a.Group, b.Group), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
}

// MembersAnswer is what a GitOps manager's status records it applies to
// this cluster, as one caller may see it.
type MembersAnswer struct {
	Subject Subject
	// Membership is what the manager's status records; Members and the counts
	// are set only when it is MembersRecorded. A remote manager's list names
	// objects of another cluster.
	Membership MembershipState
	// Members are the recorded objects Radar holds that the caller may read,
	// sorted.
	Members []Observation
	// Missing counts the recorded objects Radar has loaded the stores of and
	// doesn't hold. Withheld counts those the caller may not read, and
	// Unknown those Radar can't say whether it holds (Gaps say why). The
	// manager's status, which the caller can read, names them all.
	Missing, Withheld, Unknown int
	Gaps                       []Gap
}

// Members reads the member list in the status of manager, which must be of a
// RecordingManagers kind, and reads each member.
func (v *View) Members(manager resourceid.Ref) MembersAnswer {
	subject := v.subject(manager)
	a := MembersAnswer{Subject: subject}
	if !subject.Present() || !IsRecordingManager(manager.GroupKind()) {
		return a
	}
	inventory, read := v.inventories[manager]
	if !read {
		inventory = v.session.Inventory(manager)
		v.inventories[manager] = inventory
	}
	captured := subject.Read.Object
	switch {
	case inventory.Manager.State == Unavailable:
		// As when the caller fetched a manager Radar doesn't collect.
		a.Gaps = append(a.Gaps, inventory.Manager.Gap)
		return a
	case inventory.Manager.State != Present || inventory.Manager.Object.UID != captured.UID ||
		captured.ResourceVersion != "" && inventory.Manager.Object.ResourceVersion != captured.ResourceVersion:
		// The manager changed between the two reads, which one capture can't
		// reconcile.
		v.changed = true
		a.Gaps = append(a.Gaps, Gap{Kind: manager.GroupKind(), Namespace: manager.Namespace, Reason: readreason.KindSyncPending})
		return a
	}
	a.Membership = inventory.Membership
	if a.Membership != MembersRecorded {
		return a
	}
	for _, m := range inventory.Members {
		read, may := v.read(m)
		switch {
		case may == Denied:
			a.Withheld++
		case may == Undecided:
			a.Unknown++
			a.Gaps = append(a.Gaps, Gap{Kind: m.GroupKind(), Namespace: m.Namespace, Reason: readreason.UserPermissionCheckFailed})
		case read.State == Present:
			a.Members = append(a.Members, read.Object)
		case read.State == Absent:
			a.Missing++
		default:
			a.Unknown++
			if read.State == Unavailable {
				a.Gaps = append(a.Gaps, read.Gap)
			}
		}
	}
	slices.SortFunc(a.Gaps, compareGaps)
	a.Gaps = slices.Compact(a.Gaps)
	return a
}

func compareGaps(a, b Gap) int {
	return cmp.Or(cmp.Compare(a.Kind.Group, b.Kind.Group), cmp.Compare(a.Kind.Kind, b.Kind.Kind), cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Reason, b.Reason))
}
