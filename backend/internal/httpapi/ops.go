package httpapi

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

func (s *Server) opsCoaches(w http.ResponseWriter, r *http.Request) {
	rows, e := s.db.Query(r.Context(), `SELECT id,registration,name,capacity,seat_layout FROM coaches WHERE active ORDER BY registration`)
	if e != nil {
		write(w, 500, map[string]string{"error": "could not load coaches"})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, registration, name string
		var cap int
		var layout []byte
		if rows.Scan(&id, &registration, &name, &cap, &layout) == nil {
			var seats any
			_ = json.Unmarshal(layout, &seats)
			out = append(out, map[string]any{"id": id, "registration": registration, "name": name, "capacity": cap, "seat_layout": seats})
		}
	}
	write(w, 200, out)
}
func (s *Server) manifest(w http.ResponseWriter, r *http.Request) {
	rows, e := s.db.Query(r.Context(), `SELECT b.ticket_reference,b.passenger_name,b.passenger_phone,b.status,COALESCE(string_agg(bs.seat_number,','),''),COALESCE((SELECT p.status FROM payments p WHERE p.booking_id=b.id ORDER BY p.created_at DESC LIMIT 1),'pending'),(ci.booking_id IS NOT NULL) FROM bookings b LEFT JOIN booking_seats bs ON bs.booking_id=b.id LEFT JOIN check_ins ci ON ci.booking_id=b.id WHERE b.trip_id=$1 GROUP BY b.id,ci.booking_id ORDER BY b.passenger_name`, chi.URLParam(r, "id"))
	if e != nil {
		write(w, 404, map[string]string{"error": "trip manifest unavailable"})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var ref, name, phone, status, seats, payment string
		var checked bool
		if rows.Scan(&ref, &name, &phone, &status, &seats, &payment, &checked) == nil {
			out = append(out, map[string]any{"ticket_reference": ref, "passenger_name": name, "passenger_phone": phone, "booking_status": status, "seats": strings.Split(seats, ","), "payment_status": payment, "checked_in": checked})
		}
	}
	write(w, 200, out)
}
func (s *Server) listRefunds(w http.ResponseWriter, r *http.Request) {
	rows, e := s.db.Query(r.Context(), `SELECT rr.id,b.ticket_reference,rr.amount_minor,b.currency,rr.status,rr.finance_note,rr.external_reference,rr.requested_at FROM refund_requests rr JOIN bookings b ON b.id=rr.booking_id ORDER BY rr.requested_at DESC LIMIT 500`)
	if e != nil {
		write(w, 500, map[string]string{"error": "refund queue unavailable"})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, ref, currency, status, note, external string
		var amount int64
		var requested time.Time
		if rows.Scan(&id, &ref, &amount, &currency, &status, &note, &external, &requested) == nil {
			out = append(out, map[string]any{"id": id, "ticket_reference": ref, "amount_minor": amount, "currency": strings.TrimSpace(currency), "status": status, "finance_note": note, "external_reference": external, "requested_at": requested})
		}
	}
	write(w, 200, out)
}
func (s *Server) reviewRefund(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(userKey).(identity)
	var in struct {
		Status            string `json:"status"`
		Note              string `json:"finance_note"`
		ExternalReference string `json:"external_reference"`
	}
	if decode(r, &in) != nil || (in.Status != "approved" && in.Status != "declined" && in.Status != "paid") {
		write(w, 400, map[string]string{"error": "status must be approved, declined or paid"})
		return
	}
	if in.Status == "paid" && strings.TrimSpace(in.ExternalReference) == "" {
		write(w, 400, map[string]string{"error": "external payment reference is required to mark a refund paid"})
		return
	}
	tx, e := s.db.Begin(r.Context())
	if e != nil {
		write(w, 500, map[string]string{"error": "refund review unavailable"})
		return
	}
	defer tx.Rollback(r.Context())
	tag, e := tx.Exec(r.Context(), `UPDATE refund_requests SET status=$2,finance_note=$3,external_reference=NULLIF($4,''),reviewed_by=$5,reviewed_at=now() WHERE id=$1 AND status IN ('requested','approved')`, chi.URLParam(r, "id"), in.Status, in.Note, in.ExternalReference, u.ID)
	if e != nil || tag.RowsAffected() == 0 {
		write(w, 409, map[string]string{"error": "refund request unavailable or already reviewed"})
		return
	}
	if in.Status == "paid" {
		_, e = tx.Exec(r.Context(), `UPDATE payments SET status='refunded' WHERE id=(SELECT payment_id FROM refund_requests WHERE id=$1) AND status='succeeded'`, chi.URLParam(r, "id"))
		if e != nil {
			write(w, 500, map[string]string{"error": "could not update payment refund status"})
			return
		}
	}
	if e = tx.Commit(r.Context()); e != nil {
		write(w, 500, map[string]string{"error": "could not save refund review"})
		return
	}
	write(w, 200, map[string]string{"status": in.Status})
}

