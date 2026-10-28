// Package kube is just enough of the Kubernetes API for serve to label job
// pods (pod_labels) and run their service pods (services), from inside the
// cluster, as its ServiceAccount.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// ErrNotFound means the object is gone, and ErrExists that one by that
// name is already there.
var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
)

// Client talks to the API server for one namespace.
type Client struct {
	URL       string // https://host:port
	Namespace string
	// TokenFile is re-read for every call: the kubelet rotates projected
	// tokens.
	TokenFile string
	HTTP      *http.Client
}

// InCluster is a Client for the pod's own namespace and ServiceAccount.
func InCluster() (*Client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not in a Kubernetes cluster ($KUBERNETES_SERVICE_HOST is unset)")
	}
	ns, err := os.ReadFile(saDir + "/namespace")
	if err != nil {
		return nil, fmt.Errorf("no ServiceAccount token mounted: %w", err)
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("the ServiceAccount's ca.crt has no certificates")
	}
	return &Client{
		URL: "https://" + net.JoinHostPort(host, port), Namespace: strings.TrimSpace(string(ns)), TokenFile: saDir + "/token",
		HTTP: &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}},
	}, nil
}

// PodMeta is the part of a pod devbot reads and writes.
type PodMeta struct {
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// PodMeta reads a pod's labels and annotations.
func (c *Client) PodMeta(ctx context.Context, name string) (PodMeta, error) {
	var pod struct {
		Metadata PodMeta `json:"metadata"`
	}
	err := c.do(ctx, http.MethodGet, c.pods(name), nil, &pod)
	return pod.Metadata, err
}

// PatchPodMeta adds labels and annotations to a pod, leaving its others and
// its spec alone: a JSON merge patch of metadata only.
func (c *Client) PatchPodMeta(ctx context.Context, name string, m PodMeta) error {
	b, err := json.Marshal(map[string]PodMeta{"metadata": m})
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPatch, c.pods(name), b, nil)
}

// path is one object's or one collection's API path, in c's namespace.
func (c *Client) path(group, resource, name string) string {
	p := "/api/v1"
	if group != "" {
		p = "/apis/" + group
	}
	p += "/namespaces/" + url.PathEscape(c.Namespace) + "/" + resource
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}

func (c *Client) pods(name string) string { return c.path("", "pods", name) }

func (c *Client) networkPolicies(name string) string {
	return c.path("networking.k8s.io/v1", "networkpolicies", name)
}

// OwnerReference is one of an object's owners.
type OwnerReference struct {
	APIVersion         string `json:"apiVersion"`
	Kind               string `json:"kind"`
	Name               string `json:"name"`
	UID                string `json:"uid"`
	Controller         *bool  `json:"controller,omitempty"`
	BlockOwnerDeletion *bool  `json:"blockOwnerDeletion,omitempty"`
}

// ObjectMeta is the part of an object's metadata devbot reads.
type ObjectMeta struct {
	Name              string            `json:"name"`
	UID               string            `json:"uid,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	OwnerReferences   []OwnerReference  `json:"ownerReferences,omitempty"`
	CreationTimestamp time.Time         `json:"creationTimestamp,omitzero"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp,omitempty"`
}

// Pod is the part of a pod devbot reads: who it is, and how it is doing.
type Pod struct {
	Metadata ObjectMeta `json:"metadata"`
	Status   PodStatus  `json:"status"`
}

