// Command amp-bb takes incremental, deduplicating backups of CubeCoders AMP
// instances.
package main

import (
	"os"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
