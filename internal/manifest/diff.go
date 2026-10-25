// Package manifest parses rendered YAML into resources with a stable identity
// (Parse, ParseMultiSource) and computes redacted semantic diffs between base
// and head targets (Compare, ComparePartial). ParseValues and CompareValues
// read and compare Helm values for input changes. It normalizes only known
// non-semantic fields. A changed resource also gets YAML line hunks with
// context (lines.go), a view of the same comparison. MaskHunkLines
// masks in line hunks the values a run masked elsewhere (masked.go).
//
// Parsing runs as stages in parse.go: decodeObjects (limits, YAML safety,
// flattenList), crdScopes, then identify (scope and namespace) and normalize
// (normalizedMetadataFields) per object. Comparing runs compareResource
// (compareObjects and maskSecretPair for resources on both sides) and then
// applyUnstablePaths per identity in diff.go.
package manifest

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// Compare returns one diff per resource identity in base or head, ordered by
// identity, and whether every comparison was complete. Each identity goes
// through compareResource and then applyUnstablePaths. Objects must be
// JSON-normalized as Parse returns them (domain.CloneSpec).
func Compare(base, head map[string]Resource, redactor security.Redactor) ([]domain.ResourceDiff, bool) {
	keys := map[string]bool{}
	for k := range base {
		keys[k] = true
	}
	for k := range head {
		keys[k] = true
	}
	ordered := slices.Sorted(maps.Keys(keys))
	out := make([]domain.ResourceDiff, 0, len(ordered))
	complete := true
	for _, k := range ordered {
		b, bok := base[k]
		h, hok := head[k]
		d := compareResource(b, h, bok, hok, redactor)
		applyUnstablePaths(&d, unstablePaths(b, h, bok, hok))
		if d.Comparison == domain.ComparisonIncomplete {
			complete = false
		}
		out = append(out, d)
	}
	return out, complete
}

// compareResource compares one identity present on at least one side (bok,
// hok). A resource on one side only is a single whole-object change; a
// resource on both sides is compared field by field by compareObjects.
func compareResource(b, h Resource, bok, hok bool, redactor security.Redactor) domain.ResourceDiff {
	id := b.ID
	if hok {
		id = h.ID
	}
	d := domain.ResourceDiff{ID: id, State: domain.StateUnchanged, Changes: []domain.Change{}, Comparison: domain.ComparisonComplete}
	if (bok && b.ObservedScope) || (hok && h.ObservedScope) {
		d.ScopeSource = "argocd-observed"
	}
	switch {
	case !bok:
		d.State = domain.StateAdded
		d.Changes = append(d.Changes, domain.Change{Path: "/", Operation: domain.OperationAdd, After: safeObject(h, redactor), Redacted: h.Secret})
	case !hok:
		d.State = domain.StateRemoved
		d.Changes = append(d.Changes, domain.Change{Path: "/", Operation: domain.OperationRemove, Before: safeObject(b, redactor), Redacted: b.Secret})
	case !b.Secret && !h.Secret && jsonEqual(b.Object, h.Object):
		// Equal inputs have equal clones, so walk would record nothing.
	default:
		compareObjects(&d, b, h, redactor)
	}
	return d
}

// compareObjects records the field changes between two present objects in d,
// with line hunks for non-Secret resources, and sets d's state: changed when
// any change remains, unknown when Argo CD masked Secret values.
func compareObjects(d *domain.ResourceDiff, b, h Resource, redactor security.Redactor) {
	// walk compares the JSON-normalized objects and leaves them unchanged;
	// maskSecretPair mutates Secrets, so it masks copies.
	bo, ho := b.Object, h.Object
	var changedValues map[string]payloadKey
	if b.Secret || h.Secret {
		bo, ho = domain.CloneSpec(b.Object), domain.CloneSpec(h.Object)
		var comparable bool
		changedValues, comparable = maskSecretPair(bo, ho, redactor.Masked)
		if !comparable {
			// Argo CD replaced the values; whether they differ is unknown.
			d.Comparison, d.IncompleteReason = domain.ComparisonIncomplete, domain.IncompleteMaskedSecret
		}
	}
	walk("", nil, bo, ho, true, true, redactor, &d.Changes)
	if !b.Secret && !h.Secret && len(d.Changes) > 0 {
		d.LineHunks = lineHunks(redactor.Value(bo, ""), redactor.Value(ho, ""), hiddenPaths(d.Changes))
	}
	for i := range d.Changes {
		if _, ok := changedValues[d.Changes[i].Path]; ok {
			d.Changes[i].Before, d.Changes[i].After, d.Changes[i].Redacted = security.Mask, security.Mask, true
		}
	}
	if len(d.Changes) > 0 {
		d.State = domain.StateChanged
	} else if d.Comparison == domain.ComparisonIncomplete {
		d.State = domain.StateUnknown
	}
}

