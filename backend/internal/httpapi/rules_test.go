package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBookingSeatValidation(t *testing.T) {
	for _, tt := range []struct {
		name  string
		seats []string
		cap   int
		want  bool
	}{{"one seat", []string{"1"}, 40, true}, {"several", []string{"1", "2", "40"}, 40, true}, {"empty", nil, 40, false}, {"duplicate", []string{"2", "2"}, 40, false}, {"outside coach", []string{"41"}, 40, false}, {"not numeric", []string{"A1"}, 40, false}, {"too many", []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}, 40, false}} {
		t.Run(tt.name, func(t *testing.T) {
			if got := seatSelectionValid(tt.seats, tt.cap); got != tt.want {
				t.Fatalf("seatSelectionValid()=%v want %v", got, tt.want)
			}
		})
	}
}
func TestSeatAvailabilityNeverGoesNegative(t *testing.T) {
	for _, tt := range []struct{ cap, used, want int }{{40, 0, 40}, {40, 13, 27}, {8, 8, 0}, {8, 10, 0}} {
		if got := seatsAvailable(tt.cap, tt.used); got != tt.want {
			t.Fatalf("seatsAvailable(%d,%d)=%d want %d", tt.cap, tt.used, got, tt.want)
		}
	}
}
func TestStaffPermissions(t *testing.T) {
	endpoint := roles("operations", "admin")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for _, tt := range []struct {
		role string
		want int
	}{{"operations", 204}, {"admin", 204}, {"client", 403}, {"agent", 403}, {"finance", 403}} {
		t.Run(tt.role, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			ctx := context.WithValue(req.Context(), userKey, identity{ID: "test", Role: tt.role})
			res := httptest.NewRecorder()
			endpoint.ServeHTTP(res, req.WithContext(ctx))
			if res.Code != tt.want {
				t.Fatalf("status=%d want %d", res.Code, tt.want)
			}
		})
	}
}
func TestOnlyMTNSuccessCanConfirm(t *testing.T) {
	for _, tt := range []struct {
		status string
		want   bool
	}{{"SUCCESSFUL", true}, {" successful ", true}, {"PENDING", false}, {"FAILED", false}, {"CREATED", false}, {"SUCCESS", false}, {"", false}} {
		if got := confirmedPayment(tt.status); got != tt.want {
			t.Errorf("confirmedPayment(%q)=%v want %v", tt.status, got, tt.want)
		}
	}
}
func TestNormalizeRwandaMSISDN(t *testing.T) {
	for in, want := range map[string]string{"+250 78 123 4567": "250781234567", "0781234567": "250781234567", "781234567": "250781234567"} {
		if got := normalizeRwandaMSISDN(in); got != want {
			t.Errorf("normalizeRwandaMSISDN(%q)=%q want %q", in, got, want)
		}
	}
}
