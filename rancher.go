package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type httpError struct {
	Code int
	URL  string
	Body string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("HTTP %d from %s: %s", e.Code, e.URL, e.Body)
}

func isStatus(err error, code int) bool {
	var he *httpError
	return errors.As(err, &he) && he.Code == code
}

type rancherClient struct {
	ep    Endpoint
	base  string
	token string
	http  *http.Client
}

func newRancherClient(ep Endpoint, timeout time.Duration) (*rancherClient, error) {
	token := strings.TrimSpace(os.Getenv(ep.TokenEnv))
	if token == "" {
		return nil, fmt.Errorf("endpoint %s: env var %s is empty", ep.Name, ep.TokenEnv)
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: ep.InsecureSkipTLSVerify} //nolint:gosec // opt-in
	if ep.CAFile != "" {
		pem, err := os.ReadFile(ep.CAFile)
		if err != nil {
			return nil, fmt.Errorf("endpoint %s: %w", ep.Name, err)
		}
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("endpoint %s: no certs found in %s", ep.Name, ep.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment, // honours HTTPS_PROXY / NO_PROXY
		TLSClientConfig:     tlsCfg,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	return &rancherClient{
		ep:    ep,
		base:  strings.TrimRight(ep.URL, "/"),
		token: token,
		http:  &http.Client{Transport: tr, Timeout: timeout},
	}, nil
}

func (c *rancherClient) getJSON(ctx context.Context, pathOrURL string, v any) error {
	u := pathOrURL
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = c.base + pathOrURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return &httpError{Code: resp.StatusCode, URL: u, Body: strings.TrimSpace(string(b))}
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

type rancherCluster struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	State  string            `json:"state"`
	Labels map[string]string `json:"labels"`
}

func (c *rancherClient) listClusters(ctx context.Context) ([]rancherCluster, error) {
	var out []rancherCluster
	next := "/v3/clusters?limit=1000"
	for next != "" {
		var page struct {
			Data       []rancherCluster `json:"data"`
			Pagination struct {
				Next string `json:"next"`
			} `json:"pagination"`
		}
		if err := c.getJSON(ctx, next, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Data...)
		next = page.Pagination.Next
	}
	return out, nil
}

// listAll pages through a Kubernetes list endpoint via the Rancher proxy.
func listAll[T any](ctx context.Context, c *rancherClient, path string) ([]T, error) {
	var out []T
	cont := ""
	for {
		q := url.Values{}
		q.Set("limit", "500")
		if cont != "" {
			q.Set("continue", cont)
		}
		var page struct {
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []T `json:"items"`
		}
		if err := c.getJSON(ctx, path+"?"+q.Encode(), &page); err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.Metadata.Continue == "" {
			return out, nil
		}
		cont = page.Metadata.Continue
	}
}

func proxyPath(clusterID string) string {
	return "/k8s/clusters/" + url.PathEscape(clusterID)
}

// ---- minimal Kubernetes types (only the fields we need) ----

type ownerRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

type objectMeta struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations"`
	OwnerReferences []ownerRef        `json:"ownerReferences"`
}

type ingressBackend struct {
	Service *struct {
		Name string `json:"name"`
		Port struct {
			Name   string `json:"name"`
			Number int32  `json:"number"`
		} `json:"port"`
	} `json:"service"`
	Resource *struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"resource"`
}

type ingressSpec struct {
	IngressClassName *string         `json:"ingressClassName"`
	DefaultBackend   *ingressBackend `json:"defaultBackend"`
	TLS              []struct {
		Hosts      []string `json:"hosts"`
		SecretName string   `json:"secretName"`
	} `json:"tls"`
	Rules []struct {
		Host string `json:"host"`
		HTTP *struct {
			Paths []struct {
				Path     string         `json:"path"`
				PathType *string        `json:"pathType"`
				Backend  ingressBackend `json:"backend"`
			} `json:"paths"`
		} `json:"http"`
	} `json:"rules"`
}

type k8sIngress struct {
	Metadata objectMeta  `json:"metadata"`
	Spec     ingressSpec `json:"spec"`
}

type k8sGslb struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Ingress     *ingressSpec `json:"ingress"`
		ResourceRef *struct {
			APIVersion       string             `json:"apiVersion"`
			Kind             string             `json:"kind"`
			Name             string             `json:"name"`
			Namespace        string             `json:"namespace"`
			MatchLabels      map[string]string  `json:"matchLabels"`
			MatchExpressions []labelSelectorReq `json:"matchExpressions"`
		} `json:"resourceRef"`
		Strategy struct {
			Type                       string         `json:"type"`
			PrimaryGeoTag              string         `json:"primaryGeoTag"`
			DNSTtlSeconds              int64          `json:"dnsTtlSeconds"`
			SplitBrainThresholdSeconds int64          `json:"splitBrainThresholdSeconds"`
			Weight                     map[string]any `json:"weight"`
		} `json:"strategy"`
	} `json:"spec"`
	Status struct {
		ServiceHealth  map[string]string   `json:"serviceHealth"`
		HealthyRecords map[string][]string `json:"healthyRecords"`
		GeoTag         string              `json:"geoTag"`
		Hosts          string              `json:"hosts"`
	} `json:"status"`
}

type labelSelectorReq struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"` // In | NotIn | Exists | DoesNotExist
	Values   []string `json:"values"`
}

type fleetCluster struct {
	Metadata objectMeta `json:"metadata"`
}