// maskSecretPair masks the payload values and annotations of both Secret
// copies in place. It returns, by pointer, the payload keys whose values
// differ, and false when Argo CD already masked the values so whether they
// differ is unknown.
//
// After masking, every payload value on both sides is security.Mask, so walk
// would see no difference at a changed key. Each changed key on the head copy
// is therefore set to security.Mask+" ", a value that still differs from the
// base copy but carries nothing of the original values, so walk reports a
// replace at that key's own path. The caller overwrites the before and after
// of those changes with security.Mask, so the marker never reaches a report;
// line hunks are not built for Secrets.
func maskSecretPair(bo, ho map[string]any, masked *security.MaskedValues) (map[string]payloadKey, bool) {
	var changed map[string]payloadKey
	comparable := !argoMasked(bo) && !argoMasked(ho)
	if comparable {
		changed = changedPayload(bo, ho)
	}
	maskSecret(bo, masked)
	maskSecret(ho, masked)
	for _, key := range changed {
		ho[key.section].(map[string]any)[key.name] = security.Mask + " "
	}
	return changed, comparable
}

// unstablePaths returns the paths of fields marked as differing between two
// renders of identical inputs on either present side, unsorted and possibly
// repeated.
func unstablePaths(b, h Resource, bok, hok bool) []string {
	var unknown []string
	if bok {
		unknown = append(unknown, domain.UnstablePaths("", b.Object)...)
	}
	if hok {
		unknown = append(unknown, domain.UnstablePaths("", h.Object)...)
	}
	return unknown
}

// applyUnstablePaths marks d incomplete when unknown is not empty: it records
// the sorted unique paths, drops line hunks and the replace changes at those
// paths, and makes an unchanged diff, or a changed diff left without
// changes, unknown. A field that differed between two renders of identical
// inputs is not compared; a replaced value there is no evidence of a change
// .
func applyUnstablePaths(d *domain.ResourceDiff, unknown []string) {
	if len(unknown) == 0 {
		return
	}
	slices.Sort(unknown)
	d.UnknownPaths = slices.Compact(unknown)
	d.Comparison = domain.ComparisonIncomplete
	if d.IncompleteReason == "" {
		d.IncompleteReason = domain.IncompleteUnstableFields
	}
	// Lines would show the uncompared fields as changes.
	d.LineHunks = nil
	d.Changes = slices.DeleteFunc(d.Changes, func(c domain.Change) bool {
		return c.Operation == domain.OperationReplace && slices.Contains(d.UnknownPaths, c.Path)
	})
	if d.State == domain.StateUnchanged || (d.State == domain.StateChanged && len(d.Changes) == 0) {
		d.State = domain.StateUnknown
	}
}

// argoMasked reports a Secret payload whose values Argo CD replaced with "+"
// characters, or whose shape cannot be compared key by key.
func argoMasked(m map[string]any) bool {
	for _, key := range []string{"data", "stringData"} {
		value, present := m[key]
		// A null section, as a template ranging over nothing renders,
		// holds no payload; Argo CD did not mask it.
		if !present || value == nil {
			continue
		}
		values, ok := value.(map[string]any)
		if !ok {
			return true
		}
		for _, v := range values {
			if s, ok := v.(string); ok && s != "" && strings.Trim(s, "+") == "" {
				return true
			}
		}
	}
	return false
}

type payloadKey struct{ section, name string }

// changedPayload returns, by pointer, the Secret payload keys present on both
// sides with different values. Only the fact of the change is reported; the
// values stay masked.
func changedPayload(b, h map[string]any) map[string]payloadKey {
	changed := map[string]payloadKey{}
	for _, section := range []string{"data", "stringData"} {
		bm, _ := b[section].(map[string]any)
		hm, _ := h[section].(map[string]any)
		for name, before := range bm {
			if after, ok := hm[name]; ok && !jsonEqual(before, after) {
				changed["/"+section+"/"+security.EscapePointer(name)] = payloadKey{section, name}
			}
		}
	}
	return changed
}

// maskSecret masks a Secret copy's payload and annotations in place and
// records the payload values in masked.
func maskSecret(m map[string]any, masked *security.MaskedValues) {
	for _, key := range []string{"data", "stringData"} {
		if value, ok := m[key]; ok {
			if data, ok := value.(map[string]any); ok {
				for k, v := range data {
					if key == "data" {
						masked.AddSecretData(v)
					} else {
						masked.Add(v)
					}
					data[k] = security.Mask
				}
			} else {
				masked.Add(value)
				m[key] = security.Mask
			}
		}
	}
	if meta, ok := m["metadata"].(map[string]any); ok {
		if annotations, ok := meta["annotations"].(map[string]any); ok {
			for k := range annotations {
				annotations[k] = security.Mask
			}
		}
	}
}
func safeObject(r Resource, redactor security.Redactor) any {
	m := domain.CloneSpec(r.Object)
	if r.Secret {
		maskSecret(m, redactor.Masked)
	}
	return redactor.Value(m, "")
}

