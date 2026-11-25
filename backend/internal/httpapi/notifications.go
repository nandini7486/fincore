package httpapi

import (
	"net/http"
	"time"

	"github.com/nandini7486/fincore/backend/internal/auth"
	"github.com/nandini7486/fincore/backend/internal/notifications"
	"github.com/nandini7486/fincore/backend/internal/platform/httpx"
)

type notificationResponse struct {
	NotificationID string    `json:"notification_id"`
	Message        string    `json:"message"`
	PaymentID      string    `json:"payment_id"`
	CreatedAt      time.Time `json:"created_at"`
}

type notificationsResponse struct {
	Notifications []notificationResponse `json:"notifications"`
	// NextCursor is null on the last page.
	NextCursor *string `json:"next_cursor"`
}

// listNotifications handles GET /api/v1/notifications: the caller's own
// notifications, newest first, with the same keyset pagination as payment
// history (limit 1-100, default 20; opaque cursor).
func listNotifications(p Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := auth.FromContext(r.Context())
		limit, after, ok := pageParams(w, r)
		if !ok {
			return
		}
		page, next, err := notifications.List(r.Context(), p, id.UserID, after, limit)
		if err != nil {
			httpx.WriteInternalError(w, r, err)
			return
		}
		resp := notificationsResponse{Notifications: make([]notificationResponse, 0, len(page))}
		for _, n := range page {
			resp.Notifications = append(resp.Notifications, notificationResponse{
				NotificationID: n.ID, Message: n.Message, PaymentID: n.PaymentID, CreatedAt: n.CreatedAt.UTC(),
			})
		}
		if next != nil {
			s := next.Encode()
			resp.NextCursor = &s
		}
		httpx.WriteJSON(w, http.StatusOK, resp)
	}
}
