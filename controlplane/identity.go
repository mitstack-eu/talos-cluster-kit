package controlplane

// Every call finds a subscription's resources by their labels alone, since a name is only a readable handle.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

const (
	KeyBlock           = "mitstack.dev/block"
	KeySubscription    = "mitstack.dev/subscription"
	KeyRole            = "mitstack.dev/role"
	KeyEnrolmentSHA256 = "mitstack.dev/enrolment-sha256"
	KeyIssuerBucket    = "mitstack.dev/issuer-bucket"
	// KeyGatewayList is the digest of the pinned list a gateway was built from, as gateway.ListDigest gives it.
	KeyGatewayList = "mitstack.dev/gateway-list"
	// KeyPoweredOnAt is when the plane last powered a gateway back on, in Unix seconds, so it does so once an outage.
	KeyPoweredOnAt = "mitstack.dev/powered-on-at"

	// maxNameLength is the longest hostname label, which a Hetzner server name must be.
	maxNameLength           = 63
	nameSuffixLength        = 8
	enrolmentTokenBytes     = 32
	issuerBucketPrefix      = "mitstack-oidc-"
	issuerBucketRandomBytes = 16
	issuerRegion            = "eu-central-1"
)

// Role names what a labelled resource is to the cluster, since several share a Hetzner kind.
type Role string

const (
	RoleGateway Role = "gateway"
	RoleBuilder Role = "builder"
	RoleNode    Role = "node"
	RoleImage   Role = "image"
	RoleAPI     Role = "api"
	RoleIngress Role = "ingress"
	// RoleServing is the short-lived server that hands the builder a hosting set's keys and a signer list's policy.
	RoleServing Role = "serving"
	// RoleWorkerBuilder builds the worker image, which RoleWorkerImage labels once it is a snapshot.
	RoleWorkerBuilder Role = "worker-builder"
	RoleWorkerImage   Role = "worker-image"
	// RoleCloudWorker labels each server of the Cloud pool, which boots from the worker image.
	RoleCloudWorker Role = "cloud-worker"
)

var labelValueRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?$`)

var lowerBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// ValidLabelValue reports whether s may stand as a Hetzner label value, as a subscription id must.
func ValidLabelValue(s string) bool {
	return labelValueRe.MatchString(s)
}

// Selector finds every resource of the plane's subscription.
func (p Plane) Selector() string {
	return KeyBlock + "=" + p.Block + "," + KeySubscription + "=" + p.Subscription
}

// RoleSelector is the label selector a load balancer finds its targets by, so a replaced node needs no edit to it.
func (p Plane) RoleSelector(r Role) string {
	return p.Selector() + "," + KeyRole + "=" + string(r)
}

// Labels are the labels a resource of role r carries.
func (p Plane) Labels(r Role) map[string]string {
	return map[string]string{KeyBlock: p.Block, KeySubscription: p.Subscription, KeyRole: string(r)}
}

// Name is the app's name and the subscription's first characters, with the role after them.
func (p Plane) Name(r Role) string {
	suffix := hostnamePart(p.Subscription)
	if len(suffix) > nameSuffixLength {
		suffix = strings.Trim(suffix[:nameSuffixLength], "-")
	}
	tail := "-" + suffix + "-" + string(r)
	prefix := hostnamePart(p.AppName)
	if room := maxNameLength - len(tail); len(prefix) > room {
		prefix = strings.Trim(prefix[:room], "-")
	}
	if prefix == "" {
		prefix = "talos"
	}
	return prefix + tail
}

// LocatedName is Name with the location after the role, so the name says where the resource runs.
func (p Plane) LocatedName(r Role, loc string) string {
	return p.Name(Role(string(r) + "-" + loc))
}

func (p Plane) clusterName() string {
	if name := hostnamePart(p.AppName); name != "" {
		return name
	}
	return "talos"
}

func hostnamePart(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// enrolmentTokenDigest is base32 rather than hex, since its 52 characters fit a Hetzner label value.
func enrolmentTokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return strings.ToLower(lowerBase32.EncodeToString(sum[:]))
}

func mintEnrolmentToken() (string, error) {
	raw := make([]byte, enrolmentTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("minting the enrolment token: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// mintIssuerBucket takes nothing from the subscription, org or app, so the bucket name cannot be guessed.
func mintIssuerBucket() (string, error) {
	raw := make([]byte, issuerBucketRandomBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("minting the issuer bucket name: %w", err)
	}
	return issuerBucketPrefix + strings.ToLower(lowerBase32.EncodeToString(raw)), nil
}

// issuerURL is where the platform publishes the cluster's discovery document and key set, in the bucket's own host.
func issuerURL(bucket, subscriptionID string) *url.URL {
	return &url.URL{Scheme: "https", Host: bucket + ".s3." + issuerRegion + ".amazonaws.com", Path: "/" + subscriptionID}
}

// ClassGatewayLabelsLost refuses to rebuild the only gateway of a plane in one location whose network holds no
// enrolment digest or issuer bucket, which a release before the network held them kept on the gateway alone.
const ClassGatewayLabelsLost = "gateway-labels-lost"

// identityKeys are the cluster's enrolment digest and issuer bucket, which its network holds for the cluster's whole
// life, since a gateway rebuilt while no other stands has no sibling to copy them from.
var identityKeys = []string{KeyEnrolmentSHA256, KeyIssuerBucket}

func holdsIdentity(network Network) bool { return len(missingIdentity(network)) == 0 }

// missingIdentity names each identity label the network lacks, joined for a message.
func missingIdentity(network Network) []string {
	return slices.DeleteFunc(slices.Clone(identityKeys), func(key string) bool { return network.Labels[key] != "" })
}

// setIdentity writes values to the network under identityKeys, and takes each key with no value off.
func (p Plane) setIdentity(ctx context.Context, network Network, values map[string]string) error {
	labels := maps.Clone(network.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	for _, key := range identityKeys {
		delete(labels, key)
		if values[key] != "" {
			labels[key] = values[key]
		}
	}
	return p.API.SetNetworkLabels(ctx, network.ID, labels)
}

// copyIdentityStep copies onto the network each identity label that only the gateways of a cluster an earlier release
// built carry, since every reader reads them from the network.
func (p Plane) copyIdentityStep(st State, network Network) (Step, bool) {
	gw, ok := st.SharedGateway()
	if !ok || holdsIdentity(network) {
		return Step{}, false
	}
	values := map[string]string{}
	var copied []string
	for _, key := range identityKeys {
		values[key] = network.Labels[key]
		if values[key] == "" && gw.Labels[key] != "" {
			values[key] = gw.Labels[key]
			copied = append(copied, key)
		}
	}
	if len(copied) == 0 {
		return Step{}, false
	}
	return Step{fmt.Sprintf("copy %s from gateway %d onto the network, which holds them for the cluster's whole life", strings.Join(copied, " and "), gw.ID),
		func(ctx context.Context) error { return p.setIdentity(ctx, network, values) }}, true
}
