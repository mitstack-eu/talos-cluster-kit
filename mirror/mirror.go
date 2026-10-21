// Package mirror renders the image list a cluster's private mirror holds.
package mirror

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"github.com/mitstack-eu/talos-cluster-kit/components"
)

// List returns every image the nodes pull, the set's and extra, sorted and each once, and refuses an image from a
// registry the nodes do not send to the mirror, since that pull would leave the cluster's network.
func List(set components.Set, extra ...components.Image) ([]components.Image, error) {
	images, err := set.Images()
	if err != nil {
		return nil, err
	}
	images = append(images, extra...)
	slices.SortFunc(images, func(a, b components.Image) int { return strings.Compare(a.Ref(), b.Ref()) })
	images = slices.Compact(images)
	for _, image := range images {
		if !slices.Contains(components.MirroredRegistries, image.Registry()) {
			return nil, fmt.Errorf("image %s is on %s, which no node sends to the mirror", image.Ref(), image.Registry())
		}
	}
	return images, nil
}

// Render writes the list one name:tag@digest reference per line, the form the gateway's prepare.sh reads.
func Render(images []components.Image) []byte {
	var out bytes.Buffer
	for _, image := range images {
		out.WriteString(image.Ref() + "\n")
	}
	return out.Bytes()
}
