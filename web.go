package main

import (
	"fmt"
	ds "github.com/starfederation/datastar-go/datastar"
	"net/http"
	"strings"
)

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
	Data        []GraphData
}

var charts = []chartProps{
	{"TPS", "Throotle", nil},
	{"RPM", "Revvie wevvy", nil},
}

func buildUpdateChartScript(name string, label, data int) string {
	return fmt.Sprintf(`(function(){
		let chart = Chart.getChart("%s-chart");
		chart.data.datasets[0].data.push({x: %d, y: %d});


		if (chart.data.datasets[0].length > 50) {
			chart.data.datasets[0].shift();
		}
	})()`, strings.ToLower(name), label, data)
}

func IndexHandler(w http.ResponseWriter, _ *http.Request) {
	err := Templates.ExecuteTemplate(w, "index", map[string]interface{}{
		"cards": cards,
		"tpsChartProps": chartProps{
			Name:        "TPS",
			Description: "Throttle Position Sensor",
			Data:        tpsHistory,
		},
		"rpmChartProps": chartProps{
			Name:        "RPM",
			Description: "Revolutions Per Minute",
			Data:        rpmHistory,
		},
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func EventsHandler(w http.ResponseWriter, r *http.Request) {
	sse := ds.NewSSE(w, r)

	_, ch, cancel := EventHub.Subscribe()
	defer cancel()

	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-ch:
			updateFunc := generatePatch(event)
			err := updateFunc(sse)
			if err != nil {
				fmt.Println(err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
	}
}
