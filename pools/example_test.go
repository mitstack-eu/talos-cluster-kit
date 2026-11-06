package pools_test

import (
	"fmt"

	"github.com/mitstack-eu/talos-cluster-kit/pools"
)

func ExampleParse() {
	p, err := pools.Parse(map[string]string{
		"dedicatedPool":        `[{"serverNumber": "3041839", "robotAccount": "inst_robot_main"}]`,
		"cloudPoolServerTypes": "cpx32, cx43",
		"cloudPoolLocations":   "fsn1, nbg1",
		"cloudPoolMin":         "2",
		"cloudPoolMax":         "2",
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(len(p.Dedicated), "dedicated server and", p.Cloud.Max, "Cloud servers over", p.Cloud.Locations)
	// Output: 1 dedicated server and 2 Cloud servers over [fsn1 nbg1]
}
