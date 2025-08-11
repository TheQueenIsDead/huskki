package main

import (
	"bufio"
	"encoding/hex"
	"flag"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	ds "github.com/starfederation/datastar-go/datastar"
	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

type eventHub struct {
	mu   sync.Mutex
	subs map[int]chan map[string]any
	next int
	last map[string]any
}

func newHub() *eventHub {
	return &eventHub{subs: map[int]chan map[string]any{}, last: map[string]any{}}
}

func (h *eventHub) subscribe() (int, <-chan map[string]any, func()) {
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

func (h *eventHub) copy(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (h *eventHub) broadcast(sig map[string]any) {
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

type cardProps struct {
	Name  string
	Value any
	Unit  string
}

var cards = []cardProps{
	{"Throttle", 0, "%"},
	{"Grip", 0, "%"},
	{"TPS", 0, "%"},
	{"RPM", 0, "RPM"},
	{"Coolant", 0, "°C"},
}

// tpsHistoryData contains all the data points of the throttle position history readings in order to display as a graph
var tpsHistoryData []int

// tpsHistoryLabels contains a timestamp (label) for each data point in history
var tpsHistoryLabels []int

func main() {
	port := flag.String("port", "auto", "serial device path or 'auto'")
	baud := flag.Int("baud", 115200, "baud rate (ignored when -port=auto)")
	addr := flag.String("addr", ":8080", "http listen address")
	replayFile := flag.String("replay", "", "path to replay file (csv log)")
	flag.Parse()

	var serialPort serial.Port
	var err error
	if *replayFile == "" {
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
		serialPort, err = serial.Open(*port, mode)
		if err != nil {
			log.Fatalf("open serial %s: %v", *port, err)
		}
		log.Printf("Connected to %s @ %d", *port, *baud)
		defer func() {
			if err := serialPort.Close(); err != nil {
				log.Printf("close serial: %v", err)
			}
		}()
	}

	hub := newHub()

	// CSV lines from scanner
	go func() {
		var scanner *bufio.Scanner

		isReplay := *replayFile != ""

		if isReplay {
			file, err := os.Open(*replayFile)
			if err != nil {
				log.Fatal(err)
			}
			defer func(file *os.File) {
				err := file.Close()
				if err != nil {
					log.Fatal(err)
				}
			}(file)
			scanner = bufio.NewScanner(file)
		} else {
			scanner = bufio.NewScanner(serialPort)
		}

		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)

		readScanner(scanner, hub, isReplay)
	}()

	// Initialise HTML templating
	t := template.New("").Funcs(template.FuncMap{
		"ToLower": strings.ToLower,
	})
	t, err = t.ParseGlob("templates/*.gohtml")
	if err != nil {
		log.Fatal(err)
	}

	// HTTP: index
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/html; charset=utf-8")

		err := t.ExecuteTemplate(w, "index", map[string]interface{}{
			"cards": cards,
			"history": map[string]interface{}{
				"tpsData":   tpsHistoryData,
				"tpsLabels": tpsHistoryLabels,
			},
		})

		if err != nil {
			log.Printf("write index.html: %v", err)
		}
	})

	// HTTP: SSE
	http.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		sse := ds.NewSSE(w, r)

		_, ch, cancel := hub.subscribe()
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
					t.ExecuteTemplate(&b, "card.value", cardProps{Name: "RPM", Value: v})
				}
				if v, ok := sig["throttle"]; ok {
					t.ExecuteTemplate(&b, "card.value", cardProps{Name: "Throttle", Value: v})
				}
				if v, ok := sig["grip"]; ok {
					t.ExecuteTemplate(&b, "card.value", cardProps{Name: "Grip", Value: v})
				}
				if v, ok := sig["tps"]; ok {
					t.ExecuteTemplate(&b, "card.value", cardProps{Name: "TPS", Value: v})

					fmt.Printf("Trying to push %v into the chart...\n", v)
					err = sse.ExecuteScript(fmt.Sprintf(`
(function(){
	let chart = Chart.getChart("tps-chart");
	chart.data.labels.push('%d');
	chart.data.datasets.forEach((dataset) => {
		dataset.data.push('%d');
	});
	chart.update();
})()
`, time.Now().UnixMilli(), v.(int))) // FIXME: Bad practice to cast like this
					if err != nil {
						log.Printf("execute script: %v", err)
					}

				}
				if v, ok := sig["coolant"]; ok {
					t.ExecuteTemplate(&b, "card.value", cardProps{Name: "Coolant", Value: v})
				}
				if b.Len() > 0 {
					err = sse.PatchElements(b.String())
					if err != nil {
						fmt.Printf("patch elements: %v (%s)", err, b.String())
					}
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

func readScanner(scanner *bufio.Scanner, hub *eventHub, isReplay bool) {
	start := time.Now()
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Expect: millis,DID,data_hex[,u16be]
		parts := strings.SplitN(line, ",", 4)
		if len(parts) < 3 {
			continue
		}
		timestamp, err := strconv.Atoi(parts[0])
		if err != nil {
			log.Printf("parse timestamp: %v", err)
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

		if isReplay {
			elapsed := time.Now().Sub(start)
			timeToWait := timestamp - int(elapsed.Milliseconds())
			if timeToWait > 0 {
				time.Sleep(time.Duration(timeToWait) * time.Millisecond)
			}
		}

		//fmt.Println(line)

		switch uint16(didVal) {
		case 0x0100: // RPM = u16be / 4
			if len(b) >= 2 {
				raw := int(b[0])<<8 | int(b[1])
				rpm := raw / 4
				hub.broadcast(map[string]any{"rpm": rpm})
			}

		case 0x0001: // Throttle: low byte range 3..17
			if len(b) >= 1 {
				raw8 := int(b[len(b)-1])
				//pct := scalePct(raw8, 3, 17) // -> 0..100%
				hub.broadcast(map[string]any{"throttle": raw8})
			}

		case 0x0070: // Grip: low byte range 10..23
			if len(b) >= 1 {
				raw8 := int(b[len(b)-1])
				//pct := scalePct(raw8, 20, 59) // -> 0..100%
				hub.broadcast(map[string]any{"grip": raw8})
			}

		case 0x0076: // TPS (0..1023) -> %
			if len(b) >= 2 {
				raw := int(b[0])<<8 | int(b[1])
				if raw > 1023 {
					raw = 1023
				}
				pct := (raw*100 + 511) / 1023 // integer rounding
				hub.broadcast(map[string]any{"tps": pct})
				tpsHistoryLabels = append(tpsHistoryLabels, timestamp)
				tpsHistoryData = append(tpsHistoryData, pct)
			}

		case 0x0009: // Coolant °C
			if len(b) >= 2 {
				val := int(b[0])<<8 | int(b[1])
				hub.broadcast(map[string]any{"coolant": val})
			} else if len(b) == 1 {
				hub.broadcast(map[string]any{"coolant": int(b[0])})
			}
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("serial scanner error: %v", err)
	}
}

func scalePct(raw, min, max int) int {
	if max <= min {
		return 0
	}
	if raw < min {
		raw = min
	}
	if raw > max {
		raw = max
	}
	return int(math.Round(float64(raw-min) * 100.0 / float64(max-min)))
}