func walk(p string, names []string, b, h any, bok, hok bool, r security.Redactor, out *[]domain.Change) {
	if bok && hok && jsonEqual(b, h) {
		return
	}
	bm, bmap := b.(map[string]any)
	hm, hmap := h.(map[string]any)
	if bok && hok && bmap && hmap {
		keys := map[string]bool{}
		for k := range bm {
			keys[k] = true
		}
		for k := range hm {
			keys[k] = true
		}
		for _, k := range slices.Sorted(maps.Keys(keys)) {
			bv, be := bm[k]
			hv, he := hm[k]
			walk(p+"/"+security.EscapePointer(k), names, bv, hv, be, he, r, out)
		}
		return
	}
	bl, blist := b.([]any)
	hl, hlist := h.([]any)
	if bok && hok && blist && hlist && r.ComparesListItems(p) && walkNamedList(p, names, bl, hl, r, out) {
		return
	}
	op := domain.OperationReplace
	if !bok {
		op = domain.OperationAdd
	}
	if !hok {
		op = domain.OperationRemove
	}
	if p == "" {
		p = "/"
	}
	d := domain.Change{Path: p, Operation: op, ItemNames: names}
	setValues(&d, r, p, b, h, bok, hok)
	*out = append(*out, d)
}

// setValues stores the redacted before and after values of a change. The
// change is marked redacted when redaction hid any part of either value, not
// merely because its path or item name is sensitive: a reference such as an
// env var's secretKeyRef stays visible.
func setValues(d *domain.Change, r security.Redactor, p string, before, after any, bok, hok bool) {
	if bok {
		d.Before = r.Value(before, p)
		d.Redacted = d.Redacted || !jsonEqual(d.Before, before)
	}
	if hok {
		d.After = r.Value(after, p)
		d.Redacted = d.Redacted || !jsonEqual(d.After, after)
	}
}

// walkNamedList compares lists whose items are all objects with a unique,
// non-empty string key (domain.ItemKeys), such as Kubernetes containers, env and
// volumes or Prometheus rules, item by item. Changed items are compared field by field at
// their head index, added items index the head list and removed items the
// base list. It returns false, leaving one change for the whole list, when an
// item has no unique name or the items both lists share changed order.
func walkNamedList(p string, names []string, b, h []any, r security.Redactor, out *[]domain.Change) bool {
	keys, baseIndex, headIndex := domain.ListItemKeys(b, h)
	if keys == nil {
		return false
	}
	itemName := func(item any) string { return domain.ItemKey(item, keys) }
	last := -1
	for _, item := range h {
		if i, shared := baseIndex[itemName(item)]; shared {
			if i < last {
				return false
			}
			last = i
		}
	}
	item := func(path, name string, op domain.Operation, before, after any) {
		itemNames := append(slices.Clone(names), name)
		// Value masks the value of an item whose name is sensitive, such as
		// an env var named DB_PASSWORD, and keeps its references visible.
		d := domain.Change{Path: path, Operation: op, ItemNames: itemNames}
		setValues(&d, r, path, before, after, before != nil, after != nil)
		*out = append(*out, d)
	}
	for i, value := range h {
		name, path := itemName(value), p+"/"+strconv.Itoa(i)
		j, shared := baseIndex[name]
		switch {
		case !shared:
			item(path, name, domain.OperationAdd, nil, value)
		case jsonEqual(b[j], value):
			// Unchanged item.
		case r.Sensitive("/"+name) && !r.Sensitive(p):
			// Field-level paths would not carry the item's sensitive name.
			// Under a sensitive list path, as in value files, they stay
			// sensitive themselves.
			item(path, name, domain.OperationReplace, b[j], value)
		default:
			walk(path, append(slices.Clone(names), name), b[j], value, true, true, r, out)
		}
	}
	for j, value := range b {
		name := itemName(value)
		if _, kept := headIndex[name]; !kept {
			item(p+"/"+strconv.Itoa(j), name, domain.OperationRemove, value, nil)
		}
	}
	return true
}

// ComparePartial keeps comparisons supported by both observed targets. Absence
// on an incomplete side cannot establish an addition or removal.
func ComparePartial(base, head map[string]Resource, redactor security.Redactor, baseComplete, headComplete bool) ([]domain.ResourceDiff, bool) {
	out, complete := Compare(base, head, redactor)
	for i := range out {
		if (out[i].State == domain.StateAdded && !baseComplete) || (out[i].State == domain.StateRemoved && !headComplete) {
			out[i].State = domain.StateUnknown
			out[i].Comparison, out[i].IncompleteReason = domain.ComparisonIncomplete, domain.IncompletePartialTarget
			out[i].Changes = []domain.Change{}
		}
	}
	return out, complete
}

// hiddenPaths returns the named paths of changes that redaction hides on
// both sides.
func hiddenPaths(changes []domain.Change) []string {
	var paths []string
	for _, c := range changes {
		if c.Hidden() {
			paths = append(paths, domain.NamedPath(c.Path, c.ItemNames))
		}
	}
	return paths
}
