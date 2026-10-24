# Power sharing: evse-master vs. SmartEVSE-3 firmware master

How this master divides the available current between the nodes, and how that
compares with the master in the SmartEVSE-3 firmware.

Compared against firmware **v3.11.2** (tag `v3.11.2`, commit `0de2aa0`). Firmware
references are to `SmartEVSE-3/src/main.cpp` unless a file is named.

| Firmware | Repo |
|---|---|
| `ModbusRequestLoop` (2 s cycle) | `Master.Cycle` / `control` ([master.go](../smartevse/master.go)) |
| `CalcBalancedCurrent` (main.cpp:866) | `Balancer.Calc` ([balancer.go](../smartevse/balancer.go)) |
| `IsCurrentAvailable` (main.cpp:789) | `Balancer.Available` |
| `processAllNodeStates` (main.cpp:1790) | `Balancer.NodeUpdate` |
| `NoCurrent > 2` → broadcast `LESS_6A` (main.cpp:2157) | `Balancer.Shortage` + `Master.control` |

Only Smart mode is ported. Currents are in 0.1 A, as in the firmware.

## How the current is distributed

Every 2 s:

1. **Mains.** The master reads the Sensorbox. `Imeasured` is the highest of the
   three phases, as in the firmware's `CalcImeasured`. Balancing is not
   phase-aware: a 1-phase car on L1 is also limited by a load on L2.
2. **Poll nodes.** Each node's cap `Max[n]` is the lowest of the node's
   reported max (reg 0x0007), the `-nodes` hard cap and the evcc limit. It is 0
   when evcc has disabled the node or set a limit below MinCurrent.
