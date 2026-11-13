package controlplane

import (
	"bytes"
	_ "embed"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/image"
	"github.com/mitstack-eu/talos-cluster-kit/serve"
	"go.yaml.in/yaml/v4"
)

var (
	//go:embed scripts/build-image.sh
	buildImageScript string
	//go:embed scripts/bootstrap-first-node.sh
	bootstrapFirstNodeScript string
)

const (
	cloudConfigHeader = "#cloud-config\n"

	builderConfigPath    = "/root/mitstack-build/config.yaml"
	builderScriptPath    = "/usr/local/bin/mitstack-build-image"
	builderGatewayCAPath = "/root/mitstack-build/gateway-ca.crt"
	// gatewayMirrorURL answers only once the gateway's prepare has filled the mirror.
	gatewayMirrorURL = "https://" + GatewayAddress + "/v2/"

	bootstrapTalosconfigPath = "/etc/mitstack-gateway/admin-talosconfig"
	bootstrapScriptPath      = "/usr/local/bin/mitstack-bootstrap-first-node"
	bootstrapUnit            = "mitstack-bootstrap-first-node.service"
	// subnetPrefix is how every address of the cluster's subnet begins, which the bootstrap looks for its node among.
	subnetPrefix = "10.0.0."
)

type cloudFile struct {
	Path        string `yaml:"path"`
	Permissions string `yaml:"permissions"`
	Content     string `yaml:"content"`
	Encoding    string `yaml:"encoding,omitempty"`
}

// mergeRule tells cloud-init to append this part's lists to the kit's, so neither part replaces the other's files or commands.
type mergeRule struct {
	Name     string   `yaml:"name"`
	Settings []string `yaml:"settings"`
}

type cloudConfig struct {
	MergeHow   []mergeRule `yaml:"merge_how,omitempty"`
	WriteFiles []cloudFile `yaml:"write_files"`
	RunCmd     [][]string  `yaml:"runcmd"`
}

func (c cloudConfig) render() (string, error) {
	out, err := yaml.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encoding a cloud-config: %w", err)
	}
	return cloudConfigHeader + string(out), nil
}

// servingFetch is where a builder fetches the manifests its user data has no room for, and the pin of the serving server's key.
type servingFetch struct {
	url, pin string
}

// builderUserData carries the configuration and the script gzipped, since as text they pass Hetzner's limit, and refuses a result still over it.
func builderUserData(what string, machineConfig, gatewayCA []byte, fetch *servingFetch) (string, error) {
	config, err := serve.Gzipped(machineConfig)
	if err != nil {
		return "", err
	}
	script, err := serve.Gzipped([]byte(withoutCommentLines(buildImageScript)))
	if err != nil {
		return "", err
	}
	run := []string{builderScriptPath, image.PinnedImager, builderConfigPath, routerAddress, gatewayMirrorURL, builderGatewayCAPath}
	if fetch != nil {
		run = append(run, fetch.url, fetch.pin)
	}
	out, err := cloudConfig{
		WriteFiles: []cloudFile{
			{Path: builderConfigPath, Permissions: "0600", Content: config, Encoding: serve.GzipBase64},
			{Path: builderScriptPath, Permissions: "0755", Content: script, Encoding: serve.GzipBase64},
			{Path: builderGatewayCAPath, Permissions: "0644", Content: string(gatewayCA)},
		},
		RunCmd: [][]string{run},
	}.render()
	if err != nil {
		return "", err
	}
	if len(out) > gateway.UserDataLimit {
		return "", fmt.Errorf("the %s's user data is %d bytes, over Hetzner's %d", what, len(out), gateway.UserDataLimit)
	}
	return out, nil
}

// withoutCommentLines drops every comment-only line after the first to save user data, and would drop one inside a heredoc.
func withoutCommentLines(script string) string {
	lines := strings.SplitAfter(script, "\n")
	kept := []string{lines[0]}
	for _, line := range lines[1:] {
		if !strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "")
}

