package main

import (
	"bufio"
	"encoding/hex"
	"flag"
	"fmt"
	"html/template"
	"huskki/hub"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	ds "github.com/starfederation/datastar-go/datastar"
	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

const DEFAULT_BAUD_RATE = 115200

const (
	RPM_DID      = 0x0100
	THROTTLE_DID = 0x0001
	GRIP_DID     = 0x0070
	TPS_DID      = 0x0076
	COOLANT_DID  = 0x0009
)

// Arduino & clones common VIDs
var preferredVIDs = map[string]bool{
	"2341": true, // Arduino
	"2A03": true, // Arduino (older)
	"1A86": true, // CH340
	"10C4": true, // CP210x
	"0403": true, // FTDI
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

type chartProps struct {
	Name        string
	Description string
	Labels      []int
	Data        []int
}

// tpsHistoryData contains all the data points of the throttle position history readings in order to display as a graph
var tpsHistoryData []int

// tpsHistoryLabels contains a timestamp (label) for each data point in history
var tpsHistoryLabels []int

// rpmHistoryData contains all the data points of the revolutions per minute readings in order to display as a graph
var rpmHistoryData []int

// rpmHistoryLabels contains a timestamp (label) for each data point in history
var rpmHistoryLabels []int

var Templates *template.Template

func buildUpdateChartScript(name string, label, data int) string {
	return fmt.Sprintf(`(function(){
		let chart = Chart.getChart("%s-chart");
		chart.data.labels.push('%d');
		chart.data.datasets.forEach((dataset) => {
			dataset.data.push('%d');
		});
		chart.update();
	})()`, strings.ToLower(name), label, data)
}

func main() {
	port, baud, addr, replayFile := getFlags()

	isReplay := *replayFile != ""

	var serialPort serial.Port
	var err error
	if !isReplay {
		serialPort, err = getArduinoPort(port, baud, serialPort, err)
		defer func() {
			if err := serialPort.Close(); err != nil {
				log.Printf("close serial: %v", err)
			}
		}()
	}

	eventHub := hub.NewHub()

	// scan CSV lines from scanner
	go func() {
		scan(isReplay, replayFile, serialPort, eventHub)
	}()

	// Initialise HTML templating
	Templates = template.New("").Funcs(template.FuncMap{
		"ToLower": strings.ToLower,
	})
	Templates, err = Templates.ParseGlob("templates/*.gohtml")
	if err != nil {
		log.Fatal(err)
	}

	handler := http.NewServeMux()

	// HTTP: index
	handler.HandleFunc("/", func(writer http.ResponseWriter, req *http.Request) {
		err := Templates.ExecuteTemplate(writer, "index", map[string]interface{}{
			"cards": cards,
			"tpsChartProps": chartProps{
				Name:        "TPS",
				Description: "Throttle Position Sensor",
				Labels:      tpsHistoryLabels,
				Data:        tpsHistoryData,
			},
			"rpmChartProps": chartProps{
				Name:        "RPM",
				Description: "Revolutions Per Minute",
				Labels:      rpmHistoryLabels,
				Data:        rpmHistoryData,
			},
		})
		if err != nil {
			log.Fatal(err)
		}
	})

	// HTTP: SSE
	handler.HandleFunc("/events", func(writer http.ResponseWriter, req *http.Request) {
		sse := ds.NewSSE(writer, req)

		_, ch, cancel := eventHub.Subscribe()
		defer cancel()

		for {
			if patch(req, ch, sse) {
				return
			}
		}
	})

	log.Printf("Listening on %s …", *addr)
	log.Fatal(http.ListenAndServe(*addr, handler))
}

func getFlags() (*string, *int, *string, *string) {
	port := flag.String("port", "auto", "serial device path or 'auto'")
	baud := flag.Int("baud", DEFAULT_BAUD_RATE, "baud rate")
	addr := flag.String("addr", ":8080", "http listen address")
	replayFile := flag.String("replay", "", "path to replay file (csv log)")
	flag.Parse()
	return port, baud, addr, replayFile
}

func getArduinoPort(port *string, baud *int, serialPort serial.Port, err error) (serial.Port, error) {
	// auto-select Arduino-ish port if requested
	if *port == "auto" {
		name, err := autoSelectPort()
		if err != nil {
			log.Fatalf("auto-select: %v", err)
		}
		*port = name
	}
	mode := &serial.Mode{BaudRate: *baud}
	serialPort, err = serial.Open(*port, mode)
	if err != nil {
		log.Fatalf("open serial %s: %v", *port, err)
	}
	log.Printf("Connected to %s @ %d", *port, *baud)

	return serialPort, err
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

func scan(isReplay bool, replayFile *string, serialPort serial.Port, eventHub *hub.EventHub) {
	var scanner *bufio.Scanner

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

	readScanner(scanner, eventHub, isReplay)
}

func readScanner(scanner *bufio.Scanner, eventHub *hub.EventHub, isReplay bool) {
	start := time.Now()
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		fmt.Println(line)

		// Parse lines; Expect; millis,DID,data_hex[,u16be]
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
		dataBytes, err := hex.DecodeString(clean)
		if err != nil || len(dataBytes) == 0 {
			continue
		}

		// Replay timing
		if isReplay {
			elapsed := time.Now().Sub(start)
			timeToWait := timestamp - int(elapsed.Milliseconds())
			if timeToWait > 0 {
				time.Sleep(time.Duration(timeToWait) * time.Millisecond)
			}
		}

		broadcastParsedSensorData(eventHub, didVal, dataBytes, timestamp)
	}
	if err := scanner.Err(); err != nil {
		log.Printf("serial scanner error: %v", err)
	}
}

func patch(req *http.Request, signalChan <-chan map[string]any, sse *ds.ServerSentEventGenerator) bool {
	select {
	case <-req.Context().Done():
		return true
	case signal, ok := <-signalChan:
		if !ok {
			return true
		}
		var writer = strings.Builder{}
		if v, ok := signal["rpm"]; ok {
			Templates.ExecuteTemplate(&writer, "card.value", cardProps{Name: "RPM", Value: v})
			_ = sse.ExecuteScript(buildUpdateChartScript("RPM", int(time.Now().UnixMilli()), v.(int))) // FIXME: Bad practice to cast like this
		}
		if v, ok := signal["throttle"]; ok {
			Templates.ExecuteTemplate(&writer, "card.value", cardProps{Name: "Throttle", Value: v})
		}
		if v, ok := signal["grip"]; ok {
			Templates.ExecuteTemplate(&writer, "card.value", cardProps{Name: "Grip", Value: v})
		}
		if v, ok := signal["tps"]; ok {
			Templates.ExecuteTemplate(&writer, "card.value", cardProps{Name: "TPS", Value: v})
			_ = sse.ExecuteScript(buildUpdateChartScript("TPS", int(time.Now().UnixMilli()), v.(int))) // FIXME: Bad practice to cast like this
		}
		if v, ok := signal["coolant"]; ok {
			Templates.ExecuteTemplate(&writer, "card.value", cardProps{Name: "Coolant", Value: v})
		}
		if writer.Len() > 0 {
			_ = sse.PatchElements(writer.String())
		}
	}
	return false
}

func broadcastParsedSensorData(eventHub *hub.EventHub, didVal uint64, dataBytes []byte, timestamp int) {
	switch uint16(didVal) {
	case RPM_DID: // RPM = u16be / 4
		if len(dataBytes) >= 2 {
			raw := int(dataBytes[0])<<8 | int(dataBytes[1])
			rpm := raw / 4
			eventHub.Broadcast(map[string]any{"rpm": rpm})
			rpmHistoryLabels = append(rpmHistoryLabels, timestamp)
			rpmHistoryData = append(rpmHistoryData, rpm)
		}

	case THROTTLE_DID: // Throttle: (0..255?) no fucking clue what this is smoking, I think this is computed target throttle?
		if len(dataBytes) >= 1 {
			raw8 := int(dataBytes[len(dataBytes)-1])
			//pct := scalePct(raw8, 3, 17) // -> 0..100%
			eventHub.Broadcast(map[string]any{"throttle": raw8})
		}

	case GRIP_DID: // Grip: (0..255) gives raw pot value in percent from the grip (throttle twist)
		if len(dataBytes) >= 1 {
			raw8 := int(dataBytes[len(dataBytes)-1])
			//pct := scalePct(raw8, 20, 59) // -> 0..100%
			eventHub.Broadcast(map[string]any{"grip": raw8})
		}

	case TPS_DID: // TPS (0..1023) -> %
		if len(dataBytes) >= 2 {
			raw := int(dataBytes[0])<<8 | int(dataBytes[1])
			if raw > 1023 {
				raw = 1023
			}
			pct := (raw*100 + 511) / 1023 // integer rounding
			eventHub.Broadcast(map[string]any{"tps": pct})
			tpsHistoryLabels = append(tpsHistoryLabels, timestamp)
			tpsHistoryData = append(tpsHistoryData, pct)
		}

	case COOLANT_DID: // Coolant °C
		if len(dataBytes) >= 2 {
			val := int(dataBytes[0])<<8 | int(dataBytes[1])
			eventHub.Broadcast(map[string]any{"coolant": val - 40})
		} else if len(dataBytes) == 1 {
			eventHub.Broadcast(map[string]any{"coolant": int(dataBytes[0]) - 40})
		}
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
