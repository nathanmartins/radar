package gitops

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

// Member is one object a GitOps manager's status lists, as recorded.
type Member struct {
	Group, Kind, Namespace, Name string
}

// MemberList is what a GitOps manager's status lists as the objects it
// applies.
type MemberList struct {
	Entries []Member
	// Destination is an Argo CD Application's destination namespace.
	Destination string
}

// ArgoMembers reads an Application's status.resources, as of the destination
// Argo CD compared it against. ok is false when the list can't be trusted, so
// that any object may be a member: the Application has no destination, Argo CD
// hasn't reconciled it, or an entry names no object.
func ArgoMembers(app *unstructured.Unstructured) (list MemberList, ok bool) {
	name, server, destination := ArgoComparedDestination(app)
	if name == "" && server == "" {
		return MemberList{}, false
	}
	list.Destination = destination
	raw, _, err := unstructured.NestedFieldNoCopy(app.Object, "status", "resources")
	if err != nil {
		return MemberList{}, false
	}
	if raw == nil {
		// Argo CD omits an empty list, so once it has reconciled the
		// Application, no list is an empty one.
		if reconciled, _, _ := unstructured.NestedString(app.Object, "status", "reconciledAt"); reconciled == "" {
			return MemberList{}, false
		}
		return list, true
	}
	items, isList := raw.([]any)
	if !isList {
		return MemberList{}, false
	}
	for _, item := range items {
		e, isMap := item.(map[string]any)
		if !isMap {
			return MemberList{}, false
		}
		kind, kindOK := e["kind"].(string)
		name, nameOK := e["name"].(string)
		group, groupOK := optionalString(e["group"])
		namespace, namespaceOK := optionalString(e["namespace"])
		if !kindOK || !nameOK || kind == "" || name == "" || !groupOK || !namespaceOK {
			return MemberList{}, false
		}
		list.Entries = append(list.Entries, Member{Group: group, Kind: kind, Namespace: namespace, Name: name})
	}
	return list, true
}

// FluxMembers reads a Kustomization's or HelmRelease's status.inventory. ok is
// false when there is no inventory or an entry's ID doesn't parse.
func FluxMembers(obj *unstructured.Unstructured) (list MemberList, ok bool) {
	raw, _, err := unstructured.NestedFieldNoCopy(obj.Object, "status", "inventory")
	inventory, isMap := raw.(map[string]any)
	if err != nil || !isMap {
		return MemberList{}, false
	}
	items, isList := inventory["entries"].([]any)
	if !isList && inventory["entries"] != nil {
		return MemberList{}, false
	}
	for _, item := range items {
		e, isMap := item.(map[string]any)
		if !isMap {
			return MemberList{}, false
		}
		id, isString := e["id"].(string)
		if !isString {
			return MemberList{}, false
		}
		group, kind, namespace, name, parsed := ParseFluxInventoryID(id)
		if !parsed {
			return MemberList{}, false
		}
		list.Entries = append(list.Entries, Member{Group: group, Kind: kind, Namespace: namespace, Name: name})
	}
	return list, true
}

// MemberNamespace is the namespace of the object an entry recording namespace
// means, for a kind of known scope: none for a cluster-scoped kind, and the
// destination for a namespaced one recorded without a namespace.
func MemberNamespace(namespaced bool, namespace, destination string) string {
	switch {
	case !namespaced:
		return ""
	case namespace == "":
		return destination
	}
	return namespace
}

// optionalString reads a field that is either absent or a string.
func optionalString(v any) (string, bool) {
	if v == nil {
		return "", true
	}
	s, ok := v.(string)
	return s, ok
}
