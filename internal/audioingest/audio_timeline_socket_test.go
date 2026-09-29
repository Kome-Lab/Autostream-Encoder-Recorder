//go:build autostream_media_diagnostics

package audioingest

import (
	"fmt"
	"os"
	"strings"
)

type timelineSocket struct {
	AtNS      int64  `json:"elapsed_ns"`
	Port      int    `json:"port"`
	Observed  bool   `json:"observed"`
	TxRxQueue string `json:"tx_rx_queue_hex"`
	Drops     string `json:"kernel_drops"`
}

// A read-only observation of the one receiver in this isolated test container.
func captureTimelineSocket(port int, at int64) timelineSocket {
	r := timelineSocket{AtNS: at, Port: port}
	data, err := os.ReadFile("/proc/net/udp")
	if err != nil {
		return r
	}
	suffix := fmt.Sprintf(":%04X", port)
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) >= 13 && strings.HasSuffix(f[1], suffix) {
			r.Observed = true
			r.TxRxQueue = f[4]
			r.Drops = f[len(f)-1]
			return r
		}
	}
	return r
}