func (s *Server) routes(w http.ResponseWriter, r *http.Request) {
	rows, e := s.db.Query(r.Context(), `SELECT id,name,origin,destination,stops,distance_km FROM routes WHERE active ORDER BY origin,destination`)
	if e != nil {
		write(w, 500, map[string]string{"error": "could not load routes"})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name, origin, dest string
		var stops []byte
		var distance *float64
		if rows.Scan(&id, &name, &origin, &dest, &stops, &distance) == nil {
			var parsed any
			_ = json.Unmarshal(stops, &parsed)
			out = append(out, map[string]any{"id": id, "name": name, "origin": origin, "destination": dest, "stops": parsed, "distance_km": distance})
		}
	}
	write(w, 200, out)
}

type routeInput struct {
	Name        string          `json:"name"`
	Origin      string          `json:"origin"`
	Destination string          `json:"destination"`
	Stops       json.RawMessage `json:"stops"`
	Distance    *float64        `json:"distance_km"`
	Active      *bool           `json:"active"`
}

func validRoute(v routeInput) bool {
	return strings.TrimSpace(v.Name) != "" && strings.TrimSpace(v.Origin) != "" && strings.TrimSpace(v.Destination) != "" && v.Origin != v.Destination && (len(v.Stops) == 0 || json.Valid(v.Stops))
}
func (s *Server) createRoute(w http.ResponseWriter, r *http.Request) {
	var in routeInput
	if decode(r, &in) != nil || !validRoute(in) {
		write(w, 400, map[string]string{"error": "route name, distinct origin/destination and valid stops are required"})
		return
	}
	stops := in.Stops
	if len(stops) == 0 {
		stops = []byte("[]")
	}
	var id string
	e := s.db.QueryRow(r.Context(), `INSERT INTO routes(name,origin,destination,stops,distance_km) VALUES($1,$2,$3,$4,$5) RETURNING id`, in.Name, in.Origin, in.Destination, stops, in.Distance).Scan(&id)
	if e != nil {
		write(w, 409, map[string]string{"error": "could not create route"})
		return
	}
	s.recordStaffActivity(r, "Route created", id, in.Name)
	write(w, 201, map[string]string{"id": id})
}
func (s *Server) updateRoute(w http.ResponseWriter, r *http.Request) {
	var in routeInput
	if decode(r, &in) != nil || !validRoute(in) {
		write(w, 400, map[string]string{"error": "invalid route details"})
		return
	}
	stops := in.Stops
	if len(stops) == 0 {
		stops = []byte("[]")
	}
	active := true
	if in.Active != nil {
		active = *in.Active
	}
	tag, e := s.db.Exec(r.Context(), `UPDATE routes SET name=$2,origin=$3,destination=$4,stops=$5,distance_km=$6,active=$7 WHERE id=$1`, chi.URLParam(r, "id"), in.Name, in.Origin, in.Destination, stops, in.Distance, active)
	if e != nil || tag.RowsAffected() == 0 {
		write(w, 404, map[string]string{"error": "route not found or update failed"})
		return
	}
	s.recordStaffActivity(r, "Route updated", chi.URLParam(r, "id"), in.Name)
	write(w, 200, map[string]string{"status": "updated"})
}

