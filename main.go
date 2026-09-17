package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	defer safePanic()
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		dir = "./data"
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		b, e := os.ReadFile(filepath.Join(dir, "health"))
		if e != nil || now()-num(string(b)) > 120000 {
			os.Exit(1)
		}
		return nil
	}
	must(os.MkdirAll(dir, 0700))
	if len(os.Args) > 1 && os.Args[1] == "selftest" {
		s := newStore(dir)
		defer s.db.Close()
		transaction(s.db, func(q queryer) { put(q, "selftest", true) })
		if !policy(s.db).Paused {
			panic("unexpected defaults")
		}
		fmt.Println("Go binary and SQLite initialized successfully")
		return nil
	}
	owner, e := strconv.ParseInt(os.Getenv("OWNER_ID"), 10, 64)
	if e != nil || owner <= 0 || os.Getenv("BOT_TOKEN") == "" {
		log.Print("Set BOT_TOKEN and numeric OWNER_ID")
		os.Exit(1)
	}
	lock := openDB(filepath.Join(dir, "instance.lock.sqlite"), false)
	defer lock.Close()
	exec(lock, "CREATE TABLE IF NOT EXISTS lock(id INTEGER)")
	conn, e := lock.Conn(context.Background())
	must(e)
	defer conn.Close()
	_, e = conn.ExecContext(context.Background(), "BEGIN EXCLUSIVE")
	if e != nil {
		log.Print("Data volume in use; stop the other bot instance")
		os.Exit(1)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	// Refuse a different owner before reading or migrating the legacy databases.
	var identity string
	err := conn.QueryRowContext(context.Background(), "SELECT value FROM identity WHERE id=1").Scan(&identity)
	if err != nil && err != sql.ErrNoRows {
		identity = ""
	}
	store := newStore(dir)
	defer store.db.Close()
	if identity != "" {
		put(store.db, "identity", identity)
	}
	store.migrate(dir, str(owner))
	store.recover()
	bot := &Bot{Store: store, Owner: owner, Dir: dir, API: newAPI(os.Getenv("BOT_TOKEN"), os.Getenv("AI_BASE_URL"), os.Getenv("AI_MODEL"), os.Getenv("AI_API_KEY"))}
	if value := os.Getenv("CLEANUP_AT_MIDNIGHT"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("CLEANUP_AT_MIDNIGHT must be true or false")
		}
		bot.CleanupMidnight = enabled
	}
	bot.privacyStartup()
	eraseLegacy(dir, str(owner))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if e = bot.setup(ctx); e != nil {
		return fmt.Errorf("setup failed: %w", e)
	}
	bot.startWorkers(ctx)
	log.Print("Go secretary started: SQLite, private topics, long polling")
	e = bot.poll(ctx)
	stop()
	bot.wg.Wait()
	return e
}
