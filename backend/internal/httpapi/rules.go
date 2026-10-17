package httpapi

import (
	"strconv"
	"strings"
)

func seatSelectionValid(seats []string, capacity int) bool {
	if len(seats) == 0 || len(seats) > 8 || capacity < 1 {
		return false
	}
	seen := map[string]bool{}
	for _, s := range seats {
		n, e := strconv.Atoi(s)
		if e != nil || n < 1 || n > capacity || seen[s] {
			return false
		}
		seen[s] = true
	}
	return true
}
func seatsAvailable(capacity, occupied int) int {
	n := capacity - occupied
	if n < 0 {
		return 0
	}
	return n
}
func confirmedPayment(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "SUCCESSFUL")
}
