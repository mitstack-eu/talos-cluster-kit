package controlplane_test

// A hosting install mints its keys, has the block seal its deploy keys to them, and hands the builder the hosting
// set from a serving server that goes with the builder.

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"net/netip"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane"
	"github.com/mitstack-eu/talos-cluster-kit/controlplane/cloudtest"
	"github.com/mitstack-eu/talos-cluster-kit/serve"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
)

// hostingInline are the inline manifests the hosting set adds, which hold its keys.
var hostingInline = []string{
	"mitstack-openbao-seal-key", "mitstack-sealed-secrets-key", "mitstack-openbao-deploy-key",
	"mitstack-cluster-repository", "mitstack-openbao-storage", "mitstack-openbao-init",
}

// hosting makes the plane a hosting install whose DeployKeys hook seals the fixture deploy keys as a block does.
func hosting(h *harness) {
	_, recipient := testutil.Recipient(h.t)
	h.plane.Hosting = &controlplane.Hosting{
		Repository: testutil.InitRepository,
		Recipient:  recipient,
		DeployKeys: func(_ context.Context, keys controlplane.HostingKeys) (controlplane.SealedDeployKeys, error) {
			h.hostingKeys = append(h.hostingKeys, keys)
			if h.deployKeysErr != nil {
				return controlplane.SealedDeployKeys{}, h.deployKeysErr
			}
			write, err := components.Seal(keys.SealedSecretsCertPEM, components.DeployKeyNamespace, components.DeployKeySecret, components.DeployKeyItem, []byte(testutil.WriteKey(h.t)))
			require.NoError(h.t, err)
			read, err := components.Seal(keys.SealedSecretsCertPEM, components.RepositoryCredentialNamespace, components.RepositoryCredentialName, components.RepositoryCredentialItem, []byte(testutil.ReadKey(h.t)))
			require.NoError(h.t, err)
			return controlplane.SealedDeployKeys{Write: write, Read: read}, nil
		},
	}
}

// servingFile is the file the serving server serves, decoded as cloud-init writes it.
func servingFile(t *testing.T, h *harness) []map[string]any {
	t.Helper()
	data := decodeCloudConfig(t, h.cloud.SpecFor(t, controlplane.RoleServing).UserData)
	return testutil.Documents(t, []byte(data.file(t, controlplane.ServingFile).Content))
}

// pinOf is the pin curl takes for the certificate's public key.
func pinOf(t *testing.T, certPEM string) string {
	t.Helper()
	block, _ := pem.Decode([]byte(certPEM))
	require.NotNil(t, block, "the serving certificate is not PEM")
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	spki, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	require.NoError(t, err)
	sum := sha256.Sum256(spki)
	return "sha256//" + base64.StdEncoding.EncodeToString(sum[:])
}

func names(docs []map[string]any) []string {
	var out []string
	for _, doc := range testutil.Kind(docs, "KubeInlineManifestConfig") {
		out = append(out, testutil.ManifestName(doc["name"].(string)))
	}
	return out
}

func TestHosting_InstallsEveryNodeWithTheBuilderFetchingTheHostingSetFromTheServingServer(t *testing.T) {
	h := newHarness(t, hosting)
	require.NoError(t, h.settle(h.building))
	network := h.network()
	serving := h.byRole(controlplane.RoleServing)
	require.Len(t, serving, 1, "serving servers while the builder builds")
	builderRun := decodeCloudConfig(t, h.cloud.SpecFor(t, controlplane.RoleBuilder).UserData).RunCmd[0]
	servingConfig := decodeCloudConfig(t, h.cloud.SpecFor(t, controlplane.RoleServing).UserData)

	h.built()

	assert.Equal(t, controlplane.ServingAddress, serving[0].PrivateIPs[network.ID], "the serving server's address")
	assert.Equal(t, controlplane.ServerRunning, serving[0].Status, "the serving server while the builder builds")
	assert.Empty(t, serving[0].PublicIPv4, "the serving server's public address")
	assert.Equal(t, serve.Command(netip.MustParseAddr(controlplane.BuilderAddress), serve.DefaultPort, controlplane.ServingFile), servingConfig.RunCmd[0],
		"the serving server serves its file to the builder alone")
	assert.Equal(t, []string{"https://" + controlplane.ServingAddress + ":" + strconv.Itoa(serve.DefaultPort) + "/hosting.yaml",
		pinOf(t, servingConfig.file(t, serve.CertPath).Content)}, builderRun[len(builderRun)-2:], "where the builder fetches the hosting set, and the key it pins")
	assert.Len(t, h.byRole(controlplane.RoleNode), len(controlplane.Locations), "nodes")
	assert.Empty(t, h.byRole(controlplane.RoleServing), "a serving server outlived the build")
}

