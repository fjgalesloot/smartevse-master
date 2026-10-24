# smartevse-master

External SmartEVSE-3 load-balancing master, so evcc can control two SmartEVSE-3
units (Stegen firmware v3.11.2) configured as Node 1 / Node 2, while MaxMains
stays enforced outside evcc. Goal: later port `smartevse/` into evcc as a charger.

## Why (findings from SmartEVSE-3 v3.11.2 source)

- Normal mode ignores MaxMains (`if (MainsMeter.Type && Mode != MODE_NORMAL)`); the
  default evcc template uses Normal mode → no mains protection if evcc dies.
- `override_current` is rejected on nodes (`LoadBl < 2` in esp32.cpp REST/MQTT) and only
  limits the master's own slot (`BalancedMax[0]`), so evcc cannot steer nodes.
- Nodes stop charging (`CT_NOCOMM`) when no broadcast arrives for ~11 s
  (`COMM_TIMEOUT`) → built-in fail-safe if this daemon dies.
- Nodes trust the master blindly: `SetCurrent()` only clamps to 80 A. This master MUST
  cap each node at its reported max (reg 0x0007) and the configured hard cap.
- Nodes never poll their own EV meter: the master does (ModbusRequestLoop states 3-5,
  20) and the node listens to the answers (`MBEVMeterResponse`). Without it a node with
  an EV meter set gets EV_NOCOMM after 64 s (`COMM_EVTIMEOUT`, ~11 s after boot or a
  settings save) in Smart/Solar and stops, and its OCPP meter values (`EVMeter.Energy`)
  stay stale.

## Protocol (RS485, 9600 8N1)

- Node address = LoadBl (Node1 = 0x02, Node2 = 0x03); broadcast slot index = address - 1.
- Poll node: FC04, reg 0x0000, 8 regs. state = byte 1, error = byte 3, mode = byte 7,
  access = byte 11, max A = byte 15.
- Access: reg 0x0005, OFF=0 / ON=1 / PAUSE=2, FC06, stored on the node (NVS, ≤ 1 write
  per 60 s). Only ON lets a node request (COMM_B/COMM_C). PAUSE/OFF stop a charging car
  (C→C1, B→B1) but leave COMM_B/COMM_C as is, and a paused node in A ignores a car being
  plugged in (stays A). Node display, web UI and MQTT show "Pause".
- Handshake: state 4 (COMM_B) → write 5; state 6 (COMM_C) → write 7. FC10 to reg 0x0000
  with [state, error] (+ [current(ignored), mode] when correcting mode).
- Every ~2 s broadcast FC10 to addr 0x09, reg 0x0020, 11 regs: 8 currents (0.1 A) +
  mains L1-L3 (0.1 A, int16). No reply on broadcast.
- Persistent shortage (NoCurrent > 2) or Imeasured > 2×MaxMains: broadcast FC06
  reg 0x0001 = LESS_6A.
- Mains lost: set CT_NOCOMM on nodes and STOP broadcasting (mirrors firmware).
- Sensorbox-2: addr 0x0A, FC04 reg 0x0000, 20 regs. reg1 low byte: 0x80 = P1, else CT.
  P1 volts at reg 0x0002, P1 amps at reg 0x0008, CT amps at reg 0x000E, float32
  big-endian. No power or energy registers (PIC and ESP firmware, Sensorbox-2 repo).
  P1 amps = (21.7.0 - 22.7.0) / 32.7.0, so V × I is the meter's per-phase power
  exactly; a missing P1 voltage reads 1 V. CT amps = real power / nominal V.
- EV meter setting: status byte 13 (reg 0x0006) = config changed → FC04 reg 0x0108,
  2 regs (type = byte 1, address = byte 3), then FC06 reg 0x0006 = 0 to clear the flag.
