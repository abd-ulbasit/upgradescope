// Command seed fills a lab apiserver for the agent scale benchmark
// (hack/bench/agent.sh, #71): KWOK fake nodes that report Ready with a
// kubelet version, pods placed on them, ConfigMaps, zero-replica
// Deployments, and Helm release Secrets with real helm.sh/release.v1
// payloads (base64 of gzip of JSON) of realistic sizes.
//
// It reads the kubeconfig it is given and never the default one: the file
// must be named with --kubeconfig. It creates objects only; the lab is reset
// to vanilla by recreating the cluster, not by this tool.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// summary is what seeding created, printed as JSON for the harness.
type summary struct {
	Nodes, Namespaces, Pods, ConfigMaps, Deployments int
	HelmReleases, HelmSecrets                        int
	// Stored Helm payloads of the installed revisions, in bytes.
	HelmJSONBytes, HelmStoredBytes int
	HelmByClass                    map[string]int
	NodesReady, PodsRunning        int
	Seconds                        float64
}

func main() {
	var (
		kubeconfig string
		cfg        config
		workers    int
		wait       time.Duration
	)
	flag.StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig of the lab cluster (required; the default is never read)")
	flag.IntVar(&cfg.Nodes, "nodes", 2000, "KWOK fake nodes")
	flag.IntVar(&cfg.Namespaces, "namespaces", 100, "namespaces the objects spread over")
	flag.IntVar(&cfg.Pods, "pods", 10000, "pods, placed on the fake nodes")
	flag.IntVar(&cfg.ConfigMaps, "configmaps", 6000, "ConfigMaps")
	flag.IntVar(&cfg.Deployments, "deployments", 4000, "Deployments at zero replicas")
	flag.IntVar(&cfg.HelmReleases, "helm-releases", 1000, "Helm releases")
	flag.IntVar(&cfg.HelmRevisions, "helm-revisions", 1, "stored revisions per release (Secrets = releases x revisions)")
	flag.StringVar(&cfg.KubeletVersion, "kubelet-version", "v1.37.0", "kubelet version the fake nodes report")
	flag.Uint64Var(&cfg.Seed, "seed", 1, "random seed: the same seed creates the same objects")
	flag.IntVar(&workers, "workers", 32, "concurrent creates")
	flag.DurationVar(&wait, "wait", 10*time.Minute, "how long to wait for nodes Ready and pods Running; 0 does not wait")
	flag.Parse()
	if kubeconfig == "" {
		fmt.Fprintln(os.Stderr, "seed: --kubeconfig is required (the default kubeconfig is never used)")
		os.Exit(2)
	}
	rc, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
	rc.QPS, rc.Burst = 500, 1000
	rc.UserAgent = "upgradescope-bench-seed"
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
	sum, err := run(context.Background(), cs, cfg, workers, wait, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(sum)
}

// run seeds in dependency order and, with wait > 0, until KWOK has made the
// nodes Ready and the pods Running.
func run(ctx context.Context, cs kubernetes.Interface, cfg config, workers int, wait time.Duration, log io.Writer) (summary, error) {
	start := time.Now()
	sum := summary{HelmByClass: map[string]int{}}
	phase := func(name string, n int, create func(ctx context.Context, i int) error) error {
		if n == 0 {
			return nil
		}
		t := time.Now()
		if err := parallel(ctx, n, workers, create); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		fmt.Fprintf(log, "seed: %d %s in %s\n", n, name, time.Since(t).Round(time.Millisecond))
		return nil
	}
	if err := phase("namespaces", cfg.Namespaces, func(ctx context.Context, i int) error {
		return created(cs.CoreV1().Namespaces().Create(ctx, namespaceObject(i), metav1.CreateOptions{}))
	}); err != nil {
		return sum, err
	}
	sum.Namespaces = cfg.Namespaces
	if err := phase("nodes", cfg.Nodes, func(ctx context.Context, i int) error {
		return created(cs.CoreV1().Nodes().Create(ctx, nodeObject(i, cfg.KubeletVersion), metav1.CreateOptions{}))
	}); err != nil {
		return sum, err
	}
	sum.Nodes = cfg.Nodes
	var mu sync.Mutex
	if err := phase("helm releases", cfg.HelmReleases, func(ctx context.Context, i int) error {
		rel, err := helmReleaseSecrets(i, cfg)
		if err != nil {
			return err
		}
		for _, s := range rel.secrets {
			if err := created(cs.CoreV1().Secrets(s.Namespace).Create(ctx, s, metav1.CreateOptions{})); err != nil {
				return err
			}
		}
		mu.Lock()
		sum.HelmSecrets += len(rel.secrets)
		sum.HelmJSONBytes += rel.jsonSize
		sum.HelmStoredBytes += rel.gzSize
		sum.HelmByClass[rel.class]++
		mu.Unlock()
		return nil
	}); err != nil {
		return sum, err
	}
	sum.HelmReleases = cfg.HelmReleases
	if err := phase("configmaps", cfg.ConfigMaps, func(ctx context.Context, i int) error {
		cm := configMapObject(i, cfg)
		return created(cs.CoreV1().ConfigMaps(cm.Namespace).Create(ctx, cm, metav1.CreateOptions{}))
	}); err != nil {
		return sum, err
	}
	sum.ConfigMaps = cfg.ConfigMaps
	if err := phase("deployments", cfg.Deployments, func(ctx context.Context, i int) error {
		d := deploymentObject(i, cfg)
		return created(cs.AppsV1().Deployments(d.Namespace).Create(ctx, d, metav1.CreateOptions{}))
	}); err != nil {
		return sum, err
	}
	sum.Deployments = cfg.Deployments
	if err := phase("pods", cfg.Pods, func(ctx context.Context, i int) error {
		p := podObject(i, cfg)
		return created(cs.CoreV1().Pods(p.Namespace).Create(ctx, p, metav1.CreateOptions{}))
	}); err != nil {
		return sum, err
	}
	sum.Pods = cfg.Pods

	if wait > 0 {
		wctx, cancel := context.WithTimeout(ctx, wait)
		defer cancel()
		ready, running, err := awaitKWOK(wctx, cs, cfg, log)
		sum.NodesReady, sum.PodsRunning = ready, running
		if err != nil {
			return sum, err
		}
	}
	sum.Seconds = time.Since(start).Seconds()
	return sum, nil
}

// created treats an existing object as created: a rerun after a partial
// failure continues instead of failing on what it already made.
func created[T any](_ T, err error) error {
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// parallel runs f for 0..n-1 on workers goroutines and returns the first error.
func parallel(ctx context.Context, n, workers int, f func(ctx context.Context, i int) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		next     atomic.Int64
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for range max(workers, 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				if err := f(ctx, i); err != nil {
					once.Do(func() { firstErr = err; cancel() })
					return
				}
			}
		}()
	}
	wg.Wait()
	if firstErr == nil {
		firstErr = ctx.Err()
		if errors.Is(firstErr, context.Canceled) && next.Load() >= int64(n) {
			firstErr = nil
		}
	}
	return firstErr
}

