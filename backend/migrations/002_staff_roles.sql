ALTER TYPE user_role ADD VALUE IF NOT EXISTS 'tickateur';
ALTER TYPE user_role ADD VALUE IF NOT EXISTS 'ticket_manager';
ALTER TYPE user_role ADD VALUE IF NOT EXISTS 'hr';
CREATE TABLE staff_audit_events (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 actor_id uuid REFERENCES users(id),
 action text NOT NULL,
 reference text NOT NULL DEFAULT '',
 status text NOT NULL DEFAULT '',
 occurred_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX staff_audit_events_time_idx ON staff_audit_events(occurred_at DESC);
