package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/remotetrigger"

	"github.com/consi/grosz/internal/abrp"
	"github.com/consi/grosz/internal/events"
	"github.com/consi/grosz/internal/meter"
	"github.com/consi/grosz/internal/ocpp"
	"github.com/consi/grosz/internal/scheduler"
	"github.com/consi/grosz/internal/store"
	"github.com/consi/grosz/internal/tariff"
	"github.com/consi/grosz/internal/vehicle"
	"github.com/consi/grosz/internal/web"
	"github.com/consi/grosz/internal/zappi"
)

var (
	version = "dev"
	commit  = "unknown"
)

// Default settings seeded on first run.
var defaultSettings = map[string]string{
	"auth.username":                "admin",
	"auth.password":                "admin",
	"auth.session_lifetime_days":   "30",
	"ocpp.port":                    "8887",
	"ocpp.path":                    "/{ws}",
	"ocpp.auth_key":                "",
	"zappi.charge_box_id":          "",
	"zappi.commercial_mode":        "true",
	"zappi.id_tag":                 "grosz",
	"zappi.meter_interval":         "10",
	"zappi.charger_name":           "",
	"zappi.charge_id":              "",
	"zappi.qr_url":                 "",
	"charger.max_power":            "11000",
	"charger.min_power":            "1380",
	"charger.phases":               "3",
	"charger.status_check_minutes": "25",
	"tariff.pstryk_token":          "",
	"abrp.token":                   "",
	"scheduler.enabled":            "true",
	"scheduler.target_energy":      "30",
	"scheduler.deadline_time":      "07:00",
	"scheduler.battery_capacity":   "0",
	"scheduler.target_soc":         "0",
	"scheduler.skip_above_soc":     "0",
	"scheduler.min_soc":            "0",
	"scheduler.current_soc":        "0",
	"scheduler.max_price":          "0",
	"scheduler.charge_headroom":    "3",
	"vehicle.renault_user":         "",
	"vehicle.renault_password":     "",
	"vehicle.vin":                  "",
	"vehicle.poll_interval":        "15",
	"vehicle.require_plug_check":   "false",
	"charger.mode":                 "schedule",
	"meter.url":                    "",
	"meter.interval":               "5",
	"web.port":                     "3000",
	"log.level":                    "info",
	"log.format":                   "text",
}