type coachInput struct {
	Registration string          `json:"registration"`
	Name         string          `json:"name"`
	Capacity     int             `json:"capacity"`
	Layout       json.RawMessage `json:"seat_layout"`
	Active       *bool           `json:"active"`
}

func validCoach(v coachInput) bool {
	return strings.TrimSpace(v.Registration) != "" && strings.TrimSpace(v.Name) != "" && v.Capacity > 0 && v.Capacity <= 100 && len(v.Layout) > 0 && json.Valid(v.Layout)
}
func (s *Server) createCoach(w http.ResponseWriter, r *http.Request) {
	var in coachInput
	if decode(r, &in) != nil || !validCoach(in) {
		write(w, 400, map[string]string{"error": "valid registration, name, capacity and seat layout are required"})
		return
	}
	var id string
	e := s.db.QueryRow(r.Context(), `INSERT INTO coaches(registration,name,capacity,seat_layout) VALUES($1,$2,$3,$4) RETURNING id`, in.Registration, in.Name, in.Capacity, in.Layout).Scan(&id)
	if e != nil {
		write(w, 409, map[string]string{"error": "coach registration already exists or details are invalid"})
		return
	}
	s.recordStaffActivity(r, "Coach added", id, in.Registration)
	write(w, 201, map[string]string{"id": id})
}
func (s *Server) updateCoach(w http.ResponseWriter, r *http.Request) {
	var in coachInput
	if decode(r, &in) != nil || !validCoach(in) {
		write(w, 400, map[string]string{"error": "invalid coach details"})
		return
	}
	active := true
	if in.Active != nil {
		active = *in.Active
	}
	tag, e := s.db.Exec(r.Context(), `UPDATE coaches SET registration=$2,name=$3,capacity=$4,seat_layout=$5,active=$6 WHERE id=$1`, chi.URLParam(r, "id"), in.Registration, in.Name, in.Capacity, in.Layout, active)
	if e != nil || tag.RowsAffected() == 0 {
		write(w, 404, map[string]string{"error": "coach not found or update failed"})
		return
	}
	s.recordStaffActivity(r, "Coach updated", chi.URLParam(r, "id"), in.Registration)
	write(w, 200, map[string]string{"status": "updated"})
}

type tripInput struct {
	RouteID            string `json:"route_id"`
	CoachID            string `json:"coach_id"`
	Driver             string `json:"driver_name"`
	Departure          string `json:"departure_at"`
	Arrival            string `json:"arrival_estimate"`
	Fare               int64  `json:"fare_minor"`
	Currency           string `json:"currency"`
	Capacity           int    `json:"capacity"`
	CancellationCutoff int    `json:"cancellation_cutoff_minutes"`
	RefundPercent      int    `json:"refund_percent"`
}

