// Package image builds a cluster's disk image with imager, Sidero Labs' open-source image builder.
package image

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/mitstack-eu/talos-cluster-kit/internal/check"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
)

// PinnedImager is the imager release that builds Talos v1.14.4, by digest.
const PinnedImager = "ghcr.io/siderolabs/imager:v1.14.4@sha256:91d453a7cf57d499fe7fe7923e1e381b6e1535136367246d190e38486c11651d"

// arch is the CPU architecture of every image the kit builds.
const arch = "amd64"

// configName is the file name imager reads the embedded configuration from, inside its /in mount.
const configName = "config.yaml"

// Build is one run of imager.
type Build struct {
	Profile nodeconfig.Profile
	// Config is the machine configuration the image embeds, which holds every key of the cluster.
	Config []byte
	// OutDir receives the image and nothing else.
	OutDir string
	// Imager is the imager image by digest; PinnedImager when empty.
	Imager string
	// Docker is the container runtime's command; docker when empty.
	Docker string
}

// Run builds the image and returns its path, discarding imager's output, since imager prints its whole profile and
// so every key of the embedded configuration.
func Run(ctx context.Context, b Build, log io.Writer) (string, error) {
	b = b.withDefaults()
	if err := b.validate(); err != nil {
		return "", err
	}
	in, err := writeConfig(b.Config)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(in)

	cmd := exec.CommandContext(ctx, b.Docker, b.args(in)...)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("imager %s for profile %s failed with %s; its output is discarded because it holds every cluster key", b.Imager, b.Profile, exitStatus(err))
	}
	image, err := builtImage(b.OutDir)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(log, "imager built %s for profile %s\n", filepath.Base(image), b.Profile)
	return image, nil
}

func (b Build) withDefaults() Build {
	if b.Imager == "" {
		b.Imager = PinnedImager
	}
	if b.Docker == "" {
		b.Docker = "docker"
	}
	return b
}

func (b Build) validate() error {
	if b.Profile != nodeconfig.HCloud && b.Profile != nodeconfig.Metal {
		return fmt.Errorf("profile %q is neither %q nor %q", b.Profile, nodeconfig.HCloud, nodeconfig.Metal)
	}
	if err := check.DigestRef(b.Imager); err != nil {
		return fmt.Errorf("imager: %w", err)
	}
	if len(b.Config) == 0 {
		return errors.New("the build has no machine configuration to embed")
	}
	if b.OutDir == "" {
		return errors.New("the build has no output directory")
	}
	return nil
}

// args mount the configuration read-only and /dev for the loop devices imager partitions the image with.
func (b Build) args(in string) []string {
	return []string{
		"run", "--rm", "--privileged",
		"-v", "/dev:/dev",
		"-v", in + ":/in:ro",
		"-v", b.OutDir + ":/out",
		b.Imager, string(b.Profile),
		"--arch", arch,
		"--embedded-config-path", "/in/" + configName,
	}
}

// writeConfig writes the configuration into a directory only the build's user can read.
func writeConfig(config []byte) (string, error) {
	dir, err := os.MkdirTemp("", "talos-imager-in-")
	if err != nil {
		return "", fmt.Errorf("create the imager's input directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, configName), config, 0o600); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("write the imager's input: %w", err)
	}
	return dir, nil
}

func builtImage(outDir string) (string, error) {
	images, err := filepath.Glob(filepath.Join(outDir, "*.raw.zst"))
	if err != nil || len(images) != 1 {
		return "", fmt.Errorf("imager left %d images in %s, want 1", len(images), outDir)
	}
	return images[0], nil
}

func exitStatus(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Sprintf("exit status %d", exit.ExitCode())
	}
	return fmt.Sprintf("no exit status, as it did not start: %v", err)
}
