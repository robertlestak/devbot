package kube

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPodMeta(t *testing.T) {
	var patch map[string]any
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-2" {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch {
		case r.URL.Path == "/api/v1/namespaces/devbot/pods/gone":
			http.Error(w, `{"reason":"NotFound"}`, 404)
		case r.URL.Path != "/api/v1/namespaces/devbot/pods/devbot-implement-a":
			http.Error(w, "wrong path "+r.URL.Path, 400)
		case r.Method == http.MethodGet:
			io.WriteString(w, `{"metadata":{"name":"devbot-implement-a","labels":{"app.kubernetes.io/component":"implement"}},"spec":{"containers":[]}}`)
		case r.Method == http.MethodPatch && r.Header.Get("Content-Type") == "application/merge-patch+json":
			json.NewDecoder(r.Body).Decode(&patch)
			io.WriteString(w, `{}`)
		default:
			http.Error(w, "unexpected "+r.Method, 405)
		}
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("tok-1\n"), 0o600)
	c := &Client{URL: srv.URL, Namespace: "devbot", TokenFile: tok, HTTP: srv.Client()}
	if _, err := c.PodMeta(t.Context(), "devbot-implement-a"); err == nil {
		t.Fatal("a stale token was accepted")
	}
	// The token is read for every call, so a rotated one is picked up.
	os.WriteFile(tok, []byte("tok-2\n"), 0o600)
	m, err := c.PodMeta(t.Context(), "devbot-implement-a")
	if err != nil || m.Labels["app.kubernetes.io/component"] != "implement" {
		t.Fatalf("PodMeta: %+v, %v", m, err)
	}
	if _, err := c.PodMeta(t.Context(), "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing pod: %v", err)
	}
	err = c.PatchPodMeta(t.Context(), "devbot-implement-a", PodMeta{Labels: map[string]string{"devbot.io/task": "7"}, Annotations: map[string]string{"devbot.io/origin": "chat:1"}})
	if err != nil {
		t.Fatal(err)
	}
	// Metadata only: the patch can't touch the pod's spec.
	b, _ := json.Marshal(patch)
	if string(b) != `{"metadata":{"annotations":{"devbot.io/origin":"chat:1"},"labels":{"devbot.io/task":"7"}}}` {
		t.Fatalf("patch %s", b)
	}
}

func TestServiceObjects(t *testing.T) {
	var calls []string
	var created map[string]any
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		switch {
		case r.Method == http.MethodPost && r.Header.Get("Content-Type") != "application/json":
			http.Error(w, "wrong content type", 415)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces/devbot/pods":
			json.NewDecoder(r.Body).Decode(&created)
			if created["metadata"].(map[string]any)["name"] == "taken" {
				http.Error(w, `{"reason":"AlreadyExists"}`, 409)
				return
			}
			if created["metadata"].(map[string]any)["name"] == "denied" {
				http.Error(w, `{"reason":"Forbidden"}`, 403)
				return
			}
			io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/devbot/pods":
			io.WriteString(w, `{"items":[{"metadata":{"name":"a-services","annotations":{"devbot.io/job-pod":"a"}},"status":{"phase":"Running","podIP":"10.0.0.9",
				"containerStatuses":[{"name":"pg","ready":false,"state":{"waiting":{"reason":"ImagePullBackOff","message":"nope"}}}]}}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/networking.k8s.io/v1/namespaces/devbot/networkpolicies":
			io.WriteString(w, `{"items":[{"metadata":{"name":"a-services"}}]}`)
		case r.Method == http.MethodDelete:
			io.WriteString(w, `{}`)
		default:
			http.Error(w, "unexpected", 500)
		}
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("tok"), 0o600)
	c := &Client{URL: srv.URL, Namespace: "devbot", TokenFile: tok, HTTP: srv.Client()}
	ctx := t.Context()
	if err := c.CreatePod(ctx, map[string]any{"metadata": map[string]any{"name": "a-services"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.CreatePod(ctx, map[string]any{"metadata": map[string]any{"name": "taken"}}); !errors.Is(err, ErrExists) {
		t.Fatalf("an existing pod: %v", err)
	}
	if err := c.CreatePod(ctx, map[string]any{"metadata": map[string]any{"name": "denied"}}); !Refused(err) {
		t.Fatalf("a denied pod: %v", err)
	}
	if err := c.CreatePod(ctx, map[string]any{"metadata": map[string]any{"name": "x"}, "boom": true}); err != nil {
		t.Fatal(err)
	}
	pods, err := c.Pods(ctx, "app.kubernetes.io/component=services")
	if err != nil || len(pods) != 1 || pods[0].Status.PodIP != "10.0.0.9" || pods[0].Status.ContainerStatuses[0].State.Waiting.Reason != "ImagePullBackOff" {
		t.Fatalf("pods: %+v, %v", pods, err)
	}
	nps, err := c.NetworkPolicies(ctx, "a=b")
	if err != nil || len(nps) != 1 || nps[0] != "a-services" {
		t.Fatalf("policies: %v, %v", nps, err)
	}
	if err := c.DeletePod(ctx, "a-services"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteNetworkPolicy(ctx, "a-services"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"GET /api/v1/namespaces/devbot/pods?labelSelector=app.kubernetes.io%2Fcomponent%3Dservices",
		"DELETE /api/v1/namespaces/devbot/pods/a-services?gracePeriodSeconds=0",
		"DELETE /apis/networking.k8s.io/v1/namespaces/devbot/networkpolicies/a-services?",
	} {
		if !slices.Contains(calls, want) {
			t.Errorf("no %s in %v", want, calls)
		}
	}
}