func (s *Server) createTrip(w http.ResponseWriter, r *http.Request) {
	var in tripInput
	if decode(r, &in) != nil || in.RouteID == "" || in.Departure == "" || in.Arrival == "" || in.Fare < 0 {
		write(w, 400, map[string]string{"error": "route, departure, arrival and nonnegative fare are required"})
		return
	}
	dep, e1 := time.Parse(time.RFC3339, in.Departure)
	arr, e2 := time.Parse(time.RFC3339, in.Arrival)
	if e1 != nil || e2 != nil || !arr.After(dep) || dep.Before(time.Now()) {
		write(w, 400, map[string]string{"error": "trip times must be valid future RFC3339 timestamps"})
		return
	}
	currency := in.Currency
	if currency == "" {
		currency = os.Getenv("DEFAULT_CURRENCY")
		if currency == "" {
			currency = "RWF"
		}
	}
	if len(currency) != 3 {
		write(w, 400, map[string]string{"error": "currency must be a three-letter code"})
		return
	}
	cutoff := in.CancellationCutoff
	if cutoff == 0 {
		cutoff = 60
	}
	refund := in.RefundPercent
	if refund == 0 {
		refund = 100
	}
	capacity := in.Capacity
	if in.CoachID != "" {
		var coachCap int
		if e := s.db.QueryRow(r.Context(), "SELECT capacity FROM coaches WHERE id=$1 AND active", in.CoachID).Scan(&coachCap); e != nil {
			write(w, 400, map[string]string{"error": "active coach not found"})
			return
		}
		if capacity == 0 {
			capacity = coachCap
		}
		if capacity > coachCap {
			write(w, 400, map[string]string{"error": "trip capacity exceeds assigned coach capacity"})
			return
		}
	}
	if capacity < 1 || capacity > 100 || cutoff < 0 || refund < 0 || refund > 100 {
		write(w, 400, map[string]string{"error": "trip capacity or cancellation policy is invalid"})
		return
	}
	var id string
	e := s.db.QueryRow(r.Context(), `INSERT INTO trips(route_id,coach_id,driver_name,departure_at,arrival_estimate,fare_minor,currency,capacity,cancellation_cutoff_minutes,refund_percent) VALUES($1,NULLIF($2,'')::uuid,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`, in.RouteID, in.CoachID, in.Driver, dep, arr, in.Fare, currency, capacity, cutoff, refund).Scan(&id)
	if e != nil {
		write(w, 400, map[string]string{"error": "could not schedule trip; verify route and coach"})
		return
	}
	s.recordStaffActivity(r, "Trip scheduled", id, in.RouteID)
	write(w, 201, map[string]string{"id": id})
}
func (s *Server) updateTrip(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Status    string  `json:"status"`
		CoachID   *string `json:"coach_id"`
		Driver    *string `json:"driver_name"`
		Delay     *int    `json:"delay_minutes"`
		Incident  *string `json:"incident_notes"`
		Fare      *int64  `json:"fare_minor"`
		Capacity  *int    `json:"capacity"`
		Departure *string `json:"departure_at"`
		Arrival   *string `json:"arrival_estimate"`
	}
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "invalid trip update"})
		return
	}
	allowed := map[string]bool{"scheduled": true, "boarding": true, "departed": true, "arrived": true, "delayed": true, "cancelled": true}
	if in.Status != "" && !allowed[in.Status] {
		write(w, 400, map[string]string{"error": "invalid trip status"})
		return
	}
	if (in.Fare != nil && *in.Fare < 0) || (in.Capacity != nil && (*in.Capacity < 1 || *in.Capacity > 100)) || (in.Delay != nil && *in.Delay < 0) {
		write(w, 400, map[string]string{"error": "fare, capacity or delay value is invalid"})
		return
	}
	if in.CoachID != nil && *in.CoachID != "" && in.Capacity != nil {
		var cap int
		if s.db.QueryRow(r.Context(), "SELECT capacity FROM coaches WHERE id=$1 AND active", *in.CoachID).Scan(&cap) != nil || *in.Capacity > cap {
			write(w, 400, map[string]string{"error": "trip capacity exceeds assigned coach capacity"})
			return
		}
	}
	dep, arr := in.Departure, in.Arrival
	if in.Departure != nil && in.Arrival == nil {
		var old time.Time
		if s.db.QueryRow(r.Context(), "SELECT arrival_estimate FROM trips WHERE id=$1", chi.URLParam(r, "id")).Scan(&old) != nil {
			write(w, 404, map[string]string{"error": "trip not found"})
			return
		}
		v := old.Format(time.RFC3339)
		arr = &v
	}
	if in.Arrival != nil && in.Departure == nil {
		var old time.Time
		if s.db.QueryRow(r.Context(), "SELECT departure_at FROM trips WHERE id=$1", chi.URLParam(r, "id")).Scan(&old) != nil {
			write(w, 404, map[string]string{"error": "trip not found"})
			return
		}
		v := old.Format(time.RFC3339)
		dep = &v
	}
	if dep != nil && arr != nil {
		d, e1 := time.Parse(time.RFC3339, *dep)
		a, e2 := time.Parse(time.RFC3339, *arr)
		if e1 != nil || e2 != nil || !a.After(d) {
			write(w, 400, map[string]string{"error": "arrival must be after departure"})
			return
		}
	}
	if in.Capacity != nil {
		var occupied int
		if s.db.QueryRow(r.Context(), "SELECT count(*) FROM occupied_seats WHERE trip_id=$1", chi.URLParam(r, "id")).Scan(&occupied) != nil || *in.Capacity < occupied {
			write(w, 409, map[string]string{"error": "capacity cannot be lower than current occupied seats"})
			return
		}
	}
	tag, e := s.db.Exec(r.Context(), `UPDATE trips SET status=COALESCE(NULLIF($2,'')::trip_status,status),coach_id=COALESCE(NULLIF($3,'')::uuid,coach_id),driver_name=COALESCE($4,driver_name),delay_minutes=COALESCE($5,delay_minutes),incident_notes=COALESCE($6,incident_notes),fare_minor=COALESCE($7,fare_minor),capacity=COALESCE($8,capacity),departure_at=COALESCE(NULLIF($9,'')::timestamptz,departure_at),arrival_estimate=COALESCE(NULLIF($10,'')::timestamptz,arrival_estimate) WHERE id=$1`, chi.URLParam(r, "id"), in.Status, in.CoachID, in.Driver, in.Delay, in.Incident, in.Fare, in.Capacity, dep, arr)
	if e != nil || tag.RowsAffected() == 0 {
		write(w, 404, map[string]string{"error": "trip not found or update failed"})
		return
	}
	if in.Status == "departed" || in.Status == "arrived" {
		u := r.Context().Value(userKey).(identity)
		details, _ := json.Marshal(map[string]any{"recorded_from": "staff_operations"})
		_, _ = s.db.Exec(r.Context(), `INSERT INTO trip_events(trip_id,actor_id,event_type,details) VALUES($1,$2,$3,$4)`, chi.URLParam(r, "id"), u.ID, in.Status, details)
	}
	s.recordStaffActivity(r, "Trip updated", chi.URLParam(r, "id"), in.Status)
	write(w, 200, map[string]string{"status": "updated"})
}
func (s *Server) setRole(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Role   string `json:"role"`
		Active *bool  `json:"active"`
	}
	valid := map[string]bool{"client": true, "agent": true, "operations": true, "finance": true, "admin": true}
	if decode(r, &in) != nil || !valid[in.Role] {
		write(w, 400, map[string]string{"error": "valid role required"})
		return
	}
	active := true
	if in.Active != nil {
		active = *in.Active
	}
	tag, e := s.db.Exec(r.Context(), `UPDATE users SET role=$2::user_role,active=$3 WHERE id=$1`, chi.URLParam(r, "id"), in.Role, active)
	if e != nil || tag.RowsAffected() == 0 {
		write(w, 404, map[string]string{"error": "user not found"})
		return
	}
	write(w, 200, map[string]string{"status": "updated"})
}
func (s *Server) operationsCSV(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("type")
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	dateClause := ` >= COALESCE(NULLIF($1,'')::date,'1900-01-01') AND `
	dateEnd := ` < COALESCE(NULLIF($2,'')::date + interval '1 day','9999-12-31')`
	query := ""
	switch kind {
	case "bookings":
		query = `SELECT b.ticket_reference,t.id,rt.name,t.departure_at,b.status,b.total_minor,b.currency FROM bookings b JOIN trips t ON t.id=b.trip_id JOIN routes rt ON rt.id=t.route_id WHERE t.departure_at` + dateClause + "t.departure_at" + dateEnd + " ORDER BY t.departure_at"
	case "occupancy":
		query = `SELECT t.id,rt.name,t.departure_at,t.capacity,count(os.seat_number) FROM trips t JOIN routes rt ON rt.id=t.route_id LEFT JOIN occupied_seats os ON os.trip_id=t.id WHERE t.departure_at` + dateClause + "t.departure_at" + dateEnd + " GROUP BY t.id,rt.name ORDER BY t.departure_at"
	case "payments":
		query = `SELECT b.ticket_reference,p.provider_reference,p.status,p.amount_minor,p.currency,p.created_at FROM payments p JOIN bookings b ON b.id=p.booking_id WHERE p.created_at` + dateClause + "p.created_at" + dateEnd + " ORDER BY p.created_at DESC"
	case "cancellations":
		query = `SELECT b.ticket_reference,rt.name,t.departure_at,b.cancelled_at,b.status FROM bookings b JOIN trips t ON t.id=b.trip_id JOIN routes rt ON rt.id=t.route_id WHERE b.status IN ('cancelled','refunded') AND t.departure_at` + dateClause + "t.departure_at" + dateEnd + " ORDER BY b.cancelled_at DESC"
	case "incidents":
		query = `SELECT e.trip_id,t.departure_at,e.event_type,e.details,e.recorded_at FROM trip_events e JOIN trips t ON t.id=e.trip_id WHERE e.event_type IN ('incident','delay') AND t.departure_at` + dateClause + "t.departure_at" + dateEnd + " ORDER BY e.recorded_at DESC"
	case "performance":
		query = `SELECT t.id,rt.name,t.departure_at,t.arrival_estimate,MAX(e.recorded_at) FILTER (WHERE e.event_type='departed'),MAX(e.recorded_at) FILTER (WHERE e.event_type='arrived'),t.delay_minutes,t.status FROM trips t JOIN routes rt ON rt.id=t.route_id LEFT JOIN trip_events e ON e.trip_id=t.id WHERE t.departure_at` + dateClause + "t.departure_at" + dateEnd + " GROUP BY t.id,rt.name ORDER BY t.departure_at"
	default:
		write(w, 400, map[string]string{"error": "type must be bookings, occupancy, payments, cancellations, incidents or performance"})
		return
	}
	rows, e := s.db.Query(r.Context(), query, from, to)
	if e != nil {
		write(w, 500, map[string]string{"error": "report query failed"})
		return
	}
	defer rows.Close()
	cols := map[string][]string{"bookings": {"ticket_reference", "trip_id", "route", "departure_at", "booking_status", "total_minor", "currency"}, "occupancy": {"trip_id", "route", "departure_at", "capacity", "occupied"}, "payments": {"ticket_reference", "payment_reference", "payment_status", "amount_minor", "currency", "created_at"}, "cancellations": {"ticket_reference", "route", "departure_at", "cancelled_at", "booking_status"}, "incidents": {"trip_id", "departure_at", "event_type", "details", "recorded_at"}, "performance": {"trip_id", "route", "scheduled_departure", "estimated_arrival", "actual_departure", "actual_arrival", "delay_minutes", "trip_status"}}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s-report.csv", kind))
	c := csv.NewWriter(w)
	_ = c.Write(cols[kind])
	n := len(cols[kind])
	for rows.Next() {
		raw := make([]any, n)
		ptr := make([]any, n)
		for i := range raw {
			ptr[i] = &raw[i]
		}
		if rows.Scan(ptr...) != nil {
			continue
		}
		values := make([]string, n)
		for i, v := range raw {
			if b, ok := v.([]byte); ok {
				values[i] = string(b)
			} else if v != nil {
				values[i] = fmt.Sprint(v)
			}
		}
		_ = c.Write(values)
	}
	c.Flush()
}
