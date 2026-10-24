package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/fjgalesloot/smartevse-master/smartevse"
)

// sniffBus prints every frame on the bus with its time since the previous one.
// On a bus with only slaves (smartevse-master stopped) it should print nothing.
func sniffBus(ctx context.Context, port string) error {
	fmt.Fprintf(os.Stderr, "sniffing %s (never transmits), ctrl-c to stop\n", port)
	var (
		prev   time.Time
		frames int
	)
	err := smartevse.Sniff(ctx, port, func(f smartevse.SniffFrame) {
		frames++
		gap := "     -"
		if !prev.IsZero() {
			gap = fmt.Sprintf("%6d", f.At.Sub(prev).Milliseconds())
		}
		prev = f.At
		crc := "crc ok "
		if !f.CRCOK {
			crc = "crc BAD"
		}
		desc := ""
		if len(f.Data) >= 2 {
			desc = fmt.Sprintf("addr %3d fc %02x", f.Data[0], f.Data[1])
		}
		fmt.Printf("%s +%sms %3d bytes %s %-15s % x\n", f.At.Format("15:04:05.000"), gap, len(f.Data), crc, desc, f.Data)
	})
	fmt.Fprintf(os.Stderr, "%d frames\n", frames)
	return err
}
