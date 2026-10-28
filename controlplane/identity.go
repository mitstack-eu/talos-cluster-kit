package controlplane

// Every call finds a subscription's resources by their labels alone, since a name is only a readable handle.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

const (
	KeyBlock           = "mitstack.dev/block"
	KeySubscription    = "mitstack.dev/subscription"
	KeyRole            = "mitstack.dev/role"
	KeyEnrolmentSHA256 = "mitstack.dev/enrolment-sha256"
	KeyIssuerBucket    = "mitstack.dev/issuer-bucket"

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
	// RoleServing is the short-lived server that hands the builder a hosting cluster's hosting set.
	RoleServing Role = "serving"
	// RoleWorkerBuilder builds the worker image, which RoleWorkerImage labels once it is a snapshot.
	RoleWorkerBuilder Role = "worker-builder"
	RoleWorkerImage   Role = "worker-image"
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
