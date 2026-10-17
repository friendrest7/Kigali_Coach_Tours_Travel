package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/crypto/bcrypt"
)

type Server struct {
	db     *pgxpool.Pool
	secret []byte
}
type key int

const userKey key = 1

type identity struct{ ID, Role string }
type credentials struct{ Email, Password string }

func New(db *pgxpool.Pool) http.Handler {
	s := &Server{db: db, secret: []byte(os.Getenv("JWT_SECRET"))}
	r := chi.NewRouter()
	r.Use(cors)
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) { write(w, 200, map[string]string{"status": "ok"}) })
	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/auth/register", s.register)
		r.Post("/auth/login", s.login)
		r.Get("/trips", s.trips)
		r.Get("/trips/{id}", s.trip)
		r.Get("/routes", s.routes)
		r.Post("/payments/mtn/callback", s.mtnCallback)
		r.Get("/tickets/{ref}/qr.png", s.ticketQR)
		r.Group(func(p chi.Router) {
			p.Use(s.auth)
			p.Post("/bookings", s.book)
			p.Get("/bookings", s.myBookings)
			p.Post("/bookings/{id}/cancel", s.cancel)
			p.Post("/bookings/{id}/payments/mtn", s.startPayment)
			p.Get("/bookings/{ref}/payment", s.refreshPayment)
		})
		r.Group(func(p chi.Router) {
			p.Use(s.auth)
			p.Use(roles("agent", "tickateur", "ticket_manager", "operations", "finance", "admin"))
			p.Get("/ops/trips", s.opsTrips)
			p.Get("/ops/coaches", s.opsCoaches)
			p.Post("/ops/trips/{id}/events", s.tripEvent)
			p.Post("/ops/bookings/{ref}/check-in", s.checkin)
			p.Get("/ops/trips/{id}/manifest", s.manifest)
		})
		r.Group(func(p chi.Router) {
			p.Use(s.auth)
			p.Use(roles("operations", "admin"))
			p.Post("/routes", s.createRoute)
			p.Put("/routes/{id}", s.updateRoute)
			p.Post("/coaches", s.createCoach)
			p.Put("/coaches/{id}", s.updateCoach)
			p.Post("/trips", s.createTrip)
			p.Patch("/ops/trips/{id}", s.updateTrip)
		})
		r.Group(func(p chi.Router) {
			p.Use(s.auth)
			p.Use(roles("finance", "admin"))
			p.Get("/reports/sales.csv", s.salesCSV)
			p.Get("/reports/operations.csv", s.operationsCSV)
			p.Get("/ops/refunds", s.listRefunds)
		})
		r.Group(func(p chi.Router) {
			p.Use(s.auth)
			p.Use(roles("admin", "hr", "ticket_manager"))
			p.Get("/admin/users", s.listStaff)
			p.Post("/admin/users", s.createStaff)
			p.Patch("/admin/users/{id}/role", s.setStaff)
		})
		r.Group(func(p chi.Router) { p.Use(s.auth); p.Use(roles("admin")); p.Get("/admin/activity", s.staffActivity) })
		r.Group(func(p chi.Router) {
			p.Use(s.auth)
			p.Use(roles("finance", "admin"))
			p.Patch("/ops/refunds/{id}", s.reviewRefund)
		})
	})
	return r
}
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := os.Getenv("FRONTEND_ORIGIN")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func decode(r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	d.DisallowUnknownFields()
	return d.Decode(v)
}
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		t, e := jwt.Parse(h, func(t *jwt.Token) (any, error) {
			if t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
				return nil, fmt.Errorf("unexpected signing method")
			}
			return s.secret, nil
		})
		if e != nil || !t.Valid {
			write(w, 401, map[string]string{"error": "authentication required"})
			return
		}
		claims, ok := t.Claims.(jwt.MapClaims)
		if !ok {
			write(w, 401, map[string]string{"error": "invalid token"})
			return
		}
		id := fmt.Sprint(claims["sub"])
		var role string
		e = s.db.QueryRow(r.Context(), "SELECT role FROM users WHERE id=$1 AND active", id).Scan(&role)
		if e != nil {
			write(w, 401, map[string]string{"error": "account is inactive"})
			return
		}
		ctx := context.WithValue(r.Context(), userKey, identity{ID: id, Role: role})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
