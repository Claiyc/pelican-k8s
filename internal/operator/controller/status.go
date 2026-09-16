package controller

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
)

// updateStatus writes only the status fields this reconcile changed as a JSON
// merge patch. The gateway patches its own fields (process, usage, backups,
// install result, sftp host key) concurrently; optimistic updates would lose
// recorded power actions on conflicts, merge patches never conflict.
func (r *GameServerReconciler) updateStatus(s *scope) error {
	patch, err := StatusPatch(&s.orig.Status, &s.gs.Status)
	if err != nil {
		return err
	}
	if patch == nil {
		return nil
	}
	return r.Status().Patch(s.ctx, s.gs, client.RawPatch(types.MergePatchType, patch))
}

// StatusPatch returns a merge patch turning orig into cur, diffing one level
// into the sub-objects the gateway also writes to. nil means no change.
func StatusPatch(orig, cur *v1alpha1.GameServerStatus) ([]byte, error) {
	o, err := toMap(orig)
	if err != nil {
		return nil, err
	}
	c, err := toMap(cur)
	if err != nil {
		return nil, err
	}
	diff := diffMaps(o, c, map[string]bool{"install": true, "agent": true, "power": true})
	if len(diff) == 0 {
		return nil, nil
	}
	return json.Marshal(map[string]any{"status": diff})
}

func toMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}
	return m, nil
}

// diffMaps returns the keys of cur that differ from orig. Keys listed in
// nested are compared one level deeper so untouched sibling fields written by
// someone else are not overwritten; removed keys become explicit nulls.
func diffMaps(orig, cur map[string]any, nested map[string]bool) map[string]any {
	out := map[string]any{}
	for k, cv := range cur {
		ov, had := orig[k]
		if had && jsonEqual(ov, cv) {
			continue
		}
		om, ook := ov.(map[string]any)
		cm, cok := cv.(map[string]any)
		if nested[k] && ook && cok {
			if d := diffMaps(om, cm, nil); len(d) > 0 {
				out[k] = d
			}
			continue
		}
		out[k] = cv
	}
	for k := range orig {
		if _, ok := cur[k]; !ok {
			out[k] = nil
		}
	}
	return out
}

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}
