package mirror

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"github.com/mitstack-eu/talos-cluster-kit/components"
)

// List returns every image the nodes pull, sorted and each once, and refuses one the mirror cannot serve under the name a node asks for.
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
		if repository, ok := strings.CutPrefix(image.Name, "docker.io/"); ok && !strings.Contains(repository, "/") {
			return nil, fmt.Errorf("image %s names Docker Hub without its namespace, and a node asks the mirror for docker.io/library/%s", image.Ref(), repository)
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
