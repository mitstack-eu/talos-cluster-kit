package pools

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/mitstack-eu/talos-cluster-kit/etcd"
	"github.com/mitstack-eu/talos-cluster-kit/internal/check"
	"github.com/mitstack-eu/talos-cluster-kit/internal/tmpl"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
)

var (
	//go:embed worker-job.yaml
	workerJobTemplate string
	//go:embed scripts/worker-job.sh
	workerJobScript string
)

var workerJob = tmpl.Must("a Cloud worker's replacement Job", workerJobTemplate)

const (
	drainStep      = "drain"
	removeNodeStep = "remove-node"
	// drainAttempts gives a disruption budget about an hour: six drains of drainTimeout, and the backoff between them.
	drainAttempts   = 6
	removalAttempts = 4
	drainTimeout    = "600s"
	jobNamePrefix   = "mitstack-"
)

func jobName(step, key string) string { return jobNamePrefix + step + "-" + key }

func jobPath(step, key string) string { return etcd.RemovalDir + jobName(step, key) + ".yaml" }

// IsRemovalPath reports whether path is a file a Cloud worker's replacement writes to the cluster repository.
func IsRemovalPath(path string) bool {
	for _, step := range []string{drainStep, removeNodeStep} {
		name, inDir := strings.CutPrefix(path, etcd.RemovalDir+jobNamePrefix+step+"-")
		key, isYAML := strings.CutSuffix(name, ".yaml")
		if inDir && isYAML && check.DNS1123Label(key) == nil {
			return true
		}
	}
	return false
}

func renderWorkerJob(step, key, hostname string) ([]byte, error) {
	if err := check.DNS1123Label(hostname); err != nil {
		return nil, fmt.Errorf("Cloud worker: %w", err)
	}
	attempts := removalAttempts
	if step == drainStep {
		attempts = drainAttempts
	}
	return tmpl.Execute(workerJob, map[string]any{
		"Name": jobName(step, key), "Namespace": nodeconfig.TalosAPINamespace, "Hostname": hostname, "Step": step,
		"Image": tools.Image, "Script": workerJobScript, "BackoffLimit": attempts - 1, "Attempts": attempts, "DrainTimeout": drainTimeout,
	})
}
