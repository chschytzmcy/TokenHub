package server

import (
	"net/http"
	"strings"
)

// serveAdminTeamPoolAllocations exposes the rebalancer decisions for one team:
// current-period shares plus recent history, optionally narrowed to one period
// type via ?period_type=day|month.
func (s *Server) serveAdminTeamPoolAllocations(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r, "teams", http.MethodGet); !ok {
		return
	}
	teamID := strings.TrimSpace(r.PathValue("team_id"))
	if teamID == "" {
		writeError(w, r, NewHTTPError(http.StatusNotFound, "not_found", "Not found"))
		return
	}
	periodType := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("period_type")))
	if periodType != "" && periodType != "day" && periodType != "month" {
		writeError(w, r, NewHTTPError(http.StatusBadRequest, "invalid_period_type", "period_type must be day or month"))
		return
	}
	gormStore, ok := s.store.(*GormStore)
	if !ok {
		writeError(w, r, NewHTTPError(http.StatusNotImplemented, "team_pool_unsupported", "Team quota pools require the GORM store"))
		return
	}
	rows, err := gormStore.listTeamQuotaAllocations(teamID, periodType)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rows})
}

func (s *Server) adminTeamPoolAllocationsMethodNotAllowed(allowedMethods string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.adminMethodNotAllowed("teams", allowedMethods)(w, r)
	}
}
