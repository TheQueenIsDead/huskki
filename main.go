package main

import (
	"bufio"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"

	ds "github.com/starfederation/datastar-go/datastar"
	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

type hub struct {
	mu   sync.Mutex
	subs map[int]chan map[string]any
	next int
	last map[string]any
}

func newHub() *hub { return &hub{subs: map[int]chan map[string]any{}, last: map[string]any{}} }

func (h *hub) subscribe() (int, <-chan map[string]any, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.next
	h.next++
	ch := make(chan map[string]any, 16)
	if len(h.last) > 0 {
		ch <- h.copy(h.last)
	}
	h.subs[id] = ch
	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if c, ok := h.subs[id]; ok {
			close(c)
			delete(h.subs, id)
		}
	}
	return id, ch, cancel
}

func (h *hub) copy(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (h *hub) broadcast(sig map[string]any) {
	h.mu.Lock()
	for k, v := range sig {
		h.last[k] = v
	}
	for _, ch := range h.subs {
		select {
		case ch <- h.copy(sig):
		default:
		}
	}
	h.mu.Unlock()
}

func main() {
	port := flag.String("port", "auto", "serial device path or 'auto'")
	baud := flag.Int("baud", 115200, "baud rate (ignored when -port=auto)")
	addr := flag.String("addr", ":8080", "http listen address")
	flag.Parse()

	// auto-select Arduino-ish port if requested
	if *port == "auto" {
		name, err := autoSelectPort()
		if err != nil {
			log.Fatalf("auto-select: %v", err)
		}
		*port = name
		*baud = 115200
	}
	mode := &serial.Mode{BaudRate: *baud}
	s, err := serial.Open(*port, mode)
	if err != nil {
		log.Fatalf("open serial %s: %v", *port, err)
	}
	log.Printf("Connected to %s @ %d", *port, *baud)
	defer func() {
		if err := s.Close(); err != nil {
			log.Printf("close serial: %v", err)
		}
	}()

	h := newHub()

	// read CSV lines from Arduino
	go func() {
		sc := bufio.NewScanner(s)
		buf := make([]byte, 0, 64*1024)
		sc.Buffer(buf, 1024*1024)

		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())

			// print raw serial line to console
			//log.Println("SER:", line)

			// Expect: millis,DID,data_hex[,u16be]
			parts := strings.SplitN(line, ",", 4)
			if len(parts) < 3 {
				continue
			}
			didStr := parts[1]
			if !strings.HasPrefix(didStr, "0x") {
				continue
			}
			didVal, err := strconv.ParseUint(didStr[2:], 16, 16)
			if err != nil {
				continue
			}
			dataHex := parts[2]
			clean := strings.ReplaceAll(dataHex, " ", "")
			if len(clean)%2 == 1 {
				continue
			}
			b, err := hex.DecodeString(clean)
			if err != nil || len(b) == 0 {
				continue
			}

			switch uint16(didVal) {
			case 0x0100: // RPM = u16be / 4
				if len(b) >= 2 {
					raw := int(b[0])<<8 | int(b[1])
					rpm := raw / 4
					h.broadcast(map[string]any{"rpm": rpm})
				}
			case 0x0076: // TPS (0..1023) → %
				if len(b) >= 2 {
					raw := int(b[0])<<8 | int(b[1])
					if raw < 0 {
						raw = 0
					}
					if raw > 1023 {
						raw = 1023
					}
					pct := int(math.Round(float64(raw) * 100.0 / 1023.0))
					fmt.Println(pct)
					h.broadcast(map[string]any{"tps": pct})
				}
			case 0x0009: // Coolant °C (1:1)
				if len(b) >= 2 {
					val := int(b[0])<<8 | int(b[1])
					h.broadcast(map[string]any{"coolant": val})
				} else if len(b) == 1 {
					h.broadcast(map[string]any{"coolant": int(b[0])})
				}
			}
		}
		if err := sc.Err(); err != nil {
			log.Printf("serial scanner error: %v", err)
		}
	}()

	// HTTP: index
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		if _, err := fmt.Fprint(w, indexHTML); err != nil {
			log.Printf("write index.html: %v", err)
		}
	})

	// HTTP: SSE
	http.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		sse := ds.NewSSE(w, r)

		_, ch, cancel := h.subscribe()
		defer cancel()

		for {
			select {
			case <-r.Context().Done():
				return
			case sig, ok := <-ch:
				if !ok {
					return
				}
				var b strings.Builder
				if v, ok := sig["rpm"]; ok {
					fmt.Fprintf(&b, `<span id="rpm">%v</span>`, v)
				}
				if v, ok := sig["tps"]; ok {
					fmt.Fprintf(&b, `<span id="tps">%v</span>`, v)
				}
				if v, ok := sig["coolant"]; ok {
					fmt.Fprintf(&b, `<span id="coolant">%v</span>`, v)
				}
				if b.Len() > 0 {
					_ = sse.PatchElements(b.String())
				}
			}
		}
	})

	log.Printf("Listening on %s …", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// --- auto-detect a likely Arduino/clone serial device ---
var preferredVIDs = map[string]bool{
	"2341": true, // Arduino
	"2A03": true, // Arduino (older)
	"1A86": true, // CH340
	"10C4": true, // CP210x
	"0403": true, // FTDI
}

func autoSelectPort() (string, error) {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return "", fmt.Errorf("enumerate ports: %w", err)
	}
	for _, p := range ports {
		if p.IsUSB && preferredVIDs[strings.ToUpper(p.VID)] {
			return p.Name, nil
		}
	}
	for _, p := range ports {
		if p.IsUSB {
			return p.Name, nil
		}
	}
	if len(ports) > 0 {
		return ports[0].Name, nil
	}
	return "", fmt.Errorf("no serial ports found")
}

const indexHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>ECU Live</title>
<script type="module" src="https://cdn.jsdelivr.net/gh/starfederation/datastar@main/bundles/datastar.js"></script>
<style>
  body { font-family: system-ui, -apple-system, Segoe UI, Roboto, sans-serif; margin: 2rem; display:flex; gap:1rem; flex-wrap:wrap; }
  .card { padding:1.25rem 1.5rem; border-radius:14px; box-shadow:0 8px 24px rgba(0,0,0,.08); min-width:200px; }
  .label { color:#666; font-size:.9rem; }
  .value { font-size:3rem; font-weight:700; letter-spacing:.02em; }
  .unit { font-size:1.1rem; color:#777; padding-left:.25rem; }
</style>
</head>
<body>
  <div data-on-load="@get('/events', {openWhenHidden: true})"></div>

  <div class="card">
    <div class="label">RPM</div>
    <div class="value"><span id="rpm">—</span><span class="unit">rpm</span></div>
  </div>

  <div class="card">
    <div class="label">TPS</div>
    <div class="value"><span id="tps">—</span><span class="unit">%</span></div>
  </div>

  <div class="card">
    <div class="label">Coolant</div>
    <div class="value"><span id="coolant">—</span><span class="unit">°C</span></div>
  </div>
</body>
</html>`
