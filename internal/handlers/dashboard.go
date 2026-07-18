package handlers

import (
	"net/http"

	"github.com/BryanBaluyut/slatedesk/internal/api"
)

// GetDashboardCounters implements GET /dashboard/counters (agent/admin):
// the four workspace counters in one query.
func (h *Handlers) GetDashboardCounters(w http.ResponseWriter, r *http.Request) {
	counts, err := h.q.DashboardCounts(r.Context())
	if err != nil {
		serverError(w, r, "dashboard counters", err)
		return
	}
	writeJSON(w, r, http.StatusOK, api.DashboardCounters{
		Open:              counts.OpenCount,
		Unassigned:        counts.UnassignedCount,
		WaitingOnCustomer: counts.WaitingOnCustomerCount,
		ClosedToday:       counts.ClosedTodayCount,
	})
}
