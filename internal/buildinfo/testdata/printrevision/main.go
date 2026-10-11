// Command printrevision prints buildinfo.Revision for the link-flag test.
package main

import (
	"fmt"

	"go.kenn.io/benchdb/internal/buildinfo"
)

func main() {
	fmt.Print(buildinfo.Revision())
}
