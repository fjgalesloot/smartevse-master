// Command evse-master is an external load-balancing master for SmartEVSE-3
// units configured as nodes, with an HTTP API for evcc.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/floris/evse-master/smartevse"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "evse-master:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		port           = flag.String("port", "/dev/ttyUSB0", "RS485 bus: serial device, or tcp://host:port for a transparent gateway")
		listen         = flag.String("listen", ":7090", "HTTP API listen address")
		maxMains       = flag.Int("max-mains", 0, "max mains current per phase in A (required)")
		maxCircuit     = flag.Int("max-circuit", 0, "max current per phase for all EVSEs together in A, 0 = disabled")
		minCurrent     = flag.Int("min-current", 6, "minimum charge current in A")
		nodes          = flag.String("nodes", "1:16,2:16", "nodes as id:maxA, comma separated (id = Load Balancing setting 1..7)")
		stale          = flag.Duration("stale", 6*time.Second, "mains data older than this stops all nodes")
		ct200          = flag.Bool("ct200", false, "Sensorbox CTs are 200A:50mA")
		defaultEnabled = flag.Bool("default-enabled", false, "nodes may charge before evcc enabled them")
		simulate       = flag.Bool("sim", false, "run against a simulated bus in real time (no hardware)")
		logLevel       = flag.String("log-level", "info", "debug, info, warn or error")
	)
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("log level: %w", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	nodeCfg, err := parseNodes(*nodes)
	if err != nil {
		return err
	}
	cfg := smartevse.Config{
		MaxMains:       *maxMains,
		MaxCircuit:     *maxCircuit,
		MinCurrent:     *minCurrent,
		Nodes:          nodeCfg,
		Stale:          *stale,
		CT200A:         *ct200,
		DefaultEnabled: *defaultEnabled,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		bus *smartevse.Bus
		sim *smartevse.Sim
	)
	if *simulate {
		ids := make([]int, len(nodeCfg))
		for i, nc := range nodeCfg {
			ids[i] = nc.ID
		}
		sim = smartevse.NewSim(ids...)
		sim.House = [3]float64{5, 5, 5}
		bus = smartevse.NewBus(sim)
		bus.BroadcastGap = 0
		go advanceSim(ctx, sim)
		log.Info("running against simulated bus", "nodes", ids)
	} else {
		t, err := smartevse.Open(*port)
		if err != nil {
			return err
		}
		bus = smartevse.NewBus(t)
		log.Info("bus opened", "port", *port)
	}
	defer bus.Close()

	m, err := smartevse.NewMaster(bus, cfg, log)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           newAPI(m, sim, log).routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("http api listening", "addr", *listen)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err = <-errc:
		stop()
	}
	<-done

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	return err
}

// advanceSim moves the simulator's virtual clock along with real time.
func advanceSim(ctx context.Context, sim *smartevse.Sim) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sim.Advance(time.Second)
		}
	}
}

// parseNodes parses "1:16,2:16" into node configs.
func parseNodes(s string) ([]smartevse.NodeConfig, error) {
	var nodes []smartevse.NodeConfig
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idStr, maxStr, ok := strings.Cut(part, ":")
		if !ok {
			return nil, fmt.Errorf("node %q: want id:maxA", part)
		}
		id, err := strconv.Atoi(idStr)
		if err != nil {
			return nil, fmt.Errorf("node %q: id: %w", part, err)
		}
		max, err := strconv.Atoi(maxStr)
		if err != nil {
			return nil, fmt.Errorf("node %q: max: %w", part, err)
		}
		nodes = append(nodes, smartevse.NodeConfig{ID: id, MaxA: max})
	}
	return nodes, nil
}