// firstNodeBootstrap is the gateway's one etcd bootstrap: an admin talosconfig and the time it expires.
type firstNodeBootstrap struct {
	talosconfig []byte
	deadline    time.Time
}

func (b firstNodeBootstrap) cloudConfig() (string, error) {
	unit := "[Unit]\nDescription=Bootstraps etcd on the cluster's first node once it boots\n" +
		"[Service]\nType=oneshot\nExecStart=" + bootstrapScriptPath + " " + subnetPrefix + " " + GatewayAddress + " " +
		bootstrapTalosconfigPath + " " + strconv.FormatInt(b.deadline.Unix(), 10) + "\n"
	return cloudConfig{
		MergeHow: []mergeRule{{Name: "list", Settings: []string{"append"}}, {Name: "dict", Settings: []string{"no_replace", "recurse_list"}}},
		WriteFiles: []cloudFile{
			{Path: bootstrapTalosconfigPath, Permissions: "0600", Content: string(b.talosconfig)},
			{Path: bootstrapScriptPath, Permissions: "0755", Content: bootstrapFirstNodeScript},
			{Path: "/etc/systemd/system/" + bootstrapUnit, Permissions: "0644", Content: unit},
		},
		RunCmd: [][]string{{"systemctl", "start", "--no-block", bootstrapUnit}},
	}.render()
}

// gatewayUserData joins the kit's cloud-config, gzipped, since a list that keeps the images the cluster runs passes
// Hetzner's limit as text, and, when bootstrap is set, the bootstrap's as MIME parts, which cloud-init decompresses and
// merges in order. It refuses a result past Hetzner's user data limit.
func (p Plane) gatewayUserData(kitPart []byte, bootstrap *firstNodeBootstrap) (string, error) {
	zipped, err := serve.Gzipped(kitPart)
	if err != nil {
		return "", err
	}
	parts := []userDataPart{{name: "gateway.yaml.gz", contentType: "application/gzip", encoding: "base64", content: wrapped(zipped)}}
	if bootstrap != nil {
		bootstrapPart, err := bootstrap.cloudConfig()
		if err != nil {
			return "", err
		}
		parts = append(parts, userDataPart{name: "bootstrap.yaml", contentType: `text/cloud-config; charset="us-ascii"`, content: bootstrapPart})
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.SetBoundary(p.Boundary); err != nil {
		return "", fmt.Errorf("setting the user data boundary: %w", err)
	}
	for _, part := range parts {
		header := textproto.MIMEHeader{
			"Content-Type":        {part.contentType},
			"Content-Disposition": {`attachment; filename="` + part.name + `"`},
		}
		if part.encoding != "" {
			header.Set("Content-Transfer-Encoding", part.encoding)
		}
		pw, err := w.CreatePart(header)
		if err != nil {
			return "", fmt.Errorf("writing the user data part %s: %w", part.name, err)
		}
		if _, err := pw.Write([]byte(part.content)); err != nil {
			return "", fmt.Errorf("writing the user data part %s: %w", part.name, err)
		}
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("closing the user data: %w", err)
	}
	out := "Content-Type: multipart/mixed; boundary=\"" + p.Boundary + "\"\nMIME-Version: 1.0\n\n" + body.String()
	if len(out) > gateway.UserDataLimit {
		return "", fmt.Errorf("the gateway's user data is %d bytes, over Hetzner's %d", len(out), gateway.UserDataLimit)
	}
	return out, nil
}

type userDataPart struct {
	name, contentType, encoding, content string
}

// wrapped breaks base64 into the 76-character lines MIME allows.
func wrapped(b64 string) string {
	var out strings.Builder
	for len(b64) > 76 {
		out.WriteString(b64[:76] + "\n")
		b64 = b64[76:]
	}
	out.WriteString(b64 + "\n")
	return out.String()
}
