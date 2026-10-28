package controlplane

// A hosting install's keys exist only in this call's memory, the serving server's file, the builder's disk, the
// snapshot it becomes and whatever the block's DeployKeys hook makes from them.

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/openbaoinit"
	"github.com/mitstack-eu/talos-cluster-kit/serve"
)

// Hosting is what a platform-hosting install takes from its block.
type Hosting struct {
	// Repository is the cluster repository, owner/name, which Argo CD and OpenBao's init Job reach with deploy keys.
	Repository string
	// Recipient is the armored OpenPGP public key OpenBao's keys leave the cluster encrypted to.
	Recipient []byte
	// DeployKeys, called once with the keys the install mints, returns the deploy keys sealed to them; an error stops the install.
	DeployKeys func(ctx context.Context, keys HostingKeys) (SealedDeployKeys, error)
}

// HostingKeys are the keys a hosting install mints.
type HostingKeys struct {
	SealKey []byte
	// SealedSecretsKeyPEM and SealedSecretsCertPEM are the pair the cluster's Sealed Secrets opens with.
	SealedSecretsKeyPEM, SealedSecretsCertPEM []byte
}

// SealedDeployKeys are SealedSecret manifests that open to the write key as components.DeployKeySecret and to the
// read-only key as components.RepositoryCredentialName.
type SealedDeployKeys struct {
	Write, Read []byte
}

const servingFile = serve.Dir + "/hosting.yaml"

func hostingURL() string {
	return "https://" + ServingAddress + ":" + strconv.Itoa(serve.DefaultPort) + "/hosting.yaml"
}

// hostingSet mints the hosting keys, has the block seal its deploy keys to them, and renders the hosting set.
func (p Plane) hostingSet(ctx context.Context, now time.Time) (components.Set, error) {
	sealKey := make([]byte, components.SealKeyBytes)
	if _, err := rand.Read(sealKey); err != nil {
		return components.Set{}, fmt.Errorf("minting OpenBao's seal key: %w", err)
	}
	keyPEM, certPEM, err := components.NewSealedSecretsKeyPair(now)
	if err != nil {
		return components.Set{}, err
	}
	sealed, err := p.Hosting.DeployKeys(ctx, HostingKeys{SealKey: sealKey, SealedSecretsKeyPEM: keyPEM, SealedSecretsCertPEM: certPEM})
	if err != nil {
		return components.Set{}, err
	}
	nodes := make([]string, len(Locations))
	for i, loc := range Locations {
		nodes[i] = p.LocatedName(RoleNode, loc)
	}
	return components.Hosting(components.HostingInputs{
		SealKey: sealKey, SealedSecretsKeyPEM: keyPEM, SealedSecretsCertPEM: certPEM,
		SealedWriteKey: sealed.Write, SealedReadKey: sealed.Read, Nodes: nodes,
		Init: openbaoinit.Inputs{ToolsImage: toolsImage(), Repository: p.Hosting.Repository, Recipient: p.Hosting.Recipient},
	})
}

// splitHosting takes out of set the inline manifests the hosting set adds to the default one, which the builder
// fetches rather than carries, since with them its user data is over Hetzner's limit.
func splitHosting(set, hostingSet components.Set) (carried, fetched []components.Manifest) {
	var defaults []string
	for _, m := range components.Default().Inline {
		defaults = append(defaults, m.Name)
	}
	var hosting []string
	for _, m := range hostingSet.Inline {
		if !slices.Contains(defaults, m.Name) {
			hosting = append(hosting, m.Name)
		}
	}
	for _, m := range set.Inline {
		if slices.Contains(hosting, m.Name) {
			fetched = append(fetched, m)
		} else {
			carried = append(carried, m)
		}
	}
	return carried, fetched
}

// servingUserData serves the hosting set's inline manifests, as configuration documents, to the builder alone.
func servingUserData(hosting []components.Manifest, now time.Time) (string, *hostingFetch, error) {
	docs, err := nodeconfig.InlineManifests(hosting)
	if err != nil {
		return "", nil, err
	}
	content, err := serve.Gzipped(docs)
	if err != nil {
		return "", nil, err
	}
	key, err := serve.NewKey("mitstack hosting set", now)
	if err != nil {
		return "", nil, err
	}
	file := serve.File{Path: servingFile, Permissions: "0600", Content: content, Encoding: serve.GzipBase64}
	data, err := serve.UserData(append(key.Files(), file), serve.Command(netip.MustParseAddr(BuilderAddress), serve.DefaultPort, servingFile))
	if err != nil {
		return "", nil, err
	}
	return data, &hostingFetch{url: hostingURL(), pin: key.Pin}, nil
}
