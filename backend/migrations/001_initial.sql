CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TYPE user_role AS ENUM ('client','agent','operations','finance','admin');
CREATE TYPE trip_status AS ENUM ('scheduled','boarding','departed','arrived','delayed','cancelled');
CREATE TYPE booking_status AS ENUM ('held','pending_payment','confirmed','cancelled','expired','refunded');
CREATE TYPE payment_status AS ENUM ('pending','succeeded','failed','refunded');

CREATE TABLE users (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), email text NOT NULL UNIQUE,
 full_name text NOT NULL, phone text NOT NULL, password_hash text NOT NULL,
 role user_role NOT NULL DEFAULT 'client', active boolean NOT NULL DEFAULT true,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE routes (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), name text NOT NULL,
 origin text NOT NULL, destination text NOT NULL, stops jsonb NOT NULL DEFAULT '[]',
 distance_km numeric(8,2), active boolean NOT NULL DEFAULT true,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE coaches (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), registration text NOT NULL UNIQUE,
 name text NOT NULL, capacity integer NOT NULL CHECK (capacity > 0),
 seat_layout jsonb NOT NULL, active boolean NOT NULL DEFAULT true
);
CREATE TABLE trips (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), route_id uuid NOT NULL REFERENCES routes(id),
 coach_id uuid REFERENCES coaches(id), driver_name text,
 departure_at timestamptz NOT NULL, arrival_estimate timestamptz NOT NULL,
 fare_minor bigint NOT NULL CHECK (fare_minor >= 0), currency char(3) NOT NULL DEFAULT 'RWF',
 capacity integer NOT NULL CHECK (capacity > 0), status trip_status NOT NULL DEFAULT 'scheduled',
 delay_minutes integer NOT NULL DEFAULT 0, incident_notes text,
 cancellation_cutoff_minutes integer NOT NULL DEFAULT 60,
 refund_percent integer NOT NULL DEFAULT 100 CHECK(refund_percent BETWEEN 0 AND 100),
 created_at timestamptz NOT NULL DEFAULT now(), CHECK(arrival_estimate > departure_at)
);
CREATE INDEX trips_departure_idx ON trips(departure_at) WHERE status <> 'cancelled';
CREATE TABLE bookings (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), user_id uuid NOT NULL REFERENCES users(id),
 trip_id uuid NOT NULL REFERENCES trips(id), ticket_reference text NOT NULL UNIQUE,
 status booking_status NOT NULL DEFAULT 'held', passenger_name text NOT NULL,
 passenger_phone text NOT NULL, total_minor bigint NOT NULL, currency char(3) NOT NULL,
 hold_expires_at timestamptz NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 cancelled_at timestamptz
);
CREATE TABLE booking_seats (
 booking_id uuid NOT NULL REFERENCES bookings(id) ON DELETE CASCADE,
 trip_id uuid NOT NULL REFERENCES trips(id), seat_number text NOT NULL,
 PRIMARY KEY(booking_id, seat_number), created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE payments (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), booking_id uuid NOT NULL REFERENCES bookings(id),
 provider text NOT NULL DEFAULT 'mtn_momo', provider_reference text NOT NULL UNIQUE,
 idempotency_key text NOT NULL UNIQUE, status payment_status NOT NULL DEFAULT 'pending',
 amount_minor bigint NOT NULL, currency char(3) NOT NULL, provider_payload jsonb,
 confirmed_at timestamptz, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payments_booking_idx ON payments(booking_id);
CREATE UNIQUE INDEX payments_one_pending_per_booking ON payments(booking_id) WHERE status='pending';
CREATE TABLE trip_events (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), trip_id uuid NOT NULL REFERENCES trips(id),
 actor_id uuid REFERENCES users(id), event_type text NOT NULL, details jsonb NOT NULL DEFAULT '{}',
 recorded_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE check_ins (
 booking_id uuid PRIMARY KEY REFERENCES bookings(id), staff_id uuid NOT NULL REFERENCES users(id),
 checked_in_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE refund_requests (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 booking_id uuid NOT NULL REFERENCES bookings(id),
 payment_id uuid NOT NULL UNIQUE REFERENCES payments(id),
 amount_minor bigint NOT NULL CHECK (amount_minor >= 0),
 status text NOT NULL DEFAULT 'requested' CHECK(status IN ('requested','approved','declined','paid')),
 finance_note text, external_reference text,
 reviewed_by uuid REFERENCES users(id),
 requested_at timestamptz NOT NULL DEFAULT now(),
 reviewed_at timestamptz
);
CREATE VIEW occupied_seats AS
 SELECT bs.trip_id, bs.seat_number FROM booking_seats bs JOIN bookings b ON b.id=bs.booking_id
 WHERE b.status IN ('held','pending_payment','confirmed') AND b.hold_expires_at > now();
