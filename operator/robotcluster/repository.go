package robotcluster

import (
	"github.com/mitstack-eu/block-kit/contract"
	"github.com/mitstack-eu/talos-cluster-kit/operator/clusterrepo"
)

type githubAccess struct {
	*clusterrepo.Access
	k *Cluster
	// held keeps each listed commit's blob SHAs by path, which never change for a commit.
	held map[string]map[string]string
}

func (k *Cluster) dialGitHub(d contract.Desired, appName string) (*githubAccess, error) {
	a, err := k.dialRepository(d, appName)
	if err != nil {
		return nil, err
	}
	return &githubAccess{Access: a, k: k}, nil
}

func (k *Cluster) dialRepository(d contract.Desired, appName string) (*clusterrepo.Access, error) {
	return clusterrepo.Dial(d, k.cfg.Block, appName, k.cfg.GitHubClient)
}
