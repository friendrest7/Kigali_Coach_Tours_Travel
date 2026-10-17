package main

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load("../.env", ".env")
	email := strings.TrimSpace(os.Getenv("BOOTSTRAP_ADMIN_EMAIL"))
	if email == "" || os.Getenv("DATABASE_URL") == "" {
		log.Fatal("BOOTSTRAP_ADMIN_EMAIL and DATABASE_URL are required")
	}
	db, e := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	if e != nil {
		log.Fatal(e)
	}
	defer db.Close()
	tag, e := db.Exec(context.Background(), "UPDATE users SET role='admin',active=true WHERE email=lower($1)", email)
	if e != nil {
		log.Fatal(e)
	}
	if tag.RowsAffected() != 1 {
		log.Fatal("no account found; register that email first")
	}
	log.Printf("promoted %s to admin", email)
}
