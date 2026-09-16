// Package store is the gateway's view of the cluster: cached reads of
// GameServers, classes, pods and secrets, and the writes the gateway owns
// (spec intents and observed status).
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Claiyc/pelican-k8s/api/v1alpha1"
	"github.com/Claiyc/pelican-k8s/internal/operator/names"
	"github.com/Claiyc/pelican-k8s/internal/operator/render"
)

// Store wraps a (cached) controller-runtime client.
type Store struct {
	Client    client.Client
	Namespace string
	// DefaultClass is written into new servers.
	DefaultClass string

	tokenMu    sync.RWMutex
	tokenCache map[string]tokenEntry // token id -> entry
}

type tokenEntry struct {
	uuid  string
	token string
}

// New returns a store for the servers namespace.
func New(c client.Client, namespace, defaultClass string) *Store {
	return &Store{Client: c, Namespace: namespace, DefaultClass: defaultClass, tokenCache: map[string]tokenEntry{}}
}

// Get returns the GameServer of a Panel server UUID.
func (s *Store) Get(ctx context.Context, uuid string) (*v1alpha1.GameServer, error) {
	gs := &v1alpha1.GameServer{}
	err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: names.ForUUID(uuid)}, gs)
	if err != nil {
		return nil, err
	}
	return gs, nil
}

// Exists reports whether the server exists.
func (s *Store) Exists(ctx context.Context, uuid string) bool {
	_, err := s.Get(ctx, uuid)
	return err == nil
}

// List returns all GameServers.
func (s *Store) List(ctx context.Context) ([]v1alpha1.GameServer, error) {
	list := &v1alpha1.GameServerList{}
	if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// Class returns the class of a server (or the default class).
func (s *Store) Class(ctx context.Context, gs *v1alpha1.GameServer) (*v1alpha1.GameServerClass, error) {
	name := s.DefaultClass
	if gs != nil && gs.Spec.ClassName != "" {
		name = gs.Spec.ClassName
	}
	cls := &v1alpha1.GameServerClass{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: name}, cls); err != nil {
		return nil, err
	}
	return cls, nil
}

// Pod returns the server pod, or nil.
func (s *Store) Pod(ctx context.Context, uuid string) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: names.Pod(uuid)}, pod)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return pod, nil
}

// AgentToken returns the agent's token id and token.
func (s *Store) AgentToken(ctx context.Context, uuid string) (id, token string, err error) {
	sec := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: names.AgentSecret(uuid)}, sec); err != nil {
		return "", "", err
	}
	return string(sec.Data["token_id"]), string(sec.Data["token"]), nil
}

// ServerByAgentToken maps an agent's "id.token" to its server UUID.
func (s *Store) ServerByAgentToken(ctx context.Context, bearer string) (string, bool) {
	id, token, ok := strings.Cut(bearer, ".")
	if !ok || id == "" || token == "" {
		return "", false
	}
	s.tokenMu.RLock()
	e, ok := s.tokenCache[id]
	s.tokenMu.RUnlock()
	if ok && e.token == token {
		if s.Exists(ctx, e.uuid) {
			return e.uuid, true
		}
	}
	// Refresh from the cached secret list.
	list := &corev1.SecretList{}
	if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace), client.MatchingLabels{v1alpha1.LabelComponent: "agent"}); err != nil {
		return "", false
	}
	s.tokenMu.Lock()
	s.tokenCache = map[string]tokenEntry{}
	for _, sec := range list.Items {
		uuid := sec.Labels[v1alpha1.LabelServerUUID]
		if uuid == "" || sec.Name != names.AgentSecret(uuid) {
			continue
		}
		s.tokenCache[string(sec.Data["token_id"])] = tokenEntry{uuid: uuid, token: string(sec.Data["token"])}
	}
	e, ok = s.tokenCache[id]
	s.tokenMu.Unlock()
	if ok && e.token == token {
		return e.uuid, true
	}
	return "", false
}

// EnvSecret returns the egg variables of a server.
func (s *Store) EnvSecret(ctx context.Context, uuid string) (map[string]string, error) {
	sec := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: names.EnvSecret(uuid)}, sec); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(sec.Data))
	for k, v := range sec.Data {
		out[k] = string(v)
	}
	return out, nil
}

// InstallScript returns the install script of a generation.
func (s *Store) InstallScript(ctx context.Context, uuid string, gen int64) (string, error) {
	cm := &corev1.ConfigMap{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: names.InstallConfigMap(uuid, gen)}, cm); err != nil {
		return "", err
	}
	return cm.Data["install.sh"], nil
}

// PatchSpec applies a JSON merge patch to the spec.
func (s *Store) PatchSpec(ctx context.Context, uuid string, spec map[string]any) error {
	b, err := json.Marshal(map[string]any{"spec": spec})
	if err != nil {
		return err
	}
	gs := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: names.ForUUID(uuid)}}
	return s.Client.Patch(ctx, gs, client.RawPatch(types.MergePatchType, b))
}

// PatchStatus applies a JSON merge patch to the status subresource.
func (s *Store) PatchStatus(ctx context.Context, uuid string, status map[string]any) error {
	b, err := json.Marshal(map[string]any{"status": status})
	if err != nil {
		return err
	}
	gs := &v1alpha1.GameServer{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: names.ForUUID(uuid)}}
	return s.Client.Status().Patch(ctx, gs, client.RawPatch(types.MergePatchType, b))
}

// SetCondition sets one status condition through a merge patch (the operator
// uses list-map merge for conditions so the type key is preserved).
func (s *Store) SetCondition(ctx context.Context, uuid string, cond metav1.Condition) error {
	gs, err := s.Get(ctx, uuid)
	if err != nil {
		return err
	}
	patch := client.MergeFrom(gs.DeepCopy())
	cond.LastTransitionTime = metav1.Now()
	found := false
	for i := range gs.Status.Conditions {
		if gs.Status.Conditions[i].Type == cond.Type {
			if gs.Status.Conditions[i].Status == cond.Status {
				cond.LastTransitionTime = gs.Status.Conditions[i].LastTransitionTime
			}
			gs.Status.Conditions[i] = cond
			found = true
		}
	}
	if !found {
		gs.Status.Conditions = append(gs.Status.Conditions, cond)
	}
	return s.Client.Status().Patch(ctx, gs, patch)
}

// ClassSpec returns the class spec for a server, or an empty spec.
func (s *Store) ClassSpec(ctx context.Context, gs *v1alpha1.GameServer) v1alpha1.GameServerClassSpec {
	cls, err := s.Class(ctx, gs)
	if err != nil {
		return v1alpha1.GameServerClassSpec{}
	}
	return cls.Spec
}

// PodAgentReady reports whether the agent container of the server pod is up and returns the pod IP.
func PodAgentReady(pod *corev1.Pod) (bool, string) {
	if pod == nil || pod.Status.PodIP == "" || !pod.DeletionTimestamp.IsZero() {
		return false, ""
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.Name == render.AgentContainer {
			return cs.Started != nil && *cs.Started, pod.Status.PodIP
		}
	}
	return false, ""
}

// Describe renders a short string for logs.
func Describe(gs *v1alpha1.GameServer) string {
	return fmt.Sprintf("%s/%s", gs.Namespace, gs.Name)
}
