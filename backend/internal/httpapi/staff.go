package httpapi

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
)

func (s *Server) recordStaffActivity(r *http.Request, action, reference, status string) {
	u, ok := r.Context().Value(userKey).(identity)
	if !ok {
		return
	}
	_, _ = s.db.Exec(r.Context(), `INSERT INTO staff_audit_events(actor_id,action,reference,status) VALUES($1,$2,$3,$4)`, u.ID, action, reference, status)
}

func (s *Server) listStaff(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(userKey).(identity)
	query := `SELECT id,email,full_name,phone,role::text,active,created_at FROM users`
	switch u.Role {
	case "hr":
		query += ` WHERE role NOT IN ('client','admin')`
	case "ticket_manager":
		query += ` WHERE role IN ('tickateur','agent')`
	}
	query += ` ORDER BY role,full_name LIMIT 1000`
	rows, err := s.db.Query(r.Context(), query)
	if err != nil {
		write(w, 500, map[string]string{"error": "could not load staff accounts"})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, email, name, phone, role string
		var active bool
		var created any
		if rows.Scan(&id, &email, &name, &phone, &role, &active, &created) == nil {
			out = append(out, map[string]any{"id": id, "email": email, "full_name": name, "phone": phone, "role": role, "active": active, "created_at": created})
		}
	}
	write(w, 200, out)
}

func (s *Server) createStaff(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(userKey).(identity)
	var in struct {
		Email    string `json:"email"`
		Name     string `json:"full_name"`
		Phone    string `json:"phone"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if decode(r, &in) != nil || !strings.Contains(in.Email, "@") || strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.Phone) == "" || len(in.Password) < 10 {
		write(w, 400, map[string]string{"error": "name, valid email, phone and a password of at least 10 characters are required"})
		return
	}
	if !canManageRole(u.Role, in.Role) {
		write(w, 403, map[string]string{"error": "you cannot create an account with that role"})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		write(w, 500, map[string]string{"error": "could not secure account password"})
		return
	}
	var id string
	err = s.db.QueryRow(r.Context(), `INSERT INTO users(email,full_name,phone,password_hash,role) VALUES(lower($1),$2,$3,$4,$5::user_role) RETURNING id`, strings.TrimSpace(in.Email), strings.TrimSpace(in.Name), strings.TrimSpace(in.Phone), string(hash), in.Role).Scan(&id)
	if err != nil {
		write(w, 409, map[string]string{"error": "email is already registered or account details are invalid"})
		return
	}
	_, _ = s.db.Exec(r.Context(), `INSERT INTO staff_audit_events(actor_id,action,reference,status) VALUES($1,'Staff account created',$2,$3)`, u.ID, id, in.Role)
	write(w, 201, map[string]string{"id": id, "role": in.Role, "status": "created"})
}

func canManageRole(actor, target string) bool {
	switch actor {
	case "admin":
		return target == "admin" || target == "hr" || target == "ticket_manager" || target == "tickateur" || target == "agent" || target == "operations" || target == "finance" || target == "client"
	case "hr":
		return target == "hr" || target == "ticket_manager" || target == "tickateur" || target == "agent" || target == "operations" || target == "finance"
	case "ticket_manager":
		return target == "tickateur" || target == "agent"
	default:
		return false
	}
}

func (s *Server) staffActivity(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	query := `SELECT activity_at,actor_name,actor_role,action,reference,status FROM (
		SELECT b.created_at AS activity_at,COALESCE(u.full_name,'Client') AS actor_name,COALESCE(u.role::text,'client') AS actor_role,'Ticket booking' AS action,b.ticket_reference AS reference,b.status::text AS status
		FROM bookings b LEFT JOIN users u ON u.id=b.user_id
		UNION ALL
		SELECT p.created_at,COALESCE(u.full_name,'Client'),COALESCE(u.role::text,'client'),'Payment '||p.status::text,b.ticket_reference,p.status::text
		FROM payments p JOIN bookings b ON b.id=p.booking_id LEFT JOIN users u ON u.id=b.user_id
		UNION ALL
		SELECT e.recorded_at,COALESCE(u.full_name,'System'),COALESCE(u.role::text,'system'),e.event_type,e.trip_id::text,COALESCE(e.details->>'status','recorded')
		FROM trip_events e LEFT JOIN users u ON u.id=e.actor_id
		UNION ALL
		SELECT ci.checked_in_at,u.full_name,u.role::text,'Ticket check-in',b.ticket_reference,'checked in'
		FROM check_ins ci JOIN users u ON u.id=ci.staff_id JOIN bookings b ON b.id=ci.booking_id
		UNION ALL
		SELECT a.occurred_at,COALESCE(u.full_name,'System'),COALESCE(u.role::text,'system'),a.action,a.reference,a.status
		FROM staff_audit_events a LEFT JOIN users u ON u.id=a.actor_id
	) activity WHERE ($1='' OR (activity_at AT TIME ZONE 'Africa/Kigali')::date=$1::date) ORDER BY activity_at DESC LIMIT 500`
	rows, err := s.db.Query(r.Context(), query, date)
	if err != nil {
		write(w, 500, map[string]string{"error": "daily activity is unavailable"})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var at any
		var name, role, action, ref, status string
		if rows.Scan(&at, &name, &role, &action, &ref, &status) == nil {
			out = append(out, map[string]any{"at": at, "actor": name, "role": role, "action": action, "reference": ref, "status": status})
		}
	}
	write(w, 200, out)
}

func (s *Server) setStaff(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(userKey).(identity)
	var in struct {
		Role   string `json:"role"`
		Active *bool  `json:"active"`
	}
	if decode(r, &in) != nil || !canManageRole(actor.Role, in.Role) {
		write(w, 403, map[string]string{"error": "you cannot assign that staff role"})
		return
	}
	active := true
	if in.Active != nil {
		active = *in.Active
	}
	query := `UPDATE users SET role=$2::user_role,active=$3 WHERE id=$1 AND id<>$4`
	if actor.Role == "hr" {
		query += ` AND role NOT IN ('client','admin')`
	}
	if actor.Role == "ticket_manager" {
		query += ` AND role IN ('tickateur','agent')`
	}
	tag, err := s.db.Exec(r.Context(), query, chi.URLParam(r, "id"), in.Role, active, actor.ID)
	if err != nil || tag.RowsAffected() == 0 {
		write(w, 404, map[string]string{"error": "staff account not found or cannot be changed"})
		return
	}
	_, _ = s.db.Exec(r.Context(), `INSERT INTO staff_audit_events(actor_id,action,reference,status) VALUES($1,'Staff access updated',$2,$3)`, actor.ID, chi.URLParam(r, "id"), in.Role+map[bool]string{true: " / active", false: " / inactive"}[active])
	write(w, 200, map[string]string{"status": "updated"})
}