- Contactor 2 (1P/3P, `EnableC2`) is NOT reachable over Modbus: MENU_C2 (item 41) lies
  outside the registers mapModbusRegister2ItemID maps (0x0200 + 22 = items 13-34). Only
  the node's WiFi REST API (POST /settings?enable_C2=1|3, empty body; reply has
  settings.enable_C2) or MQTT Set/EnableC2. The node applies it on entering C
  (setState STATE_C). Its web server answers 503 "Server busy" when > 4 HTTP
  connections are open (MAX_HTTP_CONNECTIONS).
- EV meter reads: the node takes the register of an answer from the request it saw just
  before, and only acts on IRegister (current: resets its timeout), ERegister (energy)
  and PRegister (power) of its type, so requests must match the firmware exactly
  (EMConfig in meter.cpp, requestCurrentMeasurement in modbus.cpp).

## Status

Done (`smartevse/`, stdlib only, `go vet` clean, all tests pass):
- protocol.go: constants, CRC (verified against firmware test vectors)
- transport.go / serial_linux.go: `NewBusURI` for serial (raw termios) and tcp://
  (transparent gateway / ser2net raw, NOT Modbus TCP: mbusd etc. wait for an answer to
  the address-9 broadcast); opens on first use and reconnects after I/O errors with
  backoff 1 s doubling to 30 s, so a device missing at startup is handled like one
  lost later. The bus logs loss/recovery once; requests fail with ErrBusDown meanwhile,
  which the master does not log
- bus.go: FC03/FC04 read, FC10/FC06 write, broadcast without reply
- sensorbox.go: decode P1/CT, plausibility checks; power per phase in W (P1: V × I
  when 180-270 V, else I × 230)
- evmeter.go: EMConfig port (all Modbus meter types usable as EV meter; not Sensorbox,
  SolarEdge, API, HomeWizard, Custom), request frames and decoding (currents with sign
  from phase power, power, energy) as meter.cpp
- balancer.go: Smart-mode port of CalcBalancedCurrent / IsCurrentAvailable /
  processAllNodeStates + per-node external limit (Max) from evcc