func main() {
	var (
		dbPath      string
		showVersion bool
	)
	flag.StringVar(&dbPath, "db", envOr("GROSZ_DB", "./grosz.db"), "path to SQLite database")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("grosz %s (%s)\n", version, commit)
		os.Exit(0)
	}

	// Setup logger
	logLevel := &slog.LevelVar{}
	logLevel.Set(slog.LevelInfo)
	var handler slog.Handler
	if os.Getenv("LOG_FORMAT") == "json" {
		handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	} else {
		handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	}
	log := slog.New(handler)
	slog.SetDefault(log)

	bootID := generateBootID()
	log.Info("starting grosz", "version", version, "commit", commit, "boot_id", bootID, "db", dbPath)
	startedAt := time.Now()

	// Open store
	st, err := store.New(dbPath, log)
	if err != nil {
		log.Error("failed to open database", "err", err)
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	events.Info(st, events.SourceApp, events.ActionAppStarted,
		map[string]any{"version": version, "commit": commit, "bootID": bootID},
		nil,
	)

	// Seed defaults on first run
	if err := st.SeedDefaults(defaultSettings); err != nil {
		log.Error("failed to seed defaults", "err", err)
		os.Exit(1)
	}

	// Configure log level from settings
	switch st.GetDefault("log.level", "info") {
	case "debug":
		logLevel.Set(slog.LevelDebug)
	case "warn":
		logLevel.Set(slog.LevelWarn)
	case "error":
		logLevel.Set(slog.LevelError)
	default:
		logLevel.Set(slog.LevelInfo)
	}

	// Create tariff provider
	tp := tariff.NewPstryk(st, log)
	log.Info("tariff provider started", "provider", tp.Name())

	// Create meter poller
	meterPoller := meter.New(st, log)

	// Create Renault SoC poller; wire ABRP telemetry forwarding (no-op until a
	// token is configured in settings).
	renaultPoller := vehicle.NewRenault(st, log)
	renaultPoller.SetABRP(abrp.New(st, log))

	// Create OCPP server
	srv := ocpp.NewServer(st, log)

	// Register Zappi boot hook
	zappi.RegisterBootHook(srv, st, log)

	ocppPort := st.GetInt("ocpp.port", 8887)
	ocppPath := st.GetDefault("ocpp.path", "/{ws}")

	// Start OCPP server in background
	go srv.Start(ocppPort, ocppPath)

	// Periodic stale-state checker: if a connected charger hasn't sent a
	// StatusNotification in `charger.status_check_minutes` minutes, we
	// re-request one via TriggerMessage. Catches missed boot triggers.
	statusChecker := ocpp.NewStatusChecker(srv, func() time.Duration {
		return time.Duration(st.GetInt("charger.status_check_minutes", 25)) * time.Minute
	}, log)
	statusChecker.Start()

	// Create scheduler
	var sched *scheduler.Scheduler
	if st.GetBool("scheduler.enabled", true) {
		sched = scheduler.New(&scheduler.ServerCharger{S: srv}, tp, st, log)
		log.Info("scheduler started")
	}

	// Create web server
	webSrv := web.New(srv, st, tp, sched, meterPoller, renaultPoller, bootID, version, commit, log)
	webPort := st.GetInt("web.port", 3000)

	// Wire up live SSE broadcasts
	srv.SetStatusHook(func() {
		webSrv.BroadcastStatus()
	})
	if sched != nil {
		srv.SetSoCUpdater(sched.UpdateSoC)
		srv.SetConnectorStatusHook(func(cpID string, connectorID int, status string) {
			// Every connector-status transition pokes the scheduler so post-stop
			// "Available"/"Finishing" → "Preparing" sequences are picked up
			// immediately. Recompute is cheap during an active transaction
			// (early-return freeze) and otherwise short.
			sched.NotifyImmediate()
			// Renault poll is more expensive; keep it filtered to the
			// transitions where a fresh SoC reading is most useful.
			if status == "Preparing" || status == "SuspendedEV" {
				renaultPoller.Trigger()
			}
		})
		// Defensive belt: a StopTransaction may arrive before any post-stop
		// StatusNotification (or be relayed by the cloud proxy out of order).
		// Notify the scheduler the instant txnID is cleared so a pending
		// Force/Schedule mode change can attempt RemoteStart without waiting
		// for the 30s control tick.
		srv.SetTransactionEndedHook(func(cpID string, connectorID, txnID int) {
			sched.NotifyImmediate()
		})
		sched.SetOnRecompute(webSrv.BroadcastStatus)
	}
	// Cancelled at the start of the shutdown sequence so delayed callbacks
	// (boot-hook timer below) don't fire into stopping components.
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())

	// After boot: clear profile state (charger lost its profile), then
	// delay further commands to let Zappi setup finish its ChangeConfiguration
	// calls before we send StatusNotification trigger and re-apply schedule.
	srv.RegisterBootHook(func(chargePointID string, req *core.BootNotificationRequest) {
		if sched != nil {
			sched.ResetProfileState()
		}
		time.AfterFunc(10*time.Second, func() {
			if shutdownCtx.Err() != nil {
				return
			}
			if err := srv.TriggerMessage(chargePointID, remotetrigger.MessageTrigger(core.StatusNotificationFeatureName)); err != nil {
				log.Warn("failed to trigger StatusNotification", "cpID", chargePointID, "err", err)
			}
			if sched != nil {
				sched.NotifyImmediate()
			}
		})
	})
	renaultPoller.SetOnUpdate(func(soc int) {
		if sched != nil {
			sched.Notify()
		}
		webSrv.BroadcastStatus()
	})
	meterPoller.SetOnUpdate(func(state meter.LiveState) {
		data, _ := json.Marshal(state)
		webSrv.Broadcast("meter", string(data))
	})

	go webSrv.Start(webPort)

	log.Info("grosz is running",
		"ocpp_port", ocppPort,
		"ocpp_path", ocppPath,
		"web_port", webPort,
	)

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh

	log.Info("shutting down", "signal", sig)
	events.Info(st, events.SourceApp, events.ActionAppShutdown,
		map[string]any{"signal": sig.String(), "bootID": bootID},
		map[string]any{"uptimeSec": int(time.Since(startedAt).Seconds())},
	)
	shutdownCancel()
	webSrv.Stop()
	renaultPoller.Stop()
	meterPoller.Stop()
	statusChecker.Stop()
	if sched != nil {
		sched.Stop()
	}
	tp.Stop()
	srv.Stop()
	log.Info("goodbye")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func generateBootID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
