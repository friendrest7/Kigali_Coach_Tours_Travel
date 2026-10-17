package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

type momoClient struct {
	base, sub, user, key, env string
	http                      *http.Client
}
type momoRejected struct{ status int }

func (e momoRejected) Error() string {
	return fmt.Sprintf("MTN rejected request with status %d", e.status)
}

func newMomo() momoClient {
	base := strings.TrimRight(os.Getenv("MTN_MOMO_BASE_URL"), "/")
	if base == "" {
		base = "https://sandbox.momodeveloper.mtn.com"
	}
	env := os.Getenv("MTN_MOMO_TARGET_ENVIRONMENT")
	if env == "" {
		env = "sandbox"
	}
	sub := os.Getenv("MTN_MOMO_SUBSCRIPTION_KEY")
	if sub == "" {
		sub = os.Getenv("MTN_MOMO_COLLECTION_PRIMARY_KEY")
	}
	return momoClient{base: base, sub: sub, user: os.Getenv("MTN_MOMO_API_USER"), key: os.Getenv("MTN_MOMO_API_KEY"), env: env, http: &http.Client{Timeout: 12 * time.Second}}
}
func (m momoClient) ready() bool { return m.sub != "" && m.user != "" && m.key != "" }
func (m momoClient) token(ctx context.Context) (string, error) {
	req, e := http.NewRequestWithContext(ctx, "POST", m.base+"/collection/token/", nil)
	if e != nil {
		return "", e
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(m.user+":"+m.key)))
	req.Header.Set("Ocp-Apim-Subscription-Key", m.sub)
	req.Header.Set("X-Target-Environment", m.env)
	resp, e := m.http.Do(req)
	if e != nil {
		return "", e
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("MTN token endpoint returned %d", resp.StatusCode)
	}
	e = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	if e != nil || out.AccessToken == "" {
		return "", fmt.Errorf("MTN returned no access token")
	}
	return out.AccessToken, nil
}
func referenceID() (string, error) {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", e
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}
func (m momoClient) requestToPay(ctx context.Context, ref, amount, currency, phone, token string) error {
	body := map[string]any{"amount": amount, "currency": currency, "externalId": ref, "payer": map[string]string{"partyIdType": "MSISDN", "partyId": phone}, "payerMessage": "Kigali Coach Tours ticket", "payeeNote": "Ticket " + ref}
	data, _ := json.Marshal(body)
	req, e := http.NewRequestWithContext(ctx, "POST", m.base+"/collection/v1_0/requesttopay", bytes.NewReader(data))
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Reference-Id", ref)
	req.Header.Set("X-Target-Environment", m.env)
	req.Header.Set("Ocp-Apim-Subscription-Key", m.sub)
	req.Header.Set("Content-Type", "application/json")
	if callback := os.Getenv("MTN_MOMO_CALLBACK_URL"); callback != "" {
		req.Header.Set("X-Callback-Url", callback)
	}
	resp, e := m.http.Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		if resp.StatusCode < 500 {
			return momoRejected{status: resp.StatusCode}
		}
		return fmt.Errorf("MTN request-to-pay outcome is uncertain (HTTP %d)", resp.StatusCode)
	}
	return nil
}
func (m momoClient) paymentStatus(ctx context.Context, ref, token string) (string, []byte, error) {
	req, e := http.NewRequestWithContext(ctx, "GET", m.base+"/collection/v1_0/requesttopay/"+ref, nil)
	if e != nil {
		return "", nil, e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Target-Environment", m.env)
	req.Header.Set("Ocp-Apim-Subscription-Key", m.sub)
	resp, e := m.http.Do(req)
	if e != nil {
		return "", nil, e
	}
	defer resp.Body.Close()
	data, e := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if e != nil {
		return "", nil, e
	}
	if resp.StatusCode/100 != 2 {
		return "", data, fmt.Errorf("MTN status endpoint returned %d", resp.StatusCode)
	}
	var body struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(data, &body) != nil {
		return "", data, fmt.Errorf("invalid MTN status response")
	}
	return strings.ToUpper(body.Status), data, nil
}
func recordMTNSuccess(ctx context.Context, tx pgx.Tx, ref string, payload []byte) (bool, bool, error) {
	var paymentID, bookingID string
	var amount int64
	e := tx.QueryRow(ctx, `UPDATE payments SET status='succeeded',confirmed_at=now(),provider_payload=$2 WHERE provider_reference=$1 AND status='pending' RETURNING id,booking_id,amount_minor`, ref, payload).Scan(&paymentID, &bookingID, &amount)
	if errors.Is(e, pgx.ErrNoRows) {
		return false, false, nil
	}
	if e != nil {
		return false, false, e
	}
	tag, e := tx.Exec(ctx, `UPDATE bookings SET status='confirmed' WHERE id=$1 AND status IN ('held','pending_payment') AND hold_expires_at>now()`, bookingID)
	if e != nil {
		return false, false, e
	}
	if tag.RowsAffected() == 0 {
		_, e = tx.Exec(ctx, `INSERT INTO refund_requests(booking_id,payment_id,amount_minor) VALUES($1,$2,$3) ON CONFLICT(payment_id) DO NOTHING`, bookingID, paymentID, amount)
		if e != nil {
			return false, false, e
		}
		return true, false, nil
	}
	return true, true, nil
}
func (s *Server) startPayment(w http.ResponseWriter, r *http.Request) {
	m := newMomo()
	if !m.ready() {
		write(w, 503, map[string]string{"error": "MTN MoMo sandbox credentials are not configured"})
		return
	}
	u := r.Context().Value(userKey).(identity)
	ref := chi.URLParam(r, "id")
	idem := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idem == "" || len(idem) > 128 {
		write(w, 400, map[string]string{"error": "Idempotency-Key header is required and must be at most 128 characters"})
		return
	}
	var in struct {
		Phone string `json:"phone"`
	}
	if decode(r, &in) != nil {
		write(w, 400, map[string]string{"error": "valid payer phone number is required"})
		return
	}
	phone := normalizeRwandaMSISDN(in.Phone)
	if !regexp.MustCompile(`^2507\d{8}$`).MatchString(phone) {
		write(w, 400, map[string]string{"error": "payer phone must be a valid Rwanda MTN MSISDN"})
		return
	}
	var bid, payRef, currency string
	var amount int64
	var status string
	e := s.db.QueryRow(r.Context(), `SELECT b.id,b.total_minor,b.currency FROM bookings b WHERE b.ticket_reference=$1 AND b.user_id=$2 AND b.status IN ('held','pending_payment') AND b.hold_expires_at>now()`, ref, u.ID).Scan(&bid, &amount, &currency)
	if e != nil {
		write(w, 404, map[string]string{"error": "active booking not found"})
		return
	}
	e = s.db.QueryRow(r.Context(), `SELECT p.provider_reference,p.status FROM payments p JOIN bookings b ON b.id=p.booking_id WHERE p.idempotency_key=$1 AND b.user_id=$2 AND b.id=$3`, idem, u.ID, bid).Scan(&payRef, &status)
	if e == nil {
		write(w, 200, map[string]any{"payment_reference": payRef, "status": status, "message": "Use the same idempotency key to check this payment."})
		return
	}
	payRef, e = referenceID()
	if e != nil {
		write(w, 500, map[string]string{"error": "could not create payment reference"})
		return
	}
	_, e = s.db.Exec(r.Context(), `INSERT INTO payments(booking_id,provider_reference,idempotency_key,amount_minor,currency) VALUES($1,$2,$3,$4,$5)`, bid, payRef, idem, amount, currency)
	if e != nil {
		write(w, 409, map[string]string{"error": "a payment attempt with this key already exists"})
		return
	}
	_, _ = s.db.Exec(r.Context(), `UPDATE bookings SET status='pending_payment' WHERE id=$1`, bid)
	token, e := m.token(r.Context())
	if e != nil {
		_, _ = s.db.Exec(r.Context(), `UPDATE payments SET status='failed' WHERE provider_reference=$1`, payRef)
		write(w, 502, map[string]string{"error": "could not authenticate payment request with MTN"})
		return
	}
	e = m.requestToPay(r.Context(), payRef, fmt.Sprintf("%.2f", float64(amount)/100), strings.TrimSpace(currency), phone, token)
	if e != nil {
		var rejected momoRejected
		if errors.As(e, &rejected) {
			_, _ = s.db.Exec(r.Context(), `UPDATE payments SET status='failed' WHERE provider_reference=$1`, payRef)
			write(w, 502, map[string]string{"error": "MTN rejected this payment request; no payment was confirmed"})
			return
		}
		write(w, 502, map[string]string{"error": "MTN request outcome is uncertain. Check the payment status before trying again."})
		return
	}
	write(w, 202, map[string]any{"payment_reference": payRef, "status": "pending", "message": "Approve the MTN MoMo prompt. Confirmation will update this booking after provider verification."})
}
func normalizeRwandaMSISDN(raw string) string {
	v := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "", "+", "").Replace(strings.TrimSpace(raw))
	if strings.HasPrefix(v, "07") {
		v = "250" + v[1:]
	}
	if strings.HasPrefix(v, "7") {
		v = "250" + v
	}
	return v
}
func (s *Server) refreshPayment(w http.ResponseWriter, r *http.Request) {
	m := newMomo()
	if !m.ready() {
		write(w, 503, map[string]string{"error": "MTN MoMo sandbox credentials are not configured"})
		return
	}
	u := r.Context().Value(userKey).(identity)
	ref := chi.URLParam(r, "ref")
	var payRef, payStatus string
	e := s.db.QueryRow(r.Context(), `SELECT p.provider_reference,p.status FROM payments p JOIN bookings b ON b.id=p.booking_id WHERE b.ticket_reference=$1 AND b.user_id=$2 ORDER BY p.created_at DESC LIMIT 1`, ref, u.ID).Scan(&payRef, &payStatus)
	if e != nil {
		write(w, 404, map[string]string{"error": "payment not found"})
		return
	}
	if payStatus != "pending" {
		write(w, 200, map[string]string{"status": payStatus})
		return
	}
	token, e := m.token(r.Context())
	if e != nil {
		write(w, 502, map[string]string{"error": "could not authenticate with MTN"})
		return
	}
	providerStatus, payload, e := m.paymentStatus(r.Context(), payRef, token)
	if e != nil {
		write(w, 502, map[string]string{"error": "could not verify payment with MTN"})
		return
	}
	ctx := r.Context()
	tx, e := s.db.Begin(ctx)
	if e != nil {
		write(w, 500, map[string]string{"error": "payment update unavailable"})
		return
	}
	defer tx.Rollback(ctx)
	ticketConfirmed := false
	switch {
	case confirmedPayment(providerStatus):
		_, ticketConfirmed, e = recordMTNSuccess(ctx, tx, payRef, payload)
	case providerStatus == "FAILED":
		_, e = tx.Exec(ctx, `UPDATE payments SET status='failed',provider_payload=$2 WHERE provider_reference=$1 AND status='pending'`, payRef, payload)
	case providerStatus == "PENDING" || providerStatus == "CREATED":
	default:
		write(w, 502, map[string]string{"error": "MTN returned an unrecognized payment state"})
		return
	}
	if e != nil {
		write(w, 500, map[string]string{"error": "could not save verified payment state"})
		return
	}
	if e = tx.Commit(ctx); e != nil {
		write(w, 500, map[string]string{"error": "could not save verified payment state"})
		return
	}
	responseStatus := strings.ToLower(providerStatus)
	if confirmedPayment(providerStatus) {
		responseStatus = "succeeded"
		if !ticketConfirmed {
			responseStatus = "succeeded_refund_requested"
		}
	}
	write(w, 200, map[string]string{"status": responseStatus})
}
func (s *Server) mtnCallback(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ReferenceID string `json:"referenceId"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in)
	ref := strings.TrimSpace(in.ReferenceID)
	if ref == "" {
		ref = strings.TrimSpace(r.Header.Get("X-Reference-Id"))
	}
	if ref == "" {
		write(w, 400, map[string]string{"error": "payment reference required"})
		return
	}
	m := newMomo()
	if !m.ready() {
		write(w, 503, map[string]string{"error": "MTN integration is not configured"})
		return
	}
	token, e := m.token(r.Context())
	if e != nil {
		write(w, 502, map[string]string{"error": "MTN verification unavailable"})
		return
	}
	status, payload, e := m.paymentStatus(r.Context(), ref, token)
	if e != nil {
		write(w, 502, map[string]string{"error": "could not verify payment with MTN"})
		return
	}
	if !confirmedPayment(status) && status != "FAILED" {
		write(w, 202, map[string]string{"status": "pending"})
		return
	}
	tx, e := s.db.Begin(r.Context())
	if e != nil {
		write(w, 500, map[string]string{"error": "payment update unavailable"})
		return
	}
	defer tx.Rollback(r.Context())
	ticketConfirmed := false
	if confirmedPayment(status) {
		var processed bool
		processed, ticketConfirmed, e = recordMTNSuccess(r.Context(), tx, ref, payload)
		if e == nil && !processed {
			write(w, 200, map[string]string{"status": "already_processed"})
			return
		}
	} else {
		_, e = tx.Exec(r.Context(), `UPDATE payments SET status='failed',provider_payload=$2 WHERE provider_reference=$1 AND status='pending'`, ref, payload)
	}
	if e != nil {
		write(w, 500, map[string]string{"error": "could not record verified payment"})
		return
	}
	if e = tx.Commit(r.Context()); e != nil {
		write(w, 500, map[string]string{"error": "could not record verified payment"})
		return
	}
	responseStatus := strings.ToLower(status)
	if confirmedPayment(status) && !ticketConfirmed {
		responseStatus = "succeeded_refund_requested"
	}
	write(w, 200, map[string]string{"status": responseStatus})
}