// awaitKWOK polls until every fake node is Ready and every pod Running (the
// KWOK controller does both), or ctx ends; it returns the last counts.
func awaitKWOK(ctx context.Context, cs kubernetes.Interface, cfg config, log io.Writer) (ready, running int, err error) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		ready, running, err = countReady(ctx, cs)
		if err != nil {
			return ready, running, err
		}
		fmt.Fprintf(log, "seed: %d/%d nodes Ready, %d/%d pods Running\n", ready, cfg.Nodes, running, cfg.Pods)
		if ready >= cfg.Nodes && running >= cfg.Pods {
			return ready, running, nil
		}
		select {
		case <-ctx.Done():
			return ready, running, fmt.Errorf("KWOK did not finish (is its controller running?): %d/%d nodes Ready, %d/%d pods Running", ready, cfg.Nodes, running, cfg.Pods)
		case <-tick.C:
		}
	}
}

func countReady(ctx context.Context, cs kubernetes.Interface) (ready, running int, err error) {
	opts := metav1.ListOptions{LabelSelector: benchLabel + "=true,type=kwok", Limit: 1000}
	for {
		nodes, err := cs.CoreV1().Nodes().List(ctx, opts)
		if err != nil {
			return 0, 0, err
		}
		for _, n := range nodes.Items {
			for _, c := range n.Status.Conditions {
				if c.Type == "Ready" && c.Status == "True" {
					ready++
				}
			}
		}
		if nodes.Continue == "" {
			break
		}
		opts.Continue = nodes.Continue
	}
	popts := metav1.ListOptions{LabelSelector: benchLabel + "=true", FieldSelector: "status.phase=Running", Limit: 1000}
	for {
		pods, err := cs.CoreV1().Pods(metav1.NamespaceAll).List(ctx, popts)
		if err != nil {
			return 0, 0, err
		}
		running += len(pods.Items)
		if pods.Continue == "" {
			break
		}
		popts.Continue = pods.Continue
	}
	return ready, running, nil
}