func TestHosting_TheServingServerServesTheHostingSetsInlineManifestsAlone(t *testing.T) {
	h := newHarness(t, hosting)

	require.NoError(t, h.converge())

	served := servingFile(t, h)
	assert.ElementsMatch(t, hostingInline, names(served), "the inline manifests the serving server holds")
	assert.Len(t, served, len(hostingInline), "documents the serving server holds")
	require.Len(t, h.hostingKeys, 1, "calls to the DeployKeys hook")
	seal := testutil.Documents(t, []byte(testutil.Kind(served, "KubeInlineManifestConfig")[slices.Index(names(served), "mitstack-openbao-seal-key")]["manifest"].(string)))
	assert.Equal(t, base64.StdEncoding.EncodeToString(h.hostingKeys[0].SealKey), testutil.Field(t, seal[0], "data", "key"), "the seal key served")
}

func TestHosting_TheBuildersUserDataStaysWithinHetznersLimitAndHoldsNoHostingManifest(t *testing.T) {
	h := newHarness(t, hosting)

	require.NoError(t, h.converge())

	data := h.cloud.SpecFor(t, controlplane.RoleBuilder).UserData
	t.Logf("the hosting builder's user data is %d bytes", len(data))
	assert.LessOrEqual(t, len(data), cloudtest.UserDataLimit, "the builder's user data is %d bytes; Hetzner accepts %d", len(data), cloudtest.UserDataLimit)
	carried := names(builderConfiguration(t, h))
	for _, name := range hostingInline {
		assert.NotContains(t, carried, name, "the builder's user data carries a hosting manifest")
	}
	require.Len(t, h.hostingKeys, 1, "calls to the DeployKeys hook")
	block, _ := pem.Decode(h.hostingKeys[0].SealedSecretsKeyPEM)
	require.NotNil(t, block)
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	require.NoError(t, err)
	assert.Equal(t, 4096, key.N.BitLen(), "the Sealed Secrets key's bits")
}

func TestHosting_TheServingServerGoesWithABuildThatFails(t *testing.T) {
	h := newHarness(t, hosting)
	h.buildingBuilder()
	require.Len(t, h.byRole(controlplane.RoleServing), 1, "serving servers while the builder builds")

	err := h.settle(nil)

	assert.Equal(t, "image-build-failed", refusalClass(t, err))
	assert.Empty(t, h.byRole(controlplane.RoleServing), "the serving server of a build that failed")
	assert.Empty(t, h.byRole(controlplane.RoleBuilder), "the builder past its deadline")
}

func TestHosting_ADeployKeysHookThatFailsStopsTheInstallBeforeAnyServer(t *testing.T) {
	h := newHarness(t, hosting)
	h.deployKeysErr = errors.New("GitHub refused the deploy key")

	err := h.converge()

	require.ErrorIs(t, err, h.deployKeysErr)
	for _, r := range []controlplane.Role{controlplane.RoleGateway, controlplane.RoleBuilder, controlplane.RoleServing, controlplane.RoleWorkerBuilder} {
		assert.Empty(t, h.byRole(r), "%s servers", r)
	}
}

func TestHosting_TheClusterTakesOnlyItsReportTokenFromThePlatformAndArgoCDReadsItsRepositoryOverSSH(t *testing.T) {
	for _, tc := range []struct {
		name                string
		opts                []func(*harness)
		credential, repoURL string
		renews              bool
		argoCDSecrets       []string
	}{
		{"a hosting cluster", []func(*harness){hosting}, "deploy-key", components.RepositorySSHURL(testutil.InitRepository), false,
			[]string{"argocd-notifications-secret"}},
		{"a cluster that hosts no platform", nil, "platform-token", testRepoURL, true, []string{"cluster-repository", "argocd-notifications-secret"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.opts...)

			require.NoError(t, h.converge())

			manifests := inlineManifests(t, builderConfiguration(t, h))
			tokens := manifests["mitstack-cluster-tokens"]
			jobs := testutil.Kind(tokens, "Job")
			require.Len(t, jobs, 1, "enrolment Jobs")
			env := testutil.ContainerEnv(t, testutil.Field(t, jobs[0], "spec", "template", "spec", "containers", "0"))
			assert.Equal(t, tc.credential, env["REPOSITORY_CREDENTIAL"], "how the cluster reads its repository")
			assert.Equal(t, tc.renews, len(testutil.Kind(tokens, "CronJob")) == 1, "whether the cluster renews repository tokens")
			var named []string
			for _, role := range testutil.Kind(tokens, "Role") {
				if testutil.Field(t, role, "metadata", "namespace") != "argocd" {
					continue
				}
				for _, rule := range testutil.Field(t, role, "rules").([]any) {
					names, _ := rule.(map[string]any)["resourceNames"].([]any)
					for _, name := range names {
						named = append(named, name.(string))
					}
				}
			}
			assert.Equal(t, tc.argoCDSecrets, named, "the Secrets the token identity may read and patch in argocd")
			for _, app := range testutil.Kind(manifests["mitstack-cluster-application"], "Application") {
				assert.Equal(t, tc.repoURL, testutil.Field(t, app, "spec", "source", "repoURL"), "application %s's repository", testutil.Field(t, app, "metadata", "name"))
			}
		})
	}
}
