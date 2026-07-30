package daemon

import (
	"context"
	"net/http"

	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

// readDisplayTopologyVia is the typed, read-only helper seam used by the HTTP
// handler. Tests replace it without launching ax_server; production always
// goes through the process-wide AX client and its strict v1 decoder.
var readDisplayTopologyVia = func(ctx context.Context) (tools.DisplayTopologyV1, error) {
	return tools.SharedAXClient().DisplayTopologyV1(ctx)
}

// handleComputerUseTopology serves the current coordinate authority as the
// response body itself. There is deliberately no wrapper and no capture/image
// route in this phase.
func (s *Server) handleComputerUseTopology(w http.ResponseWriter, r *http.Request) {
	topology, err := readDisplayTopologyVia(r.Context())
	if err != nil {
		writeErrorCode(
			w,
			http.StatusBadGateway,
			"computer_use_topology_unavailable",
			"display topology unavailable")
		return
	}
	// Production's typed helper call already validates. Revalidate at the HTTP
	// trust boundary so an alternate seam can never emit an invalid authority.
	if err := topology.Validate(); err != nil {
		writeErrorCode(
			w,
			http.StatusBadGateway,
			"computer_use_topology_unavailable",
			"display topology unavailable")
		return
	}
	writeJSON(w, http.StatusOK, topology)
}
