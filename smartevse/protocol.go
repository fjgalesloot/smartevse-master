// Package smartevse implements a SmartEVSE-3 compatible load-balancing master.
//
// It speaks the master/node protocol of the SmartEVSE-3 firmware (v3.11.x) over
// Modbus RTU, so SmartEVSE units configured as "Node 1".."Node 7" can be driven
// by an external controller (evcc) instead of a SmartEVSE in master mode.
//
// Protocol reference: SmartEVSE-3 firmware, src/main.cpp (BroadcastCurrent,
// requestNodeStatus, receiveNodeStatus, processAllNodeStates, CalcBalancedCurrent,
// IsCurrentAvailable) and src/modbus.cpp.
package smartevse

// Node / EVSE states as used in register 0x0000 of a node (main_c.h).
const (
	StateA          = 0  // vehicle not connected
	StateB          = 1  // vehicle connected, not charging
	StateC          = 2  // charging
	StateD          = 3  // charging, ventilation required
	StateCommB      = 4  // node requests A->B
	StateCommBOK    = 5  // master confirms A->B
	StateCommC      = 6  // node requests B->C
	StateCommCOK    = 7  // master confirms B->C
	StateActivation = 8  // activation mode
	StateB1         = 9  // connected, no PWM
	StateC1         = 10 // charging, no PWM (stopping)
)

// Error bits in register 0x0001 of a node.
const (
	ErrLess6A   = 1  // not enough current available
	ErrCTNoComm = 2  // no communication with mains meter / master
	ErrTempHigh = 4  // temperature too high
	ErrEVNoComm = 8  // EV meter communication error
	ErrRCD      = 16 // residual current detected
)

const (
	// NrEVSEs is the number of slots in the balance broadcast (master + 7 nodes).
	NrEVSEs = 8
	// BroadcastAddr is the SmartEVSE broadcast address (not the Modbus standard 0).
	BroadcastAddr = 0x09
	// SensorboxAddr is the fixed Modbus address of a Sensorbox-2.
	SensorboxAddr = 0x0A

	// ModeNormal, ModeSmart and ModeSolar are the values of the mode register.
	ModeNormal = 0
	ModeSmart  = 1
	ModeSolar  = 2

	regNodeStatus     = 0x0000 // 8 input registers: state, error, current, mode, solartimer, access, cfgchanged, max
	regNodeError      = 0x0001
	regNodeCfgChanged = 0x0006 // set by the node when its settings were saved; the master clears it
	regNodeEVMeter    = 0x0108 // 2 registers: EV meter type, EV meter address
	regBroadcastLoad  = 0x0020 // 8 balance currents + 3 mains currents (0.1 A)
	nodeStatusRegs    = 8
	broadcastLoadRegs = NrEVSEs + 3
)

// crc16 computes the Modbus RTU CRC (poly 0xA001, init 0xFFFF).
func crc16(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// appendCRC appends the CRC low byte first, as Modbus RTU requires.
func appendCRC(frame []byte) []byte {
	c := crc16(frame)
	return append(frame, byte(c), byte(c>>8))
}

// StateName returns a readable name for a node state.
func StateName(s int) string {
	names := []string{"A", "B", "C", "D", "COMM_B", "COMM_B_OK", "COMM_C", "COMM_C_OK", "ACTIVATION", "B1", "C1"}
	if s >= 0 && s < len(names) {
		return names[s]
	}
	return "UNKNOWN"
}