func roles(allowed ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, _ := r.Context().Value(userKey).(identity)
			for _, v := range allowed {
				if u.Role == v {
					next.ServeHTTP(w, r)
					return
				}
			}
			write(w, 403, map[string]string{"error": "permission denied"})
		})
	}
}
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Name     string `json:"full_name"`
		Phone    string `json:"phone"`
		Password string `json:"password"`
	}
	if decode(r, &in) != nil || !strings.Contains(in.Email, "@") || len(in.Password) < 10 || in.Name == "" || in.Phone == "" {
		write(w, 400, map[string]string{"error": "valid name, email, phone and 10 character password required"})
		return
	}
	hash, e := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if e != nil {
		write(w, 500, map[string]string{"error": "could not create account"})
		return
	}
	var id string
	e = s.db.QueryRow(r.Context(), `INSERT INTO users(email,full_name,phone,password_hash) VALUES(lower($1),$2,$3,$4) RETURNING id`, in.Email, in.Name, in.Phone, string(hash)).Scan(&id)
	if e != nil {
		write(w, 409, map[string]string{"error": "account could not be created"})
		return
	}
	s.issue(w, id, "client")
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in credentials
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	var id, hash, role string
	var active bool
	e := s.db.QueryRow(r.Context(), "SELECT id,password_hash,role,active FROM users WHERE email=lower($1)", in.Email).Scan(&id, &hash, &role, &active)
	if e != nil || !active || bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil {
		write(w, 401, map[string]string{"error": "email or password is incorrect"})
		return
	}
	s.issue(w, id, role)
}
func (s *Server) issue(w http.ResponseWriter, id, role string) {
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": id, "role": role, "exp": time.Now().Add(8 * time.Hour).Unix()})
	v, e := t.SignedString(s.secret)
	if e != nil {
		write(w, 500, map[string]string{"error": "could not create session"})
		return
	}
	write(w, 200, map[string]string{"token": v, "role": role})
}
func (s *Server) trips(w http.ResponseWriter, r *http.Request) {
	rows, e := s.db.Query(r.Context(), `SELECT t.id,rt.name,rt.origin,rt.destination,t.departure_at,t.arrival_estimate,t.fare_minor,t.currency,t.capacity,t.status,COALESCE(c.name,''),t.coach_id, (SELECT count(*) FROM occupied_seats os WHERE os.trip_id=t.id) FROM trips t JOIN routes rt ON rt.id=t.route_id LEFT JOIN coaches c ON c.id=t.coach_id WHERE rt.active AND t.status NOT IN ('cancelled','arrived') AND t.departure_at > now() ORDER BY t.departure_at LIMIT 200`)
	if e != nil {
		write(w, 500, map[string]string{"error": "could not load trips"})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name, origin, dest, currency, status, coach string
		var dep, arr time.Time
		var fare int64
		var cap, used int
		var coachID *string
		if rows.Scan(&id, &name, &origin, &dest, &dep, &arr, &fare, &currency, &cap, &status, &coach, &coachID, &used) == nil {
			out = append(out, map[string]any{"id": id, "route": name, "origin": origin, "destination": dest, "departure_at": dep, "arrival_estimate": arr, "fare_minor": fare, "currency": strings.TrimSpace(currency), "capacity": cap, "available_seats": seatsAvailable(cap, used), "status": status, "coach": coach})
		}
	}
	write(w, 200, out)
}
func (s *Server) trip(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var capacity int
	var route, origin, destination, status, currency string
	var departure, arrival time.Time
	var fare int64
	var coach, registration, driver *string
	var layout []byte
	e := s.db.QueryRow(r.Context(), `SELECT t.capacity,rt.name,rt.origin,rt.destination,t.departure_at,t.arrival_estimate,t.fare_minor,t.currency,t.status,c.name,c.registration,t.driver_name,c.seat_layout FROM trips t JOIN routes rt ON rt.id=t.route_id LEFT JOIN coaches c ON c.id=t.coach_id WHERE t.id=$1 AND t.departure_at>now() AND t.status<>'cancelled'`, id).Scan(&capacity, &route, &origin, &destination, &departure, &arrival, &fare, &currency, &status, &coach, &registration, &driver, &layout)
	if e != nil {
		write(w, 404, map[string]string{"error": "trip not found"})
		return
	}
	rows, e := s.db.Query(r.Context(), `SELECT seat_number FROM occupied_seats WHERE trip_id=$1`, id)
	if e != nil {
		write(w, 500, map[string]string{"error": "could not load seats"})
		return
	}
	defer rows.Close()
	taken := []string{}
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			taken = append(taken, n)
		}
	}
	write(w, 200, map[string]any{"id": id, "route": route, "origin": origin, "destination": destination, "departure_at": departure, "arrival_estimate": arrival, "fare_minor": fare, "currency": strings.TrimSpace(currency), "status": status, "capacity": capacity, "coach": coach, "coach_registration": registration, "driver_name": driver, "seat_layout": json.RawMessage(layout), "occupied_seats": taken})
}
func (s *Server) book(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(userKey).(identity)
	var in struct {
		TripID string   `json:"trip_id"`
		Seats  []string `json:"seats"`
		Name   string   `json:"passenger_name"`
		Phone  string   `json:"passenger_phone"`
	}
	if decode(r, &in) != nil || in.TripID == "" || in.Name == "" || in.Phone == "" || !seatSelectionValid(in.Seats, 100) {
		write(w, 400, map[string]string{"error": "trip, passenger details and 1-8 valid seat numbers are required"})
		return
	}
	ctx := r.Context()
	tx, e := s.db.Begin(ctx)
	if e != nil {
		write(w, 500, map[string]string{"error": "booking unavailable"})
		return
	}
	defer tx.Rollback(ctx)
	var fare int64
	var currency string
	var capacity int
	var departure time.Time
	e = tx.QueryRow(ctx, `SELECT fare_minor,currency,capacity,departure_at FROM trips WHERE id=$1 AND status='scheduled' FOR UPDATE`, in.TripID).Scan(&fare, &currency, &capacity, &departure)
	if e != nil || departure.Before(time.Now()) {
		write(w, 409, map[string]string{"error": "trip is no longer bookable"})
		return
	}
	if !seatSelectionValid(in.Seats, capacity) {
		write(w, 400, map[string]string{"error": "selected seat is outside this trip capacity"})
		return
	}
	_, _ = tx.Exec(ctx, `UPDATE bookings SET status='expired' WHERE trip_id=$1 AND status IN ('held','pending_payment') AND hold_expires_at<=now()`, in.TripID)
	_, _ = tx.Exec(ctx, `DELETE FROM booking_seats bs USING bookings b WHERE bs.booking_id=b.id AND b.trip_id=$1 AND b.status='expired'`, in.TripID)
	var occupied int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM booking_seats bs JOIN bookings b ON b.id=bs.booking_id WHERE bs.trip_id=$1 AND b.status IN ('held','pending_payment','confirmed') AND (b.status='confirmed' OR b.hold_expires_at>now())`, in.TripID).Scan(&occupied); e != nil || occupied+len(in.Seats) > capacity {
		write(w, 409, map[string]string{"error": "not enough seats available"})
		return
	}
	ref := strings.ToUpper(strings.ReplaceAll(randomRef(), "-", ""))[:12]
	holdMinutes := 10
	if configured, e := strconv.Atoi(os.Getenv("BOOKING_HOLD_MINUTES")); e == nil && configured > 0 && configured <= 60 {
		holdMinutes = configured
	}
	hold := time.Now().Add(time.Duration(holdMinutes) * time.Minute)
	var bid string
	e = tx.QueryRow(ctx, `INSERT INTO bookings(user_id,trip_id,ticket_reference,passenger_name,passenger_phone,total_minor,currency,hold_expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`, u.ID, in.TripID, ref, in.Name, in.Phone, fare*int64(len(in.Seats)), currency, hold).Scan(&bid)
	if e == nil {
		for _, seat := range in.Seats {
			_, e = tx.Exec(ctx, "INSERT INTO booking_seats(booking_id,trip_id,seat_number) VALUES($1,$2,$3)", bid, in.TripID, seat)
			if e != nil {
				break
			}
		}
	}
	if e != nil {
		write(w, 409, map[string]string{"error": "a selected seat was just booked; please choose again"})
		return
	}
	if e = tx.Commit(ctx); e != nil {
		write(w, 500, map[string]string{"error": "booking could not be saved"})
		return
	}
	write(w, 201, map[string]any{"reference": ref, "status": "held", "amount_minor": fare * int64(len(in.Seats)), "currency": strings.TrimSpace(currency), "hold_expires_at": hold, "ticket_qr_payload": "KCTT:" + ref})
}
func randomRef() string {
	var b [12]byte
	if _, e := rand.Read(b[:]); e != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return strings.ToUpper(hex.EncodeToString(b[:]))
}
func (s *Server) ticketQR(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")
	var status string
	e := s.db.QueryRow(r.Context(), "SELECT status FROM bookings WHERE ticket_reference=$1", ref).Scan(&status)
	if e != nil || status != "confirmed" {
		write(w, 404, map[string]string{"error": "confirmed ticket not found"})
		return
	}
	png, e := qrcode.Encode("KCTT:"+ref, qrcode.Medium, 256)
	if e != nil {
		write(w, 500, map[string]string{"error": "ticket QR unavailable"})
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=60")
	_, _ = w.Write(png)
}
func (s *Server) myBookings(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(userKey).(identity)
	rows, e := s.db.Query(r.Context(), `SELECT b.ticket_reference,b.status,b.total_minor,b.currency,b.created_at,rt.origin,rt.destination,t.departure_at,COALESCE(string_agg(bs.seat_number,','),''),COALESCE((SELECT status FROM payments p WHERE p.booking_id=b.id ORDER BY created_at DESC LIMIT 1),'pending'),COALESCE((SELECT rr.status FROM refund_requests rr WHERE rr.booking_id=b.id ORDER BY requested_at DESC LIMIT 1),'') FROM bookings b JOIN trips t ON t.id=b.trip_id JOIN routes rt ON rt.id=t.route_id LEFT JOIN booking_seats bs ON bs.booking_id=b.id WHERE b.user_id=$1 GROUP BY b.id,rt.origin,rt.destination,t.departure_at ORDER BY b.created_at DESC`, u.ID)
	if e != nil {
		write(w, 500, map[string]string{"error": "could not load bookings"})
		return
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		var ref, status, currency, origin, dest, seats, pay, refundStatus string
		var total int64
		var created, dep time.Time
		if rows.Scan(&ref, &status, &total, &currency, &created, &origin, &dest, &dep, &seats, &pay, &refundStatus) == nil {
			result = append(result, map[string]any{"reference": ref, "status": status, "payment_status": pay, "refund_status": refundStatus, "total_minor": total, "currency": strings.TrimSpace(currency), "created_at": created, "origin": origin, "destination": dest, "departure_at": dep, "seats": strings.Split(seats, ",")})
		}
	}
	write(w, 200, result)
}
func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(userKey).(identity)
	ref := chi.URLParam(r, "id")
	tx, e := s.db.Begin(r.Context())
	if e != nil {
		write(w, 500, map[string]string{"error": "cancellation unavailable"})
		return
	}
	defer tx.Rollback(r.Context())
	var status string
	var cut int
	var dep time.Time
	var pct int
	var bid string
	e = tx.QueryRow(r.Context(), `SELECT b.id,b.status,t.cancellation_cutoff_minutes,t.departure_at,t.refund_percent FROM bookings b JOIN trips t ON t.id=b.trip_id WHERE b.ticket_reference=$1 AND b.user_id=$2 FOR UPDATE OF b`, ref, u.ID).Scan(&bid, &status, &cut, &dep, &pct)
	if e != nil {
		write(w, 404, map[string]string{"error": "booking not found"})
		return
	}
	if status != "confirmed" && status != "held" && status != "pending_payment" {
		write(w, 409, map[string]string{"error": "booking cannot be cancelled"})
		return
	}
	if time.Until(dep) < time.Duration(cut)*time.Minute {
		write(w, 409, map[string]string{"error": "cancellation cutoff has passed"})
		return
	}
	_, e = tx.Exec(r.Context(), `UPDATE bookings SET status='cancelled',cancelled_at=now() WHERE id=$1`, bid)
	if e == nil {
		_, e = tx.Exec(r.Context(), `DELETE FROM booking_seats WHERE booking_id=$1`, bid)
	}
	var refundID *string
	if e == nil && status == "confirmed" && pct > 0 {
		var id string
		e = tx.QueryRow(r.Context(), `INSERT INTO refund_requests(booking_id,payment_id,amount_minor) SELECT $1,p.id,(p.amount_minor*$2)/100 FROM payments p WHERE p.booking_id=$1 AND p.status='succeeded' ORDER BY p.created_at DESC LIMIT 1 RETURNING id`, bid, pct).Scan(&id)
		if e == pgx.ErrNoRows {
			e = nil
		} else if e == nil {
			refundID = &id
		}
	}
	if e != nil {
		write(w, 500, map[string]string{"error": "could not cancel booking"})
		return
	}
	if e = tx.Commit(r.Context()); e != nil {
		write(w, 500, map[string]string{"error": "could not save cancellation"})
		return
	}
	write(w, 200, map[string]any{"status": "cancelled", "refund_percent": pct, "refund_request_id": refundID, "refund_status": map[bool]string{true: "requested", false: "not_eligible"}[refundID != nil]})
}
func (s *Server) opsTrips(w http.ResponseWriter, r *http.Request) { s.trips(w, r) }
func (s *Server) tripEvent(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(userKey).(identity)
	var in struct {
		Type    string         `json:"type"`
		Details map[string]any `json:"details"`
	}
	if decode(r, &in) != nil || in.Type == "" {
		write(w, 400, map[string]string{"error": "event type required"})
		return
	}
	body, _ := json.Marshal(in.Details)
	_, e := s.db.Exec(r.Context(), `INSERT INTO trip_events(trip_id,actor_id,event_type,details) VALUES($1,$2,$3,$4)`, chi.URLParam(r, "id"), u.ID, in.Type, body)
	if e != nil {
		write(w, 400, map[string]string{"error": "could not record trip event"})
		return
	}
	if in.Type == "delay" {
		_, _ = s.db.Exec(r.Context(), `UPDATE trips SET status='delayed',delay_minutes=COALESCE(($2::jsonb->>'minutes')::int,delay_minutes) WHERE id=$1`, chi.URLParam(r, "id"), body)
	}
	write(w, 201, map[string]string{"status": "recorded"})
}
func (s *Server) checkin(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(userKey).(identity)
	ref := chi.URLParam(r, "ref")
	tag, e := s.db.Exec(r.Context(), `INSERT INTO check_ins(booking_id,staff_id) SELECT id,$2 FROM bookings WHERE ticket_reference=$1 AND status='confirmed' ON CONFLICT DO NOTHING`, ref, u.ID)
	if e != nil || tag.RowsAffected() == 0 {
		write(w, 409, map[string]string{"error": "ticket unavailable or already checked in"})
		return
	}
	write(w, 200, map[string]string{"status": "checked_in"})
}
func (s *Server) salesCSV(w http.ResponseWriter, r *http.Request) {
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	rows, e := s.db.Query(r.Context(), `SELECT b.ticket_reference,rt.name,t.departure_at,b.total_minor,b.currency,p.status FROM bookings b JOIN trips t ON t.id=b.trip_id JOIN routes rt ON rt.id=t.route_id JOIN payments p ON p.booking_id=b.id WHERE p.status='succeeded' AND t.departure_at >= COALESCE(NULLIF($1,'')::date,'1900-01-01') AND t.departure_at < COALESCE(NULLIF($2,'')::date + interval '1 day','9999-12-31') ORDER BY t.departure_at`, from, to)
	if e != nil {
		write(w, 400, map[string]string{"error": "invalid date range"})
		return
	}
	defer rows.Close()
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename=ticket-sales.csv")
	c := csv.NewWriter(w)
	_ = c.Write([]string{"ticket_reference", "route", "departure_at", "revenue_minor", "currency", "payment_status"})
	totals := map[string]int64{}
	for rows.Next() {
		var vals [6]string
		_ = rows.Scan(&vals[0], &vals[1], &vals[2], &vals[3], &vals[4], &vals[5])
		_ = c.Write(vals[:])
		amount, _ := strconv.ParseInt(vals[3], 10, 64)
		totals[vals[4]] += amount
	}
	currencies := make([]string, 0, len(totals))
	for code := range totals {
		currencies = append(currencies, code)
	}
	sort.Strings(currencies)
	for _, code := range currencies {
		_ = c.Write([]string{"CONFIRMED_TOTAL", "", "", strconv.FormatInt(totals[code], 10), code, "succeeded"})
	}
	c.Flush()
}

var _ = pgx.ErrNoRows
var _ = errors.Is
