# Kigali Coach Tours

Coach booking and operations platform for routes across Rwanda. The Next.js web client and Go API deploy separately, with PostgreSQL for bookings and operations data.

## Project structure

- `frontend/`: Next.js App Router and TypeScript client, deployable to Vercel.
- `backend/`: Go REST API, deployable to Render.
- `backend/migrations/`: versioned PostgreSQL schema; apply with `go run ./cmd/migrate`.
- `frontend/src/data/rura-fares.json`: published RURA ticket fares used by the trip cards.

## Roles and permissions

Public registration creates client accounts. Administrators provision staff accounts and manage access. Roles include client, tickateur (ticketing and passenger check-in), ticket manager (supervises ticketing), HR (manages workers), and admin (company-wide access and daily activity oversight). API authorization enforces permissions. Run all migrations, including `002_staff_roles.sql`, before using the staff workflows.

## Local setup

1. Copy `.env.example` to `.env` and replace placeholders. Never commit `.env`. For the frontend, copy the example to `frontend/.env.local` and set `NEXT_PUBLIC_API_URL`.
2. Start PostgreSQL 16 with `docker compose up -d postgres`.
3. From `backend`, run `go run ./cmd/migrate` and `go run ./cmd/api`.
4. From `frontend`, run `npm install` and `npm run dev`.

Set `DATABASE_URL`, `JWT_SECRET`, and `FRONTEND_ORIGIN` for the API. MTN MoMo remains disabled until valid sandbox credentials and callback configuration are supplied.

## Deployment

`render.yaml` describes the PostgreSQL database and Go API. It runs migrations as a pre-deploy command. Configure the frontend origin and MTN sandbox values in Render. Deploy `frontend/` on Vercel with `NEXT_PUBLIC_API_URL` set to the API URL.

To create the first administrator, register an account, then set `BOOTSTRAP_ADMIN_EMAIL` and run `go run ./cmd/bootstrap-admin` from `backend` using the same `DATABASE_URL`. Remove the bootstrap email after use.
