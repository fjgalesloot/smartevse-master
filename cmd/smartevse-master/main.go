// Command smartevse-master is an external load-balancing master for SmartEVSE-3
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

	"github.com/fjgalesloot/smartevse-master/smartevse"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "smartevse-master:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		port           = flag.String("port", "/dev/ttyUSB0", "RS485 bus: serial device, or tcp://host:port for a transparent gateway")
		timeout        = flag.Duration("timeout", 0, "bus response timeout, 0 = default for the port type (serial 250ms, tcp 500ms)")
		listen         = flag.String("listen", ":7090", "HTTP API listen address")
		maxMains       = flag.Int("max-mains", 0, "max mains current per phase in A (required)")
		maxCircuit     = flag.Int("max-circuit", 0, "max current per phase for all EVSEs together in A, 0 = disabled")
		minCurrent     = flag.Int("min-current", 6, "minimum charge current in A")
		nodes          = flag.String("nodes", "1:16,2:16", "nodes as id:maxA[:host], comma separated (id = Load Balancing setting 1..7, host = WiFi address for 1P/3P switching)")
		stale          = flag.Duration("stale", 11*time.Second, "mains data older than this stops all nodes (firmware: 11s)")
		ct200          = flag.Bool("ct200", false, "Sensorbox CTs are 200A:50mA")
		defaultEnabled = flag.Bool("default-enabled", false, "nodes may charge before evcc enabled them")
		simulate       = flag.Bool("sim", false, "run against a simulated bus in real time (no hardware)")
		sniff          = flag.Bool("sniff", false, "only listen on -port and print all traffic, never transmit (diagnostics)")
		logLevel       = flag.String("log-level", "info", "debug, info, warn or error")
		showVersion    = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(buildVersion())
		return nil
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("log level: %w", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	log.Info("smartevse-master", "version", buildVersion())

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

	if *sniff {
		return sniffBus(ctx, *port)
	}

	var (
		bus *smartevse.Bus
		sim *smartevse.Sim
	)
	if *simulate {
		ids := make([]int, len(nodeCfg))
		for i, nc := range nodeCfg {
			ids[i] = nc.ID
			cfg.Nodes[i].Host = fmt.Sprintf("node%d", nc.ID) // the sim's REST API
		}
		sim = smartevse.NewSim(ids...)
		sim.House = [3]float64{5, 5, 5}
		bus = smartevse.NewBus(sim)
		bus.BroadcastGap = 0
		go advanceSim(ctx, sim)
		log.Info("running against simulated bus", "nodes", ids)
	} else {
		// opened by the first cycle, which logs "bus connected" or "bus not available"
		bus = smartevse.NewBusURI(*port, log)
		if *timeout > 0 {
			bus.Timeout = *timeout
		}
	}
	defer bus.Close()

	m, err := smartevse.NewMaster(bus, cfg, log)
	if err != nil {
		return err
	}
	if sim != nil {
		m.SetHTTPTransport(sim)
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

// parseNodes parses "1:16:smartevse1,2:16" into node configs. The host may
// carry a port (1:16:smartevse1:8080).
func parseNodes(s string) ([]smartevse.NodeConfig, error) {
	var nodes []smartevse.NodeConfig
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields := strings.SplitN(part, ":", 3)
		if len(fields) < 2 {
			return nil, fmt.Errorf("node %q: want id:maxA[:host]", part)
		}
		id, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, fmt.Errorf("node %q: id: %w", part, err)
		}
		max, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("node %q: max: %w", part, err)
		}
		nc := smartevse.NodeConfig{ID: id, MaxA: max}
		if len(fields) == 3 {
			nc.Host = fields[2]
		}
		nodes = append(nodes, nc)
	}
	return nodes, nil
}
