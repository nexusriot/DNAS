//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// The `dnas db` subcommands are the operator's offline path to a store: verify
// it, round-trip it through JSON, and read back what the restored copy holds.
func TestDBVerifyExportImport(t *testing.T) {
	n := startNode(t, nodeOpts{name: "dbtool"})
	fundNode(t, n)
	n.generate(2)
	height := n.height()
	n.stop() // the store must be closed before another process reads it

	verify := n.cli("db", "verify", "-db", "chain.db", "-network", "regtest")
	mustContain(t, verify, "ok:", "db verify")

	export := n.cli("db", "export", "-db", "chain.db", "-network", "regtest", "-o", "export.json")
	mustContain(t, export, "exported", "db export")

	imported := n.cli("db", "import", "-db", "restored.db", "-network", "regtest", "-in", "export.json")
	mustContain(t, imported, "imported", "db import")

	info := n.cli("db", "info", "-db", "restored.db", "-network", "regtest")
	mustContain(t, info, "genesis  ok", "db info on the restored store")
	if !strings.Contains(info, "height   "+itoa(height)) {
		t.Fatalf("restored store is not at height %d:\n%s", height, info)
	}
}