3. **Pause.** A node with a car that evcc does not allow to charge is paused
   through its access register; all others are set to ON (see
   [Stopping a car](#ramping-down-and-stopping-a-car-off)).
4. **Handshake.** Requests from nodes waiting to start (COMM_B, COMM_C) are
   answered (see [Starting a car](#starting-a-car-on)).
5. **Budget** (`iset`, the firmware's `IsetBalanced`) for all charging EVSEs
   together. With `idiff = MaxMains − Imeasured`, on each fresh measurement:
   - if `idiff > 0`: `iset += idiff / 4`, so the budget goes up slowly;
   - otherwise `iset += idiff`, so the budget drops by the full overshoot at once.

   The budget is then clamped to `MaxMains − baseload`, to `MaxCircuit`, and
   to the sum of the active caps.
6. **Split.** Any EVSE whose cap is below the average gets its cap; the rest is
   shared evenly. With one car, it gets `min(iset, cap)`.
7. **`limitIncreases`** (repo only): see [Differences](#differences-when-ramping-up-or-starting).
8. **Broadcast** the currents and the mains readings to the nodes.

The evcc limit is a **ceiling, not a setpoint**. A car gets up to its cap and
only gets less when the mains measurement leaves less room. When cars compete,
the current is split evenly; evcc can only favour one car by lowering the other
car's limit.

## Ramping up and starting a car: same as the firmware

The ramp maths matches the firmware line for line.

- **Ramp step.** `+Idifference/4` on headroom, full `Idifference` on overshoot,
  clamped to 0–80 A, applied only on a fresh mains reading (main.cpp:968–981).
  A car's own draw lags its PWM, so each cycle closes about 25 % of the gap.
- **Boundaries.** `MaxMains − Baseload`, `MaxCircuit`, then the sum of the active
  caps `ActiveMax` (main.cpp:1029–1033, 1161). Clamping to `ActiveMax` makes the
  lower value stick, so after a cap is raised the car climbs back through the
  ¼ ramp. The firmware behaves the same when a node's max is raised.
- **Split.** Same two-pass algorithm (main.cpp:1165–1211).
- **Fresh-measurement flag.** The first `Calc` of a cycle uses it up
  (`phasesLastUpdateFlag`, cleared at main.cpp:1222). In a cycle where a car
  starts, the normal ¼ step is therefore skipped. CLAUDE.md lists this as
  "deviation 1", but it is the firmware's behaviour; the deviation was against an
  earlier version of this port.
- **Shortage.** If the budget is below MinCurrent per active EVSE, each car is
  held at MinCurrent and, on a hard shortage, `NoCurrent` is incremented. After
  more than 2 cycles, or immediately if `Imeasured > 2 × MaxMains`, `LESS_6A` is
  broadcast. All nodes stop and wait their 60 s `CHARGEDELAY`.

### Starting a car (on)

- **COMM_B → COMM_B_OK** (car plugged in): approved if `IsCurrentAvailable`
  finds room for MinCurrent per active EVSE plus this one. The node is sent
  MinCurrent (6 A) for its PWM advertisement (main.cpp:1825–1842).
- **COMM_C → COMM_C_OK** (car asks to charge): the node's current is set to 0
  so the baseload is correct, then `CalcBalancedCurrent(1)` runs. That sets the
  budget to the **full** headroom (`MaxMains − Baseload`) rather than ¼ of it,
  and shares it out (main.cpp:1012–1019, 1844–1861). A new car can therefore
  start above 6 A straight away, because it is not drawing anything yet.

## Differences when ramping up or starting

| Situation | Firmware | Repo | Why |
|---|---|---|---|
| A car starts in a cycle where the Sensorbox reading failed | Nothing is shared out (main.cpp:1044 requires the fresh flag), so the new node is sent 0 A, goes to C1 (modbus.cpp:533) and stops. | Shares out anyway; all limits have already been applied. | Firmware bug: the start is lost. |
| Two cars ask to start in the same cycle | Each is approved against the same reading, taken before either started, so each gets the full headroom (+12 A in the simulator). | One start per cycle (`StartedThisCycle`); the second waits for a reading that includes the first. | Overload. |
| How much a node may go up | No limit. Current is moved between nodes in the same broadcast; a node that misses it keeps its old current (+5 A for ~10 s in the simulator). | `limitIncreases`: at most the measured headroom plus decreases of nodes that answered their last poll. A node that didn't answer is never raised. A new car always gets at least MinCurrent. | Overload on bus errors. |
| A car disabled by evcc | No such concept: every node in state C is active. Nodes have no external limit. | Excluded from the active count, given 0 A and paused (see below). A disabled node that asks to charge is paused instead of being sent `LESS_6A`. | Re-enabling takes ~6 s instead of 64 s. |

In normal operation `limitIncreases` never binds: the ¼ step is always smaller
than the measured headroom. It only matters when a node misses polls, or when
current moves from one car to another.

## Ramping down and stopping a car (off)

- **Ramp down.** The full overshoot is taken off in one step, as in the firmware.
  A sudden household load can still overload for up to one cycle.
- **Caps apply every cycle** (repo only). The firmware applies `BalancedMax` only
  while sharing out, which needs a fresh reading, so a lower limit was ignored
  while the Sensorbox missed a reading. Here a lowered evcc limit takes effect
  within one cycle (≤ 2 s).
- **evcc disables a car** (or sets its limit below MinCurrent): the master
  pauses the node by writing PAUSE to its access register 0x0005
  (`setAccess`, main.cpp:737). The node is also sent 0 A.
  - **What the node does when paused:** it stops by itself (C → C1, B → B1),
    does not ask to charge again, since every request needs access ON
    (main.cpp:2397, 2428, 2448), and its display, web UI and MQTT show "Pause".
  - **A node waiting for an answer** (COMM_B, COMM_C) is not moved by
    `setAccess`, so once its pause is confirmed the master writes state B1 to it.
  - **A node without a car (state A) stays ON.** A paused node in A ignores a
    car being plugged in, so evcc would never see the car connect and never
    enable the node. When a car is plugged in, the node asks to charge (COMM_B)
    and is paused in the same cycle; evcc sees status B.
  - **evcc wins:** a change made on the node (button, web UI, MQTT) is undone in
    the next cycle.
  - **Re-enabling:** the master writes ON; the node goes B1 → COMM_B → … → C, about 6 s.
  - **The setting is stored on the node**, so a node stays paused if evse-master
    stops; it then needs the button or web UI to charge on its own.
- **Mains measurement lost:** the master writes `CT_NOCOMM` to the nodes and stops
  broadcasting, as in the firmware. It approves nothing during the fault, whereas
  the firmware's `IsCurrentAvailable` ignores the fault. Timeout: 11 s
  (`-stale`), the same as the firmware's `COMM_TIMEOUT` (main.cpp:1421).
  - **Why not shorter:** writing `CT_NOCOMM` also starts the node's 60 s
    `CHARGEDELAY` (main.cpp:2677–2679), so a shorter timeout turns a brief
    Sensorbox gap into a stop of over a minute.
  - **The cost:** until the timeout, currents are held but not raised, so a
    household load change in that window goes unseen until the nodes stop.
  - **On recovery** this master clears `CT_NOCOMM` on the nodes itself; the
    firmware leaves that to the node's own timeout.
- **Master dead:** each node stops by itself after ~11 s without a broadcast.

## Firmware master features that are not ported

| Feature | Firmware | Notes |
|---|---|---|
| Solar mode | StartCurrent, StopTime, `SolarStopTimer` (also written to nodes), 1P/3P switching (main.cpp:984–1009, 1053–1087) | evcc handles PV. |
| Normal mode | `IsetBalanced = MaxCircuit − Baseload_Circuit` | No mains protection; not wanted. |
| MaxSumMains (capacity) | Limit on the sum of the phases, plus the `MaxSumMainsTime` timer (main.cpp:955–966, 1387–1393) | Not ported. |
| CircuitMeter | A separate sub-panel meter with its own `Baseload_Circuit` | Here `MaxCircuit` only caps the sum of the EVSE currents, as the firmware does without a circuit meter. |
| GridRelay | `GridRelayMaxSumMains` while the relay is open (main.cpp:1035–1038) | Not ported. |
| Home battery correction | Battery current subtracted from the mains readings (`CalcIsum`, main.cpp:2864) | Not ported. |
| Mains meter types | Any Modbus kWh meter, API, HomeWizard | Sensorbox-2 only. |
| Mode change from a node's display | The whole system switches to the node's mode (`NodeNewMode`, main.cpp:1706, 1869) | Here the node is switched back to Smart. |
| The master's own EVSE (slot 0) | ChargeCurrent, OverrideCurrent, OCPP limit | This master has no charger; slot 0 stays in state A. |

## Other differences

- **Offline nodes.** The firmware skips offline nodes and probes one per cycle
  (main.cpp:2035–2054). This master polls every configured node every cycle, so
  each offline node costs one read timeout per cycle on the bus.
- **EV meter polling.** Round robin, one online node per cycle, as in the
  firmware. It runs after the broadcast instead of before the node polls. The
  firmware master leaves 50 ms between frames; this bus does not (to be checked
  on hardware). Two further deviations (CLAUDE.md, items 6 and 7):
  - the EV meter setting is also re-read when a node comes back online;
  - there is no separate power read for meters whose current answer already
    holds the phase powers.
- **1P/3P switching (repo only).** The firmware master switches phases only for
  its own EVSE (Solar mode, `EnableC2 = AUTO`; "Nodes are not supported yet",
  main.cpp:1062). This master switches nodes for evcc.
  - **Over WiFi:** it sets contactor 2 through the node's REST API
    (`POST /settings?enable_C2=1|3`). Modbus cannot reach that setting: menu item
    41 lies outside the registers `mapModbusRegister2ItemID` maps.
  - **When it applies:** the node switches its contactors when it next enters C
    (main.cpp:686–702). A charging node whose setting changed is therefore held
    paused through its access register until it reports that it has left C, then
    resumed. evcc's own `smart-evse` driver does the same (`whenDisabled`).
  - **Busy node:** the node's web server answers 503 when more than 4 HTTP
    connections are open. Each request closes its connection and is retried.