type PodStatus struct {
	Phase      string `json:"phase"`
	PodIP      string `json:"podIP"`
	Conditions []struct {
		Type    string `json:"type"`
		Status  string `json:"status"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"conditions"`
	ContainerStatuses []ContainerStatus `json:"containerStatuses"`
	// InitContainerStatuses are the native sidecars' among them, a job
	// pod's agent container's.
	InitContainerStatuses []ContainerStatus `json:"initContainerStatuses"`
}

type ContainerStatus struct {
	Name         string         `json:"name"`
	Ready        bool           `json:"ready"`
	RestartCount int            `json:"restartCount"`
	State        ContainerState `json:"state"`
	LastState    ContainerState `json:"lastTerminationState"`
}

type ContainerState struct {
	Waiting *struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"waiting,omitempty"`
	Running *struct {
		StartedAt time.Time `json:"startedAt"`
	} `json:"running,omitempty"`
	Terminated *Terminated `json:"terminated,omitempty"`
}

// Terminated is how a container ended.
type Terminated struct {
	ExitCode   int       `json:"exitCode"`
	Reason     string    `json:"reason"`
	Message    string    `json:"message"`
	FinishedAt time.Time `json:"finishedAt"`
}

// Pod reads a pod.
func (c *Client) Pod(ctx context.Context, name string) (Pod, error) {
	var p Pod
	err := c.do(ctx, http.MethodGet, c.pods(name), nil, &p)
	return p, err
}

// Pods lists the pods matching a label selector ("a=b,c=d").
func (c *Client) Pods(ctx context.Context, selector string) ([]Pod, error) {
	var l struct {
		Items []Pod `json:"items"`
	}
	err := c.do(ctx, http.MethodGet, c.pods("")+"?labelSelector="+url.QueryEscape(selector), nil, &l)
	return l.Items, err
}

// CreatePod creates the pod obj describes, a v1 Pod as JSON would have it.
func (c *Client) CreatePod(ctx context.Context, obj any) error {
	return c.create(ctx, c.pods(""), obj)
}

// DeletePod deletes a pod, at once: what devbot deletes holds no state
// worth a grace period.
func (c *Client) DeletePod(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, c.pods(name)+"?gracePeriodSeconds=0", nil, nil)
}

// NetworkPolicies lists the names of the NetworkPolicies matching a label
// selector.
func (c *Client) NetworkPolicies(ctx context.Context, selector string) ([]string, error) {
	var l struct {
		Items []struct {
			Metadata ObjectMeta `json:"metadata"`
		} `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, c.networkPolicies("")+"?labelSelector="+url.QueryEscape(selector), nil, &l); err != nil {
		return nil, err
	}
	var names []string
	for _, i := range l.Items {
		names = append(names, i.Metadata.Name)
	}
	return names, nil
}

// CreateNetworkPolicy creates the networking.k8s.io/v1 NetworkPolicy obj
// describes.
func (c *Client) CreateNetworkPolicy(ctx context.Context, obj any) error {
	return c.create(ctx, c.networkPolicies(""), obj)
}

func (c *Client) DeleteNetworkPolicy(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, c.networkPolicies(name), nil, nil)
}

func (c *Client) create(ctx context.Context, path string, obj any) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, path, b, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	tok, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	req.Header.Set("Accept", "application/json")
	switch {
	case method == http.MethodPatch:
		req.Header.Set("Content-Type", "application/merge-patch+json")
	case body != nil:
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	what, _, _ := strings.Cut(strings.TrimPrefix(path, "/api/v1/namespaces/"+c.Namespace+"/"), "?")
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%s: %w", what, ErrNotFound)
	case resp.StatusCode == http.StatusConflict && method == http.MethodPost:
		return fmt.Errorf("%s: %w", what, ErrExists)
	case resp.StatusCode >= 300:
		return &APIError{Code: resp.StatusCode, Msg: fmt.Sprintf("%s %s: %s: %s", method, what, resp.Status, strings.TrimSpace(string(b)))}
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// APIError is the API server's answer to a request it didn't carry out.
type APIError struct {
	Code int
	Msg  string
}

func (e *APIError) Error() string { return e.Msg }

// Refused reports whether err is the API server refusing a request as it
// stands (forbidden, invalid, denied by admission), which asking again
// won't change, rather than failing to answer.
func Refused(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Code >= 400 && e.Code < 500 && e.Code != 429
}
