package daemon

import (
	"net/http"
	"strconv"

	"github.com/Kocoro-lab/ShanClaw/internal/guicontrol"
)

const (
	computerUsePreviewLeaseHeader      = "X-Kocoro-Computer-Use-Lease-ID"
	computerUsePreviewFrameHeader      = "X-Kocoro-Computer-Use-Frame-ID"
	computerUsePreviewRevisionHeader   = "X-Kocoro-Computer-Use-Revision"
	computerUsePreviewWidthHeader      = "X-Kocoro-Computer-Use-Width"
	computerUsePreviewHeightHeader     = "X-Kocoro-Computer-Use-Height"
	computerUsePreviewCursorXHeader    = "X-Kocoro-Computer-Use-Cursor-X"
	computerUsePreviewCursorYHeader    = "X-Kocoro-Computer-Use-Cursor-Y"
	computerUsePreviewCursorKindHeader = "X-Kocoro-Computer-Use-Cursor-Kind"
)

func (s *Server) handleComputerUsePreview(w http.ResponseWriter, r *http.Request) {
	if !requireComputerUseLocalPresence(w, r) {
		return
	}
	coordinator := s.computerUseCoordinatorOrError(w)
	if coordinator == nil {
		return
	}
	leaseID := r.URL.Query().Get("lease_id")
	if leaseID == "" {
		writeErrorCode(
			w,
			http.StatusBadRequest,
			"computer_use_preview_lease_required",
			"computer-use preview lease is required",
		)
		return
	}
	coordinator.ExpireStale()
	snapshot := coordinator.Snapshot()
	if snapshot.Active == nil ||
		snapshot.Active.LeaseID != leaseID ||
		(snapshot.Active.LeaseState != guicontrol.ComputerUseLeaseActive &&
			snapshot.Active.LeaseState != guicontrol.ComputerUseLeasePaused) {
		writeErrorCode(
			w,
			http.StatusConflict,
			"computer_use_preview_stale_lease",
			"computer-use preview lease is stale",
		)
		return
	}
	frame, ok := s.computerUsePreview.Snapshot(leaseID)
	if !ok {
		writeErrorCode(
			w,
			http.StatusNotFound,
			"computer_use_preview_unavailable",
			"computer-use preview is unavailable",
		)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", frame.MediaType)
	w.Header().Set(computerUsePreviewLeaseHeader, frame.LeaseID)
	w.Header().Set(computerUsePreviewFrameHeader, frame.FrameID)
	w.Header().Set(
		computerUsePreviewRevisionHeader,
		strconv.FormatUint(frame.Revision, 10),
	)
	w.Header().Set(computerUsePreviewWidthHeader, strconv.Itoa(frame.Width))
	w.Header().Set(computerUsePreviewHeightHeader, strconv.Itoa(frame.Height))
	if frame.Cursor != nil {
		w.Header().Set(
			computerUsePreviewCursorXHeader,
			strconv.FormatFloat(frame.Cursor.X, 'f', 6, 64),
		)
		w.Header().Set(
			computerUsePreviewCursorYHeader,
			strconv.FormatFloat(frame.Cursor.Y, 'f', 6, 64),
		)
		w.Header().Set(computerUsePreviewCursorKindHeader, frame.Cursor.Kind)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(frame.Data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(frame.Data)
}