- master.go: 2 s cycle, node handles with evcc-shaped methods
  (Status, Enabled, Enable, MaxCurrent, MaxCurrentMillis), Snapshot for an API.
  After the broadcast: one online node's EV meter per cycle, round robin (current,
  energy, power if not in the current answer); readings in NodeInfo.Meter.
  syncAccess: pauses nodes evcc disabled (access register), see deviation 5.
  Phases1p3p / GetPhases (evcc PhaseSwitcher / PhaseGetter): sets enable_C2 via the
  node's REST API; a charging node whose setting changed is held paused (same access
  mechanism) until it reports it left C, then resumed, so it restarts on the new
  phases (evcc's smart-evse driver does the same with whenDisabled). Hold ends after
  30 s at most. GetPhases reads the setting once (again after the node was offline)
- nodeweb.go: REST client for enable_C2 (Connection: close, retries on 503)
- sim.go: simulated bus (Sensorbox + nodes + an Eastron3P EV meter per node), node
  behaviour ported from the firmware state machine incl. STATUS_ERROR handling,
  ChargeDelay, COMM_TIMEOUT, access (setAccess), EV meter listening and COMM_EVTIMEOUT;
  contactor 2 (switched on entering C) and the node REST API as http.RoundTripper
  (host "node<id>", WebBusy for 503s)
- scenario_test.go: 22 scenarios; soak_test.go: 20 seeds × 2 h random events;
  evmeter_test.go: request frames per type, decode vectors (incl. one from the real bus)

Deliberate deviations from the firmware (all found by the simulator):
1. Fresh-measurement flag is consumed by the first Calc of a cycle (firmware
   `phasesLastUpdateFlag`); without it a new EVSE got a second ramp step → +1 A.
2. limitIncreases: an EVSE only goes up by measured headroom + decreases of nodes
   that answered their last poll. Firmware redistributes on paper; a node that
   misses the broadcast keeps its old current → +5 A for ~10 s.
3. Caps (Max) apply every cycle, not only during distribution (evcc limit was
   ignored while the Sensorbox missed a reading).
4. Max one new EVSE start per cycle. Firmware approves all waiting nodes against
   one stale measurement → two simultaneous plug-ins each got full headroom (+12 A).
5. A node evcc disabled is paused through its access register instead of rejected
   with LESS_6A → re-enable takes ~6 s instead of 64 s (node ChargeDelay), and the
   node shows "Pause". A waiting node (COMM_B/COMM_C) is sent to B1 once paused. evcc
   wins: a change on the node is undone next cycle. A node in state A is kept ON, or
   it would not notice a car and evcc would never see it connect.
6. EV meter setting is also re-read when a node comes back online (it may have
   restarted with other settings; firmware only reads it at master boot or on the flag).
7. No separate power read for meters whose current answer holds the phase powers
   (firmware checks the master's own EV meter type there, not the node's).

Known, inherent (same as firmware): a sudden household load is corrected on the next
measurement (≤ 1 cycle overload); a hard shortage stops all EVSEs after 3 cycles.
A car drawing less than its PWM leaves "phantom" headroom that another node may get;
if that car then ramps up, the overload lasts ≤ 1 cycle. While the Sensorbox does not
answer, currents are held (not raised) until -stale (default 11 s = COMM_TIMEOUT)
stops all nodes; a household load change in that window goes unseen until then.

Done (`cmd/smartevse-master`, Dockerfile, `examples/`):
- main.go: flags (-port, -listen :7090, -max-mains, -max-circuit, -min-current,
  -nodes id:maxA[:host], -stale (11 s), -ct200, -default-enabled (default false), -sim, -log-level), SIGTERM
  shutdown. `-sim` advances Sim in real time (1 s ticks).
- api.go: GET /api/state (evcc grid meter: .powerW, .mainsA, error on .fault), GET /api/nodes/{id} (evcc jq .status / .enabled /
  .meter.powerW / .meter.energyKWh; .access on/pause/off; status "offline" makes evcc error), POST /api/nodes/{id}/enable?value=,
  POST /api/nodes/{id}/current?value= (float), GET|POST /api/nodes/{id}/phases[?value=1|3]
  (evcc getphases jq .phases / phases1p3p; 400 without host, 502 node error), GET /healthz (503 on stalled loop or
  mains fault). With -sim also GET /api/sim, POST /api/sim/house?value=l1,l2,l3,
  /api/sim/sensorbox?offline=, /api/sim/nodes/{id}?connected=&wants=&offline=&meteroffline=&carmax=&phases=&access=
  (access=on|pause|off as if changed on the node).
- .github/workflows/docker.yml: go vet + test, then buildx amd64/arm64/arm/v7 to
  ghcr.io/fjgalesloot/smartevse-master (main → :latest/:main/:sha-x, tag v1.2.3 → :1.2.3/:1.2;
  PRs build only; docs/examples-only changes skip). Version = git describe, passed as
  build arg VERSION → -X main.version (version.go; local go build: dev-<rev>), shown by
  -version, the startup log and /api/state .version. Release: push a tag v1.2.3.
- Dockerfile: buildx multi-arch, CGO_ENABLED=0, distroless static nonroot (needs
  group_add with the tty device gid). examples/evcc.yaml uses Go templates in uri
  (`{{.enable}}`, `{{.maxcurrent}}`, `{{.maxcurrentmillis}}`) per docs.evcc.io
  /en/reference/plugins and /en/user-defined-devices.

Verified: real evcc (template rendering, resync after a smartevse-master restart),
real hardware (node timing, pause, EV meters, 1P/3P), Docker build (docker8).

TODO:
1. Later: port to evcc as `charger/smartevse-master.go` (one master, N chargers).

## Conventions

- No external dependencies (stdlib only) to ease porting into evcc.
- Currents internally in 0.1 A, like the firmware.
- Code comments in English.
